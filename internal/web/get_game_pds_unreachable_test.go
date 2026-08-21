package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/mux"
	"github.com/justinabrahms/atchess/internal/config"
)

// This file is the atchess-1c9.67 companion to
// read_handlers_incomplete_derivation_test.go's atchess-1c9.53 tests.
//
// .53 fixed the case where the game record itself is readable but a
// per-repo scan of an OPPONENT's PDS fails: GetGame returns a populated
// game plus ErrIncompleteDerivation, and the handlers render it with an
// unverified flag intact rather than 404ing.
//
// This file covers the OTHER branch: the game record's OWN PDS is
// unreachable, so GetGame fails BEFORE it ever has a record to derive
// from -- there is no partial game to render. Before atchess-1c9.67, every
// pre-derivation failure (this one included) was collapsed into the same
// 404 "Game not found" response as a genuinely absent game, which is
// exactly the misdirection .53 set out to kill, just reached by a
// different path. These tests assert the two cases separately, as .53's
// did: a transport failure reaching the owning PDS must NOT produce the
// same response as a genuine absence.

// newDeadServer returns the address of an httptest.Server that has already
// been closed, i.e. a host nothing is listening on -- the same pattern
// read_handlers_incomplete_derivation_test.go uses to simulate an
// unreachable opponent PDS, applied here to the game record's OWN PDS.
func newDeadServer(t *testing.T) string {
	t.Helper()
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	dead.Close()
	return dead.URL
}

// TestGetGameHandler_OwnPDSUnreachable_502NotFound is the atchess-1c9.67
// regression test for GetGameHandler: the game record's own PDS being
// unreachable must return 502, never 404 (which would falsely claim the
// game does not exist) and never 200 (there is no partial game to show).
func TestGetGameHandler_OwnPDSUnreachable_502NotFound(t *testing.T) {
	const ownerDID = "did:plc:owner-unreachable"

	mock := newMockFederatedPDS(t, ownerDID)
	srv := mock.server()
	defer srv.Close()
	mock.setBaseURL(srv.URL)

	// The game record's own PDS (ownerDID's) is unreachable -- distinct
	// from the game record simply never having been seeded.
	mock.setUnreachable(ownerDID, newDeadServer(t))

	gameURI := fmt.Sprintf("at://%s/app.atchess.game/%s", ownerDID, "game1")

	svc := &Service{
		config:       &config.Config{ATProto: config.ATProtoConfig{PLCDirectoryURL: srv.URL}},
		serverClient: newTestServerClient(t, srv, "did:plc:server"),
	}

	req := httptest.NewRequest("GET", "/api/games/"+encodeGameID(gameURI), nil)
	req = mux.SetURLVars(req, map[string]string{"id": encodeGameID(gameURI)})

	w := httptest.NewRecorder()
	svc.GetGameHandler(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected HTTP 502 (upstream unreachable, not a verdict about existence), got %d: %s", w.Code, w.Body.String())
	}
	if w.Code == http.StatusNotFound {
		t.Fatalf("must not 404 for an unreachable PDS -- that misrepresents a transient network failure as a nonexistent game")
	}
	body := w.Body.String()
	if body == "" {
		t.Fatalf("expected a non-empty response body explaining the failure")
	}
}

// TestGetSpectatorGameHandler_OwnPDSUnreachable_502NotFound mirrors the
// above for GetSpectatorGameHandler.
func TestGetSpectatorGameHandler_OwnPDSUnreachable_502NotFound(t *testing.T) {
	const ownerDID = "did:plc:owner-unreachable2"

	mock := newMockFederatedPDS(t, ownerDID)
	srv := mock.server()
	defer srv.Close()
	mock.setBaseURL(srv.URL)

	mock.setUnreachable(ownerDID, newDeadServer(t))

	gameURI := fmt.Sprintf("at://%s/app.atchess.game/%s", ownerDID, "game1")

	svc := &Service{
		config:       &config.Config{ATProto: config.ATProtoConfig{PLCDirectoryURL: srv.URL}},
		serverClient: newTestServerClient(t, srv, "did:plc:server"),
	}

	req := httptest.NewRequest("GET", "/api/spectate/"+gameURI, nil)
	req = mux.SetURLVars(req, map[string]string{"id": gameURI})

	w := httptest.NewRecorder()
	svc.GetSpectatorGameHandler(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected HTTP 502 (upstream unreachable, not a verdict about existence), got %d: %s", w.Code, w.Body.String())
	}
}

