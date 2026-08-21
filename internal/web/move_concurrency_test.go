package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/justinabrahms/atchess/internal/atproto"
	"github.com/justinabrahms/atchess/internal/config"
	"github.com/justinabrahms/atchess/internal/oauth"
)

// --- synchronization primitives -------------------------------------------
//
// Both concurrency tests below need two goroutines to observe an IDENTICAL
// pre-move game snapshot, deterministically, on every run -- not "usually",
// which a sleep-based approach would only give probabilistically (and the
// brief for atchess-1c9.74 explicitly calls out that a test that races and
// usually passes is worse than no test at all). callGate/roundGate force
// this by holding back matching requests at the mock PDS itself until the
// expected number of concurrent callers have all arrived.

// gate is implemented by callGate and roundGate.
type gate interface{ arrive() }

// callGate blocks each arrive() caller until n total callers have arrived,
// then releases all of them together. Because the release channel is never
// re-armed, any arrival after the first n also passes straight through.
type callGate struct {
	t     *testing.T
	mu    sync.Mutex
	n     int
	count int
	ready chan struct{}
}

func newCallGate(t *testing.T, n int) *callGate {
	return &callGate{t: t, n: n, ready: make(chan struct{})}
}

// gateTimeout bounds how long arrive() will wait for its partners. Without
// it, a future change that makes one racer return BEFORE reaching the gated
// call (an added early rejection, say) leaves the other blocked forever, and
// the test does not fail with a message -- it hangs until `go test`'s global
// timeout and dumps goroutines, minutes later, in CI. Failing loudly here
// costs nothing and names the actual problem.
const gateTimeout = 30 * time.Second

func (g *callGate) arrive() {
	g.mu.Lock()
	g.count++
	if g.count >= g.n {
		select {
		case <-g.ready:
		default:
			close(g.ready)
		}
	}
	g.mu.Unlock()
	select {
	case <-g.ready:
	case <-time.After(gateTimeout):
		// t.Errorf, not t.Fatalf: arrive() runs on the mock server's
		// handler goroutine, and FailNow may only be called from the
		// test goroutine. Returning also unblocks the caller so the
		// test finishes and reports rather than wedging.
		g.t.Errorf("gate deadlock: only %d of %d callers reached the gate within %s -- "+
			"a racer probably returned before making its gated call", g.count, g.n, gateTimeout)
	}
}

// roundGate is like callGate, but re-arms every n arrivals instead of
// staying open forever after the first n: arrivals 1..n (round 0) are
// released together, arrivals n+1..2n (round 1) are released together
// independently of round 0, and so on. This is needed where each of the
// two racing goroutines makes more than one call matching the gated
// XRPC/collection pair, and every one of those calls -- not just the
// first -- must be forced to observe an identical, simultaneously-read
// snapshot (see TestMakeMoveHandler_ConcurrentMove_SamePlayerTwice below
// for why a single round is not enough there).
type roundGate struct {
	t      *testing.T
	mu     sync.Mutex
	n      int
	count  int
	rounds map[int]*callGate
}

func newRoundGate(t *testing.T, n int) *roundGate {
	return &roundGate{t: t, n: n, rounds: map[int]*callGate{}}
}

func (g *roundGate) arrive() {
	g.mu.Lock()
	round := g.count / g.n
	g.count++
	rg, ok := g.rounds[round]
	if !ok {
		rg = newCallGate(g.t, g.n)
		g.rounds[round] = rg
	}
	g.mu.Unlock()
	rg.arrive()
}

// --- mock PDS with real optimistic-concurrency (swapRecord) enforcement -----

type raceRecord struct {
	cid   string
	value map[string]interface{}
}

// raceMockPDS is a single-process stand-in for the did:plc directory and a
// player's PDS, purpose-built for the two concurrent-move tests below
// (atchess-1c9.74 items 2 and 3). Unlike mockFederatedPDS
// (move_and_draw_ownership_test.go), which hands out a fixed,
// content-independent cid per rkey, this mock gives every record a cid
// that changes on every write and enforces com.atproto.repo.putRecord's
// "swapRecord" optimistic-concurrency parameter for real: a putRecord whose
// swapRecord does not match the record's current cid is rejected with a
// non-200 response, exactly as a real PDS would -- see
// internal/atproto/client.go's RecordMove, which relies on this to make
// "two concurrent writers to the same game record, only one wins" true.
//
// It also supports an optional gate: every com.atproto.repo.getRecord call
// whose collection matches gateCollection is forced through the gate
// before being served.
type raceMockPDS struct {
	t    *testing.T
	mu   sync.Mutex
	base string

	records map[string]map[string]map[string]*raceRecord // repo -> collection -> rkey -> record
	rkeyN   int
	verN    int

	gate           gate
	gateCollection string

	writeCalls []string
}

func newRaceMockPDS(t *testing.T) *raceMockPDS {
	return &raceMockPDS{t: t, records: map[string]map[string]map[string]*raceRecord{}}
}

