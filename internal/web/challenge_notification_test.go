package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/justinabrahms/atchess/internal/config"
	"github.com/justinabrahms/atchess/internal/oauth"
)

// TestCreateChallengeHandler_NotificationFailureIsNotReportedAsSuccess is a
// regression test for atchess-1c9.31: CreateChallenge used to swallow a
// failed challenge-notification write (fmt.Printf + continue) and return
// success regardless, so POST /api/challenges answered 200 even though the
// challenged player was never notified (verified in production: the write
// returns HTTP 403 AccountNotFound and zero notification records exist).
//
// This test stands up a fake PDS (httptest, no Docker/network) that accepts
// the challenger's own app.atchess.challenge write but rejects the
// cross-repo app.atchess.challengeNotification write exactly like that,
// and asserts CreateChallengeHandler does NOT report unqualified success.
func TestCreateChallengeHandler_NotificationFailureIsNotReportedAsSuccess(t *testing.T) {
	var deleteCalled bool
	var deletedCollection, deletedRkey string

	mockPDS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/xrpc/com.atproto.repo.createRecord":
			var req map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&req)
			switch req["collection"] {
			case "app.atchess.challenge":
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"uri": "at://did:plc:challenger/app.atchess.challenge/abc123",
					"cid": "challenge-cid",
				})
			case "app.atchess.challengeNotification":
				// Mirrors the real-world failure: the challenged account's
				// repo is not writable from here.
				w.WriteHeader(http.StatusForbidden)
				_ = json.NewEncoder(w).Encode(map[string]interface{}{"error": "AccountNotFound"})
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		case "/xrpc/com.atproto.repo.deleteRecord":
			deleteCalled = true
			var req map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&req)
			deletedCollection, _ = req["collection"].(string)
			deletedRkey, _ = req["rkey"].(string)
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer mockPDS.Close()

	svc := &Service{config: &config.Config{}}

	session := &oauth.Session{
		DID:                  "did:plc:challenger",
		Handle:               "challenger.test",
		PDSURL:               mockPDS.URL,
		AccessToken:          "test-jwt",
		RefreshToken:         "test-refresh",
		ExpiresAt:            time.Now().Add(time.Hour),
		AccessTokenExpiresAt: time.Now().Add(time.Hour),
	}

	body, _ := json.Marshal(map[string]string{
		"opponent_did": "did:plc:challenged",
		"color":        "white",
		"message":      "gg",
	})
	req := httptest.NewRequest("POST", "/api/challenges", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(context.WithValue(req.Context(), contextKeySession, session))

	w := httptest.NewRecorder()
	svc.CreateChallengeHandler(w, req)

	if w.Code >= 200 && w.Code < 300 {
		t.Fatalf("expected a non-2xx response when notification delivery fails, got %d: %s", w.Code, w.Body.String())
	}
	if w.Code != http.StatusBadGateway {
		t.Errorf("expected 502 Bad Gateway (upstream PDS rejected the notification write), got %d: %s", w.Code, w.Body.String())
	}

	if !deleteCalled {
		t.Errorf("expected the orphaned app.atchess.challenge record to be rolled back via com.atproto.repo.deleteRecord, but delete was never called")
	}
	if deletedCollection != "app.atchess.challenge" {
		t.Errorf("expected rollback to delete from collection app.atchess.challenge, got %q", deletedCollection)
	}
	if deletedRkey != "abc123" {
		t.Errorf("expected rollback to delete rkey abc123 (from the created challenge's URI), got %q", deletedRkey)
	}
}
