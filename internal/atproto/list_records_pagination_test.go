package atproto

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/justinabrahms/atchess/internal/chess"
)

// atchess-1c9.119: com.atproto.repo.listRecords caps every response at
// 100 records and getLatestMoveForGame (and seven sibling call sites)
// read exactly one page and never followed the response cursor. Once a
// player's app.atchess.move collection grew past 100 records -- easily
// reached by an active player across all their games, not just one --
// the scan silently went blind to every record past page one. For a game
// whose real moves happen to land past page one (which, as of
// atchess-1c9.113's deterministic hash rkeys, is NOT correlated with
// which moves are "recent" at all -- see listAllRecords's doc comment),
// the derived FEN was wrong and MakeMoveHandler's turn gate rejected a
// legal move as "It is not your turn".
//
// This file pins the fix (the shared listAllRecords pagination helper)
// with three tests:
//
//   - TestGetLatestMoveForGame_MoreThan100RecordsInRepo_HEADLINE: the
//     bug's own reproduction -- seed a repo with more than 100 move
//     records spread across several (unrelated, filler) games, then play
//     a real game in that SAME repo and confirm the real move is still
//     found and the derived FEN is correct. Run against pre-fix code
//     (single-page, no-cursor listRecords calls) this fails; see this
//     bead's report for the captured red output.
//   - TestGetChallengeDeclineOutcome_MatchOnPageTwo: a cheaper,
//     single-repo, single-page-boundary-crossing case for the same
//     defect in a DIFFERENT call site (getChallengeDeclineOutcome).
//   - TestListAllRecords_PageCapEnforced: the pagination helper's own
//     safety valve -- a repo that is NOT exhausted within
//     maxListRecordsPages pages must fail closed with an error, never
//     silently return a truncated result (which would just be today's
//     bug at a higher cliff).

// seedFillerMoves seeds n app.atchess.move records into repo's
// collection, spread across numGames DISTINCT, otherwise-unrelated
// gameURIs, none of which is realGameURI. Each filler record's own rkey
// is a plain "zzfiller-NNNNN" string -- deliberately NOT shaped like
// moveRkeyForPly's "mv"+base32 hash output (so real move rkeys and filler
// rkeys never collide) and deliberately leading with 'z', a character
// that byte-compares greater than 'm' no matter what follows it. The mock
// server orders listRecords results by rkey DESCENDING (newest-first --
// matches the real PDS's default; see deriveTestPDS.handle's doc
// comment), so every "zzfiller-*" record sorts strictly BEFORE any real
// "mv..." move recorded into the same repo. That is what reliably pushes
// the real game's own moves onto page two or later without this test
// needing to know or depend on exactly how many filler records precede
// them.
func seedFillerMoves(m *deriveTestPDS, repo string, n int, numGames int) {
	for i := 0; i < n; i++ {
		fillerGameURI := fmt.Sprintf("at://%s/app.atchess.game/filler-game-%d", repo, i%numGames)
		rkey := fmt.Sprintf("zzfiller-%05d", i)
		m.seed(repo, "app.atchess.move", rkey, map[string]interface{}{
			"$type":     "app.atchess.move",
			"createdAt": time.Now().Add(-time.Duration(n-i) * time.Minute).Format(time.RFC3339),
			"game":      map[string]interface{}{"uri": fillerGameURI},
			"player":    repo,
			"from":      "a2",
			"to":        "a3",
			"san":       "a3",
			"fen":       "rnbqkbnr/pppppppp/8/8/8/P7/1PPPPPPP/RNBQKBNR b KQkq - 0 1",
		})
	}
}

