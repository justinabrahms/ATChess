package chess

import (
	"strings"
	"testing"

	"github.com/notnil/chess"
)

// GAME-OUTCOME ORACLE.
//
// Perft (perft_test.go) proves the legal move SET is right. It says nothing
// about what this package reports when a game ENDS, and that reporting is our
// code, not the dependency's:
//
//   - MoveResult.Check / Checkmate are derived by sniffing the last byte of
//     the SAN string ('+' / '#'). That is a real inference over a formatting
//     detail of an external encoder, and nothing else in the suite pins it.
//   - GetStatus maps chess.Outcome onto this project's GameStatus enum.
//   - MoveResult.Result is a concatenation that a federated peer reads back
//     out of a PDS record.
//
// A game that is over but reports GameOver=false is worse than a crash here:
// the record is written, replicated to both players' repositories, and every
// peer that reads it disagrees with every other peer about whether the game
// ended. These positions are the terminal states an autonomous change must
// never silently alter.

// outcomeCase is a position, one move to play from it, and the terminal state
// that move must produce.
type outcomeCase struct {
	name          string
	fen           string
	from, to      string
	promo         chess.PieceType
	wantCheck     bool
	wantCheckmate bool
	wantDraw      bool
	wantGameOver  bool
	wantStatus    GameStatus
}

var outcomeCases = []outcomeCase{
	{
		// Fool's mate: 1. f3 e5 2. g4 Qh4#
		name: "fools_mate_is_checkmate",
		fen:  "rnbqkbnr/pppp1ppp/8/4p3/6P1/5P2/PPPPP2P/RNBQKBNR b KQkq g3 0 2",
		from: "d8", to: "h4",
		wantCheck:     true,
		wantCheckmate: true,
		wantGameOver:  true,
		wantStatus:    StatusBlackWon,
	},
	{
		// Back-rank mate. Rook delivers mate along the eighth rank.
		name: "back_rank_mate",
		fen:  "6k1/5ppp/8/8/8/8/8/R5K1 w - - 0 1",
		from: "a1", to: "a8",
		wantCheck:     true,
		wantCheckmate: true,
		wantGameOver:  true,
		wantStatus:    StatusWhiteWon,
	},
	{
		// Check that is NOT mate: Ra1-a8+ and the black king walks to d7,
		// e7, or f7. Guards against a Checkmate flag that is really just a
		// Check flag wearing a hat.
		name: "check_that_is_not_mate",
		fen:  "4k3/8/8/8/8/8/8/R6K w - - 0 1",
		from: "a1", to: "a8",
		wantCheck:     true,
		wantCheckmate: false,
		wantGameOver:  false,
		wantStatus:    StatusActive,
	},
	{
		// Quiet move: no check, no mate, game continues. The negative control
		// for every flag above.
		name: "quiet_move_ends_nothing",
		fen:  "rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR w KQkq - 0 1",
		from: "e2", to: "e4",
		wantStatus: StatusActive,
	},
	{
		// Stalemate: Qb3-f7 leaves the black king on h8 with no legal move
		// and NOT in check (f7 and h8 share no line). Draw and game over,
		// but emphatically not a win — the case a naive "no legal moves
		// means checkmate" shortcut gets wrong.
		name: "stalemate_is_a_draw_not_a_win",
		fen:  "7k/8/6K1/8/8/1Q6/8/8 w - - 0 1",
		from: "b3", to: "f7",
		wantDraw:     true,
		wantGameOver: true,
		wantStatus:   StatusDraw,
	},
}

func TestMoveOutcomeFlags(t *testing.T) {
	for _, tc := range outcomeCases {
		t.Run(tc.name, func(t *testing.T) {
			e, err := NewEngineFromFEN(tc.fen)
			if err != nil {
				t.Fatalf("NewEngineFromFEN(%q): %v", tc.fen, err)
			}
			res, err := e.MakeMove(tc.from, tc.to, tc.promo)
			if err != nil {
				t.Fatalf("MakeMove(%s, %s): %v", tc.from, tc.to, err)
			}

			if res.Check != tc.wantCheck {
				t.Errorf("Check = %v, want %v (SAN %q)\n"+
					"Check/Checkmate are inferred from the last byte of SAN; an "+
					"encoder change or a rewrite of that inference breaks this.",
					res.Check, tc.wantCheck, res.SAN)
			}
			if res.Checkmate != tc.wantCheckmate {
				t.Errorf("Checkmate = %v, want %v (SAN %q)", res.Checkmate, tc.wantCheckmate, res.SAN)
			}
			if res.Draw != tc.wantDraw {
				t.Errorf("Draw = %v, want %v", res.Draw, tc.wantDraw)
			}
			if res.GameOver != tc.wantGameOver {
				t.Errorf("GameOver = %v, want %v\n"+
					"A finished game reported as unfinished is written to both "+
					"players' PDS records and every peer disagrees about the result.",
					res.GameOver, tc.wantGameOver)
			}
			if got := e.GetStatus(); got != tc.wantStatus {
				t.Errorf("GetStatus() = %v, want %v", got, tc.wantStatus)
			}
			// A checkmate must always also be a check and a game over. These
			// are structural: no position may violate them.
			if res.Checkmate && !res.Check {
				t.Error("invariant violated: Checkmate=true with Check=false")
			}
			if res.Checkmate && !res.GameOver {
				t.Error("invariant violated: Checkmate=true with GameOver=false")
			}
			if res.Checkmate && res.Draw {
				t.Error("invariant violated: a position is both Checkmate and Draw")
			}
		})
	}
}

