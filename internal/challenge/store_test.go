package challenge

import (
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "challenges.db")
	s, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() {
		_ = s.Close()
	})
	return s, path
}

func TestStoreAddAndForPlayer(t *testing.T) {
	s, _ := newTestStore(t)

	c := &PendingChallenge{
		ChallengeURI:  "at://did:plc:alice/app.atchess.challenge/abc",
		ChallengerDID: "did:plc:alice",
		ChallengedDID: "did:plc:bob",
		CreatedAt:     time.Now(),
		ExpiresAt:     time.Now().Add(24 * time.Hour),
	}

	added, err := s.Add(c)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if !added {
		t.Fatal("expected Add to return true for new challenge")
	}
	added, err = s.Add(c)
	if err != nil {
		t.Fatalf("Add (duplicate): %v", err)
	}
	if added {
		t.Fatal("expected Add to return false for duplicate challenge")
	}

	got, err := s.ForPlayer("did:plc:bob")
	if err != nil {
		t.Fatalf("ForPlayer: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 challenge, got %d", len(got))
	}
	if got[0].ChallengeURI != c.ChallengeURI {
		t.Errorf("wrong URI: %s", got[0].ChallengeURI)
	}

	got, err = s.ForPlayer("did:plc:alice")
	if err != nil {
		t.Fatalf("ForPlayer(alice): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected 0 challenges for alice, got %d", len(got))
	}
}

// TestStore_ForPlayer_DoesNotLeakOtherPlayersChallenges is the security
// property the whole point of the DID-keyed index rests on: a player must
// never see a challenge addressed to someone else, even when both are
// present in the same store.
func TestStore_ForPlayer_DoesNotLeakOtherPlayersChallenges(t *testing.T) {
	s, _ := newTestStore(t)

	toBob := &PendingChallenge{
		ChallengeURI:  "at://did:plc:alice/app.atchess.challenge/to-bob",
		ChallengerDID: "did:plc:alice",
		ChallengedDID: "did:plc:bob",
		CreatedAt:     time.Now(),
		ExpiresAt:     time.Now().Add(24 * time.Hour),
	}
	toCarol := &PendingChallenge{
		ChallengeURI:  "at://did:plc:alice/app.atchess.challenge/to-carol",
		ChallengerDID: "did:plc:alice",
		ChallengedDID: "did:plc:carol",
		CreatedAt:     time.Now(),
		ExpiresAt:     time.Now().Add(24 * time.Hour),
	}
	if _, err := s.Add(toBob); err != nil {
		t.Fatalf("Add(toBob): %v", err)
	}
	if _, err := s.Add(toCarol); err != nil {
		t.Fatalf("Add(toCarol): %v", err)
	}

	bobsChallenges, err := s.ForPlayer("did:plc:bob")
	if err != nil {
		t.Fatalf("ForPlayer(bob): %v", err)
	}
	if len(bobsChallenges) != 1 || bobsChallenges[0].ChallengeURI != toBob.ChallengeURI {
		t.Fatalf("bob should see exactly his own challenge, got %+v", bobsChallenges)
	}
	for _, c := range bobsChallenges {
		if c.ChallengeURI == toCarol.ChallengeURI {
			t.Fatalf("bob must not see carol's challenge")
		}
	}

	carolsChallenges, err := s.ForPlayer("did:plc:carol")
	if err != nil {
		t.Fatalf("ForPlayer(carol): %v", err)
	}
	if len(carolsChallenges) != 1 || carolsChallenges[0].ChallengeURI != toCarol.ChallengeURI {
		t.Fatalf("carol should see exactly her own challenge, got %+v", carolsChallenges)
	}
	for _, c := range carolsChallenges {
		if c.ChallengeURI == toBob.ChallengeURI {
			t.Fatalf("carol must not see bob's challenge")
		}
	}
}

func TestStoreExpiration(t *testing.T) {
	s, _ := newTestStore(t)

	expired := &PendingChallenge{
		ChallengeURI:  "at://did:plc:alice/app.atchess.challenge/old",
		ChallengerDID: "did:plc:alice",
		ChallengedDID: "did:plc:bob",
		CreatedAt:     time.Now().Add(-48 * time.Hour),
		ExpiresAt:     time.Now().Add(-1 * time.Hour),
	}
	valid := &PendingChallenge{
		ChallengeURI:  "at://did:plc:alice/app.atchess.challenge/new",
		ChallengerDID: "did:plc:alice",
		ChallengedDID: "did:plc:bob",
		CreatedAt:     time.Now(),
		ExpiresAt:     time.Now().Add(24 * time.Hour),
	}

	if _, err := s.Add(expired); err != nil {
		t.Fatalf("Add(expired): %v", err)
	}
	if _, err := s.Add(valid); err != nil {
		t.Fatalf("Add(valid): %v", err)
	}

	// ForPlayer should only return non-expired
	got, err := s.ForPlayer("did:plc:bob")
	if err != nil {
		t.Fatalf("ForPlayer: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 non-expired challenge, got %d", len(got))
	}

	// PruneExpired should clean up
	pruned, err := s.PruneExpired()
	if err != nil {
		t.Fatalf("PruneExpired: %v", err)
	}
	if pruned != 1 {
		t.Fatalf("expected 1 pruned, got %d", pruned)
	}
}

