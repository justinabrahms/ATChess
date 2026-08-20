package web

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/justinabrahms/atchess/internal/atproto"
	"github.com/justinabrahms/atchess/internal/chess"
	"github.com/justinabrahms/atchess/internal/config"
	"github.com/justinabrahms/atchess/internal/oauth"
)

// newTestServerClient builds an *atproto.Client suitable for a Service's
// unauthenticated serverClient field (used by GetGameHandler,
// GetSpectatorGameHandler, and CheckAbandonmentHandler), backed by
// mockFederatedPDS at srv, via the same NewClientFromSession +
// sessionAuthenticator path Service.clientFor uses for authenticated
// callers -- there's no meaningful difference for a read-only client in
// tests, and it avoids needing a real com.atproto.server.createSession
// handshake against the mock.
func newTestServerClient(t *testing.T, srv *httptest.Server, did string) *atproto.Client {
	t.Helper()
	session := &oauth.Session{
		DID:                  did,
		Handle:               "server.test",
		PDSURL:               srv.URL,
		AccessToken:          "test-jwt",
		RefreshToken:         "test-refresh",
		ExpiresAt:            time.Now().Add(time.Hour),
		AccessTokenExpiresAt: time.Now().Add(time.Hour),
	}
	client, err := atproto.NewClientFromSession(srv.URL, did, "server.test", false, nil, newSessionAuthenticator(session))
	if err != nil {
		t.Fatalf("failed to build test server client: %v", err)
	}
	client.SetPLCDirectoryURL(srv.URL)
	return client
}

// encodeGameID base64-(std)encodes gameURI the way GetGameHandler's
// decodeGameID (service.go) expects for its "id" path var -- see
// invariants_test.go's encodeGameIdForURL for the same convention.
func encodeGameID(gameURI string) string {
	return base64.StdEncoding.EncodeToString([]byte(gameURI))
}

// TestGetGameHandler_IncompleteDerivation_ReturnsPartialGameNotFound is a
// regression test for atchess-1c9.53: atchess-1c9.51 made GetGame fail
// closed for WRITE authorization by returning ErrIncompleteDerivation
// whenever a per-repo scan couldn't be read, but GetGameHandler mapped ANY
// GetGame error -- including this one -- to a 404 "Game not found". A
// transient opponent-PDS blip must not make a perfectly viewable game
// vanish: GetGameHandler must return 200 with the partial game and its
// DerivationIncomplete flag intact (surfaced as JSON "derivationIncomplete")
// so a client can tell the status is unverified, not just render a
// possibly-wrong "active".
func TestGetGameHandler_IncompleteDerivation_ReturnsPartialGameNotFound(t *testing.T) {
	const ownerDID = "did:plc:owner"
	const opponentDID = "did:plc:opponent"

	mock := newMockFederatedPDS(t, ownerDID)
	srv := mock.server()
	defer srv.Close()
	mock.setBaseURL(srv.URL)

	// opponentDID's PDS is unreachable, so the cross-repo terminal-event
	// scan GetGame performs cannot complete -- exactly the condition
	// ErrIncompleteDerivation exists to report.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	dead.Close()
	mock.setUnreachable(opponentDID, dead.URL)

	const gameRkey = "game1"
	mock.seed(ownerDID, "app.atchess.game", gameRkey, map[string]interface{}{
		"$type":     "app.atchess.game",
		"createdAt": time.Now().Format(time.RFC3339),
		"white":     ownerDID,
		"black":     opponentDID,
		"status":    "active",
		"fen":       "rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR w KQkq - 0 1",
		"pgn":       "",
	})
	gameURI := fmt.Sprintf("at://%s/app.atchess.game/%s", ownerDID, gameRkey)

	svc := &Service{
		config:       &config.Config{ATProto: config.ATProtoConfig{PLCDirectoryURL: srv.URL}},
		serverClient: newTestServerClient(t, srv, "did:plc:server"),
	}

	req := httptest.NewRequest("GET", "/api/games/"+encodeGameID(gameURI), nil)
	req = mux.SetURLVars(req, map[string]string{"id": encodeGameID(gameURI)})

	w := httptest.NewRecorder()
	svc.GetGameHandler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200 (partial game, not 404), got %d: %s", w.Code, w.Body.String())
	}

	var got chess.Game
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("failed to decode response body: %v", err)
	}
	if !got.DerivationIncomplete {
		t.Errorf("expected derivationIncomplete=true in response body, got false: %s", w.Body.String())
	}
	if got.ID != gameURI {
		t.Errorf("expected partial game to still carry its ID %q, got %q", gameURI, got.ID)
	}

	// The raw wire field must be named exactly "derivationIncomplete" (the
	// existing chess.Game struct tag), not a differently-cased/invented name.
	var raw map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("failed to decode response body as raw JSON: %v", err)
	}
	if v, ok := raw["derivationIncomplete"]; !ok || v != true {
		t.Errorf(`expected raw JSON field "derivationIncomplete": true, got %v (present=%v)`, v, ok)
	}
}

