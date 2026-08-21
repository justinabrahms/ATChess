package atproto

import (
	"context"
	"testing"
	"time"
)

// --- atchess-1c9.104: getLastMove trusted a record's self-reported
// "player" field rather than the repo it was actually listed from. ---
//
// getResignationOutcome already corroborates authorship against the
// hosting repo (client.go:~1547): a resignation record found in
// playerDID's own repo may only assert that playerDID resigned, and a
// mismatch is ignored (logged, continue). getLastMove had no equivalent
// check, so a player whose violation clock is running could plant a move
// record in their OWN repo claiming player: <opponent DID>, with a fresh
// timestamp and a high ply, and getLastMove -- looking for the opponent's
// latest move -- would accept it, resetting the violation clock and
// letting them stall indefinitely.
//
// TestGetLastMove_IgnoresForgedPlayerClaim plants exactly that forged
// record in white's own repo (claiming player: blackDID, a much later
// CreatedAt, and a much higher ply than any genuine black move) and
// asserts getLastMove(ctx, gameURI, whiteDID) -- excluding white, looking
// for black's latest move -- ignores it and falls back to the one
// genuine, honestly-attributed black move instead.
func TestGetLastMove_IgnoresForgedPlayerClaim(t *testing.T) {
	mock := newDeriveTestPDS(t)
	srv := mock.server()
	defer srv.Close()

	gameURI := mock.seedActiveGame(t, time.Now().Add(-time.Hour), map[string]interface{}{
		"type":        "correspondence",
		"daysPerMove": float64(3),
	})

	// The one genuine, honestly-attributed black move: lives in black's
	// own repo, self-reported player matches the repo it lives in.
	honestMoveAt := time.Now().Add(-30 * time.Minute)
	mock.seed(blackDID, "app.atchess.move", "honest-move", map[string]interface{}{
		"$type":     "app.atchess.move",
		"createdAt": honestMoveAt.Format(time.RFC3339),
		"game":      map[string]interface{}{"uri": gameURI},
		"player":    blackDID,
		"from":      "b8",
		"to":        "c6",
		"san":       "Nc6",
		"fen":       fenAfterBlackNc6, // ply 2
	})

	// The forged move: lives in WHITE's own repo (repo == whiteDID), but
	// self-reports player: blackDID, with a fresher timestamp and a much
	// higher ply than the honest move -- exactly the shape that would win
	// moveRecordIsAfter's comparison and be selected as "black's latest
	// move" if getLastMove trusted the claimed player field instead of the
	// repo it was listed from.
	forgedMoveAt := time.Now()
	mock.seed(whiteDID, "app.atchess.move", "forged-move", map[string]interface{}{
		"$type":     "app.atchess.move",
		"createdAt": forgedMoveAt.Format(time.RFC3339),
		"game":      map[string]interface{}{"uri": gameURI},
		"player":    blackDID, // FORGED: claims to be black's move, but this record lives in white's own repo.
		"from":      "e7",
		"to":        "e5",
		"san":       "e5",
		"fen":       "rnbqkbnr/pppp1ppp/8/4p3/4P3/8/PPPP1PPP/RNBQKBNR w KQkq - 0 20", // high ply, well past the honest move.
	})

	client := newDeriveTestClient(t, mock)

	lastMove, err := client.getLastMove(context.Background(), gameURI, whiteDID)
	if err != nil {
		t.Fatalf("getLastMove: unexpected error: %v", err)
	}
	if lastMove == nil {
		t.Fatalf("expected the honest black move to be returned, got nil (forged record wrongly excluded everything)")
	}
	if lastMove.FEN != fenAfterBlackNc6 {
		t.Errorf("expected the forged record (in white's own repo, claiming player=black) to be ignored and the honest black move used instead; got FEN=%q (forged record's FEN=%q)",
			lastMove.FEN, "rnbqkbnr/pppp1ppp/8/4p3/4P3/8/PPPP1PPP/RNBQKBNR w KQkq - 0 20")
	}
}

// TestGetLastMove_HonestRecordStillSelected is the positive control for
// TestGetLastMove_IgnoresForgedPlayerClaim: an honest record (self-reported
// player matches the repo it was listed from) must still be found and
// selected as normal -- the corroboration check must not accidentally
// reject legitimate moves from either player's repo.
func TestGetLastMove_HonestRecordStillSelected(t *testing.T) {
	mock := newDeriveTestPDS(t)
	srv := mock.server()
	defer srv.Close()

	gameURI := mock.seedActiveGame(t, time.Now().Add(-time.Hour), map[string]interface{}{
		"type":        "correspondence",
		"daysPerMove": float64(3),
	})

	moveAt := time.Now().Add(-5 * time.Minute)
	mock.seed(blackDID, "app.atchess.move", "honest-move", map[string]interface{}{
		"$type":     "app.atchess.move",
		"createdAt": moveAt.Format(time.RFC3339),
		"game":      map[string]interface{}{"uri": gameURI},
		"player":    blackDID,
		"from":      "b8",
		"to":        "c6",
		"san":       "Nc6",
		"fen":       fenAfterBlackNc6,
	})

	client := newDeriveTestClient(t, mock)

	lastMove, err := client.getLastMove(context.Background(), gameURI, whiteDID)
	if err != nil {
		t.Fatalf("getLastMove: unexpected error: %v", err)
	}
	if lastMove == nil {
		t.Fatalf("expected the honest black move to be found, got nil")
	}
	if lastMove.Player != blackDID {
		t.Errorf("expected Player=%q, got %q", blackDID, lastMove.Player)
	}
	if lastMove.FEN != fenAfterBlackNc6 {
		t.Errorf("expected FEN=%q, got %q", fenAfterBlackNc6, lastMove.FEN)
	}
}