// TestStore_DeclineSurvivesReplayAndRestart is the atchess-1c9.47
// regression test: a declined challenge must NOT reappear -- neither from
// an idempotent replay of the same "create" event within the same process,
// nor after the store is closed and reopened from the same file (the
// atchess-1c9.50 durability claim).
func TestStore_DeclineSurvivesReplayAndRestart(t *testing.T) {
	s, path := newTestStore(t)

	c := &PendingChallenge{
		ChallengeURI:  "at://did:plc:alice/app.atchess.challenge/abc",
		ChallengerDID: "did:plc:alice",
		ChallengedDID: "did:plc:bob",
		CreatedAt:     time.Now(),
		ExpiresAt:     time.Now().Add(24 * time.Hour),
	}
	if added, err := s.Add(c); err != nil || !added {
		t.Fatalf("initial Add: added=%v err=%v", added, err)
	}

	if err := s.Remove(c.ChallengeURI); err != nil {
		t.Fatalf("Remove (decline): %v", err)
	}

	got, err := s.ForPlayer("did:plc:bob")
	if err != nil {
		t.Fatalf("ForPlayer after decline: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected 0 after decline, got %d", len(got))
	}

	// Replay of the same "create" event (e.g. the firehose or a rerun
	// backfill seeing the same record again) must NOT resurrect it.
	added, err := s.Add(c)
	if err != nil {
		t.Fatalf("Add (replay after decline): %v", err)
	}
	if added {
		t.Fatal("expected Add to report the URI as already present, not newly added, after a decline")
	}
	got, err = s.ForPlayer("did:plc:bob")
	if err != nil {
		t.Fatalf("ForPlayer after replay: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected declined challenge to stay declined after replay, got %d", len(got))
	}

	// Close and reopen from the same file: the decline must survive a
	// restart.
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore (reopen): %v", err)
	}
	defer reopened.Close()

	got, err = reopened.ForPlayer("did:plc:bob")
	if err != nil {
		t.Fatalf("ForPlayer after reopen: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected declined challenge to stay declined after restart, got %d", len(got))
	}

	// And a replay against the reopened store still must not resurrect it.
	added, err = reopened.Add(c)
	if err != nil {
		t.Fatalf("Add (replay after restart): %v", err)
	}
	if added {
		t.Fatal("expected Add to report the URI as already present after restart, not newly added")
	}
}

// TestStore_DurableAcrossRestart is atchess-1c9.50's core claim in
// isolation, independent of decline: data written before a close is still
// there after reopening the same file. This is what distinguishes the new
// Store from the in-memory cache it replaces.
func TestStore_DurableAcrossRestart(t *testing.T) {
	s, path := newTestStore(t)

	c := &PendingChallenge{
		ChallengeURI:     "at://did:plc:alice/app.atchess.challenge/durable",
		ChallengerDID:    "did:plc:alice",
		ChallengerHandle: "alice.test",
		ChallengedDID:    "did:plc:bob",
		Color:            "white",
		Message:          "hello",
		ProposedGameID:   "game-1",
		CreatedAt:        time.Now().Truncate(time.Second),
		ExpiresAt:        time.Now().Add(24 * time.Hour).Truncate(time.Second),
	}
	if _, err := s.Add(c); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore (reopen): %v", err)
	}
	defer reopened.Close()

	got, err := reopened.ForPlayer("did:plc:bob")
	if err != nil {
		t.Fatalf("ForPlayer after reopen: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected the challenge written before restart to still be there, got %d", len(got))
	}
	if got[0].ChallengeURI != c.ChallengeURI || got[0].ChallengerHandle != c.ChallengerHandle || got[0].Message != c.Message {
		t.Fatalf("challenge fields did not survive restart intact: %+v", got[0])
	}
}

// TestStore_IdempotentReplay verifies that replaying the exact same
// challenge event any number of times never produces more than one row
// for that URI.
func TestStore_IdempotentReplay(t *testing.T) {
	s, _ := newTestStore(t)

	c := &PendingChallenge{
		ChallengeURI:  "at://did:plc:alice/app.atchess.challenge/dup",
		ChallengerDID: "did:plc:alice",
		ChallengedDID: "did:plc:bob",
		CreatedAt:     time.Now(),
		ExpiresAt:     time.Now().Add(24 * time.Hour),
	}

	for i := 0; i < 5; i++ {
		if _, err := s.Add(c); err != nil {
			t.Fatalf("Add (iteration %d): %v", i, err)
		}
	}

	got, err := s.ForPlayer("did:plc:bob")
	if err != nil {
		t.Fatalf("ForPlayer: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 row after 5 replays, got %d", len(got))
	}
}

