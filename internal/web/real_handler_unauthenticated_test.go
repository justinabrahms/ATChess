package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/mux"
	"github.com/justinabrahms/atchess/internal/config"
	"github.com/justinabrahms/atchess/internal/oauth"
)

// TestRealHandler_UnauthenticatedRequest_Returns401 is the secondary item
// noted alongside atchess-1c9.33: invariants_test.go's TestService is a
// fake whose MakeMoveHandler always returns 200 regardless of auth, so it
// never exercises the real AuthMiddleware+Service.MakeMoveHandler chain the
// production router (cmd/protocol/main.go) actually wires up. This drives
// an unauthenticated request through that real chain -- a real *Service's
// real MakeMoveHandler method, behind the real AuthMiddleware on a real
// mux subrouter, exactly as cmd/protocol/main.go wires "authed" routes --
// and asserts it comes back 401 without ever reaching the handler body
// (proven by the response not resembling the handler's success shape, and
// by the fact that a nil challengeStore/serverClient on the Service would
// otherwise panic if the handler body ran).
func TestRealHandler_UnauthenticatedRequest_Returns401(t *testing.T) {
	old := sessionStore
	sessionStore = oauth.NewSessionStore()
	defer func() { sessionStore = old }()

	// Deliberately nil serverClient/challengeStore: if AuthMiddleware ever
	// let an unauthenticated request through to the real handler body, this
	// would panic (s.requireClient/clientFor dereferencing a nil session or
	// s.challengeStore) rather than silently succeeding -- a stronger
	// failure signal than a fake stub could give.
	svc := &Service{config: &config.Config{}}

	router := mux.NewRouter()
	authed := router.PathPrefix("/api").Subrouter()
	authed.Use(AuthMiddleware)
	authed.HandleFunc("/moves", svc.MakeMoveHandler).Methods("POST")

	req := httptest.NewRequest("POST", "/api/moves", strings.NewReader(`{"game_id":"x","from":"e2","to":"e4"}`))
	// No X-Session-ID header set.
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for an unauthenticated request to the real MakeMoveHandler behind the real AuthMiddleware, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), `"status":"success"`) {
		t.Errorf("response looks like the handler's success body -- AuthMiddleware did not actually block the request: %s", rr.Body.String())
	}
}