// TestGetGameHandler_GenuinelyAbsentGame_404 is the companion case to the
// above: a game record that genuinely does not exist (never seeded, no
// unreachable repo involved) must still 404. This is the separation
// atchess-1c9.53 requires: the two GetGame failure modes must be asserted
// SEPARATELY, not just "not 404" for one of them.
func TestGetGameHandler_GenuinelyAbsentGame_404(t *testing.T) {
	const ownerDID = "did:plc:owner"

	mock := newMockFederatedPDS(t, ownerDID)
	srv := mock.server()
	defer srv.Close()
	mock.setBaseURL(srv.URL)

	// Never seeded: com.atproto.repo.getRecord will 404 for this rkey.
	gameURI := fmt.Sprintf("at://%s/app.atchess.game/%s", ownerDID, "does-not-exist")

	svc := &Service{
		config:       &config.Config{ATProto: config.ATProtoConfig{PLCDirectoryURL: srv.URL}},
		serverClient: newTestServerClient(t, srv, "did:plc:server"),
	}

	req := httptest.NewRequest("GET", "/api/games/"+encodeGameID(gameURI), nil)
	req = mux.SetURLVars(req, map[string]string{"id": encodeGameID(gameURI)})

	w := httptest.NewRecorder()
	svc.GetGameHandler(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected HTTP 404 for a genuinely absent game, got %d: %s", w.Code, w.Body.String())
	}
}

// TestGetSpectatorGameHandler_IncompleteDerivation_Returns200 mirrors
// TestGetGameHandler_IncompleteDerivation_ReturnsPartialGameNotFound for
// GetSpectatorGameHandler (internal/web/spectator.go).
func TestGetSpectatorGameHandler_IncompleteDerivation_Returns200(t *testing.T) {
	const ownerDID = "did:plc:owner2"
	const opponentDID = "did:plc:opponent2"

	mock := newMockFederatedPDS(t, ownerDID)
	srv := mock.server()
	defer srv.Close()
	mock.setBaseURL(srv.URL)

	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	dead.Close()
	mock.setUnreachable(opponentDID, dead.URL)

	const gameRkey = "game1"
	mock.seed(ownerDID, "app.atchess.game", gameRkey, map[string]interface{}{
		"$type":     "app.atchess.game",
		"createdAt": time.Now().Format(time.RFC3339),
		"white":     ownerDID,
		"black":     opponentDID,
		"status":    "active",
		"fen":       "rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR w KQkq - 0 1",
		"pgn":       "",
	})
	gameURI := fmt.Sprintf("at://%s/app.atchess.game/%s", ownerDID, gameRkey)

	svc := &Service{
		config:       &config.Config{ATProto: config.ATProtoConfig{PLCDirectoryURL: srv.URL}},
		serverClient: newTestServerClient(t, srv, "did:plc:server"),
	}

	req := httptest.NewRequest("GET", "/api/spectate/"+gameURI, nil)
	req = mux.SetURLVars(req, map[string]string{"id": gameURI})

	w := httptest.NewRecorder()
	svc.GetSpectatorGameHandler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200 (partial game, not 404), got %d: %s", w.Code, w.Body.String())
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("failed to decode response body: %v", err)
	}
	gameField, ok := raw["game"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected a \"game\" object in response body, got: %s", w.Body.String())
	}
	if v, ok := gameField["derivationIncomplete"]; !ok || v != true {
		t.Errorf(`expected nested game.derivationIncomplete: true, got %v (present=%v)`, v, ok)
	}
}