// TestCheckAbandonmentHandler_OwnPDSUnreachable_502NotFound mirrors the
// above for CheckAbandonmentHandler.
func TestCheckAbandonmentHandler_OwnPDSUnreachable_502NotFound(t *testing.T) {
	const ownerDID = "did:plc:owner-unreachable3"

	mock := newMockFederatedPDS(t, ownerDID)
	srv := mock.server()
	defer srv.Close()
	mock.setBaseURL(srv.URL)

	mock.setUnreachable(ownerDID, newDeadServer(t))

	gameURI := fmt.Sprintf("at://%s/app.atchess.game/%s", ownerDID, "game1")

	svc := &Service{
		config:       &config.Config{ATProto: config.ATProtoConfig{PLCDirectoryURL: srv.URL}},
		serverClient: newTestServerClient(t, srv, "did:plc:server"),
	}

	req := httptest.NewRequest("GET", "/api/games/"+gameURI+"/abandoned", nil)
	req = mux.SetURLVars(req, map[string]string{"id": gameURI})

	w := httptest.NewRecorder()
	svc.CheckAbandonmentHandler(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected HTTP 502 (upstream unreachable, not a verdict about existence), got %d: %s", w.Code, w.Body.String())
	}
}

// TestGetGameHandler_MalformedGameURI_400 asserts a game URI that is not a
// well-formed at:// URI (the caller's fault, never anything the PDS could
// have said) is rejected as a 400, not folded into either the 404 (genuine
// absence) or 502 (upstream unreachable) buckets above.
func TestGetGameHandler_MalformedGameURI_400(t *testing.T) {
	const ownerDID = "did:plc:owner-malformed"

	mock := newMockFederatedPDS(t, ownerDID)
	srv := mock.server()
	defer srv.Close()
	mock.setBaseURL(srv.URL)

	svc := &Service{
		config:       &config.Config{ATProto: config.ATProtoConfig{PLCDirectoryURL: srv.URL}},
		serverClient: newTestServerClient(t, srv, "did:plc:server"),
	}

	// "not-a-valid-at-uri" base64-decodes fine (decodeGameID never
	// validates its content) but fails GetGame's own at:// URI format
	// check.
	badID := "not-a-valid-at-uri"
	req := httptest.NewRequest("GET", "/api/games/"+encodeGameID(badID), nil)
	req = mux.SetURLVars(req, map[string]string{"id": encodeGameID(badID)})

	w := httptest.NewRecorder()
	svc.GetGameHandler(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected HTTP 400 for a malformed game URI, got %d: %s", w.Code, w.Body.String())
	}
}

// TestGetSpectatorGameHandler_MalformedGameURI_400 mirrors the above for
// GetSpectatorGameHandler, which takes gameID directly from the path var
// with no base64 decoding step.
func TestGetSpectatorGameHandler_MalformedGameURI_400(t *testing.T) {
	const ownerDID = "did:plc:owner-malformed2"

	mock := newMockFederatedPDS(t, ownerDID)
	srv := mock.server()
	defer srv.Close()
	mock.setBaseURL(srv.URL)

	svc := &Service{
		config:       &config.Config{ATProto: config.ATProtoConfig{PLCDirectoryURL: srv.URL}},
		serverClient: newTestServerClient(t, srv, "did:plc:server"),
	}

	badID := "not-a-valid-at-uri"
	req := httptest.NewRequest("GET", "/api/spectate/"+badID, nil)
	req = mux.SetURLVars(req, map[string]string{"id": badID})

	w := httptest.NewRecorder()
	svc.GetSpectatorGameHandler(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected HTTP 400 for a malformed game URI, got %d: %s", w.Code, w.Body.String())
	}
}