func TestStoreRemove(t *testing.T) {
	s, _ := newTestStore(t)

	c := &PendingChallenge{
		ChallengeURI:  "at://did:plc:alice/app.atchess.challenge/abc",
		ChallengerDID: "did:plc:alice",
		ChallengedDID: "did:plc:bob",
		CreatedAt:     time.Now(),
		ExpiresAt:     time.Now().Add(24 * time.Hour),
	}
	if _, err := s.Add(c); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := s.Remove(c.ChallengeURI); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	got, err := s.ForPlayer("did:plc:bob")
	if err != nil {
		t.Fatalf("ForPlayer: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected 0 after remove, got %d", len(got))
	}

	// Removing an unknown URI is not an error (mirrors the old in-memory
	// Store's no-op-if-unknown behavior).
	if err := s.Remove("at://did:plc:nobody/app.atchess.challenge/never-existed"); err != nil {
		t.Fatalf("Remove(unknown URI) should not error, got %v", err)
	}
}

// TestStore_MarkRemoved_TombstonesDeletedChallenge covers the delete/
// tombstone case: a challenge whose record was deleted upstream (from the
// challenger's repo) must not linger as open, and a later out-of-order
// replay of its original "create" must not resurrect it either.
func TestStore_MarkRemoved_TombstonesDeletedChallenge(t *testing.T) {
	s, _ := newTestStore(t)

	c := &PendingChallenge{
		ChallengeURI:  "at://did:plc:alice/app.atchess.challenge/abc",
		ChallengerDID: "did:plc:alice",
		ChallengedDID: "did:plc:bob",
		CreatedAt:     time.Now(),
		ExpiresAt:     time.Now().Add(24 * time.Hour),
	}
	if _, err := s.Add(c); err != nil {
		t.Fatalf("Add: %v", err)
	}

	if err := s.MarkRemoved(c.ChallengeURI, c.ChallengerDID, c.ChallengedDID); err != nil {
		t.Fatalf("MarkRemoved: %v", err)
	}

	got, err := s.ForPlayer("did:plc:bob")
	if err != nil {
		t.Fatalf("ForPlayer: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected 0 after MarkRemoved, got %d", len(got))
	}

	// An out-of-order replay of the original create must not resurrect it.
	added, err := s.Add(c)
	if err != nil {
		t.Fatalf("Add (replay after MarkRemoved): %v", err)
	}
	if added {
		t.Fatal("expected Add to report the URI as already present after MarkRemoved")
	}
	got, err = s.ForPlayer("did:plc:bob")
	if err != nil {
		t.Fatalf("ForPlayer after replay: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected removed challenge to stay removed after replay, got %d", len(got))
	}
}

// TestStore_MarkRemoved_BeforeCreate covers observing a delete before ever
// observing the corresponding create (e.g. this instance started watching
// partway through the challenge's lifetime): MarkRemoved must still leave
// a durable tombstone so a LATER out-of-order create replay cannot
// resurrect it.
func TestStore_MarkRemoved_BeforeCreate(t *testing.T) {
	s, _ := newTestStore(t)

	uri := "at://did:plc:alice/app.atchess.challenge/never-seen-created"
	if err := s.MarkRemoved(uri, "did:plc:alice", "did:plc:bob"); err != nil {
		t.Fatalf("MarkRemoved: %v", err)
	}

	c := &PendingChallenge{
		ChallengeURI:  uri,
		ChallengerDID: "did:plc:alice",
		ChallengedDID: "did:plc:bob",
		CreatedAt:     time.Now(),
		ExpiresAt:     time.Now().Add(24 * time.Hour),
	}
	added, err := s.Add(c)
	if err != nil {
		t.Fatalf("Add (create arriving after delete): %v", err)
	}
	if added {
		t.Fatal("expected Add to report the URI as already tombstoned, not newly added")
	}

	got, err := s.ForPlayer("did:plc:bob")
	if err != nil {
		t.Fatalf("ForPlayer: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected tombstoned challenge to stay excluded, got %d", len(got))
	}
}

// TestStore_QueryErrorSurfaces proves ForPlayer returns a genuine error
// (not an empty, successful-looking result) when the underlying database
// is unusable -- the property atchess-1c9.51 established and this store
// must not regress.
func TestStore_QueryErrorSurfaces(t *testing.T) {
	s, _ := newTestStore(t)

	// Sabotage the schema out from under the store to force a query
	// failure that has nothing to do with "no rows".
	if _, err := s.db.Exec(`DROP TABLE challenges`); err != nil {
		t.Fatalf("dropping table for test setup: %v", err)
	}

	_, err := s.ForPlayer("did:plc:bob")
	if err == nil {
		t.Fatal("expected ForPlayer to return an error when the underlying table is gone, not a nil error with an empty result")
	}
}

// TestStore_AddErrorSurfaces is Add's equivalent of
// TestStore_QueryErrorSurfaces: a write failure must be reported, not
// silently discarded (which, prior to atchess-1c9.50, is exactly what the
// old in-memory Store's bool-only Add signature made impossible to even
// express).
func TestStore_AddErrorSurfaces(t *testing.T) {
	s, _ := newTestStore(t)

	if _, err := s.db.Exec(`DROP TABLE challenges`); err != nil {
		t.Fatalf("dropping table for test setup: %v", err)
	}

	_, err := s.Add(&PendingChallenge{
		ChallengeURI:  "at://did:plc:alice/app.atchess.challenge/abc",
		ChallengerDID: "did:plc:alice",
		ChallengedDID: "did:plc:bob",
		CreatedAt:     time.Now(),
		ExpiresAt:     time.Now().Add(24 * time.Hour),
	})
	if err == nil {
		t.Fatal("expected Add to return an error when the underlying table is gone")
	}
}

