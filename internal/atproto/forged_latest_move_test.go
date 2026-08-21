package atproto

import (
	"context"
	"testing"
	"time"

	"github.com/justinabrahms/atchess/internal/chess"
)

// --- atchess-1c9.108: getLatestMoveForGame accepted a move record without
// corroborating its claimed "player" against the repo it was listed from
// (unlike getLastMove, fixed by atchess-1c9.104, and getResignationOutcome,
// which has done this all along), and trusted the record's self-reported
// checkmate/draw flags rather than deriving them from the FEN it already
// reads. Found by reviewer during atchess-1c9.101, which made the gap
// materially worse: a move-sourced terminal event now deterministically
// outranks a same-second resignation/draw-accept (terminalEventIsAfter), so
// a forged "checkmate: true" would reliably beat a genuine resignation
// instead of merely winning a coin flip. ---

// TestGetLatestMoveForGame_IgnoresForgedPlayerClaim mirrors
// TestGetLastMove_IgnoresForgedPlayerClaim (atchess-1c9.104) but exercises
// getLatestMoveForGame directly: a forged move record planted in WHITE's
// own repo claiming player: blackDID, with a fresher timestamp and a much
// higher ply than the one honest black move, must not be selected as the
// game's latest move.
func TestGetLatestMoveForGame_IgnoresForgedPlayerClaim(t *testing.T) {
	mock := newDeriveTestPDS(t)
	srv := mock.server()
	defer srv.Close()

	gameURI := mock.seedActiveGame(t, time.Now().Add(-time.Hour), nil)

	// The one genuine, honestly-attributed black move: lives in black's own
	// repo, self-reported player matches the repo it lives in.
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
	// moveRecordIsAfter's comparison and be selected as "the latest move"
	// if getLatestMoveForGame trusted the claimed player field instead of
	// the repo it was listed from.
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

	latest, err := client.getLatestMoveForGame(context.Background(), gameURI, whiteDID, blackDID)
	if err != nil {
		t.Fatalf("getLatestMoveForGame: unexpected error: %v", err)
	}
	if latest == nil {
		t.Fatalf("expected the honest black move to be returned, got nil (forged record wrongly excluded everything)")
	}
	if latest.FEN != fenAfterBlackNc6 {
		t.Errorf("expected the forged record (in white's own repo, claiming player=black) to be ignored and the honest black move used instead; got FEN=%q (forged record's FEN=%q)",
			latest.FEN, "rnbqkbnr/pppp1ppp/8/4p3/4P3/8/PPPP1PPP/RNBQKBNR w KQkq - 0 20")
	}
}

