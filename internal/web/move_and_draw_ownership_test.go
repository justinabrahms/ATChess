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
	"github.com/justinabrahms/atchess/internal/challenge"
	"github.com/justinabrahms/atchess/internal/config"
	"github.com/justinabrahms/atchess/internal/oauth"
)

// mockFederatedPDS is a single httptest server standing in for BOTH the
// did:plc directory and every player's PDS: any did:plc:<x> resolves (via
// its "/did:plc:<x>" path) to this same server's own URL as that DID's
// serviceEndpoint, and the usual com.atproto.repo.* XRPC endpoints are
// served for whichever "repo" query/body parameter is requested. This is
// enough to exercise cross-repo READS (GetGame, getLatestMoveForGame, and
// this file's terminal-event derivation helpers all need to read a
// "foreign" repo) while still being a single process the test controls, no
// Docker/dual-PDS harness required.
//
// Its defining behaviour for these regression tests (atchess-1c9.48) is
// createRecordRepo/putRecordRepo enforcement: every createRecord/putRecord
// call is recorded, and any call naming a "repo" other than wantOwnRepoDID
// fails the test immediately with a 403 AccountNotFound response -- mirroring
// the real PDS's actual behaviour for a cross-repo write (see this bead's
// empirically captured failure) rather than silently succeeding, which
// would let a regression slip back in unnoticed.
type mockFederatedPDS struct {
	t              *testing.T
	wantOwnRepoDID string

	mu      sync.Mutex
	records map[string]map[string]map[string]interface{} // repo -> collection -> rkey -> value
	rkeyN   int
	base    string

	writeCalls []string // "createRecord repo=... collection=..." / "putRecord ..."
}

func newMockFederatedPDS(t *testing.T, wantOwnRepoDID string) *mockFederatedPDS {
	return &mockFederatedPDS{
		t:              t,
		wantOwnRepoDID: wantOwnRepoDID,
		records:        map[string]map[string]map[string]interface{}{},
	}
}

func (m *mockFederatedPDS) seed(repo, collection, rkey string, value map[string]interface{}) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.records[repo] == nil {
		m.records[repo] = map[string]map[string]interface{}{}
	}
	if m.records[repo][collection] == nil {
		m.records[repo][collection] = map[string]interface{}{}
	}
	m.records[repo][collection][rkey] = value
}

func (m *mockFederatedPDS) newRkey() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rkeyN++
	return fmt.Sprintf("rkey%d", m.rkeyN)
}

func (m *mockFederatedPDS) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(m.handle))
}

func (m *mockFederatedPDS) handle(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	// did:plc directory lookups: path is exactly "/did:plc:<x>".
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

	case "/xrpc/com.atproto.repo.createRecord":
		var req map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&req)
		repo, _ := req["repo"].(string)
		collection, _ := req["collection"].(string)
		m.mu.Lock()
		m.writeCalls = append(m.writeCalls, fmt.Sprintf("createRecord repo=%s collection=%s", repo, collection))
		m.mu.Unlock()
		if repo != m.wantOwnRepoDID {
			m.t.Errorf("createRecord called with repo=%q collection=%q; only the authenticated caller's own repo (%s) should ever be named -- a cross-repo write must never be attempted", repo, collection, m.wantOwnRepoDID)
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"error": "AccountNotFound", "message": "Account not found"})
			return
		}
		rkey := m.newRkey()
		record, _ := req["record"].(map[string]interface{})
		m.seed(repo, collection, rkey, record)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"uri": fmt.Sprintf("at://%s/%s/%s", repo, collection, rkey),
			"cid": "cid-" + rkey,
		})
		return

	case "/xrpc/com.atproto.repo.putRecord":
		var req map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&req)
		repo, _ := req["repo"].(string)
		collection, _ := req["collection"].(string)
		rkey, _ := req["rkey"].(string)
		m.mu.Lock()
		m.writeCalls = append(m.writeCalls, fmt.Sprintf("putRecord repo=%s collection=%s rkey=%s", repo, collection, rkey))
		m.mu.Unlock()
		if repo != m.wantOwnRepoDID {
			m.t.Errorf("putRecord called with repo=%q collection=%q rkey=%q; only the authenticated caller's own repo (%s) should ever be named -- a cross-repo write must never be attempted", repo, collection, rkey, m.wantOwnRepoDID)
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"error": "AccountNotFound", "message": "Account not found"})
			return
		}
		record, _ := req["record"].(map[string]interface{})
		m.seed(repo, collection, rkey, record)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"uri": fmt.Sprintf("at://%s/%s/%s", repo, collection, rkey),
			"cid": "cid-" + rkey,
		})
		return

	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// baseURL is filled in by the caller right after server() starts (needed
// because the DID-document handler above must advertise the server's own
// URL, which httptest only assigns once the listener is up).
func (m *mockFederatedPDS) setBaseURL(u string) { m.mu.Lock(); m.base = u; m.mu.Unlock() }
func (m *mockFederatedPDS) baseURL() string     { m.mu.Lock(); defer m.mu.Unlock(); return m.base }

