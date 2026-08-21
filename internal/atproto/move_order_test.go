package atproto

import (
	"context"
	"testing"
	"time"

	"github.com/justinabrahms/atchess/internal/chess"
)

// --- atchess-1c9.100: cross-repo move-record ordering ---
//
// moveIsAfter used to break a same-second CreatedAt tie by comparing TID
// rkey strings across repos. AT Protocol TIDs are only monotonic PER REPO
// (their low-order clock-identifier bits are a random per-process
// tiebreak, not a synchronized counter), so that comparison carried no
// chronological guarantee whatsoever once the two records came from
// different PDS processes -- and RFC3339 CreatedAt is only second-
// resolution, so cross-repo ties are ordinary, not exotic. This is a
// CONSTRUCTED regression test, not a timing test: it builds the exact
// failure mode directly (two move records, identical CreatedAt, TIDs whose
// lexicographic order is the OPPOSITE of their true chess order) rather
// than relying on a race to sometimes reproduce it.

// --- plyFromFEN unit coverage ---

func TestPlyFromFEN(t *testing.T) {
	cases := []struct {
		name    string
		fen     string
		wantPly int
		wantOK  bool
	}{
		{
			name:    "starting position, white to move",
			fen:     "rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR w KQkq - 0 1",
			wantPly: 0,
			wantOK:  true,
		},
		{
			name:    "after white's 1st move (1. e4), black to move",
			fen:     "rnbqkbnr/pppppppp/8/8/4P3/8/PPPP1PPP/RNBQKBNR b KQkq e3 0 1",
			wantPly: 1,
			wantOK:  true,
		},
		{
			name:    "after black's 1st move, white to move, fullmove now 2",
			fen:     "rnbqkbnr/pppp1ppp/8/4p3/4P3/8/PPPP1PPP/RNBQKBNR w KQkq e6 0 2",
			wantPly: 2,
			wantOK:  true,
		},
		{
			name:    "after white's 3rd move (e.g. Qh5), black to move, fullmove 3",
			fen:     "rnb1kbnr/pppp1ppp/2n5/4p2Q/4P3/8/PPPP1PPP/RNB1KBNR b KQkq - 3 3",
			wantPly: 5,
			wantOK:  true,
		},
		{
			name:    "after black's 4th move (e.g. Ke7), white to move, fullmove 5",
			fen:     "rnbqk1nr/ppppbppp/8/4p2Q/4P3/8/PPPP1PPP/RNB1K1NR w KQ - 5 5",
			wantPly: 8,
			wantOK:  true,
		},
		{name: "too few fields", fen: "rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR w KQkq", wantOK: false},
		{name: "bad active color", fen: "rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR x KQkq - 0 1", wantOK: false},
		{name: "non-numeric fullmove", fen: "rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR w KQkq - 0 many", wantOK: false},
		{name: "zero fullmove", fen: "rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR w KQkq - 0 0", wantOK: false},
		{name: "empty string", fen: "", wantOK: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ply, ok := plyFromFEN(tc.fen)
			if ok != tc.wantOK {
				t.Fatalf("plyFromFEN(%q) ok = %v, want %v", tc.fen, ok, tc.wantOK)
			}
			if ok && ply != tc.wantPly {
				t.Errorf("plyFromFEN(%q) = %d, want %d", tc.fen, ply, tc.wantPly)
			}
		})
	}
}

// --- moveRecordIsAfter: the constructed regression, using the real rkeys
// from atchess-1c9.100's measured evidence ---

// These are alice's Qh5 (move 3, ply 5) and bob's Ke7 (move 4, played
// LATER, ply 8), exactly as pulled from the two players' PDSes in the
// bead's report. Lexicographically "...t76jn2y" < "...tb22726", i.e. the
// OLDER move's rkey sorts AFTER the newer move's rkey -- the reverse of
// their true chess order. Any fix that still falls through to rkey
// comparison on a tie would get this backwards.
const (
	qh5FEN  = "rnb1kbnr/pppp1ppp/2n5/4p2Q/4P3/8/PPPP1PPP/RNB1KBNR b KQkq - 3 3" // move 3 (white), ply 5
	qh5Rkey = "3mtkgxtb22726"

	ke7FEN  = "rnbqk1nr/ppppbppp/8/4p2Q/4P3/8/PPPP1PPP/RNB1K1NR w KQ - 5 5" // move 4 (black), ply 8, played LATER
	ke7Rkey = "3mtkgxt76jn2y"
)