// server starts m's mock PDS on a real local TLS listener and advertises a
// validator-passing fake hostname (see newFakeHTTPSEndpoint,
// atchess-1c9.95) rather than the listener's own http://127.0.0.1:<port>,
// which internal/atproto.parseServiceEndpoint now refuses to accept as a
// DID document's serviceEndpoint.
func (m *raceMockPDS) server() *httptest.Server {
	srv := newFakeHTTPSEndpoint(m.t, http.HandlerFunc(m.handle))
	m.setBaseURL(srv.URL)
	return srv
}

func (m *raceMockPDS) setBaseURL(u string) { m.mu.Lock(); m.base = u; m.mu.Unlock() }
func (m *raceMockPDS) baseURL() string     { m.mu.Lock(); defer m.mu.Unlock(); return m.base }

func (m *raceMockPDS) seed(repo, collection, rkey string, value map[string]interface{}) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.records[repo] == nil {
		m.records[repo] = map[string]map[string]*raceRecord{}
	}
	if m.records[repo][collection] == nil {
		m.records[repo][collection] = map[string]*raceRecord{}
	}
	m.verN++
	m.records[repo][collection][rkey] = &raceRecord{cid: fmt.Sprintf("cid-%s-v%d", rkey, m.verN), value: value}
}

func (m *raceMockPDS) newRkey() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.newRkeyLocked()
}

// newRkeyLocked is newRkey's body, for callers that already hold m.mu.
func (m *raceMockPDS) newRkeyLocked() string {
	m.rkeyN++
	return fmt.Sprintf("rkey%d", m.rkeyN)
}

func (m *raceMockPDS) handle(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if strings.HasPrefix(r.URL.Path, "/did:plc:") {
		did := strings.TrimPrefix(r.URL.Path, "/")
		_ = json.NewEncoder(w).Encode(atproto.DIDDocument{
			ID: did,
			Service: []atproto.DIDService{
				{ID: "#atproto_pds", Type: "AtprotoPersonalDataServer", ServiceEndpoint: m.baseURL()},
			},
		})
		return
	}

	switch r.URL.Path {
	case "/xrpc/com.atproto.repo.getRecord":
		q := r.URL.Query()
		repo, collection, rkey := q.Get("repo"), q.Get("collection"), q.Get("rkey")
		if m.gate != nil && collection == m.gateCollection {
			m.gate.arrive()
		}
		m.mu.Lock()
		rec := m.records[repo][collection][rkey]
		m.mu.Unlock()
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
		return

	case "/xrpc/com.atproto.repo.listRecords":
		q := r.URL.Query()
		repo, collection := q.Get("repo"), q.Get("collection")
		m.mu.Lock()
		coll := m.records[repo][collection]
		type rec struct {
			URI   string      `json:"uri"`
			CID   string      `json:"cid"`
			Value interface{} `json:"value"`
		}
		var recs []rec
		for rkey, rr := range coll {
			recs = append(recs, rec{URI: fmt.Sprintf("at://%s/%s/%s", repo, collection, rkey), CID: rr.cid, Value: rr.value})
		}
		m.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"records": recs})
		return

	case "/xrpc/com.atproto.repo.createRecord":
		var req map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&req)
		repo, _ := req["repo"].(string)
		collection, _ := req["collection"].(string)
		record, _ := req["record"].(map[string]interface{})
		// Honor an explicit rkey if the caller supplied one (RecordMove's
		// deterministic move rkey, atchess-1c9.113); otherwise mint one,
		// exactly as a real PDS assigns a TID when rkey is omitted.
		explicitRkey, hasExplicitRkey := req["rkey"].(string)

		if m.gate != nil && collection == m.gateCollection {
			m.gate.arrive()
		}

		m.mu.Lock()
		defer m.mu.Unlock()

		rkey := explicitRkey
		if !hasExplicitRkey || rkey == "" {
			rkey = m.newRkeyLocked()
		} else if m.records[repo] != nil && m.records[repo][collection] != nil && m.records[repo][collection][rkey] != nil {
			// A real PDS was verified live (atchess-1c9.113) to reject a
			// createRecord whose explicit rkey already exists with a bare,
			// non-distinguishing HTTP 500 -- no structured "already
			// exists" error, and the existing record is NOT overwritten.
			// Reproduce that exactly, rather than silently overwriting.
			// Deliberately NOT prefixed "createRecord" -- callers count
			// successful writes via strings.HasPrefix(c, "createRecord"),
			// and a rejected collision must never be miscounted as one.
			m.writeCalls = append(m.writeCalls, fmt.Sprintf("rejectedCreateRecord(rkey collision) repo=%s collection=%s rkey=%s", repo, collection, rkey))
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "InternalServerError", "message": "Internal Server Error"})
			return
		}

		if m.records[repo] == nil {
			m.records[repo] = map[string]map[string]*raceRecord{}
		}
		if m.records[repo][collection] == nil {
			m.records[repo][collection] = map[string]*raceRecord{}
		}
		m.verN++
		cid := fmt.Sprintf("cid-%s-v%d", rkey, m.verN)
		m.records[repo][collection][rkey] = &raceRecord{cid: cid, value: record}
		m.writeCalls = append(m.writeCalls, fmt.Sprintf("createRecord repo=%s collection=%s rkey=%s", repo, collection, rkey))
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"uri": fmt.Sprintf("at://%s/%s/%s", repo, collection, rkey),
			"cid": cid,
		})
		return

	case "/xrpc/com.atproto.repo.putRecord":
		var req map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&req)
		repo, _ := req["repo"].(string)
		collection, _ := req["collection"].(string)
		rkey, _ := req["rkey"].(string)
		record, _ := req["record"].(map[string]interface{})
		swapRecord, hasSwap := req["swapRecord"].(string)

		m.mu.Lock()
		defer m.mu.Unlock()

		var cur *raceRecord
		if m.records[repo] != nil {
			cur = m.records[repo][collection][rkey]
		}
		if hasSwap && (cur == nil || cur.cid != swapRecord) {
			m.writeCalls = append(m.writeCalls, fmt.Sprintf("putRecord REJECTED(swapRecord mismatch) repo=%s collection=%s rkey=%s", repo, collection, rkey))
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "InvalidSwap", "message": "record was concurrently updated"})
			return
		}

		m.verN++
		newCid := fmt.Sprintf("cid-%s-v%d", rkey, m.verN)
		if m.records[repo] == nil {
			m.records[repo] = map[string]map[string]*raceRecord{}
		}
		if m.records[repo][collection] == nil {
			m.records[repo][collection] = map[string]*raceRecord{}
		}
		m.records[repo][collection][rkey] = &raceRecord{cid: newCid, value: record}
		m.writeCalls = append(m.writeCalls, fmt.Sprintf("putRecord OK repo=%s collection=%s rkey=%s", repo, collection, rkey))
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"uri": fmt.Sprintf("at://%s/%s/%s", repo, collection, rkey),
			"cid": newCid,
		})
		return

	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// TestRaceMockPDS_SwapRecordMismatch_EmitsExactInvalidSwapString is
