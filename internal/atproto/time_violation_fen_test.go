package atproto

import (
	"context"
	"errors"
	"testing"
	"time"
)

// --- CheckTimeViolation/GetTimeRemaining raw-cached-FEN regression tests
// (atchess-1c9.103) ---
//
// RecordMove (client.go:820) only refreshes the app.atchess.game record's
// cached "fen" field when the mover owns that record (repo == c.did) --
// deliberately, since the record lives in exactly one player's repo and
// only its owner can write it. So in a federated game, every move by the
// non-owner leaves the cached fen one ply stale. GetGame already
// compensates for this by deriving FEN from the latest app.atchess.move
// record (getLatestMoveForGame) rather than trusting the cache.
// CheckTimeViolation and GetTimeRemaining used to read gameValue["fen"]
// raw instead, which named the WRONG player as the one to move right
// after the non-owner moved, which in turn made getLastMove exclude that
// fresh move and pick an OLDER one -- making a false time-violation MORE
// likely, not less, and letting ClaimTimeVictory durably record a time
// victory against a player who had just moved.

// fenStart is the standard starting position, white to move.
const fenStart = "rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR w KQkq - 0 1"

// fenAfterWhiteE4 is the position after 1. e4 -- black to move, ply 1.
const fenAfterWhiteE4 = "rnbqkbnr/pppppppp/8/8/4P3/8/PPPP1PPP/RNBQKBNR b KQkq - 0 1"

// fenAfterBlackNc6 is the position after 1. e4 Nc6 -- white to move, ply 2.
const fenAfterBlackNc6 = "r1bqkbnr/pppppppp/2n5/8/4P3/8/PPPP1PPP/RNBQKBNR w KQkq - 2 2"

// seedFederatedGameNonOwnerJustMoved seeds a correspondence game
// (daysPerMove=3) OWNED BY WHITE ("game1" lives in whiteDID's repo).
// White (the owner) made the only cache-refreshing move long ago, well
// past the 3-day deadline; the cached "fen" reflects exactly that state
// (black to move) because RecordMove only refreshes the cache when the
// mover owns the record. Black -- the NON-owner -- then just moved,
// FRESH (within the last few seconds), putting the real position back on
// white to move, but the cached "fen" was never touched (black doesn't
// own the record), so it still says "black to move".
func seedFederatedGameNonOwnerJustMoved(mock *deriveTestPDS) (gameURI string) {
	gameCreated := time.Now().Add(-30 * 24 * time.Hour)
	gameURI, _ = mock.seed(whiteDID, "app.atchess.game", "game1", map[string]interface{}{
		"$type":     "app.atchess.game",
		"createdAt": gameCreated.Format(time.RFC3339),
		"white":     whiteDID,
		"black":     blackDID,
		"status":    "active",
		"fen":       fenAfterWhiteE4, // STALE: black to move, per RecordMove's own-repo-only cache refresh.
		"pgn":       "1. e4",
		"timeControl": map[string]interface{}{
			"type":        "correspondence",
			"daysPerMove": float64(3),
		},
	})

	// White's move: 10 days ago, well past the 3-day deadline.
	whiteMoveAt := time.Now().Add(-10 * 24 * time.Hour)
	mock.seed(whiteDID, "app.atchess.move", "move1", map[string]interface{}{
		"$type":     "app.atchess.move",
		"createdAt": whiteMoveAt.Format(time.RFC3339),
		"game":      map[string]interface{}{"uri": gameURI},
		"player":    whiteDID,
		"from":      "e2",
		"to":        "e4",
		"san":       "e4",
		"fen":       fenAfterWhiteE4,
	})

	// Black's move: just now -- fresh, well within the 3-day deadline.
	// Black does NOT own the game record, so this never touches the
	// cached "fen" above.
	blackMoveAt := time.Now()
	mock.seed(blackDID, "app.atchess.move", "move2", map[string]interface{}{
		"$type":     "app.atchess.move",
		"createdAt": blackMoveAt.Format(time.RFC3339),
		"game":      map[string]interface{}{"uri": gameURI},
		"player":    blackDID,
		"from":      "b8",
		"to":        "c6",
		"san":       "Nc6",
		"fen":       fenAfterBlackNc6,
	})

	return gameURI
}