// TestMoveRecordIsAfter_SameSecondCrossRepoTie_OrdersByPly is the
// constructed test (not a timing test): both records share the exact same
// CreatedAt (a same-second tie, the ordinary case per atchess-1c9.100),
// and their TIDs sort the WRONG way round. moveRecordIsAfter must still
// report the later move (Ke7, ply 8) as after the earlier one (Qh5, ply
// 5), in both argument orders.
func TestMoveRecordIsAfter_SameSecondCrossRepoTie_OrdersByPly(t *testing.T) {
	sameSecond := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)

	if got := qh5Rkey > ke7Rkey; !got {
		t.Fatalf("test setup invariant broken: expected qh5Rkey > ke7Rkey lexicographically (the OPPOSITE of true chess order), got qh5Rkey=%q <= ke7Rkey=%q", qh5Rkey, ke7Rkey)
	}

	if !moveRecordIsAfter(ke7FEN, sameSecond, ke7Rkey, qh5FEN, sameSecond, qh5Rkey) {
		t.Errorf("moveRecordIsAfter: expected Ke7 (ply 8) to be AFTER Qh5 (ply 5) despite the same CreatedAt and a TID that sorts the wrong way")
	}
	if moveRecordIsAfter(qh5FEN, sameSecond, qh5Rkey, ke7FEN, sameSecond, ke7Rkey) {
		t.Errorf("moveRecordIsAfter: expected Qh5 (ply 5) to NOT be after Ke7 (ply 8)")
	}
}

// TestMoveRecordIsAfter_DifferentSeconds_StillOrdersByPly proves ply
// takes priority over CreatedAt too: an (impossible in practice, but
// worth pinning) later-CreatedAt-but-earlier-ply record must still lose,
// since ply is the domain-correct signal and CreatedAt is not trusted to
// override it.
func TestMoveRecordIsAfter_DifferentSeconds_StillOrdersByPly(t *testing.T) {
	earlier := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	later := earlier.Add(5 * time.Second)

	// Qh5 (ply 5) has a LATER CreatedAt than Ke7 (ply 8) here -- ply must
	// still win.
	if moveRecordIsAfter(qh5FEN, later, qh5Rkey, ke7FEN, earlier, ke7Rkey) {
		t.Errorf("moveRecordIsAfter: ply (domain-correct) must take priority over CreatedAt (attacker/clock influenced)")
	}
}

// TestMoveRecordIsAfter_UnparsableFEN_FallsBackToMoveIsAfter pins the
// defensive fallback: if either FEN cannot be parsed, ordering falls back
// to moveIsAfter's createdAt+TID tiebreak, exactly as before this fix,
// rather than panicking or silently picking an arbitrary side.
func TestMoveRecordIsAfter_UnparsableFEN_FallsBackToMoveIsAfter(t *testing.T) {
	sameSecond := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	garbageFEN := "not a fen"

	want := moveIsAfter(sameSecond, "rkeyA", sameSecond, "rkeyB")
	got := moveRecordIsAfter(garbageFEN, sameSecond, "rkeyA", garbageFEN, sameSecond, "rkeyB")
	if got != want {
		t.Errorf("moveRecordIsAfter with unparsable FENs = %v, want fallback to moveIsAfter = %v", got, want)
	}
}

// --- End-to-end: GetGame must resolve the real bug via
// getLatestMoveForGame, not just the unit-level helper ---

