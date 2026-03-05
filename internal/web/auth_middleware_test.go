package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/justinabrahms/atchess/internal/oauth"
)

func TestAuthMiddleware_NoSessionStore(t *testing.T) {
	old := sessionStore
	sessionStore = nil
	defer func() { sessionStore = old }()

	handler := AuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be called")
	}))

	req := httptest.NewRequest("GET", "/", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", rr.Code)
	}
}

func TestAuthMiddleware_NoHeader(t *testing.T) {
	sessionStore = oauth.NewSessionStore()
	defer func() { sessionStore = nil }()

	handler := AuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be called")
	}))

	req := httptest.NewRequest("GET", "/", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rr.Code)
	}
}

func TestAuthMiddleware_InvalidSession(t *testing.T) {
	sessionStore = oauth.NewSessionStore()
	defer func() { sessionStore = nil }()

	handler := AuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be called")
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Session-ID", "nonexistent")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rr.Code)
	}
}

func TestAuthMiddleware_ValidSession(t *testing.T) {
	sessionStore = oauth.NewSessionStore()
	defer func() { sessionStore = nil }()

	session := &oauth.Session{
		DID:       "did:plc:testuser123",
		Handle:    "testuser.bsky.social",
		ExpiresAt: time.Now().Add(1 * time.Hour),
	}
	sessionID := sessionStore.CreateSession(session)

	var gotDID, gotHandle string
	handler := AuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotDID = AuthenticatedDID(r)
		gotHandle = AuthenticatedHandle(r)
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Session-ID", sessionID)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rr.Code)
	}
	if gotDID != "did:plc:testuser123" {
		t.Errorf("expected DID did:plc:testuser123, got %s", gotDID)
	}
	if gotHandle != "testuser.bsky.social" {
		t.Errorf("expected handle testuser.bsky.social, got %s", gotHandle)
	}
}

func TestAuthMiddleware_ExpiredSession(t *testing.T) {
	sessionStore = oauth.NewSessionStore()
	defer func() { sessionStore = nil }()

	session := &oauth.Session{
		DID:       "did:plc:testuser123",
		Handle:    "testuser.bsky.social",
		ExpiresAt: time.Now().Add(-1 * time.Hour), // expired
	}
	sessionID := sessionStore.CreateSession(session)

	handler := AuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be called for expired session")
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Session-ID", sessionID)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rr.Code)
	}
}
