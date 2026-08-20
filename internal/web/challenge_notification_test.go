package web

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/justinabrahms/atchess/internal/challenge"
	"github.com/justinabrahms/atchess/internal/config"
	"github.com/justinabrahms/atchess/internal/oauth"
)

// TestCreateChallengeHandler_NoCrossRepoWriteAttempted is a regression test
// for atchess-1c9.11: CreateChallenge used to follow its own-repo challenge
// write with a second, cross-repo app.atchess.challengeNotification write
// naming the challenged player's DID as "repo" -- a write AT Protocol never
// permits, which always failed in real federation (atchess-1c9.31 made that
// failure honest: HTTP 502 with the challenge record rolled back). That
// write has been removed entirely rather than retried. This test stands up
// a fake PDS (httptest, no Docker/network) that would fail the test if it
// ever receives a createRecord naming any repo other than the authenticated
// caller's own, and asserts POST /api/challenges now succeeds (HTTP 200)
// with exactly one createRecord call.
func TestCreateChallengeHandler_NoCrossRepoWriteAttempted(t *testing.T) {
	var createRecordCalls int

	mockPDS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/xrpc/com.atproto.repo.createRecord":
			createRecordCalls++
			var req map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&req)
			repo, _ := req["repo"].(string)
			if repo != "did:plc:challenger" {
				t.Errorf("createRecord called with repo=%q; only the authenticated caller's own repo (did:plc:challenger) should ever be named -- a cross-repo write must never be attempted", repo)
				w.WriteHeader(http.StatusForbidden)
				_ = json.NewEncoder(w).Encode(map[string]interface{}{"error": "AccountNotFound"})
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"uri": "at://did:plc:challenger/app.atchess.challenge/abc123",
				"cid": "challenge-cid",
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer mockPDS.Close()

	svc := &Service{config: &config.Config{}, challengeStore: challenge.NewStore()}

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

	if w.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200 (no cross-repo write to fail), got %d: %s", w.Code, w.Body.String())
	}
	if createRecordCalls != 1 {
		t.Errorf("expected exactly 1 createRecord call (no follow-up cross-repo write attempt), got %d", createRecordCalls)
	}

	// The challenge should also be immediately visible in this instance's
	// own cache for the CHALLENGED player (not the challenger).
	pending := svc.challengeStore.ForPlayer("did:plc:challenged")
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending challenge cached for the challenged player, got %d", len(pending))
	}
	if pending[0].ChallengeURI != "at://did:plc:challenger/app.atchess.challenge/abc123" {
		t.Errorf("unexpected cached challenge URI: %s", pending[0].ChallengeURI)
	}
}

// TestDeclineChallengeHandler_WritesToOwnRepoAndClearsCache exercises the
// full decline path: the {key} route segment carries the challenge's full
// at:// URI, URL-safe-base64 encoded (defect 4 from atchess-1c9.11's brief
// -- a bare rkey can never be resolved back to a full URI on this end,
// since the challenge record lives in an arbitrary challenger's repo this
// instance does not otherwise know). It asserts the decline record is
// written to the CALLER's own repo (never the challenger's), and that the
// challenge is removed from this instance's local cache afterward.
func TestDeclineChallengeHandler_WritesToOwnRepoAndClearsCache(t *testing.T) {
	var lastRepo, lastCollection string

	mockPDS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/xrpc/com.atproto.repo.createRecord":
			var req map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&req)
			lastRepo, _ = req["repo"].(string)
			lastCollection, _ = req["collection"].(string)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"uri": "at://did:plc:responder/app.atchess.challengeResponse/resp1",
				"cid": "response-cid",
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer mockPDS.Close()

	store := challenge.NewStore()
	challengeURI := "at://did:plc:challenger/app.atchess.challenge/abc123"
	store.Add(&challenge.PendingChallenge{
		ChallengeURI:  challengeURI,
		ChallengeCID:  "challenge-cid",
		ChallengerDID: "did:plc:challenger",
		ChallengedDID: "did:plc:responder",
		Color:         "white",
		CreatedAt:     time.Now(),
		ExpiresAt:     time.Now().Add(24 * time.Hour),
	})

	svc := &Service{config: &config.Config{}, challengeStore: store}

	session := &oauth.Session{
		DID:                  "did:plc:responder",
		Handle:               "responder.test",
		PDSURL:               mockPDS.URL,
		AccessToken:          "test-jwt",
		RefreshToken:         "test-refresh",
		ExpiresAt:            time.Now().Add(time.Hour),
		AccessTokenExpiresAt: time.Now().Add(time.Hour),
	}

	encodedKey := base64.URLEncoding.EncodeToString([]byte(challengeURI))
	req := httptest.NewRequest("DELETE", "/api/challenge-notifications/"+encodedKey, nil)
	ctx := context.WithValue(req.Context(), contextKeySession, session)
	ctx = context.WithValue(ctx, contextKeyDID, session.DID)
	req = req.WithContext(ctx)
	req = mux.SetURLVars(req, map[string]string{"key": encodedKey})

	w := httptest.NewRecorder()
	svc.DeclineChallengeHandler(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected HTTP 204, got %d: %s", w.Code, w.Body.String())
	}
	if lastRepo != "did:plc:responder" {
		t.Errorf("expected the decline record to be written to the responder's own repo, got repo=%q", lastRepo)
	}
	if lastCollection != "app.atchess.challengeResponse" {
		t.Errorf("expected collection app.atchess.challengeResponse, got %q", lastCollection)
	}

	remaining := store.ForPlayer("did:plc:responder")
	if len(remaining) != 0 {
		t.Errorf("expected the declined challenge to be removed from the local cache, got %d remaining", len(remaining))
	}
}

// TestDeclineChallengeHandler_RejectsChallengeNotAddressedToCaller ensures a
// caller cannot decline a challenge that was not addressed to them (no
// cached entry for their own DID matching that URI).
func TestDeclineChallengeHandler_RejectsChallengeNotAddressedToCaller(t *testing.T) {
	store := challenge.NewStore()
	challengeURI := "at://did:plc:challenger/app.atchess.challenge/abc123"
	store.Add(&challenge.PendingChallenge{
		ChallengeURI:  challengeURI,
		ChallengeCID:  "challenge-cid",
		ChallengerDID: "did:plc:challenger",
		ChallengedDID: "did:plc:someoneelse",
		CreatedAt:     time.Now(),
		ExpiresAt:     time.Now().Add(24 * time.Hour),
	})

	svc := &Service{config: &config.Config{}, challengeStore: store}

	session := &oauth.Session{
		DID:                  "did:plc:responder",
		Handle:               "responder.test",
		PDSURL:               "http://unused.example",
		AccessToken:          "test-jwt",
		RefreshToken:         "test-refresh",
		ExpiresAt:            time.Now().Add(time.Hour),
		AccessTokenExpiresAt: time.Now().Add(time.Hour),
	}

	encodedKey := base64.URLEncoding.EncodeToString([]byte(challengeURI))
	req := httptest.NewRequest("DELETE", "/api/challenge-notifications/"+encodedKey, nil)
	ctx := context.WithValue(req.Context(), contextKeySession, session)
	ctx = context.WithValue(ctx, contextKeyDID, session.DID)
	req = req.WithContext(ctx)
	req = mux.SetURLVars(req, map[string]string{"key": encodedKey})

	w := httptest.NewRecorder()
	svc.DeclineChallengeHandler(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected HTTP 404 (challenge not addressed to this caller), got %d: %s", w.Code, w.Body.String())
	}
}