// TestStore_ConcurrentAccess exercises Add/ForPlayer/Remove from many
// goroutines at once against a single Store, for both correctness (run
// under `go test -race`) and to make sure SetMaxOpenConns(1) serialization
// doesn't deadlock or drop writes under concurrent load.
func TestStore_ConcurrentAccess(t *testing.T) {
	s, _ := newTestStore(t)

	const n = 50
	var wg sync.WaitGroup
	wg.Add(n * 2)

	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			_, err := s.Add(&PendingChallenge{
				ChallengeURI:  "at://did:plc:alice/app.atchess.challenge/" + sqlSafeRkey(i),
				ChallengerDID: "did:plc:alice",
				ChallengedDID: "did:plc:bob",
				CreatedAt:     time.Now(),
				ExpiresAt:     time.Now().Add(24 * time.Hour),
			})
			if err != nil {
				t.Errorf("concurrent Add: %v", err)
			}
		}()
		go func() {
			defer wg.Done()
			if _, err := s.ForPlayer("did:plc:bob"); err != nil {
				t.Errorf("concurrent ForPlayer: %v", err)
			}
		}()
	}
	wg.Wait()

	got, err := s.ForPlayer("did:plc:bob")
	if err != nil {
		t.Fatalf("ForPlayer: %v", err)
	}
	if len(got) != n {
		t.Fatalf("expected %d distinct challenges after concurrent adds, got %d", n, len(got))
	}
}

func sqlSafeRkey(i int) string {
	return "rkey-" + string(rune('a'+i%26)) + string(rune('0'+i/26))
}

func TestFromChallengeRecord(t *testing.T) {
	record := map[string]interface{}{
		"challenged":       "did:plc:bob",
		"challenger":       "did:plc:alice",
		"challengerHandle": "alice.test",
		"color":            "white",
		"message":          "gg",
		"proposedGameId":   "game123",
		"createdAt":        "2026-01-01T00:00:00Z",
		"expiresAt":        "2026-01-02T00:00:00Z",
	}

	pc := FromChallengeRecord("did:plc:alice", "abc123", "bafycid", record)

	if pc.ChallengeURI != "at://did:plc:alice/app.atchess.challenge/abc123" {
		t.Errorf("unexpected ChallengeURI: %s", pc.ChallengeURI)
	}
	if pc.ChallengeCID != "bafycid" {
		t.Errorf("unexpected ChallengeCID: %s", pc.ChallengeCID)
	}
	if pc.ChallengerDID != "did:plc:alice" || pc.ChallengedDID != "did:plc:bob" {
		t.Errorf("unexpected DIDs: challenger=%s challenged=%s", pc.ChallengerDID, pc.ChallengedDID)
	}
	if pc.ChallengerHandle != "alice.test" {
		t.Errorf("unexpected ChallengerHandle: %s", pc.ChallengerHandle)
	}
	if pc.Color != "white" || pc.Message != "gg" || pc.ProposedGameID != "game123" {
		t.Errorf("unexpected fields: color=%s message=%s proposedGameID=%s", pc.Color, pc.Message, pc.ProposedGameID)
	}
	if pc.CreatedAt.Format(time.RFC3339) != "2026-01-01T00:00:00Z" {
		t.Errorf("unexpected CreatedAt: %v", pc.CreatedAt)
	}
	if pc.ExpiresAt.Format(time.RFC3339) != "2026-01-02T00:00:00Z" {
		t.Errorf("unexpected ExpiresAt: %v", pc.ExpiresAt)
	}
}

func TestFromChallengeRecord_DefaultsWhenTimestampsMissing(t *testing.T) {
	record := map[string]interface{}{
		"challenged": "did:plc:bob",
		"challenger": "did:plc:alice",
	}

	before := time.Now()
	pc := FromChallengeRecord("did:plc:alice", "abc123", "bafycid", record)
	after := time.Now()

	if pc.CreatedAt.Before(before) || pc.CreatedAt.After(after) {
		t.Errorf("expected CreatedAt to default to now, got %v (window %v..%v)", pc.CreatedAt, before, after)
	}
	if !pc.ExpiresAt.Equal(pc.CreatedAt.Add(24 * time.Hour)) {
		t.Errorf("expected ExpiresAt to default to CreatedAt+24h, got created=%v expires=%v", pc.CreatedAt, pc.ExpiresAt)
	}
}

