package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/justinabrahms/atchess/internal/config"
	"github.com/justinabrahms/atchess/internal/oauth"
)

// TestMakeMoveHandler_NonPlayerMove_Forbidden is a regression test for
// atchess-1c9.74 item 1: internal/web/service.go's MakeMoveHandler must
// reject a move attempted by an authenticated caller who is neither
// game.White nor game.Black, with HTTP 403 "You are not a player in this
// game". This is distinct from TestMakeMoveHandler_NonOwnerMove_* in
// move_and_draw_ownership_test.go, which is about record-repo *ownership*
// of the app.atchess.game record (the mover IS a player, just not the
// owner of the cached game record) -- here the mover is not a player in
// the game at all. Before this test, nothing in the repository exercised
// this branch.
func TestMakeMoveHandler_NonPlayerMove_Forbidden(t *testing.T) {
	const whiteDID = "did:plc:white"
	const blackDID = "did:plc:black"
	const outsiderDID = "did:plc:outsider" // authenticated, but not a player in this game

	mock := newMockFederatedPDS(t, outsiderDID)
	srv := mock.server()
	defer srv.Close()
	mock.setBaseURL(srv.URL)

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

	session := &oauth.Session{
		DID:                  outsiderDID,
		Handle:               "outsider.test",
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

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected HTTP 403, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "You are not a player in this game") {
		t.Errorf("expected body to contain 'You are not a player in this game', got: %s", w.Body.String())
	}

	mock.mu.Lock()
	calls := append([]string(nil), mock.writeCalls...)
	mock.mu.Unlock()
	if len(calls) != 0 {
		t.Errorf("expected no write calls at all (move must be rejected before RecordMove is ever reached), got %d: %v", len(calls), calls)
	}
}

// TestMakeMoveHandler_InvalidMove_BadRequest is a regression test for
// atchess-1c9.74 item 4: internal/web/service.go's MakeMoveHandler must
// reject a chess-illegal move with HTTP 400 "Invalid move: ...". The
// engine-level rejection itself is covered by
// internal/chess/invariants_test.go:219, but nothing previously exercised
// the HTTP 400 branch this handler wraps it in. e2->e5 is not a legal
// first move (a pawn may advance at most two squares from its starting
// rank; e2->e5 is three).
func TestMakeMoveHandler_InvalidMove_BadRequest(t *testing.T) {
	const moverDID = "did:plc:white"
	const opponentDID = "did:plc:black"

	mock := newMockFederatedPDS(t, moverDID)
	srv := mock.server()
	defer srv.Close()
	mock.setBaseURL(srv.URL)

	const gameRkey = "game1"
	mock.seed(moverDID, "app.atchess.game", gameRkey, map[string]interface{}{
		"$type":     "app.atchess.game",
		"createdAt": time.Now().Format(time.RFC3339),
		"white":     moverDID,
		"black":     opponentDID,
		"status":    "active",
		"fen":       "rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR w KQkq - 0 1",
		"pgn":       "",
	})
	gameURI := fmt.Sprintf("at://%s/app.atchess.game/%s", moverDID, gameRkey)

	svc := &Service{
		config:         &config.Config{ATProto: config.ATProtoConfig{PLCDirectoryURL: srv.URL}},
		challengeStore: newTestChallengeStore(t),
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
		"to":      "e5", // illegal: pawns may advance at most two squares from the start
	})
	req := httptest.NewRequest("POST", "/api/moves", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ctx := context.WithValue(req.Context(), contextKeySession, session)
	ctx = context.WithValue(ctx, contextKeyDID, session.DID)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	svc.MakeMoveHandler(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected HTTP 400, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Invalid move") {
		t.Errorf("expected body to contain 'Invalid move', got: %s", w.Body.String())
	}

	mock.mu.Lock()
	calls := append([]string(nil), mock.writeCalls...)
	mock.mu.Unlock()
	if len(calls) != 0 {
		t.Errorf("expected no write calls at all (move must be rejected before RecordMove is ever reached), got %d: %v", len(calls), calls)
	}
}