// TestCheckAbandonmentHandler_MalformedGameURI_400 mirrors the above for
// CheckAbandonmentHandler.
func TestCheckAbandonmentHandler_MalformedGameURI_400(t *testing.T) {
	const ownerDID = "did:plc:owner-malformed3"

	mock := newMockFederatedPDS(t, ownerDID)
	srv := mock.server()
	defer srv.Close()
	mock.setBaseURL(srv.URL)

	svc := &Service{
		config:       &config.Config{ATProto: config.ATProtoConfig{PLCDirectoryURL: srv.URL}},
		serverClient: newTestServerClient(t, srv, "did:plc:server"),
	}

	badID := "not-a-valid-at-uri"
	req := httptest.NewRequest("GET", "/api/games/"+badID+"/abandoned", nil)
	req = mux.SetURLVars(req, map[string]string{"id": badID})

	w := httptest.NewRecorder()
	svc.CheckAbandonmentHandler(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected HTTP 400 for a malformed game URI, got %d: %s", w.Code, w.Body.String())
	}
}

// The following boundary tests are a reviewer-directed follow-up on
// atchess-1c9.67: GetGame's malformed-URI guard was stale (len(parts) < 4
// instead of < 5), so a 4-part URI with no rkey segment at all (e.g.
// "at://did:plc:x/app.atchess.game") slipped past it and panicked on the
// parts[4] index-out-of-range instead of producing the 400 these handlers
// are supposed to return. internal/atproto's
// TestGetGame_FourPartURI_RejectedNotPanic/TestGetGame_FivePartURI_NotRejectedAsInvalid
// pin this at the GetGame level directly; these pin it at the handler
// level, which is what an actual request hits (decodeGameID performs no
// validation, so a base64-encoded 4-part URI reaches GetGameHandler
// exactly as typed).

// TestGetGameHandler_FourPartGameURI_400NotPanic is the exact panic case
// the reviewer proved reachable from the handler: a base64-encoded 4-part
// game URI (repo + collection, no rkey) must return 400, not panic the
// handler.
func TestGetGameHandler_FourPartGameURI_400NotPanic(t *testing.T) {
	const ownerDID = "did:plc:owner-fourpart"

	mock := newMockFederatedPDS(t, ownerDID)
	srv := mock.server()
	defer srv.Close()
	mock.setBaseURL(srv.URL)

	svc := &Service{
		config:       &config.Config{ATProto: config.ATProtoConfig{PLCDirectoryURL: srv.URL}},
		serverClient: newTestServerClient(t, srv, "did:plc:server"),
	}

	badID := fmt.Sprintf("at://%s/app.atchess.game", ownerDID) // 4 parts, no rkey
	req := httptest.NewRequest("GET", "/api/games/"+encodeGameID(badID), nil)
	req = mux.SetURLVars(req, map[string]string{"id": encodeGameID(badID)})

	w := httptest.NewRecorder()
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("GetGameHandler panicked on 4-part game URI %q instead of returning 400: %v", badID, r)
			}
		}()
		svc.GetGameHandler(w, req)
	}()

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected HTTP 400 for a 4-part (missing-rkey) game URI, got %d: %s", w.Code, w.Body.String())
	}
}

