package chess

import (
	"fmt"
	"os"
	"testing"

	"github.com/notnil/chess"
)

// PERFT — the bit-exact rules oracle.
//
// WHY THIS FILE EXISTS: this repository is a candidate workload for an
// autonomous agent pipeline that merges its own pull requests. Every other
// gate here is a judgement call some reviewer (human or model) has to make.
// Perft is not. A perft count is a single integer that has been independently
// verified by every serious chess engine for thirty years, and it is wrong the
// instant move generation, castling rights, en passant, or promotion is
// wrong. There is no way to make a broken engine produce the right node count,
// and no way to argue with the result.
//
// The counts below are the canonical Chess Programming Wiki positions. They
// are NOT derived from this codebase and must never be "corrected" to match
// observed output. If a count disagrees, the code is wrong or the dependency
// changed underneath us — that is the entire point of the gate. See
// docs/ORACLES.md for the standing rule.
//
// WHAT THIS ACTUALLY GUARDS. Move generation itself lives in
// github.com/notnil/chess, not here, so on its face this tests a dependency.
// That is deliberate and it is the cheap half. A dependency bump is exactly
// the kind of change an autonomous pipeline makes confidently and cannot
// evaluate, and `go get -u` silently altering castling or en passant is a
// class of breakage nothing else in this suite would catch. The expensive
// half is TestEngineAcceptsExactlyTheLegalMoves below, which checks OUR
// wrapper — parseSquare, ParsePromotion, and the from/to/promo matching loop
// in MakeMove — against that same ground truth.

// perftPosition is one canonical position and its verified node counts,
// indexed by depth (counts[0] is depth 1).
type perftPosition struct {
	name   string
	fen    string
	counts []uint64
}

// The six standard perft positions. Counts are from the Chess Programming
// Wiki and are treated here as external ground truth.
var perftPositions = []perftPosition{
	{
		name:   "startpos",
		fen:    "rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR w KQkq - 0 1",
		counts: []uint64{20, 400, 8902, 197281, 4865609},
	},
	{
		// Kiwipete. Dense with castling, pins, and en passant opportunities;
		// the single most productive position for catching move-gen bugs.
		name:   "kiwipete",
		fen:    "r3k2r/p1ppqpb1/bn2pnp1/3PN3/1p2P3/2N2Q1p/PPPBBPPP/R3K2R w KQkq - 0 1",
		counts: []uint64{48, 2039, 97862, 4085603},
	},
	{
		// Sparse endgame; exercises en passant discovered check, which is the
		// single most commonly mis-implemented rule in chess.
		name:   "position3",
		fen:    "8/2p5/3p4/KP5r/1R3p1k/8/4P1P1/8 w - - 0 1",
		counts: []uint64{14, 191, 2812, 43238, 674624},
	},
	{
		// Promotion-heavy, including promotion with capture and underpromotion.
		name:   "position4",
		fen:    "r3k2r/Pppp1ppp/1b3nbN/nP6/BBP1P3/q4N2/Pp1P2PP/R2Q1RK1 w kq - 0 1",
		counts: []uint64{6, 264, 9467, 422333},
	},
	{
		name:   "position5",
		fen:    "rnbq1k1r/pp1Pbppp/2p5/8/2B5/8/PPP1NnPP/RNBQK2R w KQ - 1 8",
		counts: []uint64{44, 1486, 62379, 2103487},
	},
	{
		name:   "position6",
		fen:    "r4rk1/1pp1qppp/p1np1n2/2b1p1B1/2B1P1b1/P1NP1N2/1PP1QPPP/R4RK1 w - - 0 10",
		counts: []uint64{46, 2079, 89890, 3894594},
	},
}

// perft counts leaf nodes of the legal move tree to the given depth.
func perft(pos *chess.Position, depth int) uint64 {
	if depth == 0 {
		return 1
	}
	moves := pos.ValidMoves()
	if depth == 1 {
		return uint64(len(moves))
	}
	var nodes uint64
	for _, m := range moves {
		nodes += perft(pos.Update(m), depth-1)
	}
	return nodes
}