// TestCheckTimeViolation_NonOwnerJustMoved_NoViolation is the headline
// test for atchess-1c9.103: with the fix, CheckTimeViolation must derive
// the current FEN the way GetGame does (not trust the stale cached
// gameValue["fen"]), correctly conclude it is WHITE's turn (black just
// moved), and therefore find no time violation -- black's move is fresh,
// well within the 3-day deadline. Before the fix, this reports a
// violation against BLACK, the player who just moved.
func TestCheckTimeViolation_NonOwnerJustMoved_NoViolation(t *testing.T) {
	mock := newDeriveTestPDS(t)
	srv := mock.server()
	defer srv.Close()

	gameURI := seedFederatedGameNonOwnerJustMoved(mock)

	client := newDeriveTestClient(t, mock) // authenticates as whiteDID

	hasViolation, violation, err := client.CheckTimeViolation(context.Background(), gameURI)
	if err != nil {
		t.Fatalf("CheckTimeViolation: unexpected error: %v", err)
	}
	if hasViolation {
		t.Fatalf("expected no time violation (black just moved, well within the 3-day deadline), got hasViolation=true, violation=%+v", violation)
	}
	if violation != nil && violation.ViolatingPlayer != whiteDID {
		t.Errorf("if any attribution is produced it must name the correct side (white is the one who hasn't moved yet); got ViolatingPlayer=%q", violation.ViolatingPlayer)
	}
}

// TestGetLastMove_ExcludesCorrectPlayer_ConsidersFreshMove pins the
// compounding effect atchess-1c9.103 describes: once the current player
// to move is correctly derived as white (not the wrong, stale-cache
// answer of black), getLastMove(ctx, gameID, whiteDID) -- excluding
// white's OWN moves, as CheckTimeViolation calls it -- must return
// black's FRESH move (move2), not white's older one (move1). Before the
// fix, CheckTimeViolation asked getLastMove to exclude BLACK instead,
// which skipped black's fresh move entirely and returned white's older
// move -- making the elapsed interval longer and a false violation MORE
// likely.
func TestGetLastMove_ExcludesCorrectPlayer_ConsidersFreshMove(t *testing.T) {
	mock := newDeriveTestPDS(t)
	srv := mock.server()
	defer srv.Close()

	gameURI := seedFederatedGameNonOwnerJustMoved(mock)

	client := newDeriveTestClient(t, mock)

	// This is the exclude argument CheckTimeViolation now derives (the
	// correct current-player-to-move, white) once it stops reading the
	// stale cached FEN.
	lastMove, err := client.getLastMove(context.Background(), gameURI, whiteDID)
	if err != nil {
		t.Fatalf("getLastMove: unexpected error: %v", err)
	}
	if lastMove == nil {
		t.Fatalf("expected a last move (black's fresh move), got nil")
	}
	if lastMove.Player != blackDID {
		t.Errorf("expected the fresh move to be black's, got player=%q", lastMove.Player)
	}
	if lastMove.FEN != fenAfterBlackNc6 {
		t.Errorf("expected the fresh move's FEN (%q), got %q -- black's fresh move was not considered", fenAfterBlackNc6, lastMove.FEN)
	}

	// Negative control: excluding the WRONG player (black -- the old,
	// buggy attribution) skips black's fresh move and falls back to
	// white's much older one, which is exactly the compounding bug.
	wrongLastMove, err := client.getLastMove(context.Background(), gameURI, blackDID)
	if err != nil {
		t.Fatalf("getLastMove (wrong exclude): unexpected error: %v", err)
	}
	if wrongLastMove == nil {
		t.Fatalf("expected a last move even with the wrong exclude, got nil")
	}
	if wrongLastMove.Player != whiteDID {
		t.Errorf("expected excluding black to fall back to white's older move, got player=%q", wrongLastMove.Player)
	}
}

