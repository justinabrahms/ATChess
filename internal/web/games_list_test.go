package web

import (
	"testing"

	"github.com/justinabrahms/atchess/internal/chess"
)

// turnOf decides whether the list says "your move". Getting it backwards is the
// kind of bug that makes a player wait for an opponent who is waiting for them.
func TestTurnOfReadsTheActiveColour(t *testing.T) {
	cases := map[string]string{
		"rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR w KQkq - 0 1":    "white",
		"rnbqkbnr/pppppppp/8/8/4P3/8/PPPP1PPP/RNBQKBNR b KQkq e3 0 1": "black",
		"8/8/8/8/8/8/8/8 b - - 0 1":                                   "black",
		"":                                                            "white",
		"nospaces":                                                    "white",
	}
	for fen, want := range cases {
		if got := turnOf(fen); got != want {
			t.Errorf("turnOf(%q) = %q, want %q", fen, got, want)
		}
	}
}

// The exact disagreement reported on 2026-08-30: the sidebar said
// "your move · black" while the game view said "Opponent's turn".
//
// Both FENs below are real, taken from the same live game at the same moment.
// The stored record was a move behind because a game record can only be updated
// by the repo that holds it, so the opponent's move never reaches it.
func TestListingDoesNotClaimATurnFromAStaleRecord(t *testing.T) {
	const me = "did:plc:myb4mf6rlx7eymfnldeitxwb"
	const them = "did:plc:3khyur5ae4znghzoo6exl6vu"

	staleFEN := "rnbqkbnr/pppppppp/8/8/3P4/8/PPP1PPPP/RNBQKBNR b KQkq d3 0 1"
	liveFEN := "rnbqkbnr/ppp1pppp/8/3p4/3P4/8/PPP1PPPP/RNBQKBNR w KQkq d6 0 2"

	// The two disagree about whose move it is. That is the whole bug.
	if turnOf(staleFEN) == turnOf(liveFEN) {
		t.Fatal("the fixtures no longer capture the divergence this test exists for")
	}

	game := func(fen string) *chess.Game {
		return &chess.Game{ID: "at://x/app.atchess.game/1", White: them, Black: me,
			Status: chess.StatusActive, FEN: fen}
	}

	// Derived: it is white's move, and we are black, so it is NOT our turn.
	if e := buildEntry(game(liveFEN), me, false); e.YourTurn {
		t.Error("claimed it was our turn from the derived position when white is to move")
	}

	// Stale: the record says black to move, which would read as "your move".
	// The row must decline to say, rather than contradict the game view.
	if e := buildEntry(game(staleFEN), me, true); e.YourTurn {
		t.Error("claimed a turn from a stale record — this is exactly the reported bug: " +
			"the list said \"your move · black\" while the game view said \"Opponent's turn\"")
	}

	// Colour and opponent must still be right on a stale row; only the turn
	// is unknowable.
	e := buildEntry(game(staleFEN), me, true)
	if e.YourColor != "black" || e.Opponent != them || !e.Stale {
		t.Errorf("stale row lost its other fields: color=%q opponent=%q stale=%v", e.YourColor, e.Opponent, e.Stale)
	}
}