// TestMakeMoveHandler_NonOwnerMove_NoCrossRepoWriteAttempted is a
// regression test for atchess-1c9.48: RecordMove must never attempt a
// createRecord/putRecord naming a repo other than the authenticated
// mover's own, even when the app.atchess.game record lives in the
// OPPONENT's repo (the ordinary case for whichever player didn't create
// the game). white (the mover here) does not own the game record --
// black.test does -- which is exactly the shape TestFederation's Fool's
// mate exercises.
func TestMakeMoveHandler_NonOwnerMove_NoCrossRepoWriteAttempted(t *testing.T) {
	const moverDID = "did:plc:white"
	const ownerDID = "did:plc:black" // owns the app.atchess.game record

	mock := newMockFederatedPDS(t, moverDID)
	srv := mock.server()
	defer srv.Close()
	mock.setBaseURL(srv.URL)

	const gameRkey = "game1"
	mock.seed(ownerDID, "app.atchess.game", gameRkey, map[string]interface{}{
		"$type":     "app.atchess.game",
		"createdAt": time.Now().Format(time.RFC3339),
		"white":     moverDID,
		"black":     ownerDID,
		"status":    "active",
		"fen":       "rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR w KQkq - 0 1",
		"pgn":       "",
	})
	gameURI := fmt.Sprintf("at://%s/app.atchess.game/%s", ownerDID, gameRkey)

	svc := &Service{
		config:         &config.Config{ATProto: config.ATProtoConfig{PLCDirectoryURL: srv.URL}},
		challengeStore: challenge.NewStore(),
	}

	session := &oauth.Session{
		DID:                  moverDID,
		Handle:               "white.test",
		PDSURL:               srv.URL,
		AccessToken:          "test-jwt",
		RefreshToken:         "test-refresh",
		ExpiresAt:            time.Now().Add(time.Hour),
		AccessTokenExpiresAt: time.Now().Add(time.Hour),
	}

	body, _ := json.Marshal(map[string]string{
		"game_id": gameURI,
		"from":    "e2",
		"to":      "e4",
	})
	req := httptest.NewRequest("POST", "/api/moves", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ctx := context.WithValue(req.Context(), contextKeySession, session)
	ctx = context.WithValue(ctx, contextKeyDID, session.DID)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	svc.MakeMoveHandler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200 (no cross-repo write should be attempted), got %d: %s", w.Code, w.Body.String())
	}

	mock.mu.Lock()
	calls := append([]string(nil), mock.writeCalls...)
	mock.mu.Unlock()
	if len(calls) != 1 {
		t.Errorf("expected exactly 1 write call (the move record createRecord into the mover's own repo; the game record putRecord must be skipped since the mover does not own it), got %d: %v", len(calls), calls)
	}
	for _, c := range calls {
		if !strings.Contains(c, "repo="+moverDID) {
			t.Errorf("write call did not target the mover's own repo: %s", c)
		}
	}
}