// TestGetSpectatorGameHandler_GenuinelyAbsentGame_404 is
// GetSpectatorGameHandler's companion absent-game case.
func TestGetSpectatorGameHandler_GenuinelyAbsentGame_404(t *testing.T) {
	const ownerDID = "did:plc:owner2"

	mock := newMockFederatedPDS(t, ownerDID)
	srv := mock.server()
	defer srv.Close()
	mock.setBaseURL(srv.URL)

	gameURI := fmt.Sprintf("at://%s/app.atchess.game/%s", ownerDID, "does-not-exist")

	svc := &Service{
		config:       &config.Config{ATProto: config.ATProtoConfig{PLCDirectoryURL: srv.URL}},
		serverClient: newTestServerClient(t, srv, "did:plc:server"),
	}

	req := httptest.NewRequest("GET", "/api/spectate/"+gameURI, nil)
	req = mux.SetURLVars(req, map[string]string{"id": gameURI})

	w := httptest.NewRecorder()
	svc.GetSpectatorGameHandler(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected HTTP 404 for a genuinely absent game, got %d: %s", w.Code, w.Body.String())
	}
}

// TestCheckAbandonmentHandler_IncompleteDerivation_Returns200Unverified
// mirrors the above for CheckAbandonmentHandler (internal/web/spectator.go).
// Because game.Status is unproven when derivation is incomplete, the
// handler must not compute a real abandoned/canClaim verdict from it -- it
// must report the ambiguity explicitly via derivationIncomplete rather than
// silently answering "not abandoned" or "already ended" as if it knew.
func TestCheckAbandonmentHandler_IncompleteDerivation_Returns200Unverified(t *testing.T) {
	const ownerDID = "did:plc:owner3"
	const opponentDID = "did:plc:opponent3"

	mock := newMockFederatedPDS(t, ownerDID)
	srv := mock.server()
	defer srv.Close()
	mock.setBaseURL(srv.URL)

	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	dead.Close()
	mock.setUnreachable(opponentDID, dead.URL)

	const gameRkey = "game1"
	mock.seed(ownerDID, "app.atchess.game", gameRkey, map[string]interface{}{
		"$type":     "app.atchess.game",
		"createdAt": time.Now().Format(time.RFC3339),
		"white":     ownerDID,
		"black":     opponentDID,
		"status":    "active",
		"fen":       "rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR w KQkq - 0 1",
		"pgn":       "",
	})
	gameURI := fmt.Sprintf("at://%s/app.atchess.game/%s", ownerDID, gameRkey)

	svc := &Service{
		config:       &config.Config{ATProto: config.ATProtoConfig{PLCDirectoryURL: srv.URL}},
		serverClient: newTestServerClient(t, srv, "did:plc:server"),
	}

	req := httptest.NewRequest("GET", "/api/games/"+gameURI+"/abandoned", nil)
	req = mux.SetURLVars(req, map[string]string{"id": gameURI})

	w := httptest.NewRecorder()
	svc.CheckAbandonmentHandler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200 (not 404), got %d: %s", w.Code, w.Body.String())
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("failed to decode response body: %v", err)
	}
	if v, ok := raw["derivationIncomplete"]; !ok || v != true {
		t.Errorf(`expected raw JSON field "derivationIncomplete": true, got %v (present=%v)`, v, ok)
	}
	if v, ok := raw["canClaim"]; !ok || v != false {
		t.Errorf(`expected "canClaim": false while unverified, got %v (present=%v)`, v, ok)
	}
}

// TestCheckAbandonmentHandler_GenuinelyAbsentGame_404 is
// CheckAbandonmentHandler's companion absent-game case.
func TestCheckAbandonmentHandler_GenuinelyAbsentGame_404(t *testing.T) {
	const ownerDID = "did:plc:owner3"

	mock := newMockFederatedPDS(t, ownerDID)
	srv := mock.server()
	defer srv.Close()
	mock.setBaseURL(srv.URL)

	gameURI := fmt.Sprintf("at://%s/app.atchess.game/%s", ownerDID, "does-not-exist")

	svc := &Service{
		config:       &config.Config{ATProto: config.ATProtoConfig{PLCDirectoryURL: srv.URL}},
		serverClient: newTestServerClient(t, srv, "did:plc:server"),
	}

	req := httptest.NewRequest("GET", "/api/games/"+gameURI+"/abandoned", nil)
	req = mux.SetURLVars(req, map[string]string{"id": gameURI})

	w := httptest.NewRecorder()
	svc.CheckAbandonmentHandler(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected HTTP 404 for a genuinely absent game, got %d: %s", w.Code, w.Body.String())
	}
}