// TestFromChallengeRecord_ForgedChallenger_Refused pins atchess-1c9.106's
// fix (a): a record whose self-reported "challenger" disagrees with the
// repo it was actually hosted in (repoDID -- what the firehose/backfill
// caller observed the record living in, never trusted from any field
// inside the record itself) must be refused outright, not merely
// mislabeled. Without this, Mallory could write an app.atchess.challenge
// into HER OWN repo naming Carol as "challenger", and it would be indexed
// and broadcast to Bob as if it came from Carol.
func TestFromChallengeRecord_ForgedChallenger_Refused(t *testing.T) {
	record := map[string]interface{}{
		"challenged":     "did:plc:bob",
		"challenger":     "did:plc:carol", // claimed challenger
		"color":          "white",
		"proposedGameId": "game123",
		"createdAt":      "2026-01-01T00:00:00Z",
		"expiresAt":      "2026-01-02T00:00:00Z",
	}

	// repoDID ("did:plc:mallory") is the repo that ACTUALLY hosted this
	// record -- it disagrees with the claimed "challenger" (Carol).
	pc := FromChallengeRecord("did:plc:mallory", "forged1", "bafycid", record)

	if pc != nil {
		t.Fatalf("expected FromChallengeRecord to refuse a forged challenger and return nil, got %#v", pc)
	}
}

func TestBuildChallengeURI(t *testing.T) {
	got := BuildChallengeURI("did:plc:alice", "abc123")
	want := "at://did:plc:alice/app.atchess.challenge/abc123"
	if got != want {
		t.Errorf("BuildChallengeURI = %q, want %q", got, want)
	}
}

// ensure sql.ErrNoRows itself never leaks out of ForPlayer as a
// success-shaped "no error" -- sanity check that ForPlayer's error
// handling doesn't accidentally treat a driver sentinel as "no rows found,
// return empty" when it doesn't apply to a multi-row Query in the first
// place (defensive; ForPlayer uses Query, not QueryRow, so this should
// never trigger, but pins the assumption explicitly).
func TestStore_ForPlayer_EmptyIsNotAnError(t *testing.T) {
	s, _ := newTestStore(t)
	got, err := s.ForPlayer("did:plc:nobody-has-challenged-this-did")
	if err != nil {
		t.Fatalf("expected no error for a DID with zero challenges, got %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty result, got %d", len(got))
	}
	if errors.Is(err, sql.ErrNoRows) {
		t.Fatal("ForPlayer must never return sql.ErrNoRows for an empty result")
	}
}

// rowStatus reads the raw status column for uri directly, bypassing
// ForPlayer's status/expiry filtering, so tests can assert what
// PruneExpired actually left behind in the table (not just what
// ForPlayer would currently surface to a caller).
func rowStatus(t *testing.T, s *Store, uri string) (status string, found bool) {
	t.Helper()
	err := s.db.QueryRow(`SELECT status FROM challenges WHERE uri = ?`, uri).Scan(&status)
	if err == sql.ErrNoRows {
		return "", false
	}
	if err != nil {
		t.Fatalf("rowStatus(%s): %v", uri, err)
	}
	return status, true
}

// TestStore_PruneExpired_OnlyDeletesExpiredOpenRows is the atchess-1c9.47
// part-1 control the bead calls for explicitly: a prune that deleted
// EVERYTHING would still make TestStoreExpiration's "1 pruned" assertion
// pass, since that test only ever adds one expired row. This test adds an
// expired row and a non-expired row side by side and asserts BOTH
// outcomes -- the expired one is gone, the non-expired one survives --
// so a mutant that prunes unconditionally is caught here even if it
// happens to slip past a narrower test.
func TestStore_PruneExpired_OnlyDeletesExpiredOpenRows(t *testing.T) {
	s, _ := newTestStore(t)

	expired := &PendingChallenge{
		ChallengeURI:  "at://did:plc:alice/app.atchess.challenge/expired-open",
		ChallengerDID: "did:plc:alice",
		ChallengedDID: "did:plc:bob",
		CreatedAt:     time.Now().Add(-48 * time.Hour),
		ExpiresAt:     time.Now().Add(-1 * time.Hour),
	}
	notExpired := &PendingChallenge{
		ChallengeURI:  "at://did:plc:alice/app.atchess.challenge/still-open",
		ChallengerDID: "did:plc:alice",
		ChallengedDID: "did:plc:bob",
		CreatedAt:     time.Now(),
		ExpiresAt:     time.Now().Add(24 * time.Hour),
	}
	if _, err := s.Add(expired); err != nil {
		t.Fatalf("Add(expired): %v", err)
	}
	if _, err := s.Add(notExpired); err != nil {
		t.Fatalf("Add(notExpired): %v", err)
	}

	pruned, err := s.PruneExpired()
	if err != nil {
		t.Fatalf("PruneExpired: %v", err)
	}
	if pruned != 1 {
		t.Fatalf("expected exactly 1 row pruned, got %d", pruned)
	}

	if _, found := rowStatus(t, s, expired.ChallengeURI); found {
		t.Fatal("expected the expired open challenge's row to be gone after PruneExpired")
	}
	status, found := rowStatus(t, s, notExpired.ChallengeURI)
	if !found {
		t.Fatal("expected the NON-expired challenge's row to still exist after PruneExpired -- a prune that deletes everything would wrongly pass a test that only checks the expired row")
	}
	if status != statusOpen {
		t.Fatalf("expected the non-expired row's status to remain %q, got %q", statusOpen, status)
	}
}

