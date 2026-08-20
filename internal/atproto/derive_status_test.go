package atproto

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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

func (m *deriveTestPDS) server() *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(m.handle))
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
		m.mu.Unlock()
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
		m.mu.Lock()
		coll := m.records[repo][collection]
		type rec struct {
			URI   string      `json:"uri"`
			CID   string      `json:"cid"`
			Value interface{} `json:"value"`
		}
		var recs []rec
		for rkey, val := range coll {
			recs = append(recs, rec{URI: fmt.Sprintf("at://%s/%s/%s", repo, collection, rkey), CID: "cid-" + rkey, Value: val})
		}
		m.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"records": recs})
		return

	default:
		w.WriteHeader(http.StatusNotFound)
	}
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
