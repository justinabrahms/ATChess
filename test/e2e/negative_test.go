package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/justinabrahms/atchess/internal/atproto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// loginResponse matches the AuthResponse from the web service.
type loginResponse struct {
	Success     bool   `json:"success"`
	DID         string `json:"did"`
	Handle      string `json:"handle"`
	AccessToken string `json:"accessToken"`
}

// login authenticates via the HTTP API and returns the session token and DID.
func login(t *testing.T, protocolURL, handle, password string) (sessionID, did string) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{
		"handle":   handle,
		"password": password,
	})
	resp, err := http.Post(protocolURL+"/api/auth/login", "application/json", bytes.NewBuffer(body))
	require.NoError(t, err)
	defer resp.Body.Close()

	var lr loginResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&lr))
	require.True(t, lr.Success, "login must succeed for %s", handle)
	return lr.AccessToken, lr.DID
}

// authedPost sends a POST with the X-Session-ID header set.
func authedPost(t *testing.T, url, sessionID string, payload interface{}) *http.Response {
	t.Helper()
	body, _ := json.Marshal(payload)
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	if sessionID != "" {
		req.Header.Set("X-Session-ID", sessionID)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return resp
}

// setupGame creates a game via Alice's AT Proto client and returns the game ID and starting FEN.
func setupGame(t *testing.T, env testEnv) (gameID, fen string) {
	t.Helper()
	aliceClient, err := atproto.NewClient(env.pdsAlice, env.aliceHandle, env.alicePass)
	require.NoError(t, err)
	bobClient, err := atproto.NewClient(env.pdsBob, env.bobHandle, env.bobPass)
	require.NoError(t, err)

	game, err := aliceClient.CreateGame(context.Background(), bobClient.GetDID(), "white")
	require.NoError(t, err)
	return game.ID, game.FEN
}

// TestIllegalMoveRejected verifies the server rejects an illegal chess move.
func TestIllegalMoveRejected(t *testing.T) {
	env := loadEnv()

	gameID, fen := setupGame(t, env)
	aliceSession, _ := login(t, env.protocolURL, env.aliceHandle, env.alicePass)

	// e2 to e5 is illegal (pawn can only move 1 or 2 squares from starting position,
	// but e5 is 3 squares ahead)
	resp := authedPost(t, env.protocolURL+"/api/moves", aliceSession, map[string]string{
		"from":    "e2",
		"to":      "e5",
		"fen":     fen,
		"game_id": gameID,
	})
	defer resp.Body.Close()

	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "illegal move should be rejected")
	body := readBody(t, resp)
	assert.True(t, strings.Contains(body, "Invalid move"), "response should mention invalid move, got: %s", body)
}

// TestMoveOutOfTurnRejected verifies the server rejects a move when it's not the player's turn.
func TestMoveOutOfTurnRejected(t *testing.T) {
	env := loadEnv()

	gameID, fen := setupGame(t, env)
	// It's white's turn (Alice), but Bob tries to move
	bobSession, _ := login(t, env.protocolURL, env.bobHandle, env.bobPass)

	resp := authedPost(t, env.protocolURL+"/api/moves", bobSession, map[string]string{
		"from":    "e7",
		"to":      "e5",
		"fen":     fen,
		"game_id": gameID,
	})
	defer resp.Body.Close()

	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "move out of turn should be rejected")
	body := readBody(t, resp)
	assert.True(t, strings.Contains(body, "not your turn"), "response should mention turn, got: %s", body)
}

// TestMoveByNonPlayerRejected verifies the server rejects a move from someone who isn't in the game.
func TestMoveByNonPlayerRejected(t *testing.T) {
	env := loadEnv()

	gameID, fen := setupGame(t, env)

	// Create a third user session (we'll reuse alice/bob PDS but the key thing
	// is the DID won't match the game's white or black)
	// For this test, we create a session with a handle that resolves to a different DID.
	// If only two test accounts exist, we can test by having Bob try to move
	// in a game where Alice plays both colors — but simpler: just use a made-up
	// session. Since we can't easily create a 3rd PDS account, we test the
	// non-player scenario by attempting a move with Bob on a game where he's not
	// a participant.

	// Create a game where Alice plays white against herself (using her own DID as black)
	aliceClient, err := atproto.NewClient(env.pdsAlice, env.aliceHandle, env.alicePass)
	require.NoError(t, err)
	selfGame, err := aliceClient.CreateGame(context.Background(), aliceClient.GetDID(), "white")
	require.NoError(t, err)

	// Bob is not a player in this self-game
	bobSession, _ := login(t, env.protocolURL, env.bobHandle, env.bobPass)

	resp := authedPost(t, env.protocolURL+"/api/moves", bobSession, map[string]string{
		"from":    "e2",
		"to":      "e4",
		"fen":     selfGame.FEN,
		"game_id": selfGame.ID,
	})
	defer resp.Body.Close()

	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "non-player move should be rejected")
	body := readBody(t, resp)
	assert.True(t, strings.Contains(body, "not a player"), "response should mention not a player, got: %s", body)

	// Also verify the original game setup works (sanity)
	_ = gameID
	_ = fen
}

// TestMoveWithoutAuthRejected verifies the server rejects a move with no authentication.
func TestMoveWithoutAuthRejected(t *testing.T) {
	env := loadEnv()

	gameID, fen := setupGame(t, env)

	// Send move request with no X-Session-ID header
	resp := authedPost(t, env.protocolURL+"/api/moves", "", map[string]string{
		"from":    "e2",
		"to":      "e4",
		"fen":     fen,
		"game_id": gameID,
	})
	defer resp.Body.Close()

	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, "unauthenticated move should be rejected")
}

// TestMoveWithWrongPlayerAuthRejected verifies that Player A's auth can't be used for Player B's turn.
func TestMoveWithWrongPlayerAuthRejected(t *testing.T) {
	env := loadEnv()

	gameID, fen := setupGame(t, env)

	// Alice is white, it's white's turn. Login as Alice and make e4.
	aliceSession, _ := login(t, env.protocolURL, env.aliceHandle, env.alicePass)

	resp := authedPost(t, env.protocolURL+"/api/moves", aliceSession, map[string]string{
		"from":    "e2",
		"to":      "e4",
		"fen":     fen,
		"game_id": gameID,
	})
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "Alice's legal move should succeed")

	// Parse the new FEN from Alice's move
	var moveResult map[string]interface{}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&moveResult))
	newFEN, ok := moveResult["fen"].(string)
	require.True(t, ok, "response must include fen")

	// Now it's black's turn (Bob). Try to move with Alice's session.
	resp2 := authedPost(t, env.protocolURL+"/api/moves", aliceSession, map[string]string{
		"from":    "e7",
		"to":      "e5",
		"fen":     newFEN,
		"game_id": gameID,
	})
	defer resp2.Body.Close()

	assert.Equal(t, http.StatusForbidden, resp2.StatusCode, "wrong player's auth should be rejected")
	body := readBody(t, resp2)
	assert.True(t, strings.Contains(body, "not your turn"), "response should mention turn, got: %s", body)
}

// readBody reads the response body as a string.
func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	buf := new(bytes.Buffer)
	_, err := buf.ReadFrom(resp.Body)
	require.NoError(t, err)
	return buf.String()
}