// TestStore_PruneExpired_RetainsDeclinedTombstone covers the tombstone
// retention policy documented on PruneExpired: an EXPIRED declined
// challenge's row must survive a prune (never deleted, per policy), and
// -- the actual property that matters -- a replay of the original create
// after that prune run must still not resurrect it as open. This is the
// atchess-1c9.47 resurrection scenario the bead calls out: pruning a
// tombstone too early lets a firehose replay bring the challenge back.
func TestStore_PruneExpired_RetainsDeclinedTombstone(t *testing.T) {
	s, _ := newTestStore(t)

	c := &PendingChallenge{
		ChallengeURI:  "at://did:plc:alice/app.atchess.challenge/declined-expired",
		ChallengerDID: "did:plc:alice",
		ChallengedDID: "did:plc:bob",
		CreatedAt:     time.Now().Add(-48 * time.Hour),
		ExpiresAt:     time.Now().Add(-1 * time.Hour), // already expired
	}
	if _, err := s.Add(c); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := s.Remove(c.ChallengeURI); err != nil {
		t.Fatalf("Remove (decline): %v", err)
	}

	status, found := rowStatus(t, s, c.ChallengeURI)
	if !found || status != statusDeclined {
		t.Fatalf("expected row to be status %q before prune, got status=%q found=%v", statusDeclined, status, found)
	}

	pruned, err := s.PruneExpired()
	if err != nil {
		t.Fatalf("PruneExpired: %v", err)
	}
	if pruned != 0 {
		t.Fatalf("expected PruneExpired to leave the declined tombstone alone (0 rows pruned), got %d", pruned)
	}

	status, found = rowStatus(t, s, c.ChallengeURI)
	if !found {
		t.Fatal("expected the declined tombstone row to still exist after PruneExpired")
	}
	if status != statusDeclined {
		t.Fatalf("expected tombstone status to remain %q after PruneExpired, got %q", statusDeclined, status)
	}

	// The property that actually matters: a replay of the original
	// create, arriving after the tombstone survived a prune, must still
	// not resurrect the challenge.
	added, err := s.Add(c)
	if err != nil {
		t.Fatalf("Add (replay after prune): %v", err)
	}
	if added {
		t.Fatal("expected Add to report the URI as already present (tombstone retained), not newly added, after PruneExpired")
	}
	got, err := s.ForPlayer("did:plc:bob")
	if err != nil {
		t.Fatalf("ForPlayer after replay: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected declined challenge to stay suppressed after prune + replay, got %d", len(got))
	}
}

// TestStore_PruneExpired_RetainsRemovedTombstoneInsertedBeforeCreate
// exercises the specific MarkRemoved code path the PruneExpired doc
// comment calls out as the reason expires_at cannot be reused as a
// tombstone-pruning cutoff: a delete observed with no prior row inserts a
// tombstone whose expires_at is the tombstoning time itself, which is
// already in the past by the time any later PruneExpired call runs. If
// PruneExpired pruned tombstones on that basis, this row would be deleted
// on its very first opportunity and a later out-of-order create replay
// would resurrect it.
func TestStore_PruneExpired_RetainsRemovedTombstoneInsertedBeforeCreate(t *testing.T) {
	s, _ := newTestStore(t)

	uri := BuildChallengeURI("did:plc:alice", "before-create")
	if err := s.MarkRemoved(uri, "did:plc:alice", "did:plc:bob"); err != nil {
		t.Fatalf("MarkRemoved (before any create): %v", err)
	}

	status, found := rowStatus(t, s, uri)
	if !found || status != statusRemoved {
		t.Fatalf("expected tombstone row status %q, got status=%q found=%v", statusRemoved, status, found)
	}

	// This tombstone's expires_at is "now" at MarkRemoved time -- by the
	// time PruneExpired runs, it is necessarily <= now.
	pruned, err := s.PruneExpired()
	if err != nil {
		t.Fatalf("PruneExpired: %v", err)
	}
	if pruned != 0 {
		t.Fatalf("expected PruneExpired to leave the pre-create removed tombstone alone, got %d pruned", pruned)
	}

	// A later out-of-order "create" replay for the same URI must still
	// not resurrect it.
	c := &PendingChallenge{
		ChallengeURI:  uri,
		ChallengerDID: "did:plc:alice",
		ChallengedDID: "did:plc:bob",
		CreatedAt:     time.Now(),
		ExpiresAt:     time.Now().Add(24 * time.Hour),
	}
	added, err := s.Add(c)
	if err != nil {
		t.Fatalf("Add (late create replay): %v", err)
	}
	if added {
		t.Fatal("expected Add to report the URI as already present after PruneExpired, not newly added")
	}
	got, err := s.ForPlayer("did:plc:bob")
	if err != nil {
		t.Fatalf("ForPlayer after late replay: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected removed challenge to stay suppressed after prune + late create replay, got %d", len(got))
	}
}