func positionFromFEN(t *testing.T, fen string) *chess.Position {
	t.Helper()
	fenFunc, err := chess.FEN(fen)
	if err != nil {
		t.Fatalf("test corpus holds an invalid FEN %q: %v", fen, err)
	}
	return chess.NewGame(fenFunc).Position()
}

// maxDepthFor bounds the search so the default `go test ./...` stays fast
// enough to be a pre-commit gate. -short trims further; the full corpus runs
// when depth is unbounded via the long-running gate in CI.
func maxDepthFor(p perftPosition, short bool) int {
	limit := len(p.counts)
	if short {
		if limit > 2 {
			limit = 2
		}
		return limit
	}
	// Depth 4+ on the dense positions is seconds, not milliseconds. Keep the
	// default suite under a second per position; TestPerftDeep covers the rest.
	if limit > 3 {
		limit = 3
	}
	return limit
}

// deepPerftEnabled gates the multi-second full-depth corpus behind an
// explicit opt-in so the default suite stays usable as a fast local gate.
func deepPerftEnabled() bool {
	return os.Getenv("ATCHESS_DEEP_PERFT") == "1"
}

func TestPerft(t *testing.T) {
	for _, p := range perftPositions {
		t.Run(p.name, func(t *testing.T) {
			pos := positionFromFEN(t, p.fen)
			depth := maxDepthFor(p, testing.Short())
			for d := 1; d <= depth; d++ {
				want := p.counts[d-1]
				got := perft(pos, d)
				if got != want {
					t.Fatalf("perft(%s, depth %d) = %d, want %d\n"+
						"FEN: %s\n"+
						"This count is external ground truth. Do NOT update it to "+
						"match observed output — move generation, castling rights, "+
						"en passant, or promotion has regressed, or the notnil/chess "+
						"dependency changed. See docs/ORACLES.md.",
						p.name, d, got, want, p.fen)
				}
			}
		})
	}
}

// TestPerftDeep runs the full verified depth for every position. It is slow
// (tens of seconds) and is skipped in -short mode; CI runs it as a required
// gate on any change touching internal/chess or go.mod.
func TestPerftDeep(t *testing.T) {
	if testing.Short() {
		t.Skip("deep perft skipped in -short mode")
	}
	if !deepPerftEnabled() {
		t.Skip("set ATCHESS_DEEP_PERFT=1 to run the full perft corpus")
	}
	for _, p := range perftPositions {
		t.Run(p.name, func(t *testing.T) {
			pos := positionFromFEN(t, p.fen)
			for d := 1; d <= len(p.counts); d++ {
				if got, want := perft(pos, d), p.counts[d-1]; got != want {
					t.Fatalf("perft(%s, depth %d) = %d, want %d (FEN: %s)",
						p.name, d, got, want, p.fen)
				}
			}
		})
	}
}

// TestEngineAcceptsExactlyTheLegalMoves is the half of this file that tests
// OUR code rather than the dependency.
//
// For every canonical position, it asks the ground-truth move generator for
// the full legal move list, then drives each move through Engine.MakeMove
// using the same string coordinates the HTTP API accepts. Every legal move
// must be accepted; the wrapper must not silently drop or mangle any of them.
// This is where a parseSquare off-by-one, a ParsePromotion gap, or a bad
// from/to/promo comparison in MakeMove shows up — none of which perft alone
// would notice, because perft never goes through the wrapper.
func TestEngineAcceptsExactlyTheLegalMoves(t *testing.T) {
	for _, p := range perftPositions {
		t.Run(p.name, func(t *testing.T) {
			truth := positionFromFEN(t, p.fen)
			legal := truth.ValidMoves()
			if len(legal) == 0 {
				t.Fatalf("corpus position %s has no legal moves", p.name)
			}

			for _, m := range legal {
				from := m.S1().String()
				to := m.S2().String()
				promo := m.Promo()

				// Fresh engine per move: MakeMove mutates game state.
				e, err := NewEngineFromFEN(p.fen)
				if err != nil {
					t.Fatalf("NewEngineFromFEN(%s): %v", p.fen, err)
				}
				res, err := e.MakeMove(from, to, promo)
				if err != nil {
					t.Errorf("MakeMove(%s, %s, promo=%v) rejected a LEGAL move: %v\n"+
						"position %s (%s)\n"+
						"The engine wrapper must accept every move the rules allow. "+
						"Suspect parseSquare, ParsePromotion, or the move-matching "+
						"loop in MakeMove.",
						from, to, promo, err, p.name, p.fen)
					continue
				}
				if res == nil {
					t.Errorf("MakeMove(%s, %s) returned nil result with nil error", from, to)
					continue
				}
				// The resulting FEN must match what the ground-truth engine
				// produces for the same move. Catches a wrapper that accepts
				// the move but applies a different one.
				wantFEN := truth.Update(m).String()
				if res.FEN != wantFEN {
					t.Errorf("MakeMove(%s, %s) produced FEN %q, ground truth says %q\n"+
						"position %s — the wrapper applied a different move than the one requested",
						from, to, res.FEN, wantFEN, p.name)
				}
			}
		})
	}
}