// atchess-1c9.114 item 4/atchess-1c9.112's minor follow-up: pin
// raceMockPDS's own swapRecord-mismatch error string to be EXACTLY
// "InvalidSwap" (not merely something isInvalidSwapBody's prefix match
// happens to accept, e.g. the fictional "InvalidSwapError" this mock used
// to emit before atchess-1c9.112 fixed it -- see internal/atproto's
// invalid_swap_body_test.go, whose "InvalidSwapError" case exists ONLY to
// document that historical drift, not because any real server or mock
// still emits it). This is checked directly against raceMockPDS's raw HTTP
// response, independent of isInvalidSwapBody's leniency, so the mock
// cannot silently drift back to that fiction and still pass every other
// test here (which only ever exercises it through isInvalidSwapBody's
// prefix match).
func TestRaceMockPDS_SwapRecordMismatch_EmitsExactInvalidSwapString(t *testing.T) {
	mock := newRaceMockPDS(t)
	srv := mock.server()
	defer srv.Close()
	mock.setBaseURL(srv.URL)

	const repo = "did:plc:swapstringtest"
	const rkey = "rkey1"
	mock.seed(repo, "app.atchess.game", rkey, map[string]interface{}{"$type": "app.atchess.game"})

	body, _ := json.Marshal(map[string]interface{}{
		"repo":       repo,
		"collection": "app.atchess.game",
		"rkey":       rkey,
		"record":     map[string]interface{}{"$type": "app.atchess.game"},
		"swapRecord": "definitely-not-the-current-cid",
	})
	resp, err := http.Post(srv.URL+"/xrpc/com.atproto.repo.putRecord", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("putRecord request to raceMockPDS failed: %v", err)
	}
	defer resp.Body.Close()

	var decoded struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatalf("failed to decode raceMockPDS's swapRecord-mismatch response: %v", err)
	}
	if decoded.Error != "InvalidSwap" {
		t.Errorf("raceMockPDS's swapRecord-mismatch error field = %q, want exactly %q (a live PDS's verified wire format, atchess-1c9.112) -- not merely something isInvalidSwapBody's prefix match happens to accept", decoded.Error, "InvalidSwap")
	}
}

func newRaceSession(did, handle, pdsURL string) *oauth.Session {
	return &oauth.Session{
		DID:                  did,
		Handle:               handle,
		PDSURL:               pdsURL,
		AccessToken:          "test-jwt",
		RefreshToken:         "test-refresh",
		ExpiresAt:            time.Now().Add(time.Hour),
		AccessTokenExpiresAt: time.Now().Add(time.Hour),
	}
}