// TestGetLatestMoveForGame_MoreThan100RecordsInRepo_HEADLINE is the
// headline reproduction for atchess-1c9.119. See this file's doc comment.
func TestGetLatestMoveForGame_MoreThan100RecordsInRepo_HEADLINE(t *testing.T) {
	mock := newDeriveTestPDS(t)
	srv := mock.server()
	defer srv.Close()

	gameURI := mock.seedActiveGame(t, time.Now().Add(-time.Hour), nil)

	// More than 100 move records, spread across several OTHER games, all
	// living in white's own repo -- exactly the "player who accumulates
	// more than 100 move records across all their games" scenario from
	// the bug report. 105 is comfortably past the 100-record single-page
	// cap.
	seedFillerMoves(mock, whiteDID, 105, 5)

	client := newDeriveTestClient(t, mock)

	// Play a normal first move (1. e4) as white in the REAL game. This
	// is RecordMove's actual production path: it writes the move record
	// into white's own repo (c.did), which -- because of the 105 filler
	// records seeded above -- now holds 106 app.atchess.move records
	// total for this one collection.
	const fenAfterE4 = "rnbqkbnr/pppppppp/8/8/4P3/8/PPPP1PPP/RNBQKBNR b KQkq e3 0 1"
	move := &chess.MoveResult{
		From: "e2",
		To:   "e4",
		SAN:  "e4",
		FEN:  fenAfterE4,
	}
	if err := client.RecordMove(context.Background(), gameURI, move); err != nil {
		t.Fatalf("RecordMove: unexpected error: %v", err)
	}

	// getLatestMoveForGame (via GetGame) must still find this move and
	// derive the correct FEN, despite it sitting past page one of white's
	// own move collection.
	latest, err := client.getLatestMoveForGame(context.Background(), gameURI, whiteDID, blackDID)
	if err != nil {
		t.Fatalf("getLatestMoveForGame: unexpected error: %v", err)
	}
	if latest == nil {
		t.Fatalf("getLatestMoveForGame: expected the real move to be found past page one of a 106-record repo, got nil")
	}
	if latest.FEN != fenAfterE4 {
		t.Errorf("getLatestMoveForGame: expected FEN %q, got %q (the real move was not correctly found/derived past page one)", fenAfterE4, latest.FEN)
	}

	// GetGame (the actual path MakeMoveHandler relies on) must agree.
	game, err := client.GetGame(context.Background(), gameURI)
	if err != nil {
		t.Fatalf("GetGame: unexpected error: %v", err)
	}
	if game.FEN != fenAfterE4 {
		t.Errorf("GetGame: expected FEN %q, got %q", fenAfterE4, game.FEN)
	}
	if game.Status != chess.StatusActive {
		t.Errorf("GetGame: expected the game to still be active after one legal move, got status %q", game.Status)
	}
}

// TestGetChallengeDeclineOutcome_MatchOnPageTwo pins the same fix for a
// second, cheaper-to-seed call site: a genuine "declined"
// challengeResponse record that sits past page one of challengedDID's
// own repo must still be found.
func TestGetChallengeDeclineOutcome_MatchOnPageTwo(t *testing.T) {
	mock := newDeriveTestPDS(t)
	srv := mock.server()
	defer srv.Close()

	challengeURI := fmt.Sprintf("at://%s/app.atchess.challenge/real-challenge", blackDID)
	const challengeCID = "cid-real-challenge"

	// 120 filler challengeResponse records, all "accepted" (so they can
	// never match the "declined" scan even if inspected) and all naming a
	// DIFFERENT challenge strongRef, sorted (by rkey) before the real
	// record below.
	for i := 0; i < 120; i++ {
		rkey := fmt.Sprintf("filler-%05d", i)
		mock.seed(whiteDID, "app.atchess.challengeResponse", rkey, map[string]interface{}{
			"$type":     "app.atchess.challengeResponse",
			"createdAt": time.Now().Format(time.RFC3339),
			"challenge": map[string]interface{}{
				"uri": fmt.Sprintf("at://%s/app.atchess.challenge/filler-%d", blackDID, i),
				"cid": fmt.Sprintf("cid-filler-%d", i),
			},
			"response": "accepted",
		})
	}

	// The real decline: rkey sorts BEFORE every "filler-*" key above
	// ("a" < "f"). The mock server orders listRecords results by rkey
	// DESCENDING (newest-first -- matches the real PDS's default; see
	// deriveTestPDS.handle's doc comment), so a lexicographically SMALLER
	// rkey lands LATER in that descending sequence -- past page one of a
	// 121-record collection.
	mock.seed(whiteDID, "app.atchess.challengeResponse", "aaa-real-decline", map[string]interface{}{
		"$type":     "app.atchess.challengeResponse",
		"createdAt": time.Now().Format(time.RFC3339),
		"challenge": map[string]interface{}{
			"uri": challengeURI,
			"cid": challengeCID,
		},
		"response": "declined",
	})

	client := newDeriveTestClient(t, mock)

	declined, err := client.getChallengeDeclineOutcome(context.Background(), challengeURI, challengeCID, whiteDID)
	if err != nil {
		t.Fatalf("getChallengeDeclineOutcome: unexpected error: %v", err)
	}
	if !declined {
		t.Errorf("getChallengeDeclineOutcome: expected the real decline (past page one of a 121-record repo) to be found, got declined=false")
	}
}