// TestGetGame_SameSecondCrossRepoMoveTie_LaterMoveWins reproduces the
// atchess-1c9.100 bug end-to-end through GetGame: alice's Qh5 lives in
// whiteDID's repo, bob's LATER Ke7 lives in blackDID's repo, both share
// the same CreatedAt, and their rkeys sort the wrong way round (same
// constants as the unit-level test above, taken from the bead's own
// measured evidence). Before the fix this deterministically picked Qh5 as
// "latest" -- wrong, and permanently so, per the bead's 100+-retry
// evidence. After the fix, GetGame's FEN must be Ke7's.
func TestGetGame_SameSecondCrossRepoMoveTie_LaterMoveWins(t *testing.T) {
	mock := newDeriveTestPDS(t)
	srv := mock.server()
	defer srv.Close()

	gameURI := mock.seedActiveGame(t, time.Now().Add(-time.Hour), nil)

	sameSecond := time.Now().Truncate(time.Second)

	mock.seed(whiteDID, "app.atchess.move", qh5Rkey, map[string]interface{}{
		"$type":     "app.atchess.move",
		"createdAt": sameSecond.Format(time.RFC3339),
		"game":      map[string]interface{}{"uri": gameURI},
		"player":    whiteDID,
		"from":      "d1",
		"to":        "h5",
		"san":       "Qh5",
		"fen":       qh5FEN,
	})
	mock.seed(blackDID, "app.atchess.move", ke7Rkey, map[string]interface{}{
		"$type":     "app.atchess.move",
		"createdAt": sameSecond.Format(time.RFC3339),
		"game":      map[string]interface{}{"uri": gameURI},
		"player":    blackDID,
		"from":      "f8",
		"to":        "e7",
		"san":       "Ke7",
		"fen":       ke7FEN,
	})

	client := newDeriveTestClient(t, mock)
	game, err := client.GetGame(context.Background(), gameURI)
	if err != nil {
		t.Fatalf("GetGame: %v", err)
	}
	if game.FEN != ke7FEN {
		t.Errorf("expected GetGame to resolve the LATER move (Ke7, ply 8) as latest, got FEN %q (Qh5 is %q)", game.FEN, qh5FEN)
	}
}

// --- Pinning the remaining moveIsAfter call sites (getResignationOutcome,
// getTimeViolationOutcome, getDrawAcceptOutcome), which have no board
// position to derive ply from and so keep moveIsAfter's createdAt+TID
// tiebreak. These document/pin that the tiebreak is still fully
// deterministic for these types, per moveIsAfter's revised doc comment. ---

// TestGetResignationOutcome_SameSecondTie_DeterministicByRkey pins
// getResignationOutcome's (1434 in the original bead numbering) tiebreak:
// two resignations (one from each player, self-reported into their own
// repo -- both individually legitimate per-repo) in the same second
// resolve by rkey, deterministically.
func TestGetResignationOutcome_SameSecondTie_DeterministicByRkey(t *testing.T) {
	mock := newDeriveTestPDS(t)
	srv := mock.server()
	defer srv.Close()

	gameURI := mock.seedActiveGame(t, time.Now().Add(-time.Hour), nil)
	sameSecond := time.Now().Truncate(time.Second)

	mock.seed(whiteDID, "app.atchess.resignation", "3aaaaaaaaaaaa", map[string]interface{}{
		"$type":           "app.atchess.resignation",
		"createdAt":       sameSecond.Format(time.RFC3339),
		"game":            map[string]interface{}{"uri": gameURI},
		"resigningPlayer": whiteDID, // -> black_won
	})
	mock.seed(blackDID, "app.atchess.resignation", "3zzzzzzzzzzzz", map[string]interface{}{
		"$type":           "app.atchess.resignation",
		"createdAt":       sameSecond.Format(time.RFC3339),
		"game":            map[string]interface{}{"uri": gameURI},
		"resigningPlayer": blackDID, // -> white_won
	})

	client := newDeriveTestClient(t, mock)
	got, err := client.getResignationOutcome(context.Background(), gameURI, whiteDID, blackDID)
	if err != nil {
		t.Fatalf("getResignationOutcome: %v", err)
	}
	if got == nil {
		t.Fatalf("expected a non-nil terminalEvent")
	}
	// "3zzzzzzzzzzzz" > "3aaaaaaaaaaaa" lexicographically, so black's
	// resignation (white_won) wins the tie deterministically.
	if got.status != chess.StatusWhiteWon {
		t.Errorf("expected white_won (black's resignation rkey sorts after white's), got %q", got.status)
	}
}
