//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/justinabrahms/atchess/internal/atproto"
	"github.com/justinabrahms/atchess/internal/challenge"
	"github.com/justinabrahms/atchess/internal/chess"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFullGameLifecycle exercises the complete happy path:
// challenge -> discover -> accept -> moves -> checkmate -> verify repos
func TestFullGameLifecycle(t *testing.T) {
	env := loadEnv()

	// --- Step 1: Create clients for both players on separate PDSes ---
	aliceClient, err := atproto.NewClient(env.pdsAlice, env.aliceHandle, env.alicePass)
	require.NoError(t, err, "Alice should authenticate successfully")

	bobClient, err := atproto.NewClient(env.pdsBob, env.bobHandle, env.bobPass)
	require.NoError(t, err, "Bob should authenticate successfully")

	aliceDID := aliceClient.GetDID()
	bobDID := bobClient.GetDID()
	require.NotEmpty(t, aliceDID, "Alice should have a DID")
	require.NotEmpty(t, bobDID, "Bob should have a DID")
	t.Logf("Alice: %s (%s)", env.aliceHandle, aliceDID)
	t.Logf("Bob:   %s (%s)", env.bobHandle, bobDID)

	// --- Step 2: Alice creates a challenge to Bob ---
	t.Log("--- Challenge Phase ---")
	ch, err := aliceClient.CreateChallenge(context.Background(), bobDID, "white", "Let's play!")
	require.NoError(t, err, "Alice should create a challenge")

	assert.Equal(t, "pending", ch.Status)
	assert.Equal(t, aliceDID, ch.Challenger)
	assert.Equal(t, bobDID, ch.Challenged)
	assert.Equal(t, "white", ch.Color)
	assert.NotEmpty(t, ch.ProposedGameId, "Challenge should have a proposed game ID")
	assert.NotEmpty(t, ch.ID, "Challenge should have a URI")
	t.Logf("Challenge created: URI=%s, ProposedGameID=%s", ch.ID, ch.ProposedGameId)

	// --- Step 3: Index the challenge locally (simulating server behavior) ---
	store := challenge.NewStore()
	createdAt, _ := time.Parse(time.RFC3339, ch.CreatedAt)
	expiresAt, _ := time.Parse(time.RFC3339, ch.ExpiresAt)
	added := store.Add(&challenge.PendingChallenge{
		ChallengeURI:     ch.ID,
		ChallengerDID:    ch.Challenger,
		ChallengerHandle: env.aliceHandle,
		ChallengedDID:    ch.Challenged,
		Color:            ch.Color,
		Message:          ch.Message,
		ProposedGameID:   ch.ProposedGameId,
		CreatedAt:        createdAt,
		ExpiresAt:        expiresAt,
	})
	assert.True(t, added, "Challenge should be indexed successfully")

	// --- Step 4: Bob discovers the challenge ---
	t.Log("--- Discovery Phase ---")
	pending := store.ForPlayer(bobDID)
	require.Len(t, pending, 1, "Bob should see exactly one pending challenge")

	discovered := pending[0]
	assert.Equal(t, ch.ID, discovered.ChallengeURI)
	assert.Equal(t, aliceDID, discovered.ChallengerDID)
	assert.Equal(t, "white", discovered.Color)
	assert.Equal(t, "Let's play!", discovered.Message)
	t.Logf("Bob discovered challenge from %s", discovered.ChallengerHandle)

	// --- Step 5: Bob accepts by creating the game ---
	t.Log("--- Acceptance Phase ---")
	// The challenged player creates the game. Alice chose "white", so Bob is "black".
	// Parse challenge URI to get CID for the reference
	challengeURI := discovered.ChallengeURI
	challengeCID := "" // CID not stored in the challenge store; pass empty for E2E

	game, err := bobClient.CreateGameFromChallenge(
		context.Background(),
		aliceDID,       // opponent is Alice
		"black",        // Bob plays black (Alice chose white)
		discovered.ProposedGameID,
		challengeURI,
		challengeCID,
	)
	require.NoError(t, err, "Bob should create game from challenge")

	assert.Equal(t, chess.StatusActive, game.Status)
	assert.Equal(t, aliceDID, game.White, "Alice should be white")
	assert.Equal(t, bobDID, game.Black, "Bob should be black")
	t.Logf("Game created: %s", game.ID)

	// Remove accepted challenge from store
	store.Remove(challengeURI)
	assert.Empty(t, store.ForPlayer(bobDID), "No more pending challenges for Bob")

	// --- Step 6: Play Scholar's Mate (e4 e5 Bc4 Nc6 Qh5 Nf6 Qxf7#) ---
	t.Log("--- Move Phase: Scholar's Mate ---")
	moves := []struct {
		player *atproto.Client
		name   string
		from   string
		to     string
		san    string
	}{
		{aliceClient, "Alice", "e2", "e4", "e4"},
		{bobClient, "Bob", "e7", "e5", "e5"},
		{aliceClient, "Alice", "f1", "c4", "Bc4"},
		{bobClient, "Bob", "b8", "c6", "Nc6"},
		{aliceClient, "Alice", "d1", "h5", "Qh5"},
		{bobClient, "Bob", "g8", "f6", "Nf6"},
	}

	currentFEN := game.FEN
	for i, m := range moves {
		t.Logf("Move %d: %s plays %s (%s->%s)", i+1, m.name, m.san, m.from, m.to)
		currentFEN = makeMove(t, m.player, game.ID, currentFEN, m.from, m.to, "")
	}

	// --- Step 7: Final move — Qxf7# (checkmate) ---
	t.Log("Move 7: Alice plays Qxf7# (checkmate!)")
	finalResult := makeMoveExpectCheckmate(t, aliceClient, game.ID, currentFEN, "h5", "f7", "")
	t.Logf("Final FEN: %s", finalResult.FEN)

	assert.True(t, finalResult.Check, "Final move should be check")
	assert.True(t, finalResult.Checkmate, "Final move should be checkmate")
	assert.Equal(t, "Qxf7#", finalResult.SAN)

	// --- Step 8: Verify move records from both players' repos ---
	t.Log("--- Verification Phase ---")

	aliceMoves := listMoveRecords(t, aliceClient, game.ID)
	bobMoves := listMoveRecords(t, bobClient, game.ID)

	t.Logf("Alice's repo has %d move records for this game", len(aliceMoves))
	t.Logf("Bob's repo has %d move records for this game", len(bobMoves))

	// Alice made 4 moves (e4, Bc4, Qh5, Qxf7#)
	assert.Equal(t, 4, len(aliceMoves), "Alice should have 4 move records")
	// Bob made 3 moves (e5, Nc6, Nf6)
	assert.Equal(t, 3, len(bobMoves), "Bob should have 3 move records")

	// Verify the last move in Alice's repo is the checkmate
	if len(aliceMoves) > 0 {
		lastAliceMove := aliceMoves[len(aliceMoves)-1]
		assert.True(t, lastAliceMove.Checkmate, "Alice's last move should be checkmate")
		assert.Equal(t, "Qxf7#", lastAliceMove.SAN)
		assert.Equal(t, finalResult.FEN, lastAliceMove.FEN, "FEN should match final position")
	}

	// Verify the game record reflects the completed state
	fetchedGame, err := aliceClient.GetGame(context.Background(), game.ID)
	if err == nil {
		// The game is in Bob's repo (Bob created it), so Alice may not be able to
		// fetch it directly if they're on different PDSes. That's OK — the move
		// records are the source of truth.
		t.Logf("Game status from repo: %s", fetchedGame.Status)
	} else {
		t.Logf("Could not fetch game record (expected for cross-PDS): %v", err)
	}

	// Also try from Bob's side (game is in Bob's repo)
	fetchedFromBob, err := bobClient.GetGame(context.Background(), game.ID)
	if err == nil {
		t.Logf("Game status from Bob's repo: %s, FEN: %s", fetchedFromBob.Status, fetchedFromBob.FEN)
	}

	// Reconstruct game from combined move records
	allMoves := append(aliceMoves, bobMoves...)
	// Sort by creation time
	sortMovesByTime(allMoves)

	assert.Equal(t, 7, len(allMoves), "Total moves should be 7 (Scholar's Mate)")
	if len(allMoves) == 7 {
		assert.Equal(t, "e4", allMoves[0].SAN, "Move 1 should be e4")
		assert.Equal(t, "e5", allMoves[1].SAN, "Move 2 should be e5")
		assert.Equal(t, "Bc4", allMoves[2].SAN, "Move 3 should be Bc4")
		assert.Equal(t, "Nc6", allMoves[3].SAN, "Move 4 should be Nc6")
		assert.Equal(t, "Qh5", allMoves[4].SAN, "Move 5 should be Qh5")
		assert.Equal(t, "Nf6", allMoves[5].SAN, "Move 6 should be Nf6")
		assert.Equal(t, "Qxf7#", allMoves[6].SAN, "Move 7 should be Qxf7#")
	}

	t.Log("--- Full game lifecycle test PASSED ---")
}