func doMakeMove(svc *Service, session *oauth.Session, gameURI, from, to string) (int, string) {
	body, _ := json.Marshal(map[string]string{"game_id": gameURI, "from": from, "to": to})
	req := httptest.NewRequest("POST", "/api/moves", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ctx := context.WithValue(req.Context(), contextKeySession, session)
	ctx = context.WithValue(ctx, contextKeyDID, session.DID)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	svc.MakeMoveHandler(w, req)
	return w.Code, w.Body.String()
}

// TestMakeMoveHandler_ConcurrentMove_TwoPlayers_GuardedByTurnGateNotCAS_ExactlyOneSucceeds
// is a regression test for atchess-1c9.74 item 2: when white and black
// submit moves to the SAME game simultaneously, only whoever's turn it
// actually is may succeed; the other must be rejected, and only one move
// record may be written. Both players submit the identical from/to
// ("e2"->"e4") deliberately: it is a legal move for white, so if the "is
// it your turn" gate were ever bypassed, black's identical request would
// also succeed (the notnil/chess engine has no notion of which HTTP
// caller is asking) -- making the guard's absence unambiguous rather than
// merely producing a different error class.
//
// atchess-1c9.114 (this test's NAME, previously just
// "...TwoPlayers_ExactlyOneSucceeds", did not say so): what actually makes
// "exactly one move record" true here is MakeMoveHandler's "is it your
// turn" GATE (internal/web/service.go), evaluated BEFORE RecordMove is
// ever called -- NOT internal/atproto/client.go's RecordMove
// compare-and-swap (swapRecord). Black's request is rejected with 403 at
// the turn gate and never reaches RecordMove at all, so this test can
// never observe a CAS regression: atchess-1c9.112's review proved this by
// mutation -- deleting RecordMove's "swapRecord" entirely still leaves
// this test green. The test that actually exercises and would catch a CAS
// regression is TestMakeMoveHandler_ConcurrentMove_SamePlayerTwice_
// ExactlyOneSucceeds below (same player, same simultaneously-read
// snapshot, so BOTH requests pass the turn gate and the game record's CAS
// is the only thing left to decide the winner).
//
// A callGate forces both goroutines' initial game-record reads (inside
// MakeMoveHandler's client.GetGame call) to happen simultaneously, so
// this is deterministic on every run: neither goroutine can ever observe
// the other's already-applied move before making its own turn decision.
func TestMakeMoveHandler_ConcurrentMove_TwoPlayers_GuardedByTurnGateNotCAS_ExactlyOneSucceeds(t *testing.T) {
	const whiteDID = "did:plc:white"
	const blackDID = "did:plc:black"

	mock := newRaceMockPDS(t)
	srv := mock.server()
	defer srv.Close()
	mock.setBaseURL(srv.URL)
	mock.gate = newCallGate(t, 2)
	mock.gateCollection = "app.atchess.game"

	const gameRkey = "game1"
	mock.seed(whiteDID, "app.atchess.game", gameRkey, map[string]interface{}{
		"$type":     "app.atchess.game",
		"createdAt": time.Now().Format(time.RFC3339),
		"white":     whiteDID,
		"black":     blackDID,
		"status":    "active",
		"fen":       "rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR w KQkq - 0 1",
		"pgn":       "",
	})
	gameURI := fmt.Sprintf("at://%s/app.atchess.game/%s", whiteDID, gameRkey)

	svc := &Service{
		config:         &config.Config{ATProto: config.ATProtoConfig{PLCDirectoryURL: srv.URL}},
		challengeStore: newTestChallengeStore(t),
	}

	whiteSession := newRaceSession(whiteDID, "white.test", srv.URL)
	blackSession := newRaceSession(blackDID, "black.test", srv.URL)

	var wg sync.WaitGroup
	codes := make([]int, 2)
	bodies := make([]string, 2)
	wg.Add(2)
	go func() { defer wg.Done(); codes[0], bodies[0] = doMakeMove(svc, whiteSession, gameURI, "e2", "e4") }()
	go func() { defer wg.Done(); codes[1], bodies[1] = doMakeMove(svc, blackSession, gameURI, "e2", "e4") }()
	wg.Wait()

	// It is genuinely white's turn: this is deterministic, not merely "one
	// of the two ends up succeeding".
	if codes[0] != http.StatusOK {
		t.Errorf("expected white's move to succeed with HTTP 200, got %d: %s", codes[0], bodies[0])
	}
	if codes[1] != http.StatusForbidden {
		t.Errorf("expected black's move to be rejected with HTTP 403, got %d: %s", codes[1], bodies[1])
	} else if !strings.Contains(bodies[1], "It is not your turn") {
		t.Errorf("expected black's rejection body to say 'It is not your turn', got: %s", bodies[1])
	}

	mock.mu.Lock()
	var moveCreates int
	for _, c := range mock.writeCalls {
		if strings.HasPrefix(c, "createRecord") && strings.Contains(c, "collection=app.atchess.move") {
			moveCreates++
			if !strings.Contains(c, "repo="+whiteDID) {
				t.Errorf("expected the sole move record to be in white's repo, got: %s", c)
			}
		}
	}
	writeCalls := append([]string(nil), mock.writeCalls...)
	mock.mu.Unlock()
	if moveCreates != 1 {
		t.Errorf("expected exactly 1 move record created, got %d (writeCalls=%v)", moveCreates, writeCalls)
	}
}

// TestMakeMoveHandler_ConcurrentMove_SamePlayerTwice_ExactlyOneSucceeds is
// a regression test for atchess-1c9.74 item 3: the SAME player submitting
// two different moves for their own single turn simultaneously must have
// exactly one of them succeed; the game must never end up with two move
// records for one turn.
//
// Both goroutines pass the "is it your turn" check identically (same
// player, same simultaneously-read snapshot), so the guard that actually
// makes "exactly one succeeds" true here is downstream, in
// internal/atproto/client.go's RecordMove: its optimistic-concurrency
// (swapRecord) game-record update. A roundGate forces BOTH the initial
// client.GetGame read AND the second read RecordMove itself performs
// (getGameRecord, to fetch the cid to swap against) to be simultaneous
// for both goroutines on every run -- without gating that second read too,
// one goroutine's entire read-then-write could occasionally finish before
// the other's second read even started, letting that second read observe
// the first goroutine's already-applied update and "legitimately" swap
// against it, which would let both writes land and make the test flaky.
func TestMakeMoveHandler_ConcurrentMove_SamePlayerTwice_ExactlyOneSucceeds(t *testing.T) {
	const playerDID = "did:plc:player"
	const opponentDID = "did:plc:opponent"

	mock := newRaceMockPDS(t)
	srv := mock.server()
	defer srv.Close()
	mock.setBaseURL(srv.URL)
	mock.gate = newRoundGate(t, 2)
	mock.gateCollection = "app.atchess.game"

	const gameRkey = "game1"
	mock.seed(playerDID, "app.atchess.game", gameRkey, map[string]interface{}{
		"$type":     "app.atchess.game",
		"createdAt": time.Now().Format(time.RFC3339),
		"white":     playerDID,
		"black":     opponentDID,
		"status":    "active",
		"fen":       "rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR w KQkq - 0 1",
		"pgn":       "",
	})
	gameURI := fmt.Sprintf("at://%s/app.atchess.game/%s", playerDID, gameRkey)

	svc := &Service{
		config:         &config.Config{ATProto: config.ATProtoConfig{PLCDirectoryURL: srv.URL}},
		challengeStore: newTestChallengeStore(t),
	}

	session := newRaceSession(playerDID, "player.test", srv.URL)

	var wg sync.WaitGroup
	codes := make([]int, 2)
	bodies := make([]string, 2)
	wg.Add(2)
	go func() { defer wg.Done(); codes[0], bodies[0] = doMakeMove(svc, session, gameURI, "e2", "e4") }()
	go func() { defer wg.Done(); codes[1], bodies[1] = doMakeMove(svc, session, gameURI, "d2", "d4") }()
	wg.Wait()

	var successes, failures int
	for i, c := range codes {
		switch c {
		case http.StatusOK:
			successes++
		case http.StatusConflict:
			// atchess-1c9.87: a CAS conflict (the game record's swapRecord
			// lost the race) is not a server fault, so the loser gets
			// 409, not 500 -- distinct from a genuine RecordMove failure
			// (network error, PDS 5xx, malformed response), which is
			// still mapped to 500 (see TestMakeMoveHandler_RecordMove_
			// NonConflictFailureStill500).
			failures++
			if !strings.Contains(bodies[i], "Failed to record move") {
				t.Errorf("expected failure body to say 'Failed to record move', got: %s", bodies[i])
			}
		default:
			t.Errorf("unexpected status %d: %s", c, bodies[i])
		}
	}
	if successes != 1 || failures != 1 {
		t.Fatalf("expected exactly 1 success and 1 failure (CAS conflict), got codes=%v bodies=%v", codes, bodies)
	}

	mock.mu.Lock()
	var moveCreates int
	for _, c := range mock.writeCalls {
		if strings.HasPrefix(c, "createRecord") && strings.Contains(c, "collection=app.atchess.move") {
			moveCreates++
		}
	}
	writeCalls := append([]string(nil), mock.writeCalls...)
	mock.mu.Unlock()
	if moveCreates != 1 {
		t.Errorf("expected exactly 1 move record created despite both submissions reaching RecordMove, got %d (writeCalls=%v)", moveCreates, writeCalls)
	}
}

// TestMakeMoveHandler_ConcurrentMove_SamePlayerTwice_NonOwner_ExactlyOneSucceeds
// is the atchess-1c9.113 headline regression test, and the reviewer's exact
// repro during atchess-1c9.112: TestMakeMoveHandler_ConcurrentMove_
// SamePlayerTwice_ExactlyOneSucceeds above seeds the game record in the
// MOVER's own repo (playerDID), so it only ever exercises the OWNER side of
// RecordMove -- the CAS-protected "if repo == c.did" block. This test seeds
// the identical scenario (same player, two different moves for one turn,
// submitted concurrently) but with the game record living in the
// OPPONENT's repo instead, so playerDID never owns it and RecordMove's CAS
// never runs at all. Before atchess-1c9.113's deterministic move rkey, this
// produced codes=[200 200] and two move records for one turn.
func TestMakeMoveHandler_ConcurrentMove_SamePlayerTwice_NonOwner_ExactlyOneSucceeds(t *testing.T) {
	const playerDID = "did:plc:player"
	const opponentDID = "did:plc:opponent"

	mock := newRaceMockPDS(t)
	srv := mock.server()
	defer srv.Close()
	mock.setBaseURL(srv.URL)
	mock.gate = newRoundGate(t, 2)
	mock.gateCollection = "app.atchess.game"

	const gameRkey = "game1"
	// Seeded under opponentDID, NOT playerDID: playerDID (the mover) is
	// the non-owner of this game record.
	mock.seed(opponentDID, "app.atchess.game", gameRkey, map[string]interface{}{
		"$type":     "app.atchess.game",
		"createdAt": time.Now().Format(time.RFC3339),
		"white":     playerDID,
		"black":     opponentDID,
		"status":    "active",
		"fen":       "rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR w KQkq - 0 1",
		"pgn":       "",
	})
	gameURI := fmt.Sprintf("at://%s/app.atchess.game/%s", opponentDID, gameRkey)

	svc := &Service{
		config:         &config.Config{ATProto: config.ATProtoConfig{PLCDirectoryURL: srv.URL}},
		challengeStore: newTestChallengeStore(t),
	}

	session := newRaceSession(playerDID, "player.test", srv.URL)

	var wg sync.WaitGroup
	codes := make([]int, 2)
	bodies := make([]string, 2)
	wg.Add(2)
	go func() { defer wg.Done(); codes[0], bodies[0] = doMakeMove(svc, session, gameURI, "e2", "e4") }()
	go func() { defer wg.Done(); codes[1], bodies[1] = doMakeMove(svc, session, gameURI, "d2", "d4") }()
	wg.Wait()

	var successes, failures int
	for i, c := range codes {
		switch c {
		case http.StatusOK:
			successes++
		case http.StatusConflict:
			// atchess-1c9.113: the move record's deterministic (game, ply)
			// rkey collided with a DIFFERENT move at the same ply --
			// ErrMoveRecordConflict, mapped to 409 exactly like
			// ErrGameRecordConflict.
			failures++
			if !strings.Contains(bodies[i], "Failed to record move") {
				t.Errorf("expected failure body to say 'Failed to record move', got: %s", bodies[i])
			}
		default:
			t.Errorf("unexpected status %d: %s", c, bodies[i])
		}
	}
	if successes != 1 || failures != 1 {
		t.Fatalf("expected exactly 1 success and 1 failure (move-record conflict), got codes=%v bodies=%v", codes, bodies)
	}

	mock.mu.Lock()
	var moveCreates int
	for _, c := range mock.writeCalls {
		if strings.HasPrefix(c, "createRecord") && strings.Contains(c, "collection=app.atchess.move") {
			moveCreates++
			if !strings.Contains(c, "repo="+playerDID) {
				t.Errorf("expected the sole move record to be in the mover's (playerDID's) repo, got: %s", c)
			}
		}
	}
	writeCalls := append([]string(nil), mock.writeCalls...)
	mock.mu.Unlock()
	if moveCreates != 1 {
		t.Errorf("expected exactly 1 move record created for the non-owner despite both submissions reaching RecordMove, got %d (writeCalls=%v)", moveCreates, writeCalls)
	}
}

// TestMakeMoveHandler_ConcurrentMove_SamePlayerTwice_NonOwner_IdenticalRetry_Idempotent
// covers the OTHER half of atchess-1c9.113's fix: a double-click or a
// client retrying a request whose response was dropped submits the SAME
// move twice, not two different ones. Unlike the "two different moves"
// case above (which must yield exactly one success and one 409 conflict),
// an identical resubmission must be treated as an idempotent success --
// both requests report success, and still exactly one move record exists.
func TestMakeMoveHandler_ConcurrentMove_SamePlayerTwice_NonOwner_IdenticalRetry_Idempotent(t *testing.T) {
	const playerDID = "did:plc:player"
	const opponentDID = "did:plc:opponent"

	mock := newRaceMockPDS(t)
	srv := mock.server()
	defer srv.Close()
	mock.setBaseURL(srv.URL)
	mock.gate = newRoundGate(t, 2)
	mock.gateCollection = "app.atchess.game"

	const gameRkey = "game1"
	mock.seed(opponentDID, "app.atchess.game", gameRkey, map[string]interface{}{
		"$type":     "app.atchess.game",
		"createdAt": time.Now().Format(time.RFC3339),
		"white":     playerDID,
		"black":     opponentDID,
		"status":    "active",
		"fen":       "rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR w KQkq - 0 1",
		"pgn":       "",
	})
	gameURI := fmt.Sprintf("at://%s/app.atchess.game/%s", opponentDID, gameRkey)

	svc := &Service{
		config:         &config.Config{ATProto: config.ATProtoConfig{PLCDirectoryURL: srv.URL}},
		challengeStore: newTestChallengeStore(t),
	}

	session := newRaceSession(playerDID, "player.test", srv.URL)

	var wg sync.WaitGroup
	codes := make([]int, 2)
	bodies := make([]string, 2)
	wg.Add(2)
	go func() { defer wg.Done(); codes[0], bodies[0] = doMakeMove(svc, session, gameURI, "e2", "e4") }()
	go func() { defer wg.Done(); codes[1], bodies[1] = doMakeMove(svc, session, gameURI, "e2", "e4") }()
	wg.Wait()

	for i, c := range codes {
		if c != http.StatusOK {
			t.Errorf("expected an identical-move retry to be idempotently accepted with HTTP 200, got %d: %s", c, bodies[i])
		}
	}

	mock.mu.Lock()
	var moveCreates int
	for _, c := range mock.writeCalls {
		if strings.HasPrefix(c, "createRecord") && strings.Contains(c, "collection=app.atchess.move") {
			moveCreates++
		}
	}
	writeCalls := append([]string(nil), mock.writeCalls...)
	mock.mu.Unlock()
	if moveCreates != 1 {
		t.Errorf("expected exactly 1 move record created for two identical concurrent submissions, got %d (writeCalls=%v)", moveCreates, writeCalls)
	}
}

// nonConflictFailurePDS is a minimal mock PDS purpose-built for
// TestMakeMoveHandler_RecordMove_NonConflictFailureStill500 (atchess-1c9.87
// item 3): it serves the game record's getRecord normally, but always
// fails its putRecord with a genuine server error that carries NO
// "InvalidSwap"-prefixed structured "error" body -- the opposite of
// raceMockPDS's swapRecord-mismatch rejection above. This proves
// isInvalidSwapBody (internal/atproto/client.go) does not match too
// broadly: a real outage (an unreachable dependency, a malformed PDS
// response, an ordinary 500) must still surface as ErrGameRecordConflict's
// generic sibling error, which MakeMoveHandler maps to HTTP 500, NOT to
// 409 -- if it did, a client would be told to "just retry" a failure that
// retrying cannot fix.
type nonConflictFailurePDS struct {
	base string
}

func (p *nonConflictFailurePDS) server(t *testing.T) *httptest.Server {
	srv := newFakeHTTPSEndpoint(t, http.HandlerFunc(p.handle))
	p.base = srv.URL
	return srv
}

func (p *nonConflictFailurePDS) handle(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if strings.HasPrefix(r.URL.Path, "/did:plc:") {
		did := strings.TrimPrefix(r.URL.Path, "/")
		_ = json.NewEncoder(w).Encode(atproto.DIDDocument{
			ID: did,
			Service: []atproto.DIDService{
				{ID: "#atproto_pds", Type: "AtprotoPersonalDataServer", ServiceEndpoint: p.base},
			},
		})
		return
	}

	switch r.URL.Path {
	case "/xrpc/com.atproto.repo.getRecord":
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"uri": "at://did:plc:player/app.atchess.game/game1",
			"cid": "cid-game1-v1",
			"value": map[string]interface{}{
				"$type":     "app.atchess.game",
				"createdAt": time.Now().Format(time.RFC3339),
				"white":     "did:plc:player",
				"black":     "did:plc:opponent",
				"status":    "active",
				"fen":       "rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR w KQkq - 0 1",
				"pgn":       "",
			},
		})
		return

	case "/xrpc/com.atproto.repo.listRecords":
		// GetGame's derived-status scan (see ErrIncompleteDerivation) asks
		// for moves/resignations/timeViolations/drawResponses for both
		// players before MakeMoveHandler ever reaches RecordMove --
		// answer with an always-empty, always-200 list so that scan
		// succeeds and the game derives as active, exactly as it should
		// for this test's freshly-seeded, move-free game.
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"records": []interface{}{}})
		return

	case "/xrpc/com.atproto.repo.putRecord":
		// A genuine server-side failure, NOT a swapRecord conflict: no
		// "error" field at all, let alone one prefixed "InvalidSwap".
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": "internal database error"})
		return

	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// TestMakeMoveHandler_RecordMove_NonConflictFailureStill500 proves the
