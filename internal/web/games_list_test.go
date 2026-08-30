package web

import "testing"

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