// TestFENRoundTripsThroughEngine pins the serialization boundary. Every FEN
// this project stores in a PDS record must survive a load/read cycle byte for
// byte. A FEN that mutates in transit desynchronizes the two players' copies
// of the same game.
func TestFENRoundTripsThroughEngine(t *testing.T) {
	corpus := []string{
		"rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR w KQkq - 0 1",
		"r3k2r/p1ppqpb1/bn2pnp1/3PN3/1p2P3/2N2Q1p/PPPBBPPP/R3K2R w KQkq - 0 1",
		"8/2p5/3p4/KP5r/1R3p1k/8/4P1P1/8 w - - 0 1",
		"r3k2r/Pppp1ppp/1b3nbN/nP6/BBP1P3/q4N2/Pp1P2PP/R2Q1RK1 w kq - 0 1",
		"rnbq1k1r/pp1Pbppp/2p5/8/2B5/8/PPP1NnPP/RNBQK2R w KQ - 1 8",
		"4k3/8/8/8/8/8/8/4K2R w K - 0 1",
		"7k/5Q2/6K1/8/8/8/8/8 w - - 0 1",
	}
	for _, fen := range corpus {
		t.Run(fen, func(t *testing.T) {
			e, err := NewEngineFromFEN(fen)
			if err != nil {
				t.Fatalf("NewEngineFromFEN(%q): %v", fen, err)
			}
			if got := e.GetFEN(); got != fen {
				t.Errorf("FEN round-trip altered the position:\n  in:  %s\n  out: %s\n"+
					"A FEN that mutates in transit desynchronizes both players' "+
					"copies of the game.", fen, got)
			}
		})
	}
}

// TestPGNGrowsMonotonicallyAndReplays checks that the move history this
// project exports actually reconstructs the game. PGN is the durable record a
// spectator or a rebuilding peer reads; if it drifts from the live position,
// replay produces a different game than the one that was played.
func TestPGNGrowsMonotonicallyAndReplays(t *testing.T) {
	moves := []struct{ from, to string }{
		{"e2", "e4"}, {"e7", "e5"},
		{"g1", "f3"}, {"b8", "c6"},
		{"f1", "b5"}, {"a7", "a6"},
		{"b5", "a4"}, {"g8", "f6"},
		{"e1", "g1"}, // white castles kingside
	}

	e := NewEngine()
	prevLen := 0
	for i, m := range moves {
		if _, err := e.MakeMove(m.from, m.to, chess.NoPieceType); err != nil {
			t.Fatalf("move %d (%s%s): %v", i+1, m.from, m.to, err)
		}
		pgn := e.GetPGN()
		if len(pgn) <= prevLen {
			t.Fatalf("PGN did not grow after move %d (%s%s): %q\n"+
				"Move history is the durable record; a move that does not "+
				"appear in it is a move a rebuilding peer will never replay.",
				i+1, m.from, m.to, pgn)
		}
		prevLen = len(pgn)
	}

	// Castling must be recorded in standard notation, not as a king move.
	if pgn := e.GetPGN(); !strings.Contains(pgn, "O-O") {
		t.Errorf("PGN does not record kingside castling as O-O: %q", pgn)
	}

	// The final position must be reachable by replaying the recorded PGN.
	finalFEN := e.GetFEN()
	replayed, err := replayPGN(t, e.GetPGN())
	if err != nil {
		t.Fatalf("exported PGN does not parse: %v\nPGN: %q", err, e.GetPGN())
	}
	if replayed != finalFEN {
		t.Errorf("replaying the exported PGN produced a different position:\n"+
			"  live:     %s\n  replayed: %s\n"+
			"The exported history does not reconstruct the game that was played.",
			finalFEN, replayed)
	}
}

func replayPGN(t *testing.T, pgn string) (string, error) {
	t.Helper()
	pgnFunc, err := chess.PGN(strings.NewReader(pgn))
	if err != nil {
		return "", err
	}
	return chess.NewGame(pgnFunc).Position().String(), nil
}

// TestGameOverPositionRejectsFurtherMoves guards the terminal boundary. Once a
// game is over, the engine must not accept another move — an accepted move
// after checkmate is a record written to a finished game, which peers cannot
// reconcile.
func TestGameOverPositionRejectsFurtherMoves(t *testing.T) {
	// Fool's mate position, then deliver mate.
	e, err := NewEngineFromFEN("rnbqkbnr/pppp1ppp/8/4p3/6P1/5P2/PPPPP2P/RNBQKBNR b KQkq g3 0 2")
	if err != nil {
		t.Fatalf("NewEngineFromFEN: %v", err)
	}
	res, err := e.MakeMove("d8", "h4", chess.NoPieceType)
	if err != nil {
		t.Fatalf("mating move rejected: %v", err)
	}
	if !res.Checkmate {
		t.Fatalf("expected checkmate, got SAN %q", res.SAN)
	}

	// Any subsequent move must be refused.
	for _, m := range []struct{ from, to string }{
		{"e1", "f2"}, {"g1", "f3"}, {"a2", "a3"}, {"h4", "h5"},
	} {
		if _, err := e.MakeMove(m.from, m.to, chess.NoPieceType); err == nil {
			t.Errorf("MakeMove(%s, %s) accepted after checkmate\n"+
				"A move accepted on a finished game is written to a PDS record "+
				"that no peer can reconcile with the game's result.", m.from, m.to)
		}
	}
}