// TestGetSpectatorGameHandler_FourPartGameURI_400NotPanic mirrors the
// above for GetSpectatorGameHandler.
func TestGetSpectatorGameHandler_FourPartGameURI_400NotPanic(t *testing.T) {
	const ownerDID = "did:plc:owner-fourpart2"

	mock := newMockFederatedPDS(t, ownerDID)
	srv := mock.server()
	defer srv.Close()
	mock.setBaseURL(srv.URL)

	svc := &Service{
		config:       &config.Config{ATProto: config.ATProtoConfig{PLCDirectoryURL: srv.URL}},
		serverClient: newTestServerClient(t, srv, "did:plc:server"),
	}

	badID := fmt.Sprintf("at://%s/app.atchess.game", ownerDID)
	req := httptest.NewRequest("GET", "/api/spectate/"+badID, nil)
	req = mux.SetURLVars(req, map[string]string{"id": badID})

	w := httptest.NewRecorder()
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("GetSpectatorGameHandler panicked on 4-part game URI %q instead of returning 400: %v", badID, r)
			}
		}()
		svc.GetSpectatorGameHandler(w, req)
	}()

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected HTTP 400 for a 4-part (missing-rkey) game URI, got %d: %s", w.Code, w.Body.String())
	}
}

// TestCheckAbandonmentHandler_FourPartGameURI_400NotPanic mirrors the
// above for CheckAbandonmentHandler.
func TestCheckAbandonmentHandler_FourPartGameURI_400NotPanic(t *testing.T) {
	const ownerDID = "did:plc:owner-fourpart3"

	mock := newMockFederatedPDS(t, ownerDID)
	srv := mock.server()
	defer srv.Close()
	mock.setBaseURL(srv.URL)

	svc := &Service{
		config:       &config.Config{ATProto: config.ATProtoConfig{PLCDirectoryURL: srv.URL}},
		serverClient: newTestServerClient(t, srv, "did:plc:server"),
	}

	badID := fmt.Sprintf("at://%s/app.atchess.game", ownerDID)
	req := httptest.NewRequest("GET", "/api/games/"+badID+"/abandoned", nil)
	req = mux.SetURLVars(req, map[string]string{"id": badID})

	w := httptest.NewRecorder()
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("CheckAbandonmentHandler panicked on 4-part game URI %q instead of returning 400: %v", badID, r)
			}
		}()
		svc.CheckAbandonmentHandler(w, req)
	}()

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected HTTP 400 for a 4-part (missing-rkey) game URI, got %d: %s", w.Code, w.Body.String())
	}
}

// TestGetGameHandler_FivePartGameURI_NotRejectedAsMalformed pins the other
// side of the boundary at the handler level: a well-formed 5-part game URI
// for a record that genuinely does not exist must 404 (ErrRecordNotFound),
// never 400 -- the guard fix must not have overcorrected into rejecting
// well-formed URIs.
func TestGetGameHandler_FivePartGameURI_NotRejectedAsMalformed(t *testing.T) {
	const ownerDID = "did:plc:owner-fivepart"

	mock := newMockFederatedPDS(t, ownerDID)
	srv := mock.server()
	defer srv.Close()
	mock.setBaseURL(srv.URL)

	svc := &Service{
		config:       &config.Config{ATProto: config.ATProtoConfig{PLCDirectoryURL: srv.URL}},
		serverClient: newTestServerClient(t, srv, "did:plc:server"),
	}

	gameURI := fmt.Sprintf("at://%s/app.atchess.game/%s", ownerDID, "does-not-exist") // well-formed, 5 parts
	req := httptest.NewRequest("GET", "/api/games/"+encodeGameID(gameURI), nil)
	req = mux.SetURLVars(req, map[string]string{"id": encodeGameID(gameURI)})

	w := httptest.NewRecorder()
	svc.GetGameHandler(w, req)

	if w.Code == http.StatusBadRequest {
		t.Fatalf("well-formed 5-part game URI %q was rejected as malformed (400)", gameURI)
	}
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected HTTP 404 for a well-formed but genuinely absent game URI, got %d: %s", w.Code, w.Body.String())
	}
}