// TestRespondToDrawHandler_NonOwnerAccept_NoCrossRepoWriteAttempted is a
// regression test for atchess-1c9.48: RespondToDrawOffer used to
// putRecord directly into the draw offer's own repo (the OFFERING
// player's), which is a cross-repo write whenever -- as in the ordinary
// case -- the RESPONDING player is someone else. Empirically confirmed
// against the real dual-PDS harness: HTTP 403
// {"error":"AccountNotFound","message":"Account not found"}. It must now
// write only an app.atchess.drawResponse record into the responder's own
// repo.
func TestRespondToDrawHandler_NonOwnerAccept_NoCrossRepoWriteAttempted(t *testing.T) {
	const offererDID = "did:plc:offerer"
	const responderDID = "did:plc:responder"

	mock := newMockFederatedPDS(t, responderDID)
	srv := mock.server()
	defer srv.Close()
	mock.setBaseURL(srv.URL)

	const gameRkey = "game1"
	mock.seed(offererDID, "app.atchess.game", gameRkey, map[string]interface{}{
		"$type":     "app.atchess.game",
		"createdAt": time.Now().Format(time.RFC3339),
		"white":     offererDID,
		"black":     responderDID,
		"status":    "active",
		"fen":       "rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR w KQkq - 0 1",
	})
	gameURI := fmt.Sprintf("at://%s/app.atchess.game/%s", offererDID, gameRkey)

	const offerRkey = "offer1"
	mock.seed(offererDID, "app.atchess.drawOffer", offerRkey, map[string]interface{}{
		"$type":     "app.atchess.drawOffer",
		"createdAt": time.Now().Format(time.RFC3339),
		"game":      map[string]interface{}{"uri": gameURI, "cid": "cid-" + gameRkey},
		"offeredBy": offererDID,
		"status":    "pending",
	})
	offerURI := fmt.Sprintf("at://%s/app.atchess.drawOffer/%s", offererDID, offerRkey)

	svc := &Service{
		config:         &config.Config{ATProto: config.ATProtoConfig{PLCDirectoryURL: srv.URL}},
		challengeStore: challenge.NewStore(),
	}

	session := &oauth.Session{
		DID:                  responderDID,
		Handle:               "responder.test",
		PDSURL:               srv.URL,
		AccessToken:          "test-jwt",
		RefreshToken:         "test-refresh",
		ExpiresAt:            time.Now().Add(time.Hour),
		AccessTokenExpiresAt: time.Now().Add(time.Hour),
	}

	body, _ := json.Marshal(map[string]interface{}{
		"drawOfferUri": offerURI,
		"accept":       true,
	})
	req := httptest.NewRequest("POST", "/api/draw-offers/respond", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(context.WithValue(req.Context(), contextKeySession, session))

	w := httptest.NewRecorder()
	svc.RespondToDrawHandler(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected HTTP 204 (no cross-repo write should be attempted), got %d: %s", w.Code, w.Body.String())
	}

	mock.mu.Lock()
	calls := append([]string(nil), mock.writeCalls...)
	mock.mu.Unlock()
	for _, c := range calls {
		if !strings.Contains(c, "repo="+responderDID) {
			t.Errorf("write call did not target the responder's own repo: %s", c)
		}
	}
	found := false
	for _, c := range calls {
		if strings.HasPrefix(c, "createRecord repo="+responderDID) && strings.Contains(c, "collection=app.atchess.drawResponse") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected an app.atchess.drawResponse createRecord into the responder's own repo, got calls: %v", calls)
	}
}

// TestMakeMoveHandler_TerminalGame_MoveRejected is a regression test for
// atchess-1c9.48 (Defect 3 in the follow-up review): MakeMoveHandler used
// to fetch the game's derived status via GetGame and then simply discard
// it, checking only FEN/White/Black -- so nothing stopped a player moving
// in a game that had already ended by resignation (or time violation, or
// an accepted draw), so long as the mover's own repo's cached
// app.atchess.game "status" field hadn't been updated to reflect it. Here
// black resigned (the resignation record lives in BLACK's own repo, as
// ResignGame always writes it -- the game record's own cached status,
// seeded "active" below, is exactly the kind of stale cache this covers)
// and white must not be allowed to move afterward.
func TestMakeMoveHandler_TerminalGame_MoveRejected(t *testing.T) {
	const moverDID = "did:plc:white"
	const ownerDID = "did:plc:black" // owns the app.atchess.game record

	mock := newMockFederatedPDS(t, moverDID)
	srv := mock.server()
	defer srv.Close()
	mock.setBaseURL(srv.URL)

	const gameRkey = "game1"
	mock.seed(ownerDID, "app.atchess.game", gameRkey, map[string]interface{}{
		"$type":     "app.atchess.game",
		"createdAt": time.Now().Format(time.RFC3339),
		"white":     moverDID,
		"black":     ownerDID,
		"status":    "active", // stale -- black already resigned below
		"fen":       "rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR w KQkq - 0 1",
		"pgn":       "",
	})
	gameURI := fmt.Sprintf("at://%s/app.atchess.game/%s", ownerDID, gameRkey)

	mock.seed(ownerDID, "app.atchess.resignation", "resign1", map[string]interface{}{
		"$type":           "app.atchess.resignation",
		"createdAt":       time.Now().Format(time.RFC3339),
		"game":            map[string]interface{}{"uri": gameURI},
		"resigningPlayer": ownerDID,
	})

	svc := &Service{
		config:         &config.Config{ATProto: config.ATProtoConfig{PLCDirectoryURL: srv.URL}},
		challengeStore: challenge.NewStore(),
	}

	session := &oauth.Session{
		DID:                  moverDID,
		Handle:               "white.test",
		PDSURL:               srv.URL,
		AccessToken:          "test-jwt",
		RefreshToken:         "test-refresh",
		ExpiresAt:            time.Now().Add(time.Hour),
		AccessTokenExpiresAt: time.Now().Add(time.Hour),
	}

	body, _ := json.Marshal(map[string]string{
		"game_id": gameURI,
		"from":    "e2",
		"to":      "e4",
	})
	req := httptest.NewRequest("POST", "/api/moves", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ctx := context.WithValue(req.Context(), contextKeySession, session)
	ctx = context.WithValue(ctx, contextKeyDID, session.DID)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	svc.MakeMoveHandler(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected HTTP 409 (game already ended by black's resignation), got %d: %s", w.Code, w.Body.String())
	}

	mock.mu.Lock()
	calls := append([]string(nil), mock.writeCalls...)
	mock.mu.Unlock()
	if len(calls) != 0 {
		t.Errorf("expected no write calls at all (move must be rejected before RecordMove is ever reached), got %d: %v", len(calls), calls)
	}
}
