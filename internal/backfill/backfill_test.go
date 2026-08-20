package backfill

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/justinabrahms/atchess/internal/challenge"
	"github.com/rs/zerolog"
)

// newTestChallengeStore opens a fresh, file-backed challenge.Store under
// t.TempDir() (never a shared/global DB across tests).
func newTestChallengeStore(t *testing.T) *challenge.Store {
	t.Helper()
	s, err := challenge.NewStore(filepath.Join(t.TempDir(), "challenges.db"))
	if err != nil {
		t.Fatalf("challenge.NewStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

const (
	aliceDID = "did:plc:alice"
	bobDID   = "did:plc:bob"
	carolDID = "did:plc:carol"
)

// challengeRecord builds a minimal app.atchess.challenge record value.
func challengeRecord(challenger, challenged string) map[string]interface{} {
	now := time.Now().UTC().Format(time.RFC3339)
	return map[string]interface{}{
		"$type":            "app.atchess.challenge",
		"createdAt":        now,
		"expiresAt":        time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339),
		"challenger":       challenger,
		"challengerHandle": "challenger.test",
		"challenged":       challenged,
		"color":            "white",
	}
}

// newMockPDS starts an httptest server implementing just enough of
// com.atproto.sync.listRepos and com.atproto.repo.listRecords for these
// tests: repos maps a hosted DID to that repo's app.atchess.challenge
// records (rkey -> record value).
func newMockPDS(t *testing.T, repos map[string]map[string]map[string]interface{}) *httptest.Server {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("/xrpc/com.atproto.sync.listRepos", func(w http.ResponseWriter, r *http.Request) {
		type repoEntry struct {
			DID string `json:"did"`
		}
		var entries []repoEntry
		for did := range repos {
			entries = append(entries, repoEntry{DID: did})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"repos": entries})
	})
	mux.HandleFunc("/xrpc/com.atproto.repo.listRecords", func(w http.ResponseWriter, r *http.Request) {
		repo := r.URL.Query().Get("repo")
		collection := r.URL.Query().Get("collection")
		type recordEntry struct {
			URI   string                 `json:"uri"`
			CID   string                 `json:"cid"`
			Value map[string]interface{} `json:"value"`
		}
		var out []recordEntry
		if collection == "app.atchess.challenge" {
			for rkey, val := range repos[repo] {
				out = append(out, recordEntry{
					URI:   fmt.Sprintf("at://%s/app.atchess.challenge/%s", repo, rkey),
					CID:   "cid-" + rkey,
					Value: val,
				})
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"records": out})
	})

	return httptest.NewServer(mux)
}

func wsURLFor(httpURL string) string {
	return "ws" + strings.TrimPrefix(httpURL, "http") + "/xrpc/com.atproto.sync.subscribeRepos"
}

func TestBackfillChallengesForUser_FindsChallengeAddressedToUser(t *testing.T) {
	server := newMockPDS(t, map[string]map[string]map[string]interface{}{
		aliceDID: {
			"challenge1": challengeRecord(aliceDID, bobDID),
		},
	})
	defer server.Close()

	store := newTestChallengeStore(t)
	b := New(store, zerolog.Nop())

	result := b.BackfillChallengesForUser(context.Background(), bobDID, []string{wsURLFor(server.URL)})

	if result.TotalFound() != 1 {
		t.Fatalf("expected 1 challenge found, got %d (hosts: %+v)", result.TotalFound(), result.Hosts)
	}

	pending, err := store.ForPlayer(bobDID)
	if err != nil {
		t.Fatalf("ForPlayer: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending challenge indexed for bob, got %d", len(pending))
	}
	if pending[0].ChallengerDID != aliceDID {
		t.Errorf("challenger = %q, want %q", pending[0].ChallengerDID, aliceDID)
	}
}

func TestBackfillChallengesForUser_IgnoresChallengesAddressedToSomeoneElse(t *testing.T) {
	server := newMockPDS(t, map[string]map[string]map[string]interface{}{
		aliceDID: {
			"challenge1": challengeRecord(aliceDID, carolDID), // addressed to carol, not bob
		},
	})
	defer server.Close()

	store := newTestChallengeStore(t)
	b := New(store, zerolog.Nop())

	result := b.BackfillChallengesForUser(context.Background(), bobDID, []string{wsURLFor(server.URL)})

	if result.TotalFound() != 0 {
		t.Fatalf("expected 0 challenges found for bob, got %d", result.TotalFound())
	}
	got, err := store.ForPlayer(bobDID)
	if err != nil {
		t.Fatalf("ForPlayer: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected no pending challenges for bob, got %d", len(got))
	}
}

func TestBackfillChallengesForUser_MultipleHosts(t *testing.T) {
	serverA := newMockPDS(t, map[string]map[string]map[string]interface{}{
		aliceDID: {"c1": challengeRecord(aliceDID, bobDID)},
	})
	defer serverA.Close()
	serverB := newMockPDS(t, map[string]map[string]map[string]interface{}{
		carolDID: {"c2": challengeRecord(carolDID, bobDID)},
	})
	defer serverB.Close()

	store := newTestChallengeStore(t)
	b := New(store, zerolog.Nop())

	result := b.BackfillChallengesForUser(context.Background(), bobDID, []string{wsURLFor(serverA.URL), wsURLFor(serverB.URL)})

	if result.TotalFound() != 2 {
		t.Fatalf("expected 2 challenges found across both hosts, got %d (hosts: %+v)", result.TotalFound(), result.Hosts)
	}
	if len(result.Hosts) != 2 {
		t.Fatalf("expected 2 HostResults, got %d", len(result.Hosts))
	}
}

func TestBackfillChallengesForUser_DedupesAgainstAlreadyIndexedChallenge(t *testing.T) {
	server := newMockPDS(t, map[string]map[string]map[string]interface{}{
		aliceDID: {"challenge1": challengeRecord(aliceDID, bobDID)},
	})
	defer server.Close()

	store := newTestChallengeStore(t)
	// Pre-seed the store with the same challenge (as if the live firehose
	// subscription had already indexed it) to prove Store.Add's dedup
	// prevents double counting.
	if _, err := store.Add(&challenge.PendingChallenge{
		ChallengeURI:  fmt.Sprintf("at://%s/app.atchess.challenge/challenge1", aliceDID),
		ChallengerDID: aliceDID,
		ChallengedDID: bobDID,
		ExpiresAt:     time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	b := New(store, zerolog.Nop())
	result := b.BackfillChallengesForUser(context.Background(), bobDID, []string{wsURLFor(server.URL)})

	if result.TotalFound() != 0 {
		t.Errorf("expected 0 NEWLY found challenges (already indexed), got %d", result.TotalFound())
	}
	got, err := store.ForPlayer(bobDID)
	if err != nil {
		t.Fatalf("ForPlayer: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("expected exactly 1 pending challenge for bob (no duplicate), got %d", len(got))
	}
}

func TestBackfillChallengesForUser_UnreachableHost_DoesNotErrorOtherHosts(t *testing.T) {
	server := newMockPDS(t, map[string]map[string]map[string]interface{}{
		aliceDID: {"challenge1": challengeRecord(aliceDID, bobDID)},
	})
	defer server.Close()

	store := newTestChallengeStore(t)
	b := New(store, zerolog.Nop())

	// A bogus host (connection refused) alongside a working one: the
	// working host's result must still be found.
	result := b.BackfillChallengesForUser(context.Background(), bobDID, []string{
		"ws://127.0.0.1:1/xrpc/com.atproto.sync.subscribeRepos",
		wsURLFor(server.URL),
	})

	if result.TotalFound() != 1 {
		t.Fatalf("expected 1 challenge found despite one unreachable host, got %d (hosts: %+v)", result.TotalFound(), result.Hosts)
	}

	foundErr := false
	for _, h := range result.Hosts {
		if h.Err != nil {
			foundErr = true
		}
	}
	if !foundErr {
		t.Errorf("expected the unreachable host's HostResult to record an error")
	}
}

// newWedgedHost starts a TCP listener that completes the TCP handshake for
// every connection but then never writes a single byte back and never
// closes the connection -- a genuinely BLACKHOLED/wedged host, distinct
// from (and NOT covered by) the "connection refused" fast-failing host
// TestBackfillChallengesForUser_UnreachableHost_DoesNotErrorOtherHosts
// already exercises. An http.Client request against this host hangs until
// ITS OWN context is canceled; nothing here ever answers or resets the
// connection.
func newWedgedHost(t *testing.T) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start wedged-host listener: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // listener closed (test cleanup)
			}
			// Accept the connection, read (and discard) whatever the
			// client sends, but NEVER write a response and NEVER close --
			// this is what makes it "wedged" rather than merely slow.
			go func(c net.Conn) {
				buf := make([]byte, 4096)
				for {
					if _, err := c.Read(buf); err != nil {
						return
					}
				}
			}(conn)
		}
	}()

	return "ws://" + ln.Addr().String() + "/xrpc/com.atproto.sync.subscribeRepos"
}