// TestEngineRejectsIllegalMoves walks every from/to square pair that is NOT a
// legal move in the position and asserts the wrapper refuses it. An engine
// that accepts an illegal move in a federated game is a forgery vector: the
// move is written to a PDS record and every peer replays it.
func TestEngineRejectsIllegalMoves(t *testing.T) {
	// One dense position is enough; 64x64 pairs per position is 4096 engine
	// constructions and the cost adds up.
	const fen = "r3k2r/p1ppqpb1/bn2pnp1/3PN3/1p2P3/2N2Q1p/PPPBBPPP/R3K2R w KQkq - 0 1"

	truth := positionFromFEN(t, fen)
	legal := make(map[string]bool)
	for _, m := range truth.ValidMoves() {
		// Key on from/to only: any promotion piece for a legal from/to is
		// checked by the accept test above.
		legal[m.S1().String()+m.S2().String()] = true
	}

	files := "abcdefgh"
	ranks := "12345678"
	var checked int
	for _, ff := range files {
		for _, fr := range ranks {
			from := string(ff) + string(fr)
			for _, tf := range files {
				for _, tr := range ranks {
					to := string(tf) + string(tr)
					if legal[from+to] {
						continue
					}
					e, err := NewEngineFromFEN(fen)
					if err != nil {
						t.Fatalf("NewEngineFromFEN: %v", err)
					}
					if _, err := e.MakeMove(from, to, chess.NoPieceType); err == nil {
						t.Errorf("MakeMove(%s, %s) ACCEPTED an illegal move in %s\n"+
							"An accepted illegal move is a forgery vector: it is written "+
							"to the player's PDS record and replayed by every peer.",
							from, to, fen)
					}
					checked++
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("illegal-move sweep checked nothing; the gate is vacuous")
	}
	t.Logf("rejected %d illegal from/to pairs", checked)
}

// TestMalformedSquareNotationIsRejected pins the input-validation boundary of
// parseSquare. It uses byte arithmetic on untrusted strings that arrive
// straight from the HTTP API, so its underflow behaviour is load-bearing.
func TestMalformedSquareNotationIsRejected(t *testing.T) {
	malformed := []string{
		"", "a", "a1b", "11", "aa", "i1", "a9", "a0", "A1", "H8", " a1", "a1 ",
		"z9", "--", "\x00\x01", "à1",
	}
	for _, bad := range malformed {
		t.Run(fmt.Sprintf("%q", bad), func(t *testing.T) {
			e := NewEngine()
			if _, err := e.MakeMove(bad, "e4", chess.NoPieceType); err == nil {
				t.Errorf("MakeMove accepted malformed from-square %q", bad)
			}
			e2 := NewEngine()
			if _, err := e2.MakeMove("e2", bad, chess.NoPieceType); err == nil {
				t.Errorf("MakeMove accepted malformed to-square %q", bad)
			}
		})
	}
}
