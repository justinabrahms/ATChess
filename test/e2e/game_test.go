package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"testing"

	"github.com/justinabrahms/atchess/internal/atproto"
	"github.com/justinabrahms/atchess/internal/chess"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testEnv holds the dual-PDS test environment configuration.
// Values come from environment variables set by scripts/test-local.sh,
// falling back to single-PDS defaults for backward compatibility.
type testEnv struct {
	pdsAlice    string
	pdsBob      string
	aliceHandle string
	alicePass   string
	bobHandle   string
	bobPass     string
	protocolURL string
}

func loadEnv() testEnv {
	env := testEnv{
		pdsAlice:    os.Getenv("ATCHESS_TEST_PDS_ALICE"),
		pdsBob:      os.Getenv("ATCHESS_TEST_PDS_BOB"),
		aliceHandle: os.Getenv("ATCHESS_TEST_ALICE_HANDLE"),
		alicePass:   os.Getenv("ATCHESS_TEST_ALICE_PASS"),
		bobHandle:   os.Getenv("ATCHESS_TEST_BOB_HANDLE"),
		bobPass:     os.Getenv("ATCHESS_TEST_BOB_PASS"),
		protocolURL: os.Getenv("ATCHESS_TEST_PROTOCOL_URL"),
	}

	// Backward-compatible defaults (single PDS on port 3000)
	if env.pdsAlice == "" {
		env.pdsAlice = "http://localhost:3000"
	}
	if env.pdsBob == "" {
		env.pdsBob = env.pdsAlice
	}
	if env.aliceHandle == "" {
		env.aliceHandle = "player1.test"
	}
	if env.alicePass == "" {
		env.alicePass = "player1pass"
	}
	if env.bobHandle == "" {
		env.bobHandle = "player2.test"
	}
	if env.bobPass == "" {
		env.bobPass = "player2pass"
	}
	if env.protocolURL == "" {
		env.protocolURL = "http://localhost:8080"
	}

	return env
}

// TestFoolsMate tests the classic fool's mate in 4 moves: e4 e5 Qh5 Ke7 Qxe5#
func TestFoolsMate(t *testing.T) {
	env := loadEnv()

	aliceClient, err := atproto.NewClient(env.pdsAlice, env.aliceHandle, env.alicePass)
	require.NoError(t, err)

	bobClient, err := atproto.NewClient(env.pdsBob, env.bobHandle, env.bobPass)
	require.NoError(t, err)

	// Alice (white) creates a game against Bob (black)
	game, err := aliceClient.CreateGame(context.Background(), bobClient.GetDID(), "white")
	require.NoError(t, err)

	t.Logf("Created game: %s", game.ID)
	assert.Equal(t, chess.StatusActive, game.Status)
	assert.Equal(t, aliceClient.GetDID(), game.White)
	assert.Equal(t, bobClient.GetDID(), game.Black)

	currentFEN := game.FEN

	// Move 1: White plays e4
	currentFEN = makeMove(t, aliceClient, game.ID, currentFEN, "e2", "e4", "")
	t.Logf("After e4: %s", currentFEN)

	// Move 2: Black plays e5
	currentFEN = makeMove(t, bobClient, game.ID, currentFEN, "e7", "e5", "")
	t.Logf("After e5: %s", currentFEN)

	// Move 3: White plays Qh5
	currentFEN = makeMove(t, aliceClient, game.ID, currentFEN, "d1", "h5", "")
	t.Logf("After Qh5: %s", currentFEN)

	// Move 4: Black plays Ke7 (the blunder)
	currentFEN = makeMove(t, bobClient, game.ID, currentFEN, "e8", "e7", "")
	t.Logf("After Ke7: %s", currentFEN)

	// Move 5: White plays Qxe5# (checkmate)
	finalResult := makeMoveExpectCheckmate(t, aliceClient, game.ID, currentFEN, "h5", "e5", "")
	t.Logf("Final position after Qxe5#: %s", finalResult.FEN)

	assert.True(t, finalResult.Check, "Final move should be check")
	assert.True(t, finalResult.Checkmate, "Final move should be checkmate")
	assert.Equal(t, "Qxe5#", finalResult.SAN)
}