// TestBackfillChallengesForUser_WedgedHost_DoesNotStarveOtherHosts
// reproduces the exact shape of atchess-1c9.46's review defect 3: a
// blackholed/wedged host listed FIRST must not consume the entire
// login-backfill budget and leave every host listed after it never
// attempted at all (ReposScanned:0). It asserts the working, SECOND host
// is actually attempted and its challenge is actually found, and that the
// whole call finishes well before it would if the wedged host were
// allowed to eat the full context budget by itself.
func TestBackfillChallengesForUser_WedgedHost_DoesNotStarveOtherHosts(t *testing.T) {
	wedgedURL := newWedgedHost(t)

	workingServer := newMockPDS(t, map[string]map[string]map[string]interface{}{
		aliceDID: {"challenge1": challengeRecord(aliceDID, bobDID)},
	})
	defer workingServer.Close()

	store := newTestChallengeStore(t)
	b := New(store, zerolog.Nop())
	// A short per-host timeout keeps this test fast while still exercising
	// the real mechanism (BackfillChallengesForUser bounds EVERY host to
	// b.perHostTimeout, not just this test's artificially small value).
	b.perHostTimeout = 500 * time.Millisecond

	// A global budget generous enough that, if the fix regressed and the
	// wedged host were once again able to consume it entirely, the second
	// host would still be skipped (ctx.Err() != nil) -- proving this test
	// actually depends on the per-host bound, not on the global one
	// happening to expire in the second host's favor.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	result := b.BackfillChallengesForUser(ctx, bobDID, []string{
		wedgedURL, // deliberately first, reproducing the defect's exact ordering
		wsURLFor(workingServer.URL),
	})
	elapsed := time.Since(start)

	if len(result.Hosts) != 2 {
		t.Fatalf("expected 2 HostResults (both hosts attempted), got %d: %+v", len(result.Hosts), result.Hosts)
	}

	wedged, working := result.Hosts[0], result.Hosts[1]

	if working.ReposScanned == 0 {
		t.Fatalf("the wedged first host starved the working second host: ReposScanned=0 for it (expected >0) -- the second host was never actually attempted. hosts=%+v elapsed=%s", result.Hosts, elapsed)
	}
	if result.TotalFound() != 1 {
		t.Fatalf("expected 1 challenge found on the working (second) host despite the first host being wedged, got %d (hosts: %+v)", result.TotalFound(), result.Hosts)
	}
	if wedged.Err == nil {
		t.Errorf("expected the wedged host's HostResult to record an error (its per-host timeout expiring), got nil: %+v", wedged)
	}

	// The wedged host must not be able to consume anywhere near the full
	// 5s global budget by itself: with a 500ms per-host timeout and two
	// hosts, this should complete in low seconds, not ~5s (which is what
	// the pre-fix behavior -- the wedged host serially eating the whole
	// global budget -- would produce).
	if elapsed >= 4*time.Second {
		t.Errorf("expected the per-host timeout to bound the wedged host's attempt well below the global 5s budget, but the whole call took %s (suggests the wedged host is still consuming the global budget instead of just its own per-host share)", elapsed)
	}
}