// TestListAllRecords_PageCapEnforced pins listAllRecords's own fail-closed
// safety valve: a repo/collection that is not exhausted within
// maxListRecordsPages pages must return an error, never a silently
// truncated result. maxListRecordsPages is shrunk for the duration of
// this test (it is a var for exactly this reason -- see its doc comment)
// so this does not need to seed the tens of thousands of records the real
// default would require.
func TestListAllRecords_PageCapEnforced(t *testing.T) {
	mock := newDeriveTestPDS(t)
	srv := mock.server()
	defer srv.Close()

	origCap := maxListRecordsPages
	maxListRecordsPages = 1
	t.Cleanup(func() { maxListRecordsPages = origCap })

	// 150 records: page one (100 records) exhausts the shrunk cap of 1
	// page and still leaves a cursor pointing at more, so the second
	// iteration of listAllRecords's loop must fail closed instead of
	// fetching page two.
	for i := 0; i < 150; i++ {
		rkey := fmt.Sprintf("mv-%05d", i)
		mock.seed(whiteDID, "app.atchess.move", rkey, map[string]interface{}{
			"$type":     "app.atchess.move",
			"createdAt": time.Now().Format(time.RFC3339),
			"game":      map[string]interface{}{"uri": "at://" + whiteDID + "/app.atchess.game/whatever"},
			"player":    whiteDID,
			"from":      "a2",
			"to":        "a3",
			"san":       "a3",
			"fen":       "rnbqkbnr/pppppppp/8/8/8/P7/1PPPPPPP/RNBQKBNR b KQkq - 0 1",
		})
	}

	client := newDeriveTestClient(t, mock)

	_, err := client.listAllRecords(context.Background(), mock.base, true, whiteDID, "app.atchess.move")
	if err == nil {
		t.Fatalf("listAllRecords: expected an error once the page cap was exceeded, got nil (a truncated result was silently returned)")
	}
	if !errors.Is(err, errListRecordsPageCapExceeded) {
		t.Errorf("listAllRecords: expected an error wrapping errListRecordsPageCapExceeded, got: %v", err)
	}
}

// TestGetLastMove_FailsClosedOnUnreadableRepo pins the atchess-1c9.119
// fix-pass finding: getLastMove used to silently "continue" past a
// per-player listRecords failure (including, after this bead, a
// page-cap-exceeded error), returning (nil, nil) -- indistinguishable
// from "this player genuinely has no moves yet". Both of getLastMove's
// callers (CheckTimeViolation, GetTimeRemaining) treat a nil lastMove as
// license to fall back to the game's createdAt as last-activity, which
// for an old, active game could fabricate a time-violation forfeit
// against a player whose real (but unreadable) last move would have
// reset the clock. getLastMove must instead fail closed: a per-player
// read failure must surface as a non-nil error, exactly like
// getLatestMoveForGame's ErrIncompleteDerivation contract.
func TestGetLastMove_FailsClosedOnUnreadableRepo(t *testing.T) {
	mock := newDeriveTestPDS(t)
	srv := mock.server()
	defer srv.Close()

	gameURI := mock.seedActiveGame(t, time.Now().Add(-time.Hour), nil)

	// One genuine move exists in black's repo, so a nil-error, nil-lastMove
	// result would be a plainly WRONG "no moves" answer if this bug were
	// still present -- not just an ambiguous one.
	mock.seed(blackDID, "app.atchess.move", "black-move", map[string]interface{}{
		"$type":     "app.atchess.move",
		"createdAt": time.Now().Add(-5 * time.Minute).Format(time.RFC3339),
		"game":      map[string]interface{}{"uri": gameURI},
		"player":    blackDID,
		"from":      "e7",
		"to":        "e5",
		"san":       "e5",
		"fen":       "rnbqkbnr/pppp1ppp/8/4p3/4P3/8/PPPP1PPP/RNBQKBNR w KQkq - 0 2",
	})

	// black's repo (which holds the real last move) is unreadable.
	mock.setListFail(blackDID, "http500")

	client := newDeriveTestClient(t, mock)

	lastMove, err := client.getLastMove(context.Background(), gameURI, whiteDID)
	if err == nil {
		t.Fatalf("getLastMove: expected a non-nil error once black's repo (which holds the real last move) became unreadable, got nil error and lastMove=%+v -- this is exactly the fabricated-forfeit risk: a caller would silently fall back to the game's createdAt", lastMove)
	}
	if !errors.Is(err, ErrIncompleteDerivation) {
		t.Errorf("getLastMove: expected an error wrapping ErrIncompleteDerivation, got: %v", err)
	}
}