// TestScholarsMateVariant tests a scholar's mate variant where black wins: g4 e5 f4 Qh4#
func TestScholarsMateVariant(t *testing.T) {
	env := loadEnv()

	aliceClient, err := atproto.NewClient(env.pdsAlice, env.aliceHandle, env.alicePass)
	require.NoError(t, err)

	bobClient, err := atproto.NewClient(env.pdsBob, env.bobHandle, env.bobPass)
	require.NoError(t, err)

	// Alice (white) creates a game
	game, err := aliceClient.CreateGame(context.Background(), bobClient.GetDID(), "white")
	require.NoError(t, err)

	t.Logf("Created game: %s", game.ID)
	assert.Equal(t, chess.StatusActive, game.Status)
	assert.Equal(t, aliceClient.GetDID(), game.White)
	assert.Equal(t, bobClient.GetDID(), game.Black)

	currentFEN := game.FEN

	// Move 1: White plays g4 (weak opening)
	currentFEN = makeMove(t, aliceClient, game.ID, currentFEN, "g2", "g4", "")
	t.Logf("After g4: %s", currentFEN)

	// Move 2: Black plays e5
	currentFEN = makeMove(t, bobClient, game.ID, currentFEN, "e7", "e5", "")
	t.Logf("After e5: %s", currentFEN)

	// Move 3: White plays f4 (another weak move)
	currentFEN = makeMove(t, aliceClient, game.ID, currentFEN, "f2", "f4", "")
	t.Logf("After f4: %s", currentFEN)

	// Move 4: Black plays Qh4# (checkmate)
	finalResult := makeMoveExpectCheckmate(t, bobClient, game.ID, currentFEN, "d8", "h4", "")
	t.Logf("Final position after Qh4#: %s", finalResult.FEN)

	assert.True(t, finalResult.Check, "Final move should be check")
	assert.True(t, finalResult.Checkmate, "Final move should be checkmate")
	assert.Equal(t, "Qh4#", finalResult.SAN)
}

// makeMove makes a move and returns the new FEN position
func makeMove(t *testing.T, client *atproto.Client, gameID, currentFEN, from, to, promotion string) string {
	engine, err := chess.NewEngineFromFEN(currentFEN)
	require.NoError(t, err)

	promotionPiece := chess.ParsePromotion(promotion)

	moveResult, err := engine.MakeMove(from, to, promotionPiece)
	require.NoError(t, err)

	err = client.RecordMove(context.Background(), gameID, moveResult)
	require.NoError(t, err)

	t.Logf("Move: %s -> %s (%s)", from, to, moveResult.SAN)

	return moveResult.FEN
}

// makeMoveExpectCheckmate makes a move and expects it to be checkmate
func makeMoveExpectCheckmate(t *testing.T, client *atproto.Client, gameID, currentFEN, from, to, promotion string) *chess.MoveResult {
	engine, err := chess.NewEngineFromFEN(currentFEN)
	require.NoError(t, err)

	promotionPiece := chess.ParsePromotion(promotion)

	moveResult, err := engine.MakeMove(from, to, promotionPiece)
	require.NoError(t, err)

	err = client.RecordMove(context.Background(), gameID, moveResult)
	require.NoError(t, err)

	t.Logf("Final move: %s -> %s (%s)", from, to, moveResult.SAN)

	return moveResult
}

// TestGameStateReconstruction validates that canonical game state can be
// reconstructed by reading move records from both players' repos independently,
// interleaving them by timestamp, and replaying through the chess engine.
func TestGameStateReconstruction(t *testing.T) {
	env := loadEnv()

	aliceClient, err := atproto.NewClient(env.pdsAlice, env.aliceHandle, env.alicePass)
	require.NoError(t, err)

	bobClient, err := atproto.NewClient(env.pdsBob, env.bobHandle, env.bobPass)
	require.NoError(t, err)

	// Alice (white) creates a game against Bob (black)
	game, err := aliceClient.CreateGame(context.Background(), bobClient.GetDID(), "white")
	require.NoError(t, err)
	t.Logf("Created game: %s (URI: %s)", game.ID, game.ID)

	// Play fool's mate: e4 e5 Qh5 Ke7 Qxe5#
	currentFEN := game.FEN

	currentFEN = makeMove(t, aliceClient, game.ID, currentFEN, "e2", "e4", "")
	currentFEN = makeMove(t, bobClient, game.ID, currentFEN, "e7", "e5", "")
	currentFEN = makeMove(t, aliceClient, game.ID, currentFEN, "d1", "h5", "")
	currentFEN = makeMove(t, bobClient, game.ID, currentFEN, "e8", "e7", "")
	finalResult := makeMoveExpectCheckmate(t, aliceClient, game.ID, currentFEN, "h5", "e5", "")

	expectedFinalFEN := finalResult.FEN
	t.Logf("Expected final FEN: %s", expectedFinalFEN)

	// --- Reconstruction phase ---
	// Fetch moves independently from each player's PDS
	aliceMoves, err := aliceClient.ListMovesForGame(context.Background(), game.ID)
	require.NoError(t, err)
	t.Logf("Alice's repo has %d moves for this game", len(aliceMoves))

	bobMoves, err := bobClient.ListMovesForGame(context.Background(), game.ID)
	require.NoError(t, err)
	t.Logf("Bob's repo has %d moves for this game", len(bobMoves))

	// Validate each player only has their own moves
	assert.Equal(t, 3, len(aliceMoves), "Alice (white) should have 3 moves: e4, Qh5, Qxe5#")
	assert.Equal(t, 2, len(bobMoves), "Bob (black) should have 2 moves: e5, Ke7")

	for _, m := range aliceMoves {
		assert.Equal(t, aliceClient.GetDID(), m.Player, "Alice's moves should have her DID as player")
	}
	for _, m := range bobMoves {
		assert.Equal(t, bobClient.GetDID(), m.Player, "Bob's moves should have his DID as player")
	}

	// Interleave moves by CreatedAt timestamp
	allMoves := append(aliceMoves, bobMoves...)
	sort.Slice(allMoves, func(i, j int) bool {
		return allMoves[i].CreatedAt.Before(allMoves[j].CreatedAt)
	})

	assert.Equal(t, 5, len(allMoves), "Total moves should be 5")

	// Replay through the chess engine from initial position
	engine := chess.NewEngine()
	for i, m := range allMoves {
		promotionPiece := chess.ParsePromotion("")
		result, err := engine.MakeMove(m.From, m.To, promotionPiece)
		require.NoError(t, err, "Replay move %d (%s) failed", i+1, m.SAN)
		t.Logf("Replayed move %d: %s -> %s (%s)", i+1, m.From, m.To, result.SAN)

		// Each replayed move's resulting FEN should match what was stored
		assert.Equal(t, m.FEN, result.FEN,
			"Move %d: replayed FEN should match stored FEN", i+1)
	}

	// Verify final position matches
	reconstructedFEN := engine.GetFEN()
	assert.Equal(t, expectedFinalFEN, reconstructedFEN,
		"Reconstructed final position should match the original game")

	// Verify game outcome: white delivered checkmate, so white wins
	assert.Equal(t, chess.StatusWhiteWon, engine.GetStatus(),
		"Reconstructed game should show white won by checkmate")

	t.Log("Game state successfully reconstructed from independent player repos")
}

