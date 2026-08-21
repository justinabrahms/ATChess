package atproto

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// atchess-1c9.29: Client.AcceptChallenge is what gives an accepted game a
// durable back-reference to the challenge that created it (its
// "challenge" strongRef field, uri+cid) and a challenge-derived rkey
// instead of a random one, with an idempotent-but-ownership-checked
// accept for a repeated/duplicate call. This file exercises that method
// directly (rather than through internal/web's HTTP handler) against a
// minimal in-memory multi-repo AT Protocol stand-in, acceptTestPDS below,
// so the cross-repo behaviour (the challenge lives in the CHALLENGER's
// repo, the resulting game in the CHALLENGED player's) is exercised for
// real via DID resolution, exactly as it would be in production, without
// a live dual-PDS harness.

// ---------------------------------------------------------------------
// acceptTestPDS: a minimal, in-memory, multi-repo AT Protocol stand-in.
// ---------------------------------------------------------------------
//
// A single httptest server plays three roles at once, mirroring
// derive_status_test.go's deriveTestPDS: the PLC directory (GET
// /did:plc:<x> resolves to this same server as that DID's
// serviceEndpoint), every repo's PDS (com.atproto.repo.createRecord /
// getRecord), and the login endpoint (com.atproto.server.createSession,
// which mints a session for whatever DID is passed as "identifier" -- see
// acceptTestClient).
type acceptTestPDS struct {
	base string

	mu      sync.Mutex
	records map[string]map[string]map[string]*storedRecord // repo -> collection -> rkey -> record
	seq     int

	// listFail, when set for a repo DID, makes every
	// com.atproto.repo.listRecords call against that repo misbehave in the
	// given way ("http500" -- a bare HTTP 500). Used to pin
	// getChallengeDeclineOutcome's fail-open policy (atchess-1c9.91): an
	// unreachable challengeResponse listing must not block a legitimate
	// accept.
	listFail map[string]string
}

func newAcceptTestPDS(t *testing.T) *acceptTestPDS {
	t.Helper()
	p := &acceptTestPDS{records: map[string]map[string]map[string]*storedRecord{}}
	// atchess-1c9.95: p.base is both the login pdsURL passed to NewClient
	// AND embedded as a DID document's serviceEndpoint, which
	// parseServiceEndpoint now validates -- see newFakeHTTPSEndpoint for
	// why this can no longer be the httptest server's own
	// http://127.0.0.1:<port> address.
	srv := newFakeHTTPSEndpoint(t, http.HandlerFunc(p.handle))
	p.base = srv.URL
	return p
}

func (p *acceptTestPDS) nextID(prefix string) string {
	p.seq++
	return fmt.Sprintf("%s%d", prefix, p.seq)
}