// atchess-1c9.87 sentinel mapping is narrow: a RecordMove failure that is
// NOT a CAS conflict (here, an ordinary PDS 500 with no InvalidSwap-shaped
// body) must still surface to the client as HTTP 500, not be
// misclassified as a 409 "just retry" conflict.
func TestMakeMoveHandler_RecordMove_NonConflictFailureStill500(t *testing.T) {
	const playerDID = "did:plc:player"

	pds := &nonConflictFailurePDS{}
	srv := pds.server(t)
	defer srv.Close()

	svc := &Service{
		config:         &config.Config{ATProto: config.ATProtoConfig{PLCDirectoryURL: srv.URL}},
		challengeStore: newTestChallengeStore(t),
	}

	session := newRaceSession(playerDID, "player.test", srv.URL)
	gameURI := "at://did:plc:player/app.atchess.game/game1"

	code, body := doMakeMove(svc, session, gameURI, "e2", "e4")

	if code != http.StatusInternalServerError {
		t.Fatalf("expected a non-conflict RecordMove failure to yield HTTP 500, got %d: %s", code, body)
	}
	if !strings.Contains(body, "Failed to record move") {
		t.Errorf("expected failure body to say 'Failed to record move', got: %s", body)
	}
	if strings.Contains(body, "conflict") {
		t.Errorf("non-conflict failure body must not claim a conflict, got: %s", body)
	}
}