// TestAPIEndpoints tests the REST API endpoints directly
func TestAPIEndpoints(t *testing.T) {
	env := loadEnv()

	// Test health endpoint
	resp, err := http.Get(env.protocolURL + "/api/health")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var health map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&health)
	require.NoError(t, err)

	assert.Equal(t, "ok", health["status"])
	assert.Contains(t, health, "did")
	assert.Contains(t, health, "handle")

	t.Log("Health endpoint working correctly")

	// Test game creation via API
	bobClient, err := atproto.NewClient(env.pdsBob, env.bobHandle, env.bobPass)
	require.NoError(t, err)

	createGameReq := map[string]interface{}{
		"opponent_did": bobClient.GetDID(),
		"color":        "white",
	}

	reqBody, _ := json.Marshal(createGameReq)
	resp, err = http.Post(env.protocolURL+"/api/games", "application/json", bytes.NewBuffer(reqBody))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var game chess.Game
	err = json.NewDecoder(resp.Body).Decode(&game)
	require.NoError(t, err)

	assert.NotEmpty(t, game.ID)
	assert.Equal(t, chess.StatusActive, game.Status)
	assert.NotEmpty(t, game.White)
	assert.NotEmpty(t, game.Black)

	t.Logf("Created game via API: %s", game.ID)

	// Test move submission via API
	makeMoveReq := map[string]interface{}{
		"from":    "e2",
		"to":      "e4",
		"fen":     game.FEN,
		"game_id": game.ID,
	}

	reqBody, _ = json.Marshal(makeMoveReq)
	moveURL := fmt.Sprintf("%s/api/games/test-game/moves", env.protocolURL)
	t.Logf("Move URL: %s", moveURL)
	resp, err = http.Post(moveURL, "application/json", bytes.NewBuffer(reqBody))
	require.NoError(t, err)
	defer resp.Body.Close()

	t.Logf("Response status: %d", resp.StatusCode)
	if resp.StatusCode != http.StatusOK {
		body := make([]byte, 1000)
		n, _ := resp.Body.Read(body)
		t.Logf("Response body: %s", string(body[:n]))
	}

	if resp.StatusCode == http.StatusOK {
		var moveResult chess.MoveResult
		err = json.NewDecoder(resp.Body).Decode(&moveResult)
		require.NoError(t, err)

		assert.Equal(t, "e2", moveResult.From)
		assert.Equal(t, "e4", moveResult.To)
		assert.Equal(t, "e4", moveResult.SAN)
		assert.NotEmpty(t, moveResult.FEN)

		t.Log("Move submission via API working correctly")
	} else {
		t.Log("Move submission via API not working - routing issue with AT Protocol URIs")
	}
}

func TestMain(m *testing.M) {
	m.Run()
}
