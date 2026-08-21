package atproto

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/justinabrahms/atchess/internal/chess"
)

// deriveTestPDS is a minimal read-only stand-in for both a did:plc
// directory and every player's PDS, used to exercise GetGame's cross-repo
// terminal-event derivation (getResignationOutcome, getTimeViolationOutcome,
// getDrawAcceptOutcome, latestTerminalEvent -- atchess-1c9.48) without a
// real dual-PDS harness. Every did:plc:<x> resolves (via a "/did:plc:<x>"
// path) to this same server as that DID's serviceEndpoint, mirroring
// internal/web's mockFederatedPDS, and com.atproto.repo.getRecord/
// listRecords are served against an in-memory repo->collection->rkey->value
// store this test populates directly with seed (no createRecord/putRecord
// support -- these tests are about reading/deriving, not writing).
type deriveTestPDS struct {
	t    *testing.T
	base string

	mu      sync.Mutex
	records map[string]map[string]map[string]interface{} // repo -> collection -> rkey -> value

	// didDocFail, when set for a repo DID, makes that DID's DID document
	// lookup itself fail (HTTP 404), so resolveReadEndpoint's PLC
	// resolution never even produces a serviceEndpoint -- distinct from
	// unreachableEndpoint (endpoint resolves, but nothing answers there)
	// and listFail (endpoint answers, but the specific listRecords call
	// misbehaves). See setDIDDocFail/setUnreachable/setListFail.
	didDocFail map[string]bool

	// unreachableEndpoint, when set for a repo DID, makes that DID resolve
	// successfully to the given serviceEndpoint, which has nothing
	// listening on it -- simulating an opponent-PDS outage (atchess-1c9.51)
	// distinct from DID resolution itself failing.
	unreachableEndpoint map[string]string

	// listFail, when set for a repo DID, makes every
	// com.atproto.repo.listRecords call against that repo (regardless of
	// collection) misbehave in the given way: "http500" returns a bare
	// HTTP 500, "malformed" returns HTTP 200 with a body that is not valid
	// JSON.
	listFail map[string]string

	// writeCalls counts every com.atproto.repo.createRecord/putRecord
	// request this server has actually received, regardless of outcome.
	// Used by RespondToDrawOffer's terminal-game guard tests
	// (atchess-1c9.56) to prove a rejected response performed ZERO writes
	// -- asserting on requests actually observed by the fake server, not
	// on RespondToDrawOffer's return value. rkeySeq generates rkeys for
	// writes that don't specify one (createRecord never does).
	writeCalls int
	rkeySeq    int
}

// writeCallCount returns the number of com.atproto.repo.createRecord/
// putRecord requests this server has received so far.
func (m *deriveTestPDS) writeCallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.writeCalls
}

func newDeriveTestPDS(t *testing.T) *deriveTestPDS {
	return &deriveTestPDS{t: t, records: map[string]map[string]map[string]interface{}{}}
}

// seed inserts a record at an explicit rkey (rather than generating one) so
// tests can control TID ordering precisely -- see
// TestGetGame_SameSecondTie_ResolvedByTIDRkey. Returns the record's at://
// URI and synthetic CID ("cid-"+rkey), matching the shape RecordMove et al.
// actually embed as strongRefs.
func (m *deriveTestPDS) seed(repo, collection, rkey string, value map[string]interface{}) (uri, cid string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.records[repo] == nil {
		m.records[repo] = map[string]map[string]interface{}{}
	}
	if m.records[repo][collection] == nil {
		m.records[repo][collection] = map[string]interface{}{}
	}
	m.records[repo][collection][rkey] = value
	return fmt.Sprintf("at://%s/%s/%s", repo, collection, rkey), "cid-" + rkey
}

// server starts m's mock PDS on a real local TLS listener and advertises a
// validator-passing fake hostname (see newFakeHTTPSEndpoint,
// atchess-1c9.95) rather than the listener's own http://127.0.0.1:<port>,
// which parseServiceEndpoint now refuses to accept as a DID document's
// serviceEndpoint.
func (m *deriveTestPDS) server() *httptest.Server {
	srv := newFakeHTTPSEndpoint(m.t, http.HandlerFunc(m.handle))
	m.mu.Lock()
	m.base = srv.URL
	m.mu.Unlock()
	return srv
}