// repoMoveRecord represents a move as stored in a player's AT Protocol repository.
type repoMoveRecord struct {
	SAN       string
	FEN       string
	From      string
	To        string
	Player    string
	Check     bool
	Checkmate bool
	Draw      bool
	GameURI   string
	CreatedAt time.Time
}

// listMoveRecords reads all move records for a game from a player's repo.
func listMoveRecords(t *testing.T, client *atproto.Client, gameURI string) []repoMoveRecord {
	t.Helper()

	did := client.GetDID()
	pdsURL := client.GetPDSURL()

	url := fmt.Sprintf("%s/xrpc/com.atproto.repo.listRecords?repo=%s&collection=app.atchess.move&limit=100",
		pdsURL, did)

	resp, err := http.Get(url)
	require.NoError(t, err, "Should be able to list move records")
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode, "listRecords should return 200")

	var listResp struct {
		Records []struct {
			Value struct {
				Game struct {
					URI string `json:"uri"`
				} `json:"game"`
				Player    string `json:"player"`
				From      string `json:"from"`
				To        string `json:"to"`
				SAN       string `json:"san"`
				FEN       string `json:"fen"`
				Check     bool   `json:"check"`
				Checkmate bool   `json:"checkmate"`
				Draw      bool   `json:"draw"`
				CreatedAt string `json:"createdAt"`
			} `json:"value"`
		} `json:"records"`
	}

	err = json.NewDecoder(resp.Body).Decode(&listResp)
	require.NoError(t, err, "Should decode listRecords response")

	var moves []repoMoveRecord
	for _, r := range listResp.Records {
		if r.Value.Game.URI != gameURI {
			continue
		}
		created, _ := time.Parse(time.RFC3339, r.Value.CreatedAt)
		moves = append(moves, repoMoveRecord{
			SAN:       r.Value.SAN,
			FEN:       r.Value.FEN,
			From:      r.Value.From,
			To:        r.Value.To,
			Player:    r.Value.Player,
			Check:     r.Value.Check,
			Checkmate: r.Value.Checkmate,
			Draw:      r.Value.Draw,
			GameURI:   r.Value.Game.URI,
			CreatedAt: created,
		})
	}

	// Sort by creation time
	sortMovesByTime(moves)
	return moves
}

// sortMovesByTime sorts moves by their CreatedAt timestamp.
func sortMovesByTime(moves []repoMoveRecord) {
	for i := 1; i < len(moves); i++ {
		for j := i; j > 0 && moves[j].CreatedAt.Before(moves[j-1].CreatedAt); j-- {
			moves[j], moves[j-1] = moves[j-1], moves[j]
		}
	}
}