// TestCheckTimeViolation_NoMovesYet_UsesCachedFEN proves the no-moves-yet
// case is handled explicitly rather than erroring: with zero
// app.atchess.move records for the game, there is no latestMove to derive
// a FEN from, so the cached gameValue["fen"] (correct, since nobody has
// moved) must be used as-is. Here the game was created 10 days ago with a
// 3-day correspondence limit and white (the side to move per the cached
// starting-position FEN) never moved, so a violation against white is the
// correct, unsurprising result -- proving the derivation path was
// exercised (and didn't error or silently default) rather than merely
// not-crashing.
func TestCheckTimeViolation_NoMovesYet_UsesCachedFEN(t *testing.T) {
	mock := newDeriveTestPDS(t)
	srv := mock.server()
	defer srv.Close()

	gameCreated := time.Now().Add(-10 * 24 * time.Hour)
	gameURI := mock.seedActiveGame(t, gameCreated, map[string]interface{}{
		"type":        "correspondence",
		"daysPerMove": float64(3),
	})
	// seedActiveGame's cached fen is fenStart (white to move) and no move
	// records exist at all.

	client := newDeriveTestClient(t, mock)

	hasViolation, violation, err := client.CheckTimeViolation(context.Background(), gameURI)
	if err != nil {
		t.Fatalf("CheckTimeViolation: unexpected error: %v", err)
	}
	if !hasViolation {
		t.Fatalf("expected a time violation (white never moved, 10 days > 3-day limit, derived from the cached starting FEN)")
	}
	if violation.ViolatingPlayer != whiteDID {
		t.Errorf("expected ViolatingPlayer=%q (white, per the cached starting FEN), got %q", whiteDID, violation.ViolatingPlayer)
	}
}

// TestCheckTimeViolation_DerivationFailure_FailsClosed proves that when
// the FEN cannot be derived at all (here: black's PDS is unreachable, so
// getLatestMoveForGame -- reached via GetGame -- cannot read black's
// moves), CheckTimeViolation fails closed: a durable, game-ending write
// (ClaimTimeVictory gates on this) must not be decided from incomplete
// data. This is the SAME fail-closed path already proven for status
// (TestResignGame_FailsClosed_WhenDerivationIncomplete) -- reused here
// via currentGameStatusAndFEN, not a new one.
func TestCheckTimeViolation_DerivationFailure_FailsClosed(t *testing.T) {
	mock := newDeriveTestPDS(t)
	srv := mock.server()
	defer srv.Close()

	mock.setUnreachable(t, blackDID)

	gameURI := seedFederatedGameNonOwnerJustMoved(mock)

	client := newDeriveTestClient(t, mock)

	hasViolation, violation, err := client.CheckTimeViolation(context.Background(), gameURI)
	if err == nil {
		t.Fatalf("expected a non-nil error (black's PDS is unreachable), got nil with hasViolation=%v violation=%+v", hasViolation, violation)
	}
	if !errors.Is(err, ErrIncompleteDerivation) {
		t.Errorf("expected errors.Is(err, ErrIncompleteDerivation), got: %v", err)
	}
	if hasViolation {
		t.Errorf("must not report a violation when derivation is incomplete, got hasViolation=true")
	}
}

// TestGetTimeRemaining_NonOwnerJustMoved_ReflectsFreshMove mirrors the
// CheckTimeViolation headline test for GetTimeRemaining: it must derive
// the same corrected FEN and therefore compute time remaining for WHITE
// against black's FRESH move, not the stale cached "black to move" state.
func TestGetTimeRemaining_NonOwnerJustMoved_ReflectsFreshMove(t *testing.T) {
	mock := newDeriveTestPDS(t)
	srv := mock.server()
	defer srv.Close()

	gameURI := seedFederatedGameNonOwnerJustMoved(mock)

	client := newDeriveTestClient(t, mock)

	remaining, err := client.GetTimeRemaining(context.Background(), gameURI)
	if err != nil {
		t.Fatalf("GetTimeRemaining: unexpected error: %v", err)
	}
	// Measured against black's fresh move (a few seconds ago at most), not
	// white's 10-day-old move -- so remaining must be close to the full
	// 3-day budget, not ~zero/expired.
	threeDays := 3 * 24 * time.Hour
	if remaining < threeDays-time.Minute {
		t.Errorf("expected time remaining close to the full 3-day budget (measured from black's fresh move), got %v", remaining)
	}
}

// TestGetTimeRemaining_DerivationFailure_FailsClosed mirrors
// TestCheckTimeViolation_DerivationFailure_FailsClosed for GetTimeRemaining.
func TestGetTimeRemaining_DerivationFailure_FailsClosed(t *testing.T) {
	mock := newDeriveTestPDS(t)
	srv := mock.server()
	defer srv.Close()

	mock.setUnreachable(t, blackDID)

	gameURI := seedFederatedGameNonOwnerJustMoved(mock)

	client := newDeriveTestClient(t, mock)

	_, err := client.GetTimeRemaining(context.Background(), gameURI)
	if err == nil {
		t.Fatalf("expected a non-nil error (black's PDS is unreachable), got nil")
	}
	if !errors.Is(err, ErrIncompleteDerivation) {
		t.Errorf("expected errors.Is(err, ErrIncompleteDerivation), got: %v", err)
	}
}