func (m *deriveTestPDS) handle(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if strings.HasPrefix(r.URL.Path, "/did:plc:") {
		did := strings.TrimPrefix(r.URL.Path, "/")
		m.mu.Lock()
		base := m.base
		fail := m.didDocFail[did]
		if u, ok := m.unreachableEndpoint[did]; ok {
			base = u
		}
		m.mu.Unlock()
		if fail {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "DIDNotFound"})
			return
		}
		_ = json.NewEncoder(w).Encode(DIDDocument{
			ID: did,
			Service: []DIDService{
				{ID: "#atproto_pds", Type: atprotoPDSServiceType, ServiceEndpoint: base},
			},
		})
		return
	}

	switch r.URL.Path {
	case "/xrpc/com.atproto.server.createSession":
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"accessJwt": "test-jwt",
			"did":       "did:plc:white",
			"handle":    "white.test",
		})
		return

	case "/xrpc/com.atproto.repo.getRecord":
		q := r.URL.Query()
		repo, collection, rkey := q.Get("repo"), q.Get("collection"), q.Get("rkey")
		m.mu.Lock()
		val, ok := m.records[repo][collection][rkey]
		m.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "RecordNotFound"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"uri":   fmt.Sprintf("at://%s/%s/%s", repo, collection, rkey),
			"cid":   "cid-" + rkey,
			"value": val,
		})
		return

	case "/xrpc/com.atproto.repo.listRecords":
		q := r.URL.Query()
		repo, collection := q.Get("repo"), q.Get("collection")
		cursor := q.Get("cursor")
		limit := 100
		if l := q.Get("limit"); l != "" {
			if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 {
				limit = parsed
			}
		}
		m.mu.Lock()
		fail := m.listFail[repo]
		coll := m.records[repo][collection]
		m.mu.Unlock()
		switch fail {
		case "http500":
			w.WriteHeader(http.StatusInternalServerError)
			return
		case "malformed":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("{not valid json"))
			return
		}
		type rec struct {
			URI   string      `json:"uri"`
			CID   string      `json:"cid"`
			Value interface{} `json:"value"`
		}
		// Real AT Protocol listRecords pagination: records are ordered by
		// rkey (DESCENDING -- newest first -- by default; a "reverse" query
		// param would flip that, but nothing in this codebase sends one).
		// Confirmed empirically against the live local dual-PDS test
		// harness's real PDS implementation (atchess-1c9.119 fix-pass: both
		// a hash-rkey app.atchess.move collection and a TID-rkey
		// app.atchess.game collection came back newest-rkey-first with no
		// query parameters beyond repo/collection/limit). A page's
		// "cursor" (when present) is the rkey to resume immediately AFTER
		// in that same descending sequence, i.e. the next STRICTLY SMALLER
		// rkey. This mock reproduces that shape -- rather than always
		// returning every record on one page -- specifically so
		// atchess-1c9.119's tests (a repo with more than one page of
		// records) can exercise real multi-page traversal, not just a
		// single oversized page, AND so a double that returns the WRONG
		// order can't hide a client bug that only manifests when order
		// doesn't match production (this package's client code is
		// deliberately order-agnostic -- see listAllRecords's doc comment
		// -- but the double should still tell the truth about what the
		// real server actually does).
		var rkeys []string
		for rkey := range coll {
			rkeys = append(rkeys, rkey)
		}
		sort.Sort(sort.Reverse(sort.StringSlice(rkeys)))
		start := 0
		if cursor != "" {
			for i, rkey := range rkeys {
				if rkey < cursor {
					start = i
					break
				}
				start = i + 1
			}
		}
		end := start + limit
		if end > len(rkeys) {
			end = len(rkeys)
		}
		var recs []rec
		for _, rkey := range rkeys[start:end] {
			recs = append(recs, rec{URI: fmt.Sprintf("at://%s/%s/%s", repo, collection, rkey), CID: "cid-" + rkey, Value: coll[rkey]})
		}
		respBody := map[string]interface{}{"records": recs}
		if end < len(rkeys) {
			respBody["cursor"] = rkeys[end-1]
		}
		_ = json.NewEncoder(w).Encode(respBody)
		return

	case "/xrpc/com.atproto.repo.createRecord", "/xrpc/com.atproto.repo.putRecord":
		// Minimal write support, added for RespondToDrawOffer's
		// terminal-game guard tests (atchess-1c9.56): every request here
		// is counted in writeCalls (regardless of outcome) so tests can
		// assert on requests actually observed by this server, and the
		// record is stored so a legitimate accept-in-an-active-game test
		// can observe a real success end to end.
		var body struct {
			Repo       string                 `json:"repo"`
			Collection string                 `json:"collection"`
			Rkey       string                 `json:"rkey"`
			Record     map[string]interface{} `json:"record"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			m.mu.Lock()
			m.writeCalls++
			m.mu.Unlock()
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "InvalidRequest"})
			return
		}
		m.mu.Lock()
		m.writeCalls++
		rkey := body.Rkey
		if rkey == "" {
			m.rkeySeq++
			rkey = fmt.Sprintf("auto%d", m.rkeySeq)
		}
		if m.records[body.Repo] == nil {
			m.records[body.Repo] = map[string]map[string]interface{}{}
		}
		if m.records[body.Repo][body.Collection] == nil {
			m.records[body.Repo][body.Collection] = map[string]interface{}{}
		}
		m.records[body.Repo][body.Collection][rkey] = body.Record
		m.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"uri": fmt.Sprintf("at://%s/%s/%s", body.Repo, body.Collection, rkey),
			"cid": "cid-" + rkey,
		})
		return

	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// setDIDDocFail makes did's DID document lookup itself return HTTP 404, so
// resolveReadEndpoint's PLC resolution fails before ever producing a
// serviceEndpoint.
func (m *deriveTestPDS) setDIDDocFail(did string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.didDocFail == nil {
		m.didDocFail = map[string]bool{}
	}
	m.didDocFail[did] = true
}

// setUnreachable makes did resolve successfully to a serviceEndpoint that
// PASSES atproto's fetched-endpoint validation (https, non-IP-literal host
// -- see newUnreachableFakeHost) but has NOTHING listening behind it --
// simulating an opponent-PDS outage (atchess-1c9.51) distinct from DID
// resolution itself failing.
//
// atchess-1c9.95 fix-pass (reviewer-flagged): this used to take an
// explicit endpoint string, and every call site passed a real, closed,
// PLAIN-HTTP httptest.Server's address. Once parseServiceEndpoint started
// validating serviceEndpoint (this same bead), that value started failing
// at VALIDATION time instead of connection time -- same outcome (an
// error), a DIFFERENT reason: the atchess-1c9.51 "opponent PDS is
// unreachable" code path this helper exists to exercise was never
// actually reached again, even though every assertion at each call site
// kept passing. newUnreachableFakeHost's validator-passing-but-dead
// address restores that: see TestGetGame_IncompleteDerivation_OpponentUnreachable_Resigned's
// (and this file's other two setUnreachable callers') doc comments for
// the "prove it fails for the right reason" verification this fix-pass
// re-ran.
func (m *deriveTestPDS) setUnreachable(t *testing.T, did string) {
	t.Helper()
	endpoint := newUnreachableFakeHost(t)

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.unreachableEndpoint == nil {
		m.unreachableEndpoint = map[string]string{}
	}
	m.unreachableEndpoint[did] = endpoint
}

// setServiceEndpoint makes did's DID document advertise endpoint
// VERBATIM as its serviceEndpoint, bypassing setUnreachable's own
// validator-passing-but-dead address generation -- used by
// atchess-1c9.95's SSRF regression tests (service_endpoint_ssrf_test.go),
// which need to declare an ARBITRARY (including hostile) endpoint string
// rather than a guaranteed-safe-shaped one. Shares the same underlying
// unreachableEndpoint map as setUnreachable: the DID-document handler
// (see handle) does not distinguish why an override was set, only that
// one was.
func (m *deriveTestPDS) setServiceEndpoint(did, endpoint string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.unreachableEndpoint == nil {
		m.unreachableEndpoint = map[string]string{}
	}
	m.unreachableEndpoint[did] = endpoint
}

// setListFail makes every com.atproto.repo.listRecords call against did's
// repo misbehave in the given way ("http500" or "malformed" -- see handle).
func (m *deriveTestPDS) setListFail(did, mode string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.listFail == nil {
		m.listFail = map[string]string{}
	}
	m.listFail[did] = mode
}

// newDeriveTestClient wires up a *Client (as white.test / did:plc:white)
// against mock, with mock also standing in for the PLC directory.
func newDeriveTestClient(t *testing.T, mock *deriveTestPDS) *Client {
	t.Helper()
	client, err := NewClient(mock.base, "white.test", "password")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	client.SetPLCDirectoryURL(mock.base)
	return client
}

const (
	whiteDID = "did:plc:white"
	blackDID = "did:plc:black"
)

// seedActiveGame seeds a minimal, otherwise-unfinished app.atchess.game
// record owned by whiteDID and returns its gameURI. createdAt lets callers
// control the game's age (relevant for timeViolation tests, which measure
// elapsed time against it when there are no moves yet).
func (m *deriveTestPDS) seedActiveGame(t *testing.T, createdAt time.Time, timeControl map[string]interface{}) string {
	t.Helper()
	value := map[string]interface{}{
		"$type":     "app.atchess.game",
		"createdAt": createdAt.Format(time.RFC3339),
		"white":     whiteDID,
		"black":     blackDID,
		"status":    "active",
		"fen":       "rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR w KQkq - 0 1",
		"pgn":       "",
	}
	if timeControl != nil {
		value["timeControl"] = timeControl
	}
	uri, _ := m.seed(whiteDID, "app.atchess.game", "game1", value)
	return uri
}

func TestGetGame_WhiteResigns_BlackWon(t *testing.T) {
	mock := newDeriveTestPDS(t)
	srv := mock.server()
	defer srv.Close()

	gameURI := mock.seedActiveGame(t, time.Now().Add(-time.Hour), nil)

	// White resigns: the record lives in white's OWN repo (as ResignGame
	// always writes it) and names white as the resigning player.
	mock.seed(whiteDID, "app.atchess.resignation", "resign1", map[string]interface{}{
		"$type":           "app.atchess.resignation",
		"createdAt":       time.Now().Format(time.RFC3339),
		"game":            map[string]interface{}{"uri": gameURI},
		"resigningPlayer": whiteDID,
	})

	client := newDeriveTestClient(t, mock)
	game, err := client.GetGame(context.Background(), gameURI)
	if err != nil {
		t.Fatalf("GetGame: %v", err)
	}
	if game.Status != chess.StatusBlackWon {
		t.Errorf("expected black_won (white resigned), got %q", game.Status)
	}
}

func TestGetGame_BlackResigns_WhiteWon(t *testing.T) {
	mock := newDeriveTestPDS(t)
	srv := mock.server()
	defer srv.Close()

	gameURI := mock.seedActiveGame(t, time.Now().Add(-time.Hour), nil)

	// Black resigns: the record lives in black's OWN repo, not the game
	// owner's (white's) -- the ordinary cross-repo case this whole
	// derivation mechanism exists for.
	mock.seed(blackDID, "app.atchess.resignation", "resign1", map[string]interface{}{
		"$type":           "app.atchess.resignation",
		"createdAt":       time.Now().Format(time.RFC3339),
		"game":            map[string]interface{}{"uri": gameURI},
		"resigningPlayer": blackDID,
	})

	client := newDeriveTestClient(t, mock)
	game, err := client.GetGame(context.Background(), gameURI)
	if err != nil {
		t.Fatalf("GetGame: %v", err)
	}
	if game.Status != chess.StatusWhiteWon {
		t.Errorf("expected white_won (black resigned), got %q", game.Status)
	}
}

func TestGetGame_DrawAccepted_Draw(t *testing.T) {
	mock := newDeriveTestPDS(t)
	srv := mock.server()
	defer srv.Close()

	gameURI := mock.seedActiveGame(t, time.Now().Add(-time.Hour), nil)

	offerURI, offerCID := mock.seed(whiteDID, "app.atchess.drawOffer", "offer1", map[string]interface{}{
		"$type":     "app.atchess.drawOffer",
		"createdAt": time.Now().Format(time.RFC3339),
		"game":      map[string]interface{}{"uri": gameURI, "cid": "cid-game1"},
		"offeredBy": whiteDID,
		"status":    "pending",
	})

	// Black accepts: the response lives in black's OWN repo (as
	// RespondToDrawOffer always writes it) and references the real offer
	// above by strongRef.
	mock.seed(blackDID, "app.atchess.drawResponse", "resp1", map[string]interface{}{
		"$type":       "app.atchess.drawResponse",
		"createdAt":   time.Now().Format(time.RFC3339),
		"drawOffer":   map[string]interface{}{"uri": offerURI, "cid": offerCID},
		"game":        map[string]interface{}{"uri": gameURI, "cid": "cid-game1"},
		"respondedBy": blackDID,
		"response":    "accepted",
	})

	client := newDeriveTestClient(t, mock)
	game, err := client.GetGame(context.Background(), gameURI)
	if err != nil {
		t.Fatalf("GetGame: %v", err)
	}
	if game.Status != chess.StatusDraw {
		t.Errorf("expected draw, got %q", game.Status)
	}
}

func TestGetGame_DrawDeclined_StaysActive(t *testing.T) {
	mock := newDeriveTestPDS(t)
	srv := mock.server()
	defer srv.Close()

	gameURI := mock.seedActiveGame(t, time.Now().Add(-time.Hour), nil)

	offerURI, offerCID := mock.seed(whiteDID, "app.atchess.drawOffer", "offer1", map[string]interface{}{
		"$type":     "app.atchess.drawOffer",
		"createdAt": time.Now().Format(time.RFC3339),
		"game":      map[string]interface{}{"uri": gameURI, "cid": "cid-game1"},
		"offeredBy": whiteDID,
		"status":    "pending",
	})

	mock.seed(blackDID, "app.atchess.drawResponse", "resp1", map[string]interface{}{
		"$type":       "app.atchess.drawResponse",
		"createdAt":   time.Now().Format(time.RFC3339),
		"drawOffer":   map[string]interface{}{"uri": offerURI, "cid": offerCID},
		"game":        map[string]interface{}{"uri": gameURI, "cid": "cid-game1"},
		"respondedBy": blackDID,
		"response":    "declined",
	})

	client := newDeriveTestClient(t, mock)
	game, err := client.GetGame(context.Background(), gameURI)
	if err != nil {
		t.Fatalf("GetGame: %v", err)
	}
	if game.Status != chess.StatusActive {
		t.Errorf("expected active (draw declined, not accepted), got %q", game.Status)
	}
}

func TestGetGame_TimeViolation_WhiteWon(t *testing.T) {
	mock := newDeriveTestPDS(t)
	srv := mock.server()
	defer srv.Close()

	gameCreated := time.Now().Add(-10 * 24 * time.Hour)
	gameURI := mock.seedActiveGame(t, gameCreated, map[string]interface{}{
		"type":        "correspondence",
		"daysPerMove": float64(3),
	})

	// White made the only move 5 days ago (well past the 3-day limit),
	// putting it on black to move -- and black never did.
	moveAt := time.Now().Add(-5 * 24 * time.Hour)
	mock.seed(whiteDID, "app.atchess.move", "move1", map[string]interface{}{
		"$type":     "app.atchess.move",
		"createdAt": moveAt.Format(time.RFC3339),
		"game":      map[string]interface{}{"uri": gameURI},
		"player":    whiteDID,
		"from":      "e2",
		"to":        "e4",
		"san":       "e4",
		"fen":       "rnbqkbnr/pppppppp/8/8/4P3/8/PPPP1PPP/RNBQKBNR b KQkq - 0 1",
	})

	// White claims the violation: the record lives in white's OWN repo
	// (as ClaimTimeVictory always writes it), names white as the claimant
	// and black as the violator, and its own createdAt is well after the
	// real deadline (moveAt + 3 days).
	mock.seed(whiteDID, "app.atchess.timeViolation", "violation1", map[string]interface{}{
		"$type":             "app.atchess.timeViolation",
		"createdAt":         time.Now().Format(time.RFC3339),
		"game":              map[string]interface{}{"uri": gameURI},
		"claimingPlayer":    whiteDID,
		"violatingPlayer":   blackDID,
		"lastMoveTimestamp": moveAt.Format(time.RFC3339),
		"timeControlType":   "correspondence",
		"daysPerMove":       float64(3),
	})

	client := newDeriveTestClient(t, mock)
	game, err := client.GetGame(context.Background(), gameURI)
	if err != nil {
		t.Fatalf("GetGame: %v", err)
	}
	if game.Status != chess.StatusWhiteWon {
		t.Errorf("expected white_won (black violated time control), got %q", game.Status)
	}
}

// --- resolveTimeControl agreement tests (atchess-1c9.88) ---
//
// getTimeViolationOutcome (reached via GetGame) and CheckTimeViolation
// (reached via ClaimTimeVictory) used to disagree about what an absent
// timeControl means: GetGame's derivation treated it as "no timeout is
// ever possible", while CheckTimeViolation defaulted it to a 3-day
// correspondence limit and ClaimTimeVictory happily awarded a win (and
// wrote a real timeViolation record) on that basis -- a player could be
// timed out of a game GetGame's own derived status insisted was still
// active. Both paths now resolve through the single resolveTimeControl
// function; these two tests pin that they reach the SAME conclusion for
// the same underlying facts (not just individually plausible ones), for a
// game with NO persisted timeControl at all -- the only case that occurs
// in production today, since nothing in this codebase writes timeControl
// yet (atchess-1c9.88/.90).

// seedActiveGameNoTimeControlAfterMove seeds an app.atchess.game record
// (no "timeControl" field, no "challenge" field -- exactly what every real
// game looks like today) together with the single move that already
// happened moveAt, putting the FEN's turn on black. Both CheckTimeViolation
// (via the raw game record's own "fen" field) and GetGame's own move scan
// need to agree it is black's turn, so the move's resulting FEN is used
// for the game record's cached "fen" too, mirroring how RecordMove keeps
// them in sync in production.
//
// The game record is deliberately seeded into BLACK's repo (gameURI is
// at://blackDID/app.atchess.game/game1) even though white is the one who
// will claim the violation: ClaimTimeVictory only patches its record's
// cached "status" field directly when it owns the underlying repo the
// gameURI points to (client.go: `parts[2] == c.did`), which is NOT
// guaranteed -- a game can be, and often will be, canonically owned by the
// player who is NOT the one claiming the violation. If the game record
// were owned by white (the claimant) instead, that direct patch would
// mask a getTimeViolationOutcome/GetGame regression: GetGame's raw-status
// fallback would happen to already say the right thing even if the
// terminal-event derivation itself were broken, and this test would pass
// for the wrong reason. Owning it via black instead means GetGame's
// answer here can ONLY come from real derivation.
func seedActiveGameNoTimeControlAfterMove(mock *deriveTestPDS, moveAt time.Time) string {
	const fenAfterE4 = "rnbqkbnr/pppppppp/8/8/4P3/8/PPPP1PPP/RNBQKBNR b KQkq - 0 1"
	gameURI, _ := mock.seed(blackDID, "app.atchess.game", "game1", map[string]interface{}{
		"$type":     "app.atchess.game",
		"createdAt": moveAt.Add(-time.Hour).Format(time.RFC3339),
		"white":     whiteDID,
		"black":     blackDID,
		"status":    "active",
		"fen":       fenAfterE4,
		"pgn":       "1. e4",
	})
	mock.seed(whiteDID, "app.atchess.move", "move1", map[string]interface{}{
		"$type":     "app.atchess.move",
		"createdAt": moveAt.Format(time.RFC3339),
		"game":      map[string]interface{}{"uri": gameURI},
		"player":    whiteDID,
		"from":      "e2",
		"to":        "e4",
		"san":       "e4",
		"fen":       fenAfterE4,
	})
	return gameURI
}

// TestTimeControlAgreement_AbsentTimeControl_OldViolation_BothAwardTheWin
// exercises the real end-to-end paths -- an actual ClaimTimeVictory call
// (which decides via CheckTimeViolation and, if it decides yes, really
// writes the timeViolation record) followed by a fresh GetGame call
// against the same server/game -- on a game with no persisted timeControl
// at all. If ClaimTimeVictory awards the win but GetGame doesn't
// recognise it (the exact bug atchess-1c9.88 fixes), this test must fail.
func TestTimeControlAgreement_AbsentTimeControl_OldViolation_BothAwardTheWin(t *testing.T) {
	mock := newDeriveTestPDS(t)
	srv := mock.server()
	defer srv.Close()

	// White made the only move well past the resolved correspondence
	// default, putting it on black to move -- and black never did.
	moveAt := time.Now().Add(-time.Duration(defaultDaysPerMove+2) * 24 * time.Hour)
	gameURI := seedActiveGameNoTimeControlAfterMove(mock, moveAt)

	client := newDeriveTestClient(t, mock) // authenticates as whiteDID

	if err := client.ClaimTimeVictory(context.Background(), gameURI); err != nil {
		t.Fatalf("ClaimTimeVictory: expected it to award the win (black overstayed the resolved correspondence default), got error: %v", err)
	}

	game, err := client.GetGame(context.Background(), gameURI)
	if err != nil {
		t.Fatalf("GetGame: %v", err)
	}
	if game.Status != chess.StatusWhiteWon {
		t.Errorf("disagreement: ClaimTimeVictory awarded white the win, but GetGame's derived status says %q, not white_won -- the two time-control paths disagree again", game.Status)
	}
}

// TestTimeControlAgreement_AbsentTimeControl_YoungViolation_BothRefuse
// mirrors the above but with the same move well inside the resolved
// correspondence default: ClaimTimeVictory must refuse, and separately
// (mirroring TestGetGame_PrematureTimeViolation_Rejected, but for an
// absent rather than an explicit timeControl) GetGame must also refuse a
// premature claim that was filed anyway -- this is
// getTimeViolationOutcome's second job, preserved by atchess-1c9.88's fix,
// distinct from the defaulting concern the first test above covers.
func TestTimeControlAgreement_AbsentTimeControl_YoungViolation_BothRefuse(t *testing.T) {
	mock := newDeriveTestPDS(t)
	srv := mock.server()
	defer srv.Close()

	moveAt := time.Now().Add(-24 * time.Hour) // 1 day -- inside the 3-day default
	gameURI := seedActiveGameNoTimeControlAfterMove(mock, moveAt)

	client := newDeriveTestClient(t, mock)

	if err := client.ClaimTimeVictory(context.Background(), gameURI); err == nil {
		t.Fatalf("ClaimTimeVictory: expected it to refuse (black is still well inside the resolved correspondence default), got no error")
	}

	// FORGERY: white claims the violation anyway, well before its own
	// deadline could have elapsed, with an absent timeControl (the real
	// scenario, unlike TestGetGame_PrematureTimeViolation_Rejected's
	// explicit correspondence/3).
	mock.seed(whiteDID, "app.atchess.timeViolation", "forged1", map[string]interface{}{
		"$type":             "app.atchess.timeViolation",
		"createdAt":         time.Now().Format(time.RFC3339),
		"game":              map[string]interface{}{"uri": gameURI},
		"claimingPlayer":    whiteDID,
		"violatingPlayer":   blackDID,
		"lastMoveTimestamp": moveAt.Format(time.RFC3339),
	})

	game, err := client.GetGame(context.Background(), gameURI)
	if err != nil {
		t.Fatalf("GetGame: %v", err)
	}
	if game.Status != chess.StatusActive {
		t.Errorf("disagreement: ClaimTimeVictory correctly refused (still inside the resolved correspondence default), but GetGame's derived status honoured a premature claim anyway: got %q, not active", game.Status)
	}
}

// TestGetGame_SameSecondTie_ResolvedByTIDRkey is a regression test for the
// (createdAt, TID) tie-break latestTerminalEvent/moveIsAfter perform when
// two candidate terminal events share the same second-resolution
// createdAt: whichever has the lexicographically greater rkey wins, the
// same rule already applied to moves.
func TestGetGame_SameSecondTie_ResolvedByTIDRkey(t *testing.T) {
	mock := newDeriveTestPDS(t)
	srv := mock.server()
	defer srv.Close()

	gameCreated := time.Now().Add(-10 * 24 * time.Hour)
	gameURI := mock.seedActiveGame(t, gameCreated, map[string]interface{}{
		"type":        "correspondence",
		"daysPerMove": float64(3),
	})

	moveAt := time.Now().Add(-5 * 24 * time.Hour)
	mock.seed(whiteDID, "app.atchess.move", "move1", map[string]interface{}{
		"$type":     "app.atchess.move",
		"createdAt": moveAt.Format(time.RFC3339),
		"game":      map[string]interface{}{"uri": gameURI},
		"player":    whiteDID,
		"from":      "e2",
		"to":        "e4",
		"san":       "e4",
		"fen":       "rnbqkbnr/pppppppp/8/8/4P3/8/PPPP1PPP/RNBQKBNR b KQkq - 0 1",
	})

	sameSecond := time.Now().Truncate(time.Second)

	// Two DIFFERENT, both individually-valid, terminal events land in the
	// exact same second: black resigns (-> white_won) and white also
	// claims a (legitimately verifiable) time violation against black
	// (-> white_won too, as it happens, but via a different rkey/source,
	// so this only proves the tie-break -- not the outcome -- since both
	// award the same winner here would be ambiguous; use a resignation by
	// WHITE instead so the two candidates disagree and the winner
	// unambiguously reveals which one "won" the tie).
	mock.seed(whiteDID, "app.atchess.resignation", "3zzzzzzzzzzzz", map[string]interface{}{
		"$type":           "app.atchess.resignation",
		"createdAt":       sameSecond.Format(time.RFC3339),
		"game":            map[string]interface{}{"uri": gameURI},
		"resigningPlayer": whiteDID, // -> black_won
	})
	mock.seed(whiteDID, "app.atchess.timeViolation", "3aaaaaaaaaaaa", map[string]interface{}{
		"$type":             "app.atchess.timeViolation",
		"createdAt":         sameSecond.Format(time.RFC3339),
		"game":              map[string]interface{}{"uri": gameURI},
		"claimingPlayer":    whiteDID,
		"violatingPlayer":   blackDID, // -> white_won
		"lastMoveTimestamp": moveAt.Format(time.RFC3339),
		"timeControlType":   "correspondence",
		"daysPerMove":       float64(3),
	})

	client := newDeriveTestClient(t, mock)
	game, err := client.GetGame(context.Background(), gameURI)
	if err != nil {
		t.Fatalf("GetGame: %v", err)
	}
	// "3zzzzzzzzzzzz" > "3aaaaaaaaaaaa" lexicographically, so the
	// resignation (black_won) must win the tie over the timeViolation
	// (white_won), regardless of iteration/map order.
	if game.Status != chess.StatusBlackWon {
		t.Errorf("expected black_won (resignation's rkey sorts after the timeViolation's), got %q", game.Status)
	}
}

// --- atchess-1c9.101: terminal move vs resignation/drawAccept same-second
// tie, decided by kind rather than TID ---
//
// atchess-1c9.100 fixed move-vs-move ordering; atchess-1c9.101's
// reachability analysis proved the only remaining cross-kind tie possible
// in practice is a terminal move (checkmate, or a rules-forced draw) vs a
// same-second resignation or accepted draw offer. These are CONSTRUCTED
// regression tests, not timing tests: each move's rkey is deliberately
// chosen to sort LOWER, lexicographically, than the competing
// resignation/drawResponse rkey -- the OPPOSITE of what the old
// moveIsAfter TID tiebreak needed in order to (wrongly) pick the move.
// They only pass if terminalEventIsAfter's kind-based rule -- a checkmate
// is not negotiable, so it beats a resignation/draw-agreement on a tie,
// regardless of TID -- is what decides the outcome.

// scholarsMateWinFEN is the position after 1.e4 e5 2.Bc4 Nc6 3.Qh5 Nf6??
// 4.Qxf7#: white delivers checkmate. Active color "b" (black to move, but
// mated) is what latestTerminalEvent's caller (GetGame) reads to decide
// checkmate's winner -- see the "fenParts[1] == \"w\"" check next to
// moveEvent's construction.
const scholarsMateWinFEN = "r1bqkb1r/pppp1Qpp/2n2n2/4p3/2B1P3/8/PPPP1PPP/RNB1K1NR b KQkq - 0 4"

// TestGetGame_SameSecondTie_CheckmateMoveBeatsResignation pins the
// reachable worst case atchess-1c9.101 describes: white's mating move
// lands in the same recorded second as (an adversarial/pathological, but
// individually well-formed) resignation record that disagrees with the
// board. The move's rkey sorts LOWER than the resignation's, so the old
// TID tiebreak would have picked the resignation; the fix must still pick
// the move.
func TestGetGame_SameSecondTie_CheckmateMoveBeatsResignation(t *testing.T) {
	mock := newDeriveTestPDS(t)
	srv := mock.server()
	defer srv.Close()

	gameURI := mock.seedActiveGame(t, time.Now().Add(-time.Hour), nil)
	sameSecond := time.Now().Truncate(time.Second)

	mock.seed(whiteDID, "app.atchess.move", "3aaaaaaaaaaaa", map[string]interface{}{
		"$type":     "app.atchess.move",
		"createdAt": sameSecond.Format(time.RFC3339),
		"game":      map[string]interface{}{"uri": gameURI},
		"player":    whiteDID,
		"from":      "h5",
		"to":        "f7",
		"san":       "Qxf7#",
		"fen":       scholarsMateWinFEN,
		"checkmate": true,
	})

	// Same second, white ALSO (pathologically) resigns -- a claim that, if
	// honoured, would flip the outcome to black_won. Its rkey is chosen
	// HIGHER than the move's, the direction the old TID tiebreak needed to
	// pick it as "later".
	mock.seed(whiteDID, "app.atchess.resignation", "3zzzzzzzzzzzz", map[string]interface{}{
		"$type":           "app.atchess.resignation",
		"createdAt":       sameSecond.Format(time.RFC3339),
		"game":            map[string]interface{}{"uri": gameURI},
		"resigningPlayer": whiteDID, // -> black_won, if honoured
	})

	client := newDeriveTestClient(t, mock)
	game, err := client.GetGame(context.Background(), gameURI)
	if err != nil {
		t.Fatalf("GetGame: %v", err)
	}
	if game.Status != chess.StatusWhiteWon {
		t.Errorf("expected white_won (checkmate is structurally decisive over a same-second resignation), got %q", game.Status)
	}
}

// TestGetGame_SameSecondTie_CheckmateMoveBeatsDrawAccept is the bead's
// headline example: white's mating move ties, in the same recorded second,
// with an accepted draw offer -- "the UI shows a checkmated board labelled
// draw" if TID decides it. As above, the move's rkey sorts LOWER than the
// drawResponse's, the opposite of what the old TID tiebreak needed.
func TestGetGame_SameSecondTie_CheckmateMoveBeatsDrawAccept(t *testing.T) {
	mock := newDeriveTestPDS(t)
	srv := mock.server()
	defer srv.Close()

	gameURI := mock.seedActiveGame(t, time.Now().Add(-time.Hour), nil)
	sameSecond := time.Now().Truncate(time.Second)

	mock.seed(whiteDID, "app.atchess.move", "3aaaaaaaaaaaa", map[string]interface{}{
		"$type":     "app.atchess.move",
		"createdAt": sameSecond.Format(time.RFC3339),
		"game":      map[string]interface{}{"uri": gameURI},
		"player":    whiteDID,
		"from":      "h5",
		"to":        "f7",
		"san":       "Qxf7#",
		"fen":       scholarsMateWinFEN,
		"checkmate": true,
	})

	offerURI, offerCID := mock.seed(blackDID, "app.atchess.drawOffer", "offer1", map[string]interface{}{
		"$type":     "app.atchess.drawOffer",
		"createdAt": sameSecond.Format(time.RFC3339),
		"game":      map[string]interface{}{"uri": gameURI, "cid": "cid-game1"},
		"offeredBy": blackDID,
		"status":    "pending",
	})
	// Same second, white ALSO accepts a draw offer -- a claim that, if
	// honoured, would flip the outcome to draw. Its rkey is chosen HIGHER
	// than the move's, the direction the old TID tiebreak needed.
	mock.seed(whiteDID, "app.atchess.drawResponse", "3zzzzzzzzzzzz", map[string]interface{}{
		"$type":       "app.atchess.drawResponse",
		"createdAt":   sameSecond.Format(time.RFC3339),
		"drawOffer":   map[string]interface{}{"uri": offerURI, "cid": offerCID},
		"game":        map[string]interface{}{"uri": gameURI, "cid": "cid-game1"},
		"respondedBy": whiteDID,
		"response":    "accepted",
	})

	client := newDeriveTestClient(t, mock)
	game, err := client.GetGame(context.Background(), gameURI)
	if err != nil {
		t.Fatalf("GetGame: %v", err)
	}
	if game.Status != chess.StatusWhiteWon {
		t.Errorf("expected white_won (checkmate is structurally decisive over a same-second draw acceptance), got %q", game.Status)
	}
}

// TestLatestTerminalEvent_MoveBeatsResignation_OrderIndependent proves the
// fix does not depend on which candidate happens to be read/passed first --
// unlike the old moveIsAfter TID tiebreak, which was symmetric anyway, this
// directly pins terminalEventIsAfter's kind-based rule under BOTH argument
// orderings, standing in for "which repo's scan happened to finish first".
func TestLatestTerminalEvent_MoveBeatsResignation_OrderIndependent(t *testing.T) {
	sameSecond := time.Now().Truncate(time.Second)
	moveEvent := &terminalEvent{status: chess.StatusWhiteWon, at: sameSecond, rkey: "3aaaaaaaaaaaa", kind: terminalEventFromMove}
	resignationEvent := &terminalEvent{status: chess.StatusBlackWon, at: sameSecond, rkey: "3zzzzzzzzzzzz", kind: terminalEventFromResignation}

	if got := latestTerminalEvent(moveEvent, resignationEvent); got != moveEvent {
		t.Errorf("move-first order: expected the move event to win, got status %q", got.status)
	}
	if got := latestTerminalEvent(resignationEvent, moveEvent); got != moveEvent {
		t.Errorf("resignation-first order: expected the move event to STILL win (repo-read order must not matter), got status %q", got.status)
	}
}

// TestLatestTerminalEvent_MoveBeatsDrawAccept_OrderIndependent is the
// drawAccept counterpart of the above.
func TestLatestTerminalEvent_MoveBeatsDrawAccept_OrderIndependent(t *testing.T) {
	sameSecond := time.Now().Truncate(time.Second)
	moveEvent := &terminalEvent{status: chess.StatusBlackWon, at: sameSecond, rkey: "3aaaaaaaaaaaa", kind: terminalEventFromMove}
	drawAcceptEvent := &terminalEvent{status: chess.StatusDraw, at: sameSecond, rkey: "3zzzzzzzzzzzz", kind: terminalEventFromDrawAccept}

	if got := latestTerminalEvent(moveEvent, drawAcceptEvent); got != moveEvent {
		t.Errorf("move-first order: expected the move event to win, got status %q", got.status)
	}
	if got := latestTerminalEvent(drawAcceptEvent, moveEvent); got != moveEvent {
		t.Errorf("drawAccept-first order: expected the move event to STILL win (repo-read order must not matter), got status %q", got.status)
	}
}

// --- Forgery / soundness regression tests (atchess-1c9.48 review) ---

// TestGetGame_ForgedResignation_Rejected proves black cannot unilaterally
// declare a black_won outcome by writing a resignation record into black's
// OWN repo that names white (not black) as the resigning player.
func TestGetGame_ForgedResignation_Rejected(t *testing.T) {
	mock := newDeriveTestPDS(t)
	srv := mock.server()
	defer srv.Close()

	gameURI := mock.seedActiveGame(t, time.Now().Add(-time.Hour), nil)

	// FORGERY: written into BLACK's own repo, but claims WHITE resigned.
	mock.seed(blackDID, "app.atchess.resignation", "forged1", map[string]interface{}{
		"$type":           "app.atchess.resignation",
		"createdAt":       time.Now().Format(time.RFC3339),
		"game":            map[string]interface{}{"uri": gameURI},
		"resigningPlayer": whiteDID,
	})

	client := newDeriveTestClient(t, mock)
	game, err := client.GetGame(context.Background(), gameURI)
	if err != nil {
		t.Fatalf("GetGame: %v", err)
	}
	if game.Status != chess.StatusActive {
		t.Errorf("expected the forged resignation to be ignored (status active), got %q -- a player can declare themselves the winner by writing into their own repo", game.Status)
	}
}

// TestGetGame_ForgedDrawAccept_NoMatchingOffer_Rejected proves a player
// cannot unilaterally produce a draw by writing an "accepted"
// drawResponse with no corresponding drawOffer anywhere.
func TestGetGame_ForgedDrawAccept_NoMatchingOffer_Rejected(t *testing.T) {
	mock := newDeriveTestPDS(t)
	srv := mock.server()
	defer srv.Close()

	gameURI := mock.seedActiveGame(t, time.Now().Add(-time.Hour), nil)

	// FORGERY: black "accepts" a draw offer that was never made -- the
	// referenced drawOffer URI does not exist anywhere.
	mock.seed(blackDID, "app.atchess.drawResponse", "forged1", map[string]interface{}{
		"$type":     "app.atchess.drawResponse",
		"createdAt": time.Now().Format(time.RFC3339),
		"drawOffer": map[string]interface{}{
			"uri": fmt.Sprintf("at://%s/app.atchess.drawOffer/nonexistent", whiteDID),
			"cid": "cid-nonexistent",
		},
		"game":        map[string]interface{}{"uri": gameURI, "cid": "cid-game1"},
		"respondedBy": blackDID,
		"response":    "accepted",
	})

	client := newDeriveTestClient(t, mock)
	game, err := client.GetGame(context.Background(), gameURI)
	if err != nil {
		t.Fatalf("GetGame: %v", err)
	}
	if game.Status != chess.StatusActive {
		t.Errorf("expected the forged draw acceptance to be ignored (status active, no offer no draw), got %q", game.Status)
	}
}

// TestGetGame_PrematureTimeViolation_Rejected proves a timeViolation claim
// is checked against the real elapsed time (derived from the actual last
// move / game creation) rather than accepted on bare assertion: a claim
// filed before its own deadline could possibly have elapsed must be
// ignored.
func TestGetGame_PrematureTimeViolation_Rejected(t *testing.T) {
	mock := newDeriveTestPDS(t)
	srv := mock.server()
	defer srv.Close()

	gameCreated := time.Now().Add(-time.Hour)
	gameURI := mock.seedActiveGame(t, gameCreated, map[string]interface{}{
		"type":        "correspondence",
		"daysPerMove": float64(3),
	})

	// No moves have happened at all -- the game was created 1 hour ago,
	// nowhere near the 3-day deadline. FORGERY: white claims a time
	// violation against black anyway, immediately.
	mock.seed(whiteDID, "app.atchess.timeViolation", "forged1", map[string]interface{}{
		"$type":             "app.atchess.timeViolation",
		"createdAt":         time.Now().Format(time.RFC3339),
		"game":              map[string]interface{}{"uri": gameURI},
		"claimingPlayer":    whiteDID,
		"violatingPlayer":   blackDID,
		"lastMoveTimestamp": gameCreated.Format(time.RFC3339),
		"timeControlType":   "correspondence",
		"daysPerMove":       float64(3),
	})

	client := newDeriveTestClient(t, mock)
	game, err := client.GetGame(context.Background(), gameURI)
	if err != nil {
		t.Fatalf("GetGame: %v", err)
	}
	if game.Status != chess.StatusActive {
		t.Errorf("expected the premature timeViolation claim to be ignored (status active), got %q -- a time-violation deadline must be re-derived from real timestamps, not trusted on assertion", game.Status)
	}
}

// --- Incomplete-derivation / fail-closed regression tests (atchess-1c9.51) ---
//
// Each of these seeds a resignation record in WHITE's OWN repo (as
// ResignGame always writes it -- see TestGetGame_WhiteResigns_BlackWon),
// which resolveReadEndpoint always reads directly against this client's own
// PDS (c.did shortcut) regardless of PLC/network state, then breaks reading
// BLACK's repo in one specific way. This isolates "the opponent's repo
// could not be scanned" from "the terminal event itself could not be
// found": the resignation IS found (via white's reachable repo), so if
// GetGame's error handling were accidentally dropped, the status would
// still come back correct (black_won) and these tests would pass for the
// wrong reason -- which is exactly why each asserts directly on the
// returned error via errors.Is, not merely on the status.

func TestGetGame_IncompleteDerivation_OpponentUnreachable_Resigned(t *testing.T) {
	mock := newDeriveTestPDS(t)
	srv := mock.server()
	defer srv.Close()

	// black's DID resolves fine, to a validator-passing serviceEndpoint --
	// but nothing is listening behind it, so every XRPC call against it
	// fails with a connection error (atchess-1c9.95 fix-pass: see
	// setUnreachable's doc comment for why this can no longer be a plain
	// closed httptest.Server's own address).
	mock.setUnreachable(t, blackDID)

	gameURI := mock.seedActiveGame(t, time.Now().Add(-time.Hour), nil)
	mock.seed(whiteDID, "app.atchess.resignation", "resign1", map[string]interface{}{
		"$type":           "app.atchess.resignation",
		"createdAt":       time.Now().Format(time.RFC3339),
		"game":            map[string]interface{}{"uri": gameURI},
		"resigningPlayer": whiteDID,
	})

	client := newDeriveTestClient(t, mock)
	game, err := client.GetGame(context.Background(), gameURI)
	if err == nil {
		t.Fatalf("expected a non-nil error (black's PDS is unreachable), got nil with status %q", game.Status)
	}
	if !errors.Is(err, ErrIncompleteDerivation) {
		t.Errorf("expected errors.Is(err, ErrIncompleteDerivation), got: %v", err)
	}
	if game == nil {
		t.Fatalf("expected a non-nil partial *Game alongside the error")
	}
	if !game.DerivationIncomplete {
		t.Errorf("expected game.DerivationIncomplete == true")
	}
	if game.Status == chess.StatusActive {
		t.Errorf("must not report the game as active when derivation could not be verified; got %q", game.Status)
	}
}

func TestGetGame_IncompleteDerivation_OpponentHTTP500_Resigned(t *testing.T) {
	mock := newDeriveTestPDS(t)
	srv := mock.server()
	defer srv.Close()

	mock.setListFail(blackDID, "http500")

	gameURI := mock.seedActiveGame(t, time.Now().Add(-time.Hour), nil)
	mock.seed(whiteDID, "app.atchess.resignation", "resign1", map[string]interface{}{
		"$type":           "app.atchess.resignation",
		"createdAt":       time.Now().Format(time.RFC3339),
		"game":            map[string]interface{}{"uri": gameURI},
		"resigningPlayer": whiteDID,
	})

	client := newDeriveTestClient(t, mock)
	game, err := client.GetGame(context.Background(), gameURI)
	if err == nil {
		t.Fatalf("expected a non-nil error (black's PDS returns HTTP 500), got nil with status %q", game.Status)
	}
	if !errors.Is(err, ErrIncompleteDerivation) {
		t.Errorf("expected errors.Is(err, ErrIncompleteDerivation), got: %v", err)
	}
	if !game.DerivationIncomplete {
		t.Errorf("expected game.DerivationIncomplete == true")
	}
	if game.Status == chess.StatusActive {
		t.Errorf("must not report the game as active when derivation could not be verified; got %q", game.Status)
	}
}

func TestGetGame_IncompleteDerivation_OpponentMalformedJSON_Resigned(t *testing.T) {
	mock := newDeriveTestPDS(t)
	srv := mock.server()
	defer srv.Close()

	mock.setListFail(blackDID, "malformed")

	gameURI := mock.seedActiveGame(t, time.Now().Add(-time.Hour), nil)
	mock.seed(whiteDID, "app.atchess.resignation", "resign1", map[string]interface{}{
		"$type":           "app.atchess.resignation",
		"createdAt":       time.Now().Format(time.RFC3339),
		"game":            map[string]interface{}{"uri": gameURI},
		"resigningPlayer": whiteDID,
	})

	client := newDeriveTestClient(t, mock)
	game, err := client.GetGame(context.Background(), gameURI)
	if err == nil {
		t.Fatalf("expected a non-nil error (black's PDS returns malformed JSON), got nil with status %q", game.Status)
	}
	if !errors.Is(err, ErrIncompleteDerivation) {
		t.Errorf("expected errors.Is(err, ErrIncompleteDerivation), got: %v", err)
	}
	if !game.DerivationIncomplete {
		t.Errorf("expected game.DerivationIncomplete == true")
	}
	if game.Status == chess.StatusActive {
		t.Errorf("must not report the game as active when derivation could not be verified; got %q", game.Status)
	}
}

func TestGetGame_IncompleteDerivation_OpponentDIDResolutionFails_Resigned(t *testing.T) {
	mock := newDeriveTestPDS(t)
	srv := mock.server()
	defer srv.Close()

	mock.setDIDDocFail(blackDID)

	gameURI := mock.seedActiveGame(t, time.Now().Add(-time.Hour), nil)
	mock.seed(whiteDID, "app.atchess.resignation", "resign1", map[string]interface{}{
		"$type":           "app.atchess.resignation",
		"createdAt":       time.Now().Format(time.RFC3339),
		"game":            map[string]interface{}{"uri": gameURI},
		"resigningPlayer": whiteDID,
	})

	client := newDeriveTestClient(t, mock)
	game, err := client.GetGame(context.Background(), gameURI)
	if err == nil {
		t.Fatalf("expected a non-nil error (black's DID document cannot be resolved), got nil with status %q", game.Status)
	}
	if !errors.Is(err, ErrIncompleteDerivation) {
		t.Errorf("expected errors.Is(err, ErrIncompleteDerivation), got: %v", err)
	}
	if !game.DerivationIncomplete {
		t.Errorf("expected game.DerivationIncomplete == true")
	}
	if game.Status == chess.StatusActive {
		t.Errorf("must not report the game as active when derivation could not be verified; got %q", game.Status)
	}
}

// TestGetGame_NegativeControl_BothReposReadable_NoTerminalEvent_Active
// proves the new error path does not fire unconditionally: with both
// repos fully readable and no terminal event anywhere, GetGame must still
// return a nil error and StatusActive, exactly as before atchess-1c9.51.
func TestGetGame_NegativeControl_BothReposReadable_NoTerminalEvent_Active(t *testing.T) {
	mock := newDeriveTestPDS(t)
	srv := mock.server()
	defer srv.Close()

	gameURI := mock.seedActiveGame(t, time.Now().Add(-time.Hour), nil)

	client := newDeriveTestClient(t, mock)
	game, err := client.GetGame(context.Background(), gameURI)
	if err != nil {
		t.Fatalf("GetGame: unexpected error with both repos fully readable and no terminal event: %v", err)
	}
	if game.DerivationIncomplete {
		t.Errorf("expected game.DerivationIncomplete == false")
	}
	if game.Status != chess.StatusActive {
		t.Errorf("expected active, got %q", game.Status)
	}
}

// TestResignGame_FailsClosed_WhenDerivationIncomplete is a regression test
// for the fail-open bug found while auditing currentGameStatus's callers
// during atchess-1c9.51: ResignGame used to only block on
// "err == nil && status != active", so a derivation error (opponent's PDS
// unreachable) was silently treated the same as "verified active",
// letting a resignation through unchecked. It must now reject instead.
func TestResignGame_FailsClosed_WhenDerivationIncomplete(t *testing.T) {
	mock := newDeriveTestPDS(t)
	srv := mock.server()
	defer srv.Close()

	// atchess-1c9.95 fix-pass: see setUnreachable's doc comment -- must be
	// a validator-passing-but-dead endpoint, not a plain closed
	// httptest.Server's own address.
	mock.setUnreachable(t, blackDID)

	gameURI := mock.seedActiveGame(t, time.Now().Add(-time.Hour), nil)

	client := newDeriveTestClient(t, mock)
	err := client.ResignGame(context.Background(), gameURI, "")
	if err == nil {
		t.Fatalf("expected ResignGame to fail closed when the game's status could not be verified (black's PDS unreachable), got nil error")
	}
	if !errors.Is(err, ErrIncompleteDerivation) {
		t.Errorf("expected errors.Is(err, ErrIncompleteDerivation), got: %v", err)
	}
}

// --- RespondToDrawOffer terminal-game guard tests (atchess-1c9.56) ---
//
// RespondToDrawOffer used to gate only on the draw-offer record's own
// "status" field, never on the derived game status -- unlike OfferDraw,
// ResignGame and CheckTimeViolation. So a draw could be accepted into a
// game that had already ended (e.g. after the opponent resigned),
// producing two competing terminal events for one game. These tests seed
// the offer in whiteDID's own repo and respond as the white client (the
// only identity this harness's createSession stub can log in as -- see
// newDeriveTestClient); RespondToDrawOffer places no restriction on who
// may respond to whose offer, so this exercises the guard without needing
// a second client identity.

// TestRespondToDrawOffer_RejectedInResignedGame proves an accept is
// refused, and performs ZERO writes, once the game has already ended via
// resignation.
func TestRespondToDrawOffer_RejectedInResignedGame(t *testing.T) {
	mock := newDeriveTestPDS(t)
	srv := mock.server()
	defer srv.Close()

	gameURI := mock.seedActiveGame(t, time.Now().Add(-time.Hour), nil)

	offerURI, _ := mock.seed(whiteDID, "app.atchess.drawOffer", "offer1", map[string]interface{}{
		"$type":     "app.atchess.drawOffer",
		"createdAt": time.Now().Format(time.RFC3339),
		"game":      map[string]interface{}{"uri": gameURI, "cid": "cid-game1"},
		"offeredBy": whiteDID,
		"status":    "pending",
	})

	// White resigns after making the offer -- the game is now terminal
	// (black_won) even though the drawOffer record itself still says
	// "pending".
	mock.seed(whiteDID, "app.atchess.resignation", "resign1", map[string]interface{}{
		"$type":           "app.atchess.resignation",
		"createdAt":       time.Now().Format(time.RFC3339),
		"game":            map[string]interface{}{"uri": gameURI},
		"resigningPlayer": whiteDID,
	})

	client := newDeriveTestClient(t, mock)
	err := client.RespondToDrawOffer(context.Background(), offerURI, true)
	if err == nil {
		t.Fatalf("expected RespondToDrawOffer to reject an accept into a resigned (terminal) game, got nil error")
	}
	if got := mock.writeCallCount(); got != 0 {
		t.Errorf("expected ZERO createRecord/putRecord writes when the accept is rejected, got %d", got)
	}
}

// TestRespondToDrawOffer_FailsClosed_WhenDerivationIncomplete proves an
// accept is refused, and performs ZERO writes, when the game's derived
// status cannot be verified at all (here: black's PDS is unreachable),
// matching the fail-closed pattern OfferDraw/ResignGame/CheckTimeViolation
// already use (atchess-1c9.51).
func TestRespondToDrawOffer_FailsClosed_WhenDerivationIncomplete(t *testing.T) {
	mock := newDeriveTestPDS(t)
	srv := mock.server()
	defer srv.Close()

	// atchess-1c9.95 fix-pass: see setUnreachable's doc comment -- must be
	// a validator-passing-but-dead endpoint, not a plain closed
	// httptest.Server's own address.
	mock.setUnreachable(t, blackDID)

	gameURI := mock.seedActiveGame(t, time.Now().Add(-time.Hour), nil)

	offerURI, _ := mock.seed(whiteDID, "app.atchess.drawOffer", "offer1", map[string]interface{}{
		"$type":     "app.atchess.drawOffer",
		"createdAt": time.Now().Format(time.RFC3339),
		"game":      map[string]interface{}{"uri": gameURI, "cid": "cid-game1"},
		"offeredBy": whiteDID,
		"status":    "pending",
	})

	client := newDeriveTestClient(t, mock)
	err := client.RespondToDrawOffer(context.Background(), offerURI, true)
	if err == nil {
		t.Fatalf("expected RespondToDrawOffer to fail closed when the game's status could not be verified (black's PDS unreachable), got nil error")
	}
	if !errors.Is(err, ErrIncompleteDerivation) {
		t.Errorf("expected errors.Is(err, ErrIncompleteDerivation), got: %v", err)
	}
	if got := mock.writeCallCount(); got != 0 {
		t.Errorf("expected ZERO createRecord/putRecord writes when derivation is incomplete, got %d", got)
	}
}

// TestRespondToDrawOffer_Accept_ActiveGame_Succeeds is the required
// negative control: a legitimate accept in a still-active game must still
// succeed, so the new guard is not simply blocking everything.
func TestRespondToDrawOffer_Accept_ActiveGame_Succeeds(t *testing.T) {
	mock := newDeriveTestPDS(t)
	srv := mock.server()
	defer srv.Close()

	gameURI := mock.seedActiveGame(t, time.Now().Add(-time.Hour), nil)

	offerURI, _ := mock.seed(whiteDID, "app.atchess.drawOffer", "offer1", map[string]interface{}{
		"$type":     "app.atchess.drawOffer",
		"createdAt": time.Now().Format(time.RFC3339),
		"game":      map[string]interface{}{"uri": gameURI, "cid": "cid-game1"},
		"offeredBy": whiteDID,
		"status":    "pending",
	})

	client := newDeriveTestClient(t, mock)
	err := client.RespondToDrawOffer(context.Background(), offerURI, true)
	if err != nil {
		t.Fatalf("expected a legitimate accept in an active game to succeed, got error: %v", err)
	}
	// createRecord for the drawResponse, plus putRecord to refresh the
	// cached game record's status (white owns the game record here).
	if got := mock.writeCallCount(); got == 0 {
		t.Errorf("expected the accept to actually reach the fake server's write path, got %d writes", got)
	}
}

// TestRespondToDrawOffer_Decline_SucceedsWhenDerivationIncomplete is the
// atchess-1c9.65 regression test. atchess-1c9.56 made the terminal-game
// guard's derivation-error branch fail closed for BOTH accept and decline,
// contradicting the still-standing design note on the gameCID fallback a
// few lines below (a decline "must not hard-fail just because the game
// record happens to be momentarily unreadable"). A decline never updates
// the game record and is invisible to getDrawAcceptOutcome (which only
// honours "accepted" responses), so it must proceed best-effort even when
// the game's derived status can't be verified -- unlike an accept, which
// must still fail closed in the same situation (see
// TestRespondToDrawOffer_FailsClosed_WhenDerivationIncomplete, which must
// keep passing).
func TestRespondToDrawOffer_Decline_SucceedsWhenDerivationIncomplete(t *testing.T) {
	mock := newDeriveTestPDS(t)
	srv := mock.server()
	defer srv.Close()

	// atchess-1c9.95 fix-pass: see setUnreachable's doc comment -- must be
	// a validator-passing-but-dead endpoint, not a plain closed
	// httptest.Server's own address.
	mock.setUnreachable(t, blackDID)

	gameURI := mock.seedActiveGame(t, time.Now().Add(-time.Hour), nil)

	offerURI, _ := mock.seed(whiteDID, "app.atchess.drawOffer", "offer1", map[string]interface{}{
		"$type":     "app.atchess.drawOffer",
		"createdAt": time.Now().Format(time.RFC3339),
		"game":      map[string]interface{}{"uri": gameURI, "cid": "cid-game1"},
		"offeredBy": whiteDID,
		"status":    "pending",
	})

	client := newDeriveTestClient(t, mock)
	err := client.RespondToDrawOffer(context.Background(), offerURI, false)
	if err != nil {
		t.Fatalf("expected a decline to succeed best-effort when the game's status could not be verified (black's PDS unreachable), got error: %v", err)
	}
	// createRecord for the drawResponse. Declines never refresh the
	// cached game record, so this is the only write expected.
	if got := mock.writeCallCount(); got == 0 {
		t.Errorf("expected the decline to actually reach the fake server's write path, got %d writes", got)
	}
}

// TestRespondToDrawOffer_Decline_RejectedInResignedGame proves the
// terminal-status half of the guard still applies to declines: unlike the
// derivation-error half (see
// TestRespondToDrawOffer_Decline_SucceedsWhenDerivationIncomplete above),
// a DERIVABLE terminal status refuses both accept and decline, since there
// is nothing meaningful to decline on a game that has already ended.
func TestRespondToDrawOffer_Decline_RejectedInResignedGame(t *testing.T) {
	mock := newDeriveTestPDS(t)
	srv := mock.server()
	defer srv.Close()

	gameURI := mock.seedActiveGame(t, time.Now().Add(-time.Hour), nil)

	offerURI, _ := mock.seed(whiteDID, "app.atchess.drawOffer", "offer1", map[string]interface{}{
		"$type":     "app.atchess.drawOffer",
		"createdAt": time.Now().Format(time.RFC3339),
		"game":      map[string]interface{}{"uri": gameURI, "cid": "cid-game1"},
		"offeredBy": whiteDID,
		"status":    "pending",
	})

	// White resigns after making the offer -- the game is now terminal
	// (black_won) even though the drawOffer record itself still says
	// "pending".
	mock.seed(whiteDID, "app.atchess.resignation", "resign1", map[string]interface{}{
		"$type":           "app.atchess.resignation",
		"createdAt":       time.Now().Format(time.RFC3339),
		"game":            map[string]interface{}{"uri": gameURI},
		"resigningPlayer": whiteDID,
	})

	client := newDeriveTestClient(t, mock)
	err := client.RespondToDrawOffer(context.Background(), offerURI, false)
	if err == nil {
		t.Fatalf("expected RespondToDrawOffer to reject a decline in a resigned (terminal) game, got nil error")
	}
	if got := mock.writeCallCount(); got != 0 {
		t.Errorf("expected ZERO createRecord/putRecord writes when the decline is rejected, got %d", got)
	}
}