// moveCreateFailurePDS is a minimal mock PDS purpose-built for
// TestMakeMoveHandler_RecordMove_MoveCreateFailure_RecordAbsent_Still500
// (atchess-1c9.115 item 2). It serves the game record's getRecord/putRecord
// normally (so RecordMove's game-record CAS update succeeds), but always
// fails the MOVE record's createRecord with a bare, non-distinguishing HTTP
// 500 -- exactly the shape atchess-1c9.113 verified live for a duplicate-
// rkey collision -- while never actually writing anything. A subsequent
// getRecord for that move rkey therefore genuinely reports the record
// absent (RecordNotFound), not merely a stale/partial value: this is the
// "create failed for a real reason, not a collision" branch RecordMove's
// post-failure read-back must fail closed on (surface the original error,
// HTTP 500 at the handler level), never fold into an idempotent success.
type moveCreateFailurePDS struct {
	base string
}

func (p *moveCreateFailurePDS) server(t *testing.T) *httptest.Server {
	srv := newFakeHTTPSEndpoint(t, http.HandlerFunc(p.handle))
	p.base = srv.URL
	return srv
}

func (p *moveCreateFailurePDS) handle(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if strings.HasPrefix(r.URL.Path, "/did:plc:") {
		did := strings.TrimPrefix(r.URL.Path, "/")
		_ = json.NewEncoder(w).Encode(atproto.DIDDocument{
			ID: did,
			Service: []atproto.DIDService{
				{ID: "#atproto_pds", Type: "AtprotoPersonalDataServer", ServiceEndpoint: p.base},
			},
		})
		return
	}

	switch r.URL.Path {
	case "/xrpc/com.atproto.repo.getRecord":
		q := r.URL.Query()
		if q.Get("collection") == "app.atchess.game" {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"uri": "at://did:plc:player/app.atchess.game/game1",
				"cid": "cid-game1-v1",
				"value": map[string]interface{}{
					"$type":     "app.atchess.game",
					"createdAt": time.Now().Format(time.RFC3339),
					"white":     "did:plc:player",
					"black":     "did:plc:opponent",
					"status":    "active",
					"fen":       "rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR w KQkq - 0 1",
					"pgn":       "",
				},
			})
			return
		}
		// app.atchess.move: nothing was ever written (createRecord below
		// always fails before storing anything), so a real PDS answers
		// "not found" here, not a stale/partial value.
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "RecordNotFound", "message": "could not locate record"})
		return

	case "/xrpc/com.atproto.repo.listRecords":
		// GetGame's derived-status scan needs an always-empty, always-200
		// list to derive this freshly-seeded, move-free game as active
		// before MakeMoveHandler ever reaches RecordMove.
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"records": []interface{}{}})
		return

	case "/xrpc/com.atproto.repo.putRecord":
		// The game record's CAS-protected update succeeds normally: this
		// test isolates the failure to the MOVE record's createRecord
		// below, not the game record's putRecord (already covered by
		// TestMakeMoveHandler_RecordMove_NonConflictFailureStill500 above).
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"uri": "at://did:plc:player/app.atchess.game/game1",
			"cid": "cid-game1-v2",
		})
		return

	case "/xrpc/com.atproto.repo.createRecord":
		// A genuine server-side failure on the move record's FIRST and
		// ONLY creation attempt -- not a duplicate-rkey collision, and
		// nothing is ever stored. No structured "already exists" (or any
		// other) signal in the body, exactly like the live bare 500
		// atchess-1c9.113 measured for a real rkey collision: RecordMove
		// cannot distinguish this response, by shape, from that one. The
		// only thing that tells them apart is the read-back afterward
		// finding the record absent (this case) vs. present and matching
		// (a collision). See client.go's RecordMove doc comment.
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": "internal database error"})
		return

	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// TestMakeMoveHandler_RecordMove_MoveCreateFailure_RecordAbsent_Still500 is