// TestFromChallengeRecord_FarFutureExpiresAt_Clamped is the atchess-1c9.107
// regression test. FromChallengeRecord previously took a record's
// self-reported "expiresAt" verbatim once it parsed -- the 24h default
// (see the doc comment above FromChallengeRecord) applied ONLY when the
// field was missing or unparseable. A record claiming a year-3000 expiry
// was therefore never pruned by PruneExpired and never filtered out by
// ForPlayer's ExpiresAt.After(now) check: it sat in the challenged
// player's notification list indefinitely (not exploitable -- see
// atchess-1c9.106 -- but unbounded attacker-controlled durable state and
// a nuisance vector).
//
// This asserts the fix: expiresAt is clamped to createdAt+24h (the same
// ceiling CreateChallenge itself writes, internal/atproto/client.go),
// applied to a PARSED value, not just a missing one. It then proves the
// clamp actually closes the hole end-to-end by running PruneExpired once
// the clamped time has passed.
func TestFromChallengeRecord_FarFutureExpiresAt_Clamped(t *testing.T) {
	record := map[string]interface{}{
		"challenged": "did:plc:bob",
		"challenger": "did:plc:alice",
		"createdAt":  "2026-01-01T00:00:00Z",
		"expiresAt":  "3000-01-01T00:00:00Z", // absurd, attacker-controlled
	}

	pc := FromChallengeRecord("did:plc:alice", "farfuture1", "bafycid", record)
	if pc == nil {
		t.Fatal("expected a non-nil PendingChallenge")
	}

	wantMax := pc.CreatedAt.Add(24 * time.Hour)
	if !pc.ExpiresAt.Equal(wantMax) {
		t.Fatalf("expected ExpiresAt clamped to CreatedAt+24h (%v), got %v", wantMax, pc.ExpiresAt)
	}

	// Prove the clamp actually closes the hole: once the clamped expiry
	// has passed, PruneExpired must delete the row. Backdate CreatedAt
	// (via the record's own createdAt) so the clamped ExpiresAt is
	// already in the past by the time we prune.
	record["createdAt"] = "2020-01-01T00:00:00Z"
	pcPast := FromChallengeRecord("did:plc:alice", "farfuture2", "bafycid2", record)
	if pcPast == nil {
		t.Fatal("expected a non-nil PendingChallenge")
	}
	if !pcPast.ExpiresAt.Before(time.Now()) {
		t.Fatalf("expected clamped ExpiresAt to be in the past given an old createdAt, got %v", pcPast.ExpiresAt)
	}

	s, _ := newTestStore(t)
	if _, err := s.Add(pcPast); err != nil {
		t.Fatalf("Add: %v", err)
	}

	pruned, err := s.PruneExpired()
	if err != nil {
		t.Fatalf("PruneExpired: %v", err)
	}
	if pruned != 1 {
		t.Fatalf("expected the clamped far-future challenge to be pruned once its clamped expiry passed, got pruned=%d", pruned)
	}
}

// TestFromChallengeRecord_LegitimateExpiresAt_Preserved is the regression
// control for TestFromChallengeRecord_FarFutureExpiresAt_Clamped: a
// record whose self-reported expiresAt is within the clamp bound
// (CreatedAt+24h, inclusive) must be preserved UNCHANGED, not silently
// shortened. TestFromChallengeRecord above already covers the exact-24h
// boundary; this covers a value strictly inside it.
func TestFromChallengeRecord_LegitimateExpiresAt_Preserved(t *testing.T) {
	record := map[string]interface{}{
		"challenged": "did:plc:bob",
		"challenger": "did:plc:alice",
		"createdAt":  "2026-01-01T00:00:00Z",
		"expiresAt":  "2026-01-01T12:00:00Z", // 12h later, well within 24h bound
	}

	pc := FromChallengeRecord("did:plc:alice", "legit1", "bafycid", record)
	if pc == nil {
		t.Fatal("expected a non-nil PendingChallenge")
	}
	want, _ := time.Parse(time.RFC3339, "2026-01-01T12:00:00Z")
	if !pc.ExpiresAt.Equal(want) {
		t.Fatalf("expected legitimate ExpiresAt preserved unchanged, got %v want %v", pc.ExpiresAt, want)
	}
}