func (p *acceptTestPDS) handle(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if strings.HasPrefix(r.URL.Path, "/did:") {
		did := strings.TrimPrefix(r.URL.Path, "/")
		_ = json.NewEncoder(w).Encode(DIDDocument{
			ID: did,
			Service: []DIDService{
				{ID: "#atproto_pds", Type: atprotoPDSServiceType, ServiceEndpoint: p.base},
			},
		})
		return
	}

	switch r.URL.Path {
	case "/xrpc/com.atproto.server.createSession":
		var req struct{ Identifier, Password string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		did := req.Identifier
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"accessJwt":  "test-jwt-" + did,
			"refreshJwt": "test-refresh-" + did,
			"did":        did,
			"handle":     did,
		})
	case "/xrpc/com.atproto.repo.createRecord":
		p.handleCreate(w, r)
	case "/xrpc/com.atproto.repo.getRecord":
		p.handleGet(w, r)
	case "/xrpc/com.atproto.repo.listRecords":
		p.handleList(w, r)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (p *acceptTestPDS) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Repo       string                 `json:"repo"`
		Collection string                 `json:"collection"`
		RKey       string                 `json:"rkey"`
		Record     map[string]interface{} `json:"record"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.records[req.Repo] == nil {
		p.records[req.Repo] = map[string]map[string]*storedRecord{}
	}
	if p.records[req.Repo][req.Collection] == nil {
		p.records[req.Repo][req.Collection] = map[string]*storedRecord{}
	}
	rkey := req.RKey
	if rkey == "" {
		rkey = p.nextID("rkey")
	}
	if _, exists := p.records[req.Repo][req.Collection][rkey]; exists {
		// Mirrors a real PDS refusing to overwrite an existing record key
		// via createRecord -- this is the conflict AcceptChallenge's
		// double-click/retry race-recovery path (a re-read after a
		// failed create) exists to handle.
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "InvalidSwap", "message": "record already exists"})
		return
	}
	cid := p.nextID("cid")
	p.records[req.Repo][req.Collection][rkey] = &storedRecord{cid: cid, value: req.Record}
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"uri": fmt.Sprintf("at://%s/%s/%s", req.Repo, req.Collection, rkey),
		"cid": cid,
	})
}

func (p *acceptTestPDS) handleGet(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	repo, collection, rkey := q.Get("repo"), q.Get("collection"), q.Get("rkey")

	p.mu.Lock()
	var rec *storedRecord
	if byColl := p.records[repo]; byColl != nil {
		if byRKey := byColl[collection]; byRKey != nil {
			rec = byRKey[rkey]
		}
	}
	p.mu.Unlock()

	if rec == nil {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "RecordNotFound"})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"uri":   fmt.Sprintf("at://%s/%s/%s", repo, collection, rkey),
		"cid":   rec.cid,
		"value": rec.value,
	})
}

// handleList serves com.atproto.repo.listRecords, needed by
// getChallengeDeclineOutcome's cross-repo (from the accepting client's own
// perspective: same-repo) read for a durable decline.
func (p *acceptTestPDS) handleList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	repo, collection := q.Get("repo"), q.Get("collection")

	p.mu.Lock()
	fail := p.listFail[repo]
	var coll map[string]*storedRecord
	if byColl := p.records[repo]; byColl != nil {
		coll = byColl[collection]
	}
	p.mu.Unlock()

	if fail == "http500" {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	type rec struct {
		URI   string      `json:"uri"`
		CID   string      `json:"cid"`
		Value interface{} `json:"value"`
	}
	var recs []rec
	for rkey, stored := range coll {
		recs = append(recs, rec{URI: fmt.Sprintf("at://%s/%s/%s", repo, collection, rkey), CID: stored.cid, Value: stored.value})
	}
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"records": recs})
}

// setListFail makes every com.atproto.repo.listRecords call against did's
// repo return a bare HTTP 500 -- see acceptTestPDS.listFail.
func (p *acceptTestPDS) setListFail(did, mode string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.listFail == nil {
		p.listFail = map[string]string{}
	}
	p.listFail[did] = mode
}

// putChallengeResponse seeds an app.atchess.challengeResponse record
// directly into repo's simulated collection -- mirroring RespondToChallenge's
// own shape (a "challenge" strongRef of uri+cid, plus "response") -- so
// tests can plant a durable decline (or a forged one, in the WRONG repo)
// without going through the full DeclineChallengeHandler/RespondToChallenge
// path.
func (p *acceptTestPDS) putChallengeResponse(repo, rkey, challengeURI, challengeCID, response string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.records[repo] == nil {
		p.records[repo] = map[string]map[string]*storedRecord{}
	}
	if p.records[repo]["app.atchess.challengeResponse"] == nil {
		p.records[repo]["app.atchess.challengeResponse"] = map[string]*storedRecord{}
	}
	cid := p.nextID("cid")
	p.records[repo]["app.atchess.challengeResponse"][rkey] = &storedRecord{cid: cid, value: map[string]interface{}{
		"$type":     "app.atchess.challengeResponse",
		"createdAt": time.Now().Format(time.RFC3339),
		"challenge": map[string]interface{}{
			"uri": challengeURI,
			"cid": challengeCID,
		},
		"response": response,
	}}
}

// putChallenge seeds an app.atchess.challenge record directly into repo's
// simulated collection (bypassing CreateChallenge, so the fixture's
// fields -- crucially proposedGameId -- are fully controlled by the
// test), and returns the CID the fake PDS assigned it.
func (p *acceptTestPDS) putChallenge(repo, rkey string, value map[string]interface{}) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.records[repo] == nil {
		p.records[repo] = map[string]map[string]*storedRecord{}
	}
	if p.records[repo]["app.atchess.challenge"] == nil {
		p.records[repo]["app.atchess.challenge"] = map[string]*storedRecord{}
	}
	cid := p.nextID("cid")
	p.records[repo]["app.atchess.challenge"][rkey] = &storedRecord{cid: cid, value: value}
	return cid
}

// putGame seeds an app.atchess.game record directly into repo's simulated
// collection, bypassing createGame entirely -- used to plant a
// pre-existing, UNRELATED game at a chosen rkey, exactly as a real
// pre-existing game the attacker did not create would sit there (the
// crafted-proposedGameId collision scenario -- atchess-1c9.29 review
// fix).
func (p *acceptTestPDS) putGame(repo, rkey string, value map[string]interface{}) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.records[repo] == nil {
		p.records[repo] = map[string]map[string]*storedRecord{}
	}
	if p.records[repo]["app.atchess.game"] == nil {
		p.records[repo]["app.atchess.game"] = map[string]*storedRecord{}
	}
	cid := p.nextID("cid")
	p.records[repo]["app.atchess.game"][rkey] = &storedRecord{cid: cid, value: value}
	return cid
}

// gameCount reports how many app.atchess.game records exist in repo's
// simulated collection -- used to assert a duplicate accept never
// produces a second game record.
func (p *acceptTestPDS) gameCount(repo string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.records[repo]["app.atchess.game"])
}

// gameRecord returns the raw stored value of repo's app.atchess.game/rkey
// record, or nil if it does not exist -- used to assert the record ACTUALLY
// stored (not merely what AcceptChallenge's return value claims) carries
// the challenge back-reference.
func (p *acceptTestPDS) gameRecord(repo, rkey string) map[string]interface{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	rec := p.records[repo]["app.atchess.game"][rkey]
	if rec == nil {
		return nil
	}
	return rec.value
}

func acceptTestClient(t *testing.T, pds *acceptTestPDS, did string) *Client {
	t.Helper()
	c, err := NewClient(pds.base, did, "password")
	if err != nil {
		t.Fatalf("NewClient(%s): %v", did, err)
	}
	c.SetPLCDirectoryURL(pds.base)
	return c
}

// ---------------------------------------------------------------------
// The tests
// ---------------------------------------------------------------------

const (
	testChallengerDID = "did:plc:challenger"
	testChallengedDID = "did:plc:challenged"
	testStrangerDID   = "did:plc:stranger"
	testMalloryDID    = "did:plc:mallory" // hosts the forged challenge record
	testCarolDID      = "did:plc:carol"   // forged/uninvolved third party
)

func seedBasicChallenge(pds *acceptTestPDS) (challengeURI, challengeCID string) {
	return seedChallengeWith(pds, testChallengerDID, testChallengedDID, "chal1", "derivedgame1", "pending", time.Now().Add(24*time.Hour))
}

// seedChallengeWith seeds an app.atchess.challenge record with full control
// over status/expiresAt (relative to time.Now(), never a hardcoded
// timestamp -- a fixed past-dated fixture is exactly what silently broke
// this suite once already when "now" caught up with it) for the
// status/expiry rejection tests (atchess-1c9.29 review fix).
func seedChallengeWith(pds *acceptTestPDS, challengerDID, challengedDID, rkey, proposedGameID, status string, expiresAt time.Time) (challengeURI, challengeCID string) {
	cid := pds.putChallenge(challengerDID, rkey, map[string]interface{}{
		"$type":          "app.atchess.challenge",
		"createdAt":      time.Now().Add(-time.Hour).Format(time.RFC3339),
		"challenger":     challengerDID,
		"challenged":     challengedDID,
		"status":         status,
		"color":          "white",
		"proposedGameId": proposedGameID,
		"expiresAt":      expiresAt.Format(time.RFC3339),
	})
	return "at://" + challengerDID + "/app.atchess.challenge/" + rkey, cid
}

// TestAcceptChallenge_CreatesGameWithChallengeBackReferenceAndDerivedRkey
// is the core positive path: the challenged player accepts, and the
// STORED record (read back directly from the fake PDS's own map, not
// trusted from AcceptChallenge's return value) carries the challenge's
// uri+cid, and its rkey is the challenge's proposedGameId -- not a random
// TID.
func TestAcceptChallenge_CreatesGameWithChallengeBackReferenceAndDerivedRkey(t *testing.T) {
	pds := newAcceptTestPDS(t)
	challengeURI, challengeCID := seedBasicChallenge(pds)

	challenged := acceptTestClient(t, pds, testChallengedDID)

	game, err := challenged.AcceptChallenge(context.Background(), challengeURI)
	if err != nil {
		t.Fatalf("AcceptChallenge: %v", err)
	}

	wantURI := "at://" + testChallengedDID + "/app.atchess.game/derivedgame1"
	if game.ID != wantURI {
		t.Errorf("game.ID = %q, want %q (rkey must be the challenge's proposedGameId, not random)", game.ID, wantURI)
	}
	if game.White != testChallengerDID || game.Black != testChallengedDID {
		t.Errorf("game white/black = %q/%q, want challenger white (%q) / challenged black (%q) -- challenge color was \"white\" (the challenger's own choice), so the challenged player must get the opposite", game.White, game.Black, testChallengerDID, testChallengedDID)
	}

	// Read the record back directly from the fake PDS's store -- NOT
	// through the handler's/AcceptChallenge's return value -- to prove the
	// persisted record genuinely carries the challenge reference.
	stored := pds.gameRecord(testChallengedDID, "derivedgame1")
	if stored == nil {
		t.Fatalf("no app.atchess.game/derivedgame1 record found in %s's repo", testChallengedDID)
	}
	ref, ok := stored["challenge"].(map[string]interface{})
	if !ok {
		t.Fatalf("stored game record has no \"challenge\" object: %#v", stored)
	}
	if got, _ := ref["uri"].(string); got != challengeURI {
		t.Errorf("stored game record's challenge.uri = %q, want %q", got, challengeURI)
	}
	if got, _ := ref["cid"].(string); got != challengeCID {
		t.Errorf("stored game record's challenge.cid = %q, want the challenge's REAL cid %q (must not be reconstructed/guessed)", got, challengeCID)
	}
}

// TestAcceptChallenge_DuplicateByChallengedPlayer_IdempotentNoSecondRecord
// covers the orchestrator's design decision: a second accept by the same
// (challenged) participant returns the SAME game, and no second game
// record is ever created.
func TestAcceptChallenge_DuplicateByChallengedPlayer_IdempotentNoSecondRecord(t *testing.T) {
	pds := newAcceptTestPDS(t)
	challengeURI, _ := seedBasicChallenge(pds)
	challenged := acceptTestClient(t, pds, testChallengedDID)

	first, err := challenged.AcceptChallenge(context.Background(), challengeURI)
	if err != nil {
		t.Fatalf("first AcceptChallenge: %v", err)
	}
	second, err := challenged.AcceptChallenge(context.Background(), challengeURI)
	if err != nil {
		t.Fatalf("second (duplicate) AcceptChallenge: %v", err)
	}

	if first.ID != second.ID {
		t.Errorf("duplicate accept returned a DIFFERENT game: first=%q second=%q", first.ID, second.ID)
	}
	if n := pds.gameCount(testChallengedDID); n != 1 {
		t.Errorf("expected exactly 1 game record after a duplicate accept, got %d", n)
	}
}

// TestAcceptChallenge_ForgedChallenger_MintsThirdPartyGame_ChainTest is
// atchess-1c9.106's full chain proof: Mallory writes an
// app.atchess.challenge into HER OWN repo naming Carol -- an uninvolved
// third party who never wrote or agreed to anything -- as "challenger",
// with Bob as "challenged". Before the fix, Bob's honest, ordinary
// accept would mint a real, public app.atchess.game record in HIS OWN
// repo crediting Carol as a player. This is also (b)'s isolation case:
// the challenge is supplied directly by its at:// URI (exactly how
// AcceptChallenge is always invoked -- from a notification, or directly),
// never through internal/challenge.Store, so fix (a) alone cannot help
// here -- only AcceptChallenge's own corroboration (fix (b)) can.
func TestAcceptChallenge_ForgedChallenger_MintsThirdPartyGame_ChainTest(t *testing.T) {
	pds := newAcceptTestPDS(t)
	// Hosted in Mallory's repo, but claims Carol as challenger.
	pds.putChallenge(testMalloryDID, "forged1", map[string]interface{}{
		"$type":          "app.atchess.challenge",
		"createdAt":      time.Now().Add(-time.Hour).Format(time.RFC3339),
		"challenger":     testCarolDID,
		"challenged":     testChallengedDID,
		"status":         "pending",
		"color":          "white",
		"proposedGameId": "forgedgame1",
		"expiresAt":      time.Now().Add(24 * time.Hour).Format(time.RFC3339),
	})
	forgedChallengeURI := "at://" + testMalloryDID + "/app.atchess.challenge/forged1"

	bob := acceptTestClient(t, pds, testChallengedDID)

	game, err := bob.AcceptChallenge(context.Background(), forgedChallengeURI)
	if err == nil {
		t.Fatalf("expected AcceptChallenge to refuse a forged challenge, but it SUCCEEDED and minted game %q naming challenger=%q (an uninvolved third party who never wrote this challenge) -- this is the exact chain atchess-1c9.106 describes", game.ID, testCarolDID)
	}
	if !errors.Is(err, ErrChallengeChallengerForged) {
		t.Errorf("expected an error wrapping ErrChallengeChallengerForged, got: %v", err)
	}
	if n := pds.gameCount(testChallengedDID); n != 0 {
		t.Errorf("expected NO game record to have been created for the forged challenge, got %d", n)
	}
}

// TestAcceptChallenge_ChallengerNeverReachesIdempotentPath is a review-fix
// regression test (atchess-1c9.29): an EARLIER version of this method
// checked for an existing game BEFORE checking "only the challenged player
// may originate", which let the challenger reach the idempotent read-back
// path even though they could never have originated the game themselves.
// Combined with a crafted/colliding proposedGameId, that let a challenger
// read an unrelated game's data. The fix moved the role check first, so
// the challenger is rejected outright -- BEFORE the existence check ever
// runs -- regardless of whether a game already exists for this challenge.
func TestAcceptChallenge_ChallengerNeverReachesIdempotentPath(t *testing.T) {
	pds := newAcceptTestPDS(t)
	challengeURI, _ := seedBasicChallenge(pds)
	challenged := acceptTestClient(t, pds, testChallengedDID)
	challenger := acceptTestClient(t, pds, testChallengerDID)

	if _, err := challenged.AcceptChallenge(context.Background(), challengeURI); err != nil {
		t.Fatalf("challenged's AcceptChallenge: %v", err)
	}

	_, err := challenger.AcceptChallenge(context.Background(), challengeURI)
	if err == nil {
		t.Fatal("expected the challenger's accept to be rejected even though the game now exists, got nil")
	}
	if !errors.Is(err, ErrOnlyChallengedMayAccept) {
		t.Errorf("expected an error wrapping ErrOnlyChallengedMayAccept, got: %v", err)
	}
	if n := pds.gameCount(testChallengedDID); n != 1 {
		t.Errorf("expected exactly 1 game record (the challenged player's original accept), got %d", n)
	}
}

// TestAcceptChallenge_NonParticipant_AfterAccept_Forbidden is the
// information-leak scenario the orchestrator's notes call out by name: a
// caller who is neither the challenger nor the challenged party must NOT
// be handed the game, even though it already exists and its rkey is
// derivable from the challenge.
func TestAcceptChallenge_NonParticipant_AfterAccept_Forbidden(t *testing.T) {
	pds := newAcceptTestPDS(t)
	challengeURI, _ := seedBasicChallenge(pds)
	challenged := acceptTestClient(t, pds, testChallengedDID)
	stranger := acceptTestClient(t, pds, testStrangerDID)

	if _, err := challenged.AcceptChallenge(context.Background(), challengeURI); err != nil {
		t.Fatalf("challenged's AcceptChallenge: %v", err)
	}

	_, err := stranger.AcceptChallenge(context.Background(), challengeURI)
	if err == nil {
		t.Fatal("expected an error for a non-participant accepting an already-accepted challenge, got nil (information leak: the game was handed to a caller who is not one of its two players)")
	}
	if !errors.Is(err, ErrNotChallengeParticipant) {
		t.Errorf("expected an error wrapping ErrNotChallengeParticipant, got: %v", err)
	}
}

// TestAcceptChallenge_NonParticipant_BeforeAccept_Forbidden is the same
// property before any game exists at all.
func TestAcceptChallenge_NonParticipant_BeforeAccept_Forbidden(t *testing.T) {
	pds := newAcceptTestPDS(t)
	challengeURI, _ := seedBasicChallenge(pds)
	stranger := acceptTestClient(t, pds, testStrangerDID)

	_, err := stranger.AcceptChallenge(context.Background(), challengeURI)
	if err == nil {
		t.Fatal("expected an error for a non-participant, got nil")
	}
	if !errors.Is(err, ErrNotChallengeParticipant) {
		t.Errorf("expected an error wrapping ErrNotChallengeParticipant, got: %v", err)
	}
}

// TestAcceptChallenge_ChallengerCannotOriginate is the core "only the
// challenged player may accept" rule: the challenger themselves -- a real
// participant, but the wrong one -- must not be able to originate the
// game before the challenged player ever has.
func TestAcceptChallenge_ChallengerCannotOriginate(t *testing.T) {
	pds := newAcceptTestPDS(t)
	challengeURI, _ := seedBasicChallenge(pds)
	challenger := acceptTestClient(t, pds, testChallengerDID)

	_, err := challenger.AcceptChallenge(context.Background(), challengeURI)
	if err == nil {
		t.Fatal("expected an error for the challenger trying to originate their own challenge's accept, got nil")
	}
	if !errors.Is(err, ErrOnlyChallengedMayAccept) {
		t.Errorf("expected an error wrapping ErrOnlyChallengedMayAccept, got: %v", err)
	}
	if n := pds.gameCount(testChallengedDID); n != 0 {
		t.Errorf("expected no game record to have been created, got %d", n)
	}
}

// TestAcceptChallenge_MissingChallenge_ReturnsErrRecordNotFound covers the
// "challenge record unreadable because it genuinely does not exist" case:
// no game must ever be created against a challenge that cannot be
// verified.
func TestAcceptChallenge_MissingChallenge_ReturnsErrRecordNotFound(t *testing.T) {
	pds := newAcceptTestPDS(t)
	challenged := acceptTestClient(t, pds, testChallengedDID)

	missingURI := "at://" + testChallengerDID + "/app.atchess.challenge/doesnotexist"
	_, err := challenged.AcceptChallenge(context.Background(), missingURI)
	if err == nil {
		t.Fatal("expected an error for a nonexistent challenge, got nil")
	}
	if !errors.Is(err, ErrRecordNotFound) {
		t.Errorf("expected an error wrapping ErrRecordNotFound, got: %v", err)
	}
}

// TestAcceptChallenge_CraftedProposedGameId_UnrelatedGame_Conflict is the
// reviewer-found vulnerability's core regression test: a challenge whose
// proposedGameId was crafted (or merely happens) to collide with the rkey
// of an unrelated, pre-existing game in the challenged player's own repo
// (rkeys are enumerable via public com.atproto.repo.listRecords, so this
// is not far-fetched) must NEVER be silently treated as already accepted.
// The victim's own accept of this crafted challenge must fail with
// ErrChallengeConflict, return no game, and -- critically -- create no
// new game record either (the crafted challenge must not be silently
// "consumed" while producing nothing).
func TestAcceptChallenge_CraftedProposedGameId_UnrelatedGame_Conflict(t *testing.T) {
	pds := newAcceptTestPDS(t)

	// An unrelated, pre-existing game between the victim (testChallengedDID)
	// and some third party, living in the victim's own repo at rkey
	// "existinggame" -- nothing to do with the crafted challenge below.
	pds.putGame(testChallengedDID, "existinggame", map[string]interface{}{
		"$type":     "app.atchess.game",
		"createdAt": time.Now().Add(-time.Hour).Format(time.RFC3339),
		"white":     testChallengedDID,
		"black":     "did:plc:unrelatedthirdparty",
		"status":    "active",
		"fen":       "rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR w KQkq - 0 1",
	})

	// The crafted challenge: attacker -> victim, proposedGameId set to the
	// unrelated existing game's own rkey.
	challengeURI, _ := seedChallengeWith(pds, testChallengerDID, testChallengedDID, "craftedchal", "existinggame", "pending", time.Now().Add(24*time.Hour))

	victim := acceptTestClient(t, pds, testChallengedDID)
	game, err := victim.AcceptChallenge(context.Background(), challengeURI)
	if err == nil {
		t.Fatalf("expected ErrChallengeConflict, got a game instead: %+v", game)
	}
	if !errors.Is(err, ErrChallengeConflict) {
		t.Errorf("expected an error wrapping ErrChallengeConflict, got: %v", err)
	}
	if game != nil {
		t.Errorf("expected a nil game on conflict, got: %+v", game)
	}
	if n := pds.gameCount(testChallengedDID); n != 1 {
		t.Errorf("expected the victim's repo to still contain exactly the 1 pre-existing (unrelated) game, got %d", n)
	}

	// The attacker (challenger) accepting their own crafted challenge must
	// also never reach the idempotent path -- ErrOnlyChallengedMayAccept
	// fires first, before the existence check that found the unrelated
	// game even runs.
	attacker := acceptTestClient(t, pds, testChallengerDID)
	_, err = attacker.AcceptChallenge(context.Background(), challengeURI)
	if !errors.Is(err, ErrOnlyChallengedMayAccept) {
		t.Errorf("expected the attacker's own accept to fail with ErrOnlyChallengedMayAccept, got: %v", err)
	}
}

// TestAcceptChallenge_MatchingChallengeButCallerNotAPlayer_Conflict
// exercises the SECOND, independent condition of the found-record check:
// even when the found record's own "challenge" strongRef correctly names
// this exact challengeURI, the caller must still be one of ITS players.
// This is deliberately synthetic (a real CreateGameFromChallenge write
// always sets white/black to match the challenge's own
// challenger/challenged), constructed directly via putGame to prove the
// second condition has independent teeth from the first.
func TestAcceptChallenge_MatchingChallengeButCallerNotAPlayer_Conflict(t *testing.T) {
	pds := newAcceptTestPDS(t)
	challengeURI, challengeCID := seedChallengeWith(pds, testChallengerDID, testChallengedDID, "chal2", "mismatchedgame", "pending", time.Now().Add(24*time.Hour))

	// A game record that DOES reference this exact challenge, but whose
	// white/black do not include the challenged player at all.
	pds.putGame(testChallengedDID, "mismatchedgame", map[string]interface{}{
		"$type":     "app.atchess.game",
		"createdAt": time.Now().Format(time.RFC3339),
		"white":     "did:plc:someoneelse1",
		"black":     "did:plc:someoneelse2",
		"status":    "active",
		"fen":       "rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR w KQkq - 0 1",
		"challenge": map[string]interface{}{
			"uri": challengeURI,
			"cid": challengeCID,
		},
	})

	challenged := acceptTestClient(t, pds, testChallengedDID)
	_, err := challenged.AcceptChallenge(context.Background(), challengeURI)
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !errors.Is(err, ErrChallengeConflict) {
		t.Errorf("expected an error wrapping ErrChallengeConflict, got: %v", err)
	}
}

// TestAcceptChallenge_Expired_Rejected covers the challenge record's own
// expiresAt field.
func TestAcceptChallenge_Expired_Rejected(t *testing.T) {
	pds := newAcceptTestPDS(t)
	challengeURI, _ := seedChallengeWith(pds, testChallengerDID, testChallengedDID, "expchal", "expgame", "pending", time.Now().Add(-time.Hour))

	challenged := acceptTestClient(t, pds, testChallengedDID)
	_, err := challenged.AcceptChallenge(context.Background(), challengeURI)
	if err == nil {
		t.Fatal("expected an error for an expired challenge, got nil")
	}
	if !errors.Is(err, ErrChallengeNotAcceptable) {
		t.Errorf("expected an error wrapping ErrChallengeNotAcceptable, got: %v", err)
	}
	if n := pds.gameCount(testChallengedDID); n != 0 {
		t.Errorf("expected no game record to have been created for an expired challenge, got %d", n)
	}
}

// TestAcceptChallenge_DeclinedStatus_Rejected and
// TestAcceptChallenge_AcceptedStatus_Rejected cover the challenge
// record's own "status" field. See the tests below
// (TestAcceptChallenge_DurablyDeclined_Rejected and friends,
// atchess-1c9.91) for the SEPARATE app.atchess.challengeResponse-record
// case this field check alone cannot see.
func TestAcceptChallenge_DeclinedStatus_Rejected(t *testing.T) {
	pds := newAcceptTestPDS(t)
	challengeURI, _ := seedChallengeWith(pds, testChallengerDID, testChallengedDID, "declchal", "declgame", "declined", time.Now().Add(24*time.Hour))

	challenged := acceptTestClient(t, pds, testChallengedDID)
	_, err := challenged.AcceptChallenge(context.Background(), challengeURI)
	if err == nil {
		t.Fatal("expected an error for a declined challenge, got nil")
	}
	if !errors.Is(err, ErrChallengeNotAcceptable) {
		t.Errorf("expected an error wrapping ErrChallengeNotAcceptable, got: %v", err)
	}
	if n := pds.gameCount(testChallengedDID); n != 0 {
		t.Errorf("expected no game record to have been created for a declined challenge, got %d", n)
	}
}

func TestAcceptChallenge_AcceptedStatus_Rejected(t *testing.T) {
	pds := newAcceptTestPDS(t)
	challengeURI, _ := seedChallengeWith(pds, testChallengerDID, testChallengedDID, "acptchal", "acptgame", "accepted", time.Now().Add(24*time.Hour))

	challenged := acceptTestClient(t, pds, testChallengedDID)
	_, err := challenged.AcceptChallenge(context.Background(), challengeURI)
	if err == nil {
		t.Fatal("expected an error for a challenge whose own status field is already \"accepted\", got nil")
	}
	if !errors.Is(err, ErrChallengeNotAcceptable) {
		t.Errorf("expected an error wrapping ErrChallengeNotAcceptable, got: %v", err)
	}
	if n := pds.gameCount(testChallengedDID); n != 0 {
		t.Errorf("expected no game record to have been created, got %d", n)
	}
}

// ---------------------------------------------------------------------
// atchess-1c9.91: durable decline detection (getChallengeDeclineOutcome).
// ---------------------------------------------------------------------

// TestAcceptChallenge_DurablyDeclined_Rejected is the core regression this
// bead exists for: the challenge record's OWN "status" field still says
// "pending" (RespondToChallenge never touches it -- AT Protocol forbids
// writing into the challenger's repo), but the challenged player already
// declined via a SEPARATE app.atchess.challengeResponse record in their
// own repo. Per atchess-1c9.91's step-zero finding, the same player who
// declined is the only one who could ever call accept -- this is the
// stale-tab/double-submit self-override case, not a cross-party attack --
// but it must still be refused: a declined challenge must not be
// acceptable.
func TestAcceptChallenge_DurablyDeclined_Rejected(t *testing.T) {
	pds := newAcceptTestPDS(t)
	challengeURI, challengeCID := seedBasicChallenge(pds)

	// The challenged player's own honest decline, written into THEIR OWN
	// repo (exactly what RespondToChallenge does), referencing the real
	// challenge by its real strongRef.
	pds.putChallengeResponse(testChallengedDID, "declresp1", challengeURI, challengeCID, "declined")

	challenged := acceptTestClient(t, pds, testChallengedDID)
	game, err := challenged.AcceptChallenge(context.Background(), challengeURI)
	if err == nil {
		t.Fatalf("expected AcceptChallenge to refuse a durably declined challenge, but it SUCCEEDED and returned game %+v", game)
	}
	if !errors.Is(err, ErrChallengeNotAcceptable) {
		t.Errorf("expected an error wrapping ErrChallengeNotAcceptable, got: %v", err)
	}
	if n := pds.gameCount(testChallengedDID); n != 0 {
		t.Errorf("expected no game record to have been created for a durably declined challenge, got %d", n)
	}
}

// TestAcceptChallenge_ForgedDeclineWrongRepo_DoesNotBlock proves the
// corroboration half of the fix: a challengeResponse record does NOT
// count unless it lives in the CHALLENGED player's own repo -- the only
// repo RespondToChallenge could ever have written it into. A record
// planted in the CHALLENGER's own repo (or any other repo) claiming to
// decline must never block a legitimate, honest accept.
func TestAcceptChallenge_ForgedDeclineWrongRepo_DoesNotBlock(t *testing.T) {
	pds := newAcceptTestPDS(t)
	challengeURI, challengeCID := seedBasicChallenge(pds)

	// Planted in the CHALLENGER's own repo, not the challenged player's --
	// this is the "wrong repo" case: getChallengeDeclineOutcome only ever
	// lists challengedDID's own repo, so this record is never even
	// examined.
	pds.putChallengeResponse(testChallengerDID, "forgeddecl1", challengeURI, challengeCID, "declined")

	challenged := acceptTestClient(t, pds, testChallengedDID)
	game, err := challenged.AcceptChallenge(context.Background(), challengeURI)
	if err != nil {
		t.Fatalf("expected the honest accept to succeed despite a decline record forged into the WRONG repo, got error: %v", err)
	}
	if game == nil {
		t.Fatal("expected a game, got nil")
	}
	if n := pds.gameCount(testChallengedDID); n != 1 {
		t.Errorf("expected exactly 1 game record, got %d", n)
	}
}

// TestAcceptChallenge_DeclineNamesDifferentChallenge_DoesNotBlock pins the
// URI-matching half of getChallengeDeclineOutcome's corroboration
// (atchess-1c9.111 item 3, verified out-of-tree by atchess-1c9.91's
// reviewer but never pinned by a test): a challengeResponse record that
// lives in the RIGHT repo (challengedDID's own) and has
// response=="declined", but whose "challenge" strongRef names a
// DIFFERENT challenge URI than the one being accepted -- must never block
// this, unrelated, accept.
//
// The decline record's CID is deliberately set to the CURRENT challenge's
// own (correct) CID, not some other value -- if it used a different CID
// too, the CID check alone would already reject this record, masking
// whether the URI check does its job at all. Isolating the URI mismatch
// this way is what lets a red/green cycle on JUST the URI comparison mean
// anything (see this bead's brief).
func TestAcceptChallenge_DeclineNamesDifferentChallenge_DoesNotBlock(t *testing.T) {
	pds := newAcceptTestPDS(t)
	challengeURI, challengeCID := seedBasicChallenge(pds)

	otherURI := challengeURI + "-unrelated"
	if otherURI == challengeURI {
		t.Fatalf("test fixture bug: the 'other' challenge URI (%s) is the SAME as the one being accepted -- this test needs a genuine URI mismatch to mean anything", otherURI)
	}
	pds.putChallengeResponse(testChallengedDID, "declresp-other", otherURI, challengeCID, "declined")

	challenged := acceptTestClient(t, pds, testChallengedDID)
	game, err := challenged.AcceptChallenge(context.Background(), challengeURI)
	if err != nil {
		t.Fatalf("expected the accept to succeed despite an unrelated decline record naming a DIFFERENT challenge URI, got error: %v", err)
	}
	if game == nil {
		t.Fatal("expected a game, got nil")
	}
	if n := pds.gameCount(testChallengedDID); n != 1 {
		t.Errorf("expected exactly 1 game record, got %d", n)
	}
}

// TestAcceptChallenge_DeclineHasStaleCID_DoesNotBlock pins the
// CID-matching half of getChallengeDeclineOutcome's corroboration
// (atchess-1c9.111 item 3; the underlying "is a stale CID correct to
// ignore" POLICY question stays deferred to atchess-1c9.52, this test
// only pins the CODE'S CURRENT, already-shipped behaviour so a future
// refactor cannot silently drop the CID check).
//
// The challenged player declined this exact challenge URI while it still
// had CID staleCID. The challenger then re-put (mutated) that SAME
// challenge record afterwards, at the same rkey/URI, which bumps its CID
// in this fake PDS exactly as a real repo write would. The live
// challenge's CID no longer matches what the decline record pinned, so
// getChallengeDeclineOutcome must treat the decline as not corroborated
// and the accept must still succeed.
func TestAcceptChallenge_DeclineHasStaleCID_DoesNotBlock(t *testing.T) {
	pds := newAcceptTestPDS(t)
	challengeURI, staleCID := seedBasicChallenge(pds)

	pds.putChallengeResponse(testChallengedDID, "declresp-stale", challengeURI, staleCID, "declined")

	newCID := pds.putChallenge(testChallengerDID, "chal1", map[string]interface{}{
		"$type":          "app.atchess.challenge",
		"createdAt":      time.Now().Add(-time.Hour).Format(time.RFC3339),
		"challenger":     testChallengerDID,
		"challenged":     testChallengedDID,
		"status":         "pending",
		"color":          "white",
		"proposedGameId": "derivedgame1",
		"expiresAt":      time.Now().Add(24 * time.Hour).Format(time.RFC3339),
	})
	if newCID == staleCID {
		t.Fatalf("test fixture bug: re-putting the challenge produced the SAME CID (%s) as before -- this test needs a genuine CID mismatch to mean anything", newCID)
	}

	challenged := acceptTestClient(t, pds, testChallengedDID)
	game, err := challenged.AcceptChallenge(context.Background(), challengeURI)
	if err != nil {
		t.Fatalf("expected the accept to succeed despite a decline record whose challenge strongRef CID is stale (does not match the challenge's CURRENT CID), got error: %v", err)
	}
	if game == nil {
		t.Fatal("expected a game, got nil")
	}
	if n := pds.gameCount(testChallengedDID); n != 1 {
		t.Errorf("expected exactly 1 game record, got %d", n)
	}
}

// TestAcceptChallenge_HonestAcceptNoDecline_StillWorks is the regression
// that matters most: an ordinary accept, with no decline record anywhere,
// must still succeed once the durable-decline check is added to the
// accept path.
func TestAcceptChallenge_HonestAcceptNoDecline_StillWorks(t *testing.T) {
	pds := newAcceptTestPDS(t)
	challengeURI, _ := seedBasicChallenge(pds)

	challenged := acceptTestClient(t, pds, testChallengedDID)
	game, err := challenged.AcceptChallenge(context.Background(), challengeURI)
	if err != nil {
		t.Fatalf("expected an honest accept with no decline record to succeed, got error: %v", err)
	}
	if game == nil {
		t.Fatal("expected a game, got nil")
	}
	if n := pds.gameCount(testChallengedDID); n != 1 {
		t.Errorf("expected exactly 1 game record, got %d", n)
	}
}

// TestAcceptChallenge_DeclineCheckUnreachable_FailsOpen pins the
// deliberate fail-open policy (atchess-1c9.91, see
// getChallengeDeclineOutcome's doc comment): when the challenged player's
// own repo cannot be listed for a durable decline (here, a bare HTTP 500),
// AcceptChallenge proceeds with the accept rather than refusing it. This
// is deliberately NOT fail-closed, unlike ErrIncompleteDerivation's policy
// for cross-player game-status derivation -- decliner and accepter are
// provably the same identity here (AcceptChallenge's role gate already
// requires it), so there is no cross-party override this could protect
// against, and the repo being listed is the SAME one that just
// authenticated this very request.
func TestAcceptChallenge_DeclineCheckUnreachable_FailsOpen(t *testing.T) {
	pds := newAcceptTestPDS(t)
	challengeURI, _ := seedBasicChallenge(pds)
	pds.setListFail(testChallengedDID, "http500")

	challenged := acceptTestClient(t, pds, testChallengedDID)
	game, err := challenged.AcceptChallenge(context.Background(), challengeURI)
	if err != nil {
		t.Fatalf("expected AcceptChallenge to fail OPEN (proceed) when the decline check's listRecords call is unreachable, got error: %v", err)
	}
	if game == nil {
		t.Fatal("expected a game, got nil")
	}
	if n := pds.gameCount(testChallengedDID); n != 1 {
		t.Errorf("expected exactly 1 game record, got %d", n)
	}
}