// TestGetLatestMoveForGame_HonestRecordsStillSelected is the positive
// control for TestGetLatestMoveForGame_IgnoresForgedPlayerClaim: an honest
// record (self-reported player matches the repo it was listed from) must
// still be found and selected as normal -- the corroboration check must not
// accidentally reject legitimate moves from EITHER player's repo. This is
// the regression that matters most: an off-by-one comparing against the
// wrong repo would drop every legitimate move and make GetGame return an
// empty board.
func TestGetLatestMoveForGame_HonestRecordsStillSelected(t *testing.T) {
	t.Run("latest honest move is white's", func(t *testing.T) {
		mock := newDeriveTestPDS(t)
		srv := mock.server()
		defer srv.Close()

		gameURI := mock.seedActiveGame(t, time.Now().Add(-time.Hour), nil)

		mock.seed(blackDID, "app.atchess.move", "black-move", map[string]interface{}{
			"$type":     "app.atchess.move",
			"createdAt": time.Now().Add(-10 * time.Minute).Format(time.RFC3339),
			"game":      map[string]interface{}{"uri": gameURI},
			"player":    blackDID,
			"from":      "e7",
			"to":        "e5",
			"san":       "e5",
			"fen":       "rnbqkbnr/pppp1ppp/8/4p3/4P3/8/PPPP1PPP/RNBQKBNR w KQkq - 0 2",
		})
		const whiteMoveFEN = "rnbqkbnr/pppp1ppp/8/4p3/4P3/5N2/PPPP1PPP/RNBQKB1R b KQkq - 1 2"
		mock.seed(whiteDID, "app.atchess.move", "white-move", map[string]interface{}{
			"$type":     "app.atchess.move",
			"createdAt": time.Now().Add(-5 * time.Minute).Format(time.RFC3339),
			"game":      map[string]interface{}{"uri": gameURI},
			"player":    whiteDID,
			"from":      "g1",
			"to":        "f3",
			"san":       "Nf3",
			"fen":       whiteMoveFEN,
		})

		client := newDeriveTestClient(t, mock)
		latest, err := client.getLatestMoveForGame(context.Background(), gameURI, whiteDID, blackDID)
		if err != nil {
			t.Fatalf("getLatestMoveForGame: unexpected error: %v", err)
		}
		if latest == nil {
			t.Fatalf("expected white's honest move to be found, got nil")
		}
		if latest.FEN != whiteMoveFEN {
			t.Errorf("expected white's honest move to be selected as latest, got FEN=%q", latest.FEN)
		}
	})

	t.Run("latest honest move is black's", func(t *testing.T) {
		mock := newDeriveTestPDS(t)
		srv := mock.server()
		defer srv.Close()

		gameURI := mock.seedActiveGame(t, time.Now().Add(-time.Hour), nil)

		mock.seed(blackDID, "app.atchess.move", "black-move", map[string]interface{}{
			"$type":     "app.atchess.move",
			"createdAt": time.Now().Add(-5 * time.Minute).Format(time.RFC3339),
			"game":      map[string]interface{}{"uri": gameURI},
			"player":    blackDID,
			"from":      "b8",
			"to":        "c6",
			"san":       "Nc6",
			"fen":       fenAfterBlackNc6,
		})

		client := newDeriveTestClient(t, mock)
		latest, err := client.getLatestMoveForGame(context.Background(), gameURI, whiteDID, blackDID)
		if err != nil {
			t.Fatalf("getLatestMoveForGame: unexpected error: %v", err)
		}
		if latest == nil {
			t.Fatalf("expected black's honest move to be found, got nil")
		}
		if latest.FEN != fenAfterBlackNc6 {
			t.Errorf("expected black's honest move to be selected, got FEN=%q", latest.FEN)
		}
	})
}

// TestGetGame_ForgedCheckmateFlag_DisagreesWithFEN_Rejected proves a move
// record's self-reported checkmate:true is no longer trusted on its own: the
// FEN attached to this record is an ordinary, non-terminal position (after
// 1.e4), so GetGame must derive checkmate=false from that FEN via
// deriveTerminalFlagsFromFEN and leave the game active, regardless of what
// the record's own "checkmate" boolean claims.
func TestGetGame_ForgedCheckmateFlag_DisagreesWithFEN_Rejected(t *testing.T) {
	mock := newDeriveTestPDS(t)
	srv := mock.server()
	defer srv.Close()

	gameURI := mock.seedActiveGame(t, time.Now().Add(-time.Hour), nil)

	mock.seed(whiteDID, "app.atchess.move", "forged-mate-claim", map[string]interface{}{
		"$type":     "app.atchess.move",
		"createdAt": time.Now().Format(time.RFC3339),
		"game":      map[string]interface{}{"uri": gameURI},
		"player":    whiteDID,
		"from":      "e2",
		"to":        "e4",
		"san":       "e4",
		"fen":       "rnbqkbnr/pppppppp/8/8/4P3/8/PPPP1PPP/RNBQKBNR b KQkq e3 0 1", // ordinary, non-terminal position after 1.e4
		"checkmate": true,                                                          // FORGED: this FEN is not a checkmate position.
	})

	client := newDeriveTestClient(t, mock)
	game, err := client.GetGame(context.Background(), gameURI)
	if err != nil {
		t.Fatalf("GetGame: %v", err)
	}
	if game.Status != chess.StatusActive {
		t.Errorf("expected active (forged checkmate flag disagrees with its own FEN and must be ignored), got %q", game.Status)
	}
}
