//go:build e2e

package e2e

import (
	"encoding/json"
	"net/http"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestConcurrentMoveRaceCondition verifies that when two players submit moves
// simultaneously, exactly one succeeds (the player whose turn it is) and the
// other is rejected, leaving the game state consistent.
func TestConcurrentMoveRaceCondition(t *testing.T) {
	env := loadEnv()

	gameID, _ := setupGame(t, env)

	// Login both players
	aliceSession, _ := login(t, env.protocolURL, env.aliceHandle, env.alicePass)
	bobSession, _ := login(t, env.protocolURL, env.bobHandle, env.bobPass)

	// It's white's (Alice's) turn. Both players fire moves simultaneously.
	// Alice submits a legal white move (e2->e4), Bob submits a legal black move (e7->e5).
	// Only Alice's should succeed; Bob's should be rejected (not his turn).

	type result struct {
		name       string
		statusCode int
		body       string
	}

	var wg sync.WaitGroup
	results := make([]result, 2)

	wg.Add(2)

	// Alice's move (should succeed — it's white's turn)
	go func() {
		defer wg.Done()
		resp := authedPost(t, env.protocolURL+"/api/moves", aliceSession, map[string]string{
			"from":    "e2",
			"to":      "e4",
			"game_id": gameID,
		})
		defer resp.Body.Close()
		results[0] = result{
			name:       "alice",
			statusCode: resp.StatusCode,
			body:       readBody(t, resp),
		}
	}()

	// Bob's move (should fail — it's not his turn)
	go func() {
		defer wg.Done()
		resp := authedPost(t, env.protocolURL+"/api/moves", bobSession, map[string]string{
			"from":    "e7",
			"to":      "e5",
			"game_id": gameID,
		})
		defer resp.Body.Close()
		results[1] = result{
			name:       "bob",
			statusCode: resp.StatusCode,
			body:       readBody(t, resp),
		}
	}()

	wg.Wait()

	// Exactly one should succeed (200 OK) and one should fail (403 Forbidden)
	aliceResult := results[0]
	bobResult := results[1]

	t.Logf("Alice (white, her turn): HTTP %d — %s", aliceResult.statusCode, aliceResult.body)
	t.Logf("Bob (black, not his turn): HTTP %d — %s", bobResult.statusCode, bobResult.body)

	assert.Equal(t, http.StatusOK, aliceResult.statusCode, "Alice's move should succeed (it's white's turn)")
	assert.Equal(t, http.StatusForbidden, bobResult.statusCode, "Bob's move should be rejected (not his turn)")

	// Verify game state is consistent: after Alice's e4, it should be black's turn
	// Make Bob's move now to confirm the game continues normally
	resp := authedPost(t, env.protocolURL+"/api/moves", bobSession, map[string]string{
		"from":    "e7",
		"to":      "e5",
		"game_id": gameID,
	})
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode, "Bob should be able to move after Alice's turn")

	var moveResult map[string]interface{}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&moveResult))
	assert.NotEmpty(t, moveResult["fen"], "Response should include updated FEN")
	t.Logf("Game continues normally after concurrent attempt — Bob played e5, FEN: %s", moveResult["fen"])
}

// TestConcurrentMoveSamePlayer verifies that if the same player submits two
// moves concurrently, exactly one succeeds and the game state stays consistent
// (no duplicate moves, no corrupted FEN).
func TestConcurrentMoveSamePlayer(t *testing.T) {
	env := loadEnv()

	gameID, _ := setupGame(t, env)

	aliceSession, _ := login(t, env.protocolURL, env.aliceHandle, env.alicePass)

	// Alice submits two different legal white moves at the same time.
	// Exactly one should succeed; the other should fail (CAS conflict or
	// stale FEN since the first move changed the position).

	type result struct {
		name       string
		statusCode int
		body       string
	}

	var wg sync.WaitGroup
	results := make([]result, 2)

	wg.Add(2)

	// Move A: e2->e4
	go func() {
		defer wg.Done()
		resp := authedPost(t, env.protocolURL+"/api/moves", aliceSession, map[string]string{
			"from":    "e2",
			"to":      "e4",
			"game_id": gameID,
		})
		defer resp.Body.Close()
		results[0] = result{
			name:       "e2e4",
			statusCode: resp.StatusCode,
			body:       readBody(t, resp),
		}
	}()

	// Move B: d2->d4
	go func() {
		defer wg.Done()
		resp := authedPost(t, env.protocolURL+"/api/moves", aliceSession, map[string]string{
			"from":    "d2",
			"to":      "d4",
			"game_id": gameID,
		})
		defer resp.Body.Close()
		results[1] = result{
			name:       "d2d4",
			statusCode: resp.StatusCode,
			body:       readBody(t, resp),
		}
	}()

	wg.Wait()

	t.Logf("Move A (e2e4): HTTP %d — %s", results[0].statusCode, results[0].body)
	t.Logf("Move B (d2d4): HTTP %d — %s", results[1].statusCode, results[1].body)

	// Count successes — exactly one should win
	successes := 0
	failures := 0
	for _, r := range results {
		if r.statusCode == http.StatusOK {
			successes++
		} else {
			failures++
		}
	}

	assert.Equal(t, 1, successes, "Exactly one concurrent move from the same player should succeed")
	assert.Equal(t, 1, failures, "Exactly one concurrent move from the same player should fail")

	// Verify game continues: it's now black's turn
	bobSession, _ := login(t, env.protocolURL, env.bobHandle, env.bobPass)
	resp := authedPost(t, env.protocolURL+"/api/moves", bobSession, map[string]string{
		"from":    "e7",
		"to":      "e5",
		"game_id": gameID,
	})
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode, "Bob should be able to move after Alice's concurrent attempt resolved")
	t.Logf("Game state consistent — Bob played e5 successfully after concurrent same-player moves")
}