// TestFromChallengeRecord_FarFutureCreatedAtAndExpiresAt_BothClamped is the
// atchess-1c9.107 REVIEW-FIX regression test. The first fix for this bead
// anchored the expiresAt ceiling to createdAt+24h, but createdAt itself
// comes from the SAME untrusted record as expiresAt. A record that claims
// a far-future createdAt AND a far-future expiresAt together defeated
// that first fix: the ceiling simply moved out to match the forged
// createdAt, so expiresAt was never actually bounded -- PruneExpired
// removed 0 rows and ForPlayer returned the row forever, an exact repeat
// of the original bug via one extra forged field. Proven by review with a
// probe:
//
//	record: createdAt "3000-01-01T00:00:00Z", expiresAt "3000-01-01T12:00:00Z"
//	ExpiresAt after clamp = 3000-01-01 12:00:00 UTC   // inside the ceiling, not clamped
//	PruneExpired removed 0 rows
//	ForPlayer returned 1 rows                          // still visible forever
//
// The fix anchors the ceiling to a BOUNDED createdAt: createdAt is only
// trusted as the anchor when it is no more than
// challengeClockSkewTolerance ahead of our local clock (ordinary clock
// skew from a remote PDS); beyond that it is untrusted and the anchor
// falls back to "now". This test asserts the resulting ExpiresAt is
// bounded near now+24h (NOT the record's year-3000 claim), then -- since
// the point is that this row is no longer permanently unprunable, only
// bounded to at most ~24h -- proves prunability directly by
// fast-forwarding the STORED row's expiry into the past (there is no
// clock to inject into FromChallengeRecord itself, so this simulates what
// happens once wall time actually reaches the clamped point) and
// confirming PruneExpired removes it.
func TestFromChallengeRecord_FarFutureCreatedAtAndExpiresAt_BothClamped(t *testing.T) {
	record := map[string]interface{}{
		"challenged": "did:plc:bob",
		"challenger": "did:plc:alice",
		"createdAt":  "3000-01-01T00:00:00Z",
		"expiresAt":  "3000-01-01T12:00:00Z",
	}

	before := time.Now()
	pc := FromChallengeRecord("did:plc:alice", "bothfuture1", "bafycid", record)
	after := time.Now()

	if pc == nil {
		t.Fatal("expected a non-nil PendingChallenge")
	}
	if pc.ExpiresAt.Year() >= 3000 {
		t.Fatalf("expected ExpiresAt bounded away from the record's claimed year-3000 expiry, got %v", pc.ExpiresAt)
	}
	// anchorCreatedAt falls back to "now" here because the claimed
	// createdAt (year 3000) is far beyond challengeClockSkewTolerance
	// ahead of our clock, so the ceiling is bounded by
	// before.Add(24h)..after.Add(24h).
	minExpiresAt := before.Add(24 * time.Hour)
	maxExpiresAt := after.Add(24 * time.Hour)
	if pc.ExpiresAt.Before(minExpiresAt) || pc.ExpiresAt.After(maxExpiresAt) {
		t.Fatalf("expected ExpiresAt bounded to ~now+24h (%v..%v), got %v", minExpiresAt, maxExpiresAt, pc.ExpiresAt)
	}

	s, _ := newTestStore(t)
	if _, err := s.Add(pc); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Confirm it is NOT prunable yet -- correctly, since its bounded
	// expiry (~24h out) hasn't arrived. This is the crucial contrast with
	// the pre-fix bug: that row was not merely "not yet" prunable, it was
	// NEVER prunable (year 3000). This one will be, once real time
	// reaches its bounded expiry -- simulated below by fast-forwarding
	// the stored row directly.
	prunedTooEarly, err := s.PruneExpired()
	if err != nil {
		t.Fatalf("PruneExpired (too early): %v", err)
	}
	if prunedTooEarly != 0 {
		t.Fatalf("expected the still-live bounded challenge to survive an early prune, got pruned=%d", prunedTooEarly)
	}

	if _, err := s.db.Exec(`UPDATE challenges SET expires_at = ? WHERE uri = ?`,
		time.Now().Add(-1*time.Minute).UTC().Format(timeFormat), pc.ChallengeURI); err != nil {
		t.Fatalf("fast-forwarding stored expiry: %v", err)
	}

	pruned, err := s.PruneExpired()
	if err != nil {
		t.Fatalf("PruneExpired: %v", err)
	}
	if pruned != 1 {
		t.Fatalf("expected the bounded challenge to be pruned once its (simulated) clamped expiry passed, got pruned=%d", pruned)
	}
}

// TestFromChallengeRecord_CreatedAtWithinClockSkewTolerance_NotShortened is
// the clock-skew regression control: a createdAt that is only SLIGHTLY
// ahead of our local clock (within challengeClockSkewTolerance) is
// ordinary clock skew from a remote PDS, not an attack, and must NOT have
// its legitimate expiresAt window shortened.
func TestFromChallengeRecord_CreatedAtWithinClockSkewTolerance_NotShortened(t *testing.T) {
	skewedCreatedAt := time.Now().Add(2 * time.Minute) // within the 5m tolerance
	legitExpiresAt := skewedCreatedAt.Add(24 * time.Hour)

	record := map[string]interface{}{
		"challenged": "did:plc:bob",
		"challenger": "did:plc:alice",
		"createdAt":  skewedCreatedAt.Format(time.RFC3339),
		"expiresAt":  legitExpiresAt.Format(time.RFC3339),
	}

	pc := FromChallengeRecord("did:plc:alice", "skew1", "bafycid", record)
	if pc == nil {
		t.Fatal("expected a non-nil PendingChallenge")
	}
	want, _ := time.Parse(time.RFC3339, legitExpiresAt.Format(time.RFC3339))
	if !pc.ExpiresAt.Equal(want) {
		t.Fatalf("expected ExpiresAt preserved unchanged for a createdAt within clock-skew tolerance, got %v want %v", pc.ExpiresAt, want)
	}
}