// TestCheckTimeViolation_DoesNotForfeitOnUnreadableOpponentRepo is the
// end-to-end regression for the same finding via CheckTimeViolation
// itself: an old correspondence game whose opponent's repo is unreadable
// must return an error, never a fabricated "yes, time violation" verdict
// derived from falling back to the game's (old) createdAt.
func TestCheckTimeViolation_DoesNotForfeitOnUnreadableOpponentRepo(t *testing.T) {
	mock := newDeriveTestPDS(t)
	srv := mock.server()
	defer srv.Close()

	// An old game (createdAt far in the past) with a short correspondence
	// time control -- if getLastMove's failure were silently swallowed and
	// the caller fell back to createdAt, this would trivially "detect" a
	// time violation.
	gameURI := mock.seedActiveGame(t, time.Now().Add(-30*24*time.Hour), map[string]interface{}{
		"type":        "correspondence",
		"daysPerMove": 3,
	})

	mock.setListFail(blackDID, "http500")

	client := newDeriveTestClient(t, mock)

	hasViolation, violation, err := client.CheckTimeViolation(context.Background(), gameURI)
	if err == nil {
		t.Fatalf("CheckTimeViolation: expected a non-nil error once an opponent repo became unreadable, got hasViolation=%v violation=%+v err=nil -- an unreadable repo must never be silently treated as \"no moves since game creation\"", hasViolation, violation)
	}
	if hasViolation {
		t.Errorf("CheckTimeViolation: fabricated a time violation (hasViolation=true) from an unreadable opponent repo instead of failing closed")
	}
}

// TestGetChallengeDeclineOutcome_SkipsAndLogsMalformedRecord pins the
// atchess-1c9.119 fix-pass finding for the eight new
// json.Unmarshal(record.Value, &value) sites this bead introduced:
// previously, a whole-page decode failure failed the ENTIRE scan closed
// (ErrIncompleteDerivation); now, since each record is decoded
// individually so pagination can work, a single malformed record is
// skipped instead -- a granularity improvement (one bad record can no
// longer deny service to the whole read), but it must be LOGGED, never
// silent. This seeds one malformed challengeResponse record (wrong JSON
// type for "response") alongside the real, well-formed declined record,
// and asserts BOTH that the real decline is still found AND that the
// malformed record produced a structured warning naming the collection
// and record URI.
func TestGetChallengeDeclineOutcome_SkipsAndLogsMalformedRecord(t *testing.T) {
	mock := newDeriveTestPDS(t)
	srv := mock.server()
	defer srv.Close()

	challengeURI := fmt.Sprintf("at://%s/app.atchess.challenge/real-challenge", blackDID)
	const challengeCID = "cid-real-challenge"

	// Malformed: "response" is a number, not a string -- json.Unmarshal
	// into the expected struct{Response string} fails. Rkey deliberately
	// sorts BEFORE "real-decline" in the mock's descending listRecords
	// order (see deriveTestPDS.handle's doc comment), so this test
	// actually exercises the skip-and-log path instead of accidentally
	// passing because getChallengeDeclineOutcome's early "return true,
	// nil" on the real match happened to run first.
	mock.seed(whiteDID, "app.atchess.challengeResponse", "zzz-malformed-record", map[string]interface{}{
		"$type":     "app.atchess.challengeResponse",
		"createdAt": time.Now().Format(time.RFC3339),
		"challenge": map[string]interface{}{
			"uri": challengeURI,
			"cid": challengeCID,
		},
		"response": 12345,
	})
	mock.seed(whiteDID, "app.atchess.challengeResponse", "real-decline", map[string]interface{}{
		"$type":     "app.atchess.challengeResponse",
		"createdAt": time.Now().Format(time.RFC3339),
		"challenge": map[string]interface{}{
			"uri": challengeURI,
			"cid": challengeCID,
		},
		"response": "declined",
	})

	client := newDeriveTestClient(t, mock)

	// Capture the global zerolog logger's output for the duration of this
	// call, then restore it -- client.go logs via the package-level
	// github.com/rs/zerolog/log logger, not an injectable per-Client one.
	var logBuf bytes.Buffer
	origLogger := log.Logger
	log.Logger = zerolog.New(&logBuf)
	defer func() { log.Logger = origLogger }()

	declined, err := client.getChallengeDeclineOutcome(context.Background(), challengeURI, challengeCID, whiteDID)
	if err != nil {
		t.Fatalf("getChallengeDeclineOutcome: unexpected error: %v", err)
	}
	if !declined {
		t.Errorf("getChallengeDeclineOutcome: expected the real decline to still be found despite a malformed sibling record, got declined=false")
	}

	logged := logBuf.String()
	if !strings.Contains(logged, "skipping malformed challengeResponse record") {
		t.Errorf("expected a structured warning naming the skipped malformed record, got log output: %s", logged)
	}
	if !strings.Contains(logged, "zzz-malformed-record") {
		t.Errorf("expected the log line to name the specific malformed record's URI (rkey %q), got log output: %s", "zzz-malformed-record", logged)
	}
}