func TestBackfillChallengesForUser_CapsRepoEnumeration(t *testing.T) {
	repos := make(map[string]map[string]map[string]interface{})
	for i := 0; i < 5; i++ {
		did := fmt.Sprintf("did:plc:repo%d", i)
		repos[did] = map[string]map[string]interface{}{}
	}
	// One of the (many) repos actually has the challenge, but the cap is
	// set below the total repo count so it may or may not be found --
	// what this test actually asserts is that Capped is reported and the
	// backfill does not attempt to scan more than the cap.
	server := newMockPDS(t, repos)
	defer server.Close()

	store := newTestChallengeStore(t)
	b := New(store, zerolog.Nop())
	b.maxReposPerHost = 2 // force capping well below the 5 repos present

	result := b.BackfillChallengesForUser(context.Background(), bobDID, []string{wsURLFor(server.URL)})

	if len(result.Hosts) != 1 {
		t.Fatalf("expected 1 HostResult, got %d", len(result.Hosts))
	}
	hr := result.Hosts[0]
	if !hr.Capped {
		t.Errorf("expected Capped=true when repo count (5) exceeds maxReposPerHost (2)")
	}
	if hr.ReposScanned > 2 {
		t.Errorf("expected at most 2 repos scanned (the cap), got %d", hr.ReposScanned)
	}
}

func TestBackfillChallengesForUser_ContextDeadlineExceeded_ReturnsPartialResults(t *testing.T) {
	server := newMockPDS(t, map[string]map[string]map[string]interface{}{
		aliceDID: {"challenge1": challengeRecord(aliceDID, bobDID)},
	})
	defer server.Close()

	store := newTestChallengeStore(t)
	b := New(store, zerolog.Nop())

	ctx, cancel := context.WithTimeout(context.Background(), 0) // already expired
	defer cancel()
	time.Sleep(time.Millisecond)

	result := b.BackfillChallengesForUser(ctx, bobDID, []string{wsURLFor(server.URL)})

	// Must not panic or hang; the already-expired context should cause the
	// host to be skipped (or at least not crash).
	if len(result.Hosts) != 1 {
		t.Fatalf("expected 1 HostResult even for an expired context, got %d", len(result.Hosts))
	}
}