// the atchess-1c9.115 item 2 fail-closed regression test: atchess-1c9.113's
// post-collision read-back must surface the ORIGINAL error (and thus HTTP
// 500 at the handler level) when the move record is genuinely absent after
// a failed createRecord, rather than ever synthesizing an idempotent
// success for a create that never actually landed. Before this test
// existed, nothing in the repo pinned that branch: atchess-1c9.87's
// analogous test (TestMakeMoveHandler_RecordMove_NonConflictFailureStill500
// above) covers only the game record's putRecord, not the move record's
// createRecord.
func TestMakeMoveHandler_RecordMove_MoveCreateFailure_RecordAbsent_Still500(t *testing.T) {
	const playerDID = "did:plc:player"

	pds := &moveCreateFailurePDS{}
	srv := pds.server(t)
	defer srv.Close()

	svc := &Service{
		config:         &config.Config{ATProto: config.ATProtoConfig{PLCDirectoryURL: srv.URL}},
		challengeStore: newTestChallengeStore(t),
	}

	session := newRaceSession(playerDID, "player.test", srv.URL)
	gameURI := "at://did:plc:player/app.atchess.game/game1"

	code, body := doMakeMove(svc, session, gameURI, "e2", "e4")

	if code != http.StatusInternalServerError {
		t.Fatalf("expected a move-record createRecord failure with the record genuinely absent on read-back to yield HTTP 500 (fail closed, never an idempotent success), got %d: %s", code, body)
	}
	if !strings.Contains(body, "Failed to record move") {
		t.Errorf("expected failure body to say 'Failed to record move', got: %s", body)
	}
}
