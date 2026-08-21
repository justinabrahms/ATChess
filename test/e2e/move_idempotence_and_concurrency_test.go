//go:build e2e

// Move idempotence and concurrency conformance test (atchess-1c9.114,
// folding in atchess-1c9.116's third done-criterion per .114's NOTES).
//
// WHY THIS EXISTS: every prior concurrency/idempotence guarantee for
// MakeMoveHandler/RecordMove (atchess-1c9.74, .87, .112, .113, .115, .116)
// was proven only against in-process mock PDSes
// (internal/web/move_concurrency_test.go's raceMockPDS,
// internal/atproto/lexicon_test.go's fakePDS). atchess-1c9.112's review
// found that raceMockPDS used to enforce a compare-and-swap parameter
// ("swapCid") the real PDS silently ignores -- meaning production had NO
// working CAS for the life of the codebase, and nothing caught it because
// the test double invented a stricter contract than the server it stood in
// for. This file is the structural fix for that blind spot: it exercises
// duplicate-submit idempotence and concurrent-move rejection against the
// REAL dual-PDS harness (test/harness), not a mock.
//
// If any assertion below fails, that is this bead's actual deliverable,
// not a bug in the test: see atchess-1c9.114's brief ("if idempotence or
// the 409 does not actually hold live, report it -- do not adjust the test
// until it passes").
//
// FRAMING CORRECTION (atchess-1c9.114's coordinator fix-pass): the first
// version of this file asserted that atchess-1c9.113's idempotence promise
// covers a SEQUENTIAL resubmit (wait for the first response, then
// resubmit the identical move) as well as a racing one, and that
// assertion failed live -- deterministically, not flakily. That failure
// was real, but the ASSERTION was wrong, not the server: a sequential
// resubmit is correctly refused by MakeMoveHandler's turn gate before
// RecordMove is ever reached (the player is, in fact, asking to move
// twice in a row), which is deliberate and defensible, not a gap.
// atchess-1c9.113's actual promise -- proven by RecordMove's
// deterministic-rkey read-back reconciliation -- covers the RACING case:
// two requests whose turn-checks overlap before either write lands. See
// TestMoveConcurrency_Owner_SameMoveTwice_Idempotent (the racing case,
// verified live below) versus TestMoveSequentialResubmit_
// RefusedByTurnGate_Owner/_NonOwner (the sequential case, also verified
// live below, and asserted as a 403, not a bug). atchess-u3z tracks the
// separate, still-open UX question of whether a client retrying after a
// genuinely DROPPED response deserves better than that 403.
//
// GAME SETUP: every subtest below creates its game via the REAL challenge
// + accept flow (POST /api/challenges, then POST
// /api/challenge-notifications/{key}/accept), not the plain POST
// /api/games path ownership_test.go uses -- this is the only path that
// produces the CHALLENGER/CHALLENGED asymmetry atchess-1c9.113 examined:
// the resulting app.atchess.game record always lives in the CHALLENGED
// (accepting) player's own repo (see AcceptChallengeHandler's doc comment
// and atproto.Client.AcceptChallenge), which is therefore always the
// "owner" for RecordMove's "if repo == c.did" compare-and-swap-protected
// branch. The challenger is always the "non-owner": RecordMove skips the
// game record's CAS-protected update entirely for their moves, and only
// the move record's deterministic (game, ply) rkey protects them
// (atchess-1c9.113).
//
// Each of this file's four test functions below deliberately uses a
// DISTINCT (challenger, challenged) DID pair from every other one.
// atproto.generateGameID derives a challenge-accepted game's rkey from
// sha256(challengerDID:challengedDID:unixSecond) -- two challenges between
// the SAME ordered pair within the SAME wall-clock second would derive the
// SAME game rkey and collide (AcceptChallenge's ErrChallengeConflict).
// Alice-challenges-bob and bob-challenges-alice are different ordered
// pairs (order is part of the hash input), which covers two of the four;
// the other two each mint a fresh third-party account (mirroring
// ownership_test.go's carol) so their pairs can never collide with any of
// the others regardless of timing.
package e2e

import (
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/justinabrahms/atchess/test/harness"
)

// startingFEN is the standard chess starting position. The server never
// trusts a client-supplied FEN for move validation (MakeMoveHandler always
// re-derives the authoritative position via client.GetGame) -- this is
// included in every /api/moves request body only because MakeMoveRequest
// has a required-looking "fen" field and every other e2e test in this
// package populates it the same way (see ownership_test.go).
const startingFEN = "rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR w KQkq - 0 1"

// createChallengeAcceptedGame has challenger challenge challenged (with
// challenger taking challengerColor) and then has challenged immediately
// accept, returning the resulting game's at:// URI. Per
// atproto.Client.AcceptChallenge, the game record always lands in
// CHALLENGED's own repo -- challenged is therefore always this game's
// "owner" for RecordMove's compare-and-swap-protected branch, and
// challenger is always the "non-owner".
func createChallengeAcceptedGame(t *testing.T, challenger, challenged *harness.Player, challengerColor string) string {
	t.Helper()

	challengeResp := apiPost(t, challenger, "/api/challenges", map[string]interface{}{
		"opponent_did": challenged.DID,
		"color":        challengerColor,
		"message":      "atchess-1c9.114 move idempotence/concurrency e2e test",
	})
	challengeURI := recordURI(t, challengeResp, "POST /api/challenges")
	key := encodeChallengeKey(challengeURI)

	acceptResp := apiPost(t, challenged, "/api/challenge-notifications/"+key+"/accept", nil)
	gameURI := recordURI(t, acceptResp, "POST /api/challenge-notifications/{key}/accept")
	t.Logf("game created via challenge+accept: challenger=%s(did=%s) challenged/owner=%s(did=%s) challengerColor=%s gameURI=%s",
		challenger.Handle, challenger.DID, challenged.Handle, challenged.DID, challengerColor, gameURI)
	return gameURI
}

// countMoveRecordsFor counts app.atchess.move records in mover's own repo
// (moves always land in the MOVER's own repo regardless of who owns the
// game record -- see RecordMove) matching gameURI, from, and to. Used to
// pin "exactly one move record" independent of how many HTTP responses
// came back with what status.
// countMoveRecordsFor counts matching move records in the mover's own repo,
// polling until the answer SETTLES rather than reading once.
//
// A single read immediately after a write is wrong here in both directions.
// Reading too early can return 0 for a record that was accepted (HTTP 200)
// but is not queryable yet. But polling merely until the count is non-zero is
// equally wrong: it would return 1 the moment the first record appears and
// never see a SECOND one landing a beat later, which is precisely the
// duplicate this test exists to catch.
//
// HISTORICAL NOTE, because the wrong cause was believed once already: the
// intermittent zeros originally seen here were NOT read lag. They were
// atchess-1c9.119 -- harness RepoListRecords was capped at 100 records with
// the cursor decoded and ignored, and because .113 made move rkeys
// deterministic hashes (so page position is effectively random) a fresh
// record fell off page one once the repo passed 100. Measured during .119's
// review: over 40 runs the paginated count was 1 in 40/40 while the
// single-page count was 0 in 9/40. That was filed as a lost-move P1
// (atchess-1c9.118) and closed as a duplicate once .119 landed. The settle
// poll below is still correct practice, but it is not what fixed that.
//
// So: wait for a non-zero count, then require it to hold steady across two
// consecutive reads before believing it. A duplicate that arrives late still
// perturbs the count and restarts the settle.
func countMoveRecordsFor(t *testing.T, mover *harness.Player, gameURI, from, to string) int {
	t.Helper()
	const (
		settleDeadline = 15 * time.Second
		settleInterval = 250 * time.Millisecond
	)
	deadline := time.Now().Add(settleDeadline)
	last := -1
	for {
		n := countMoveRecordsOnce(t, mover, gameURI, from, to)
		if n > 0 && n == last {
			return n
		}
		last = n
		if time.Now().After(deadline) {
			// Report whatever we last saw; the caller's assertion states
			// what it expected, and a persistent 0 is itself a finding.
			return n
		}
		time.Sleep(settleInterval)
	}
}

// countMoveRecordPairSettled counts two candidate moves and settles on their
// SUM rather than on each independently. Used where exactly one of the pair is
// expected to exist: settling per-move would spend the whole deadline waiting
// for the one that correctly never appears.
func countMoveRecordPairSettled(t *testing.T, mover *harness.Player, gameURI, fromA, toA, fromB, toB string) (int, int) {
	t.Helper()
	const (
		settleDeadline = 15 * time.Second
		settleInterval = 250 * time.Millisecond
	)
	deadline := time.Now().Add(settleDeadline)
	lastSum := -1
	for {
		a := countMoveRecordsOnce(t, mover, gameURI, fromA, toA)
		b := countMoveRecordsOnce(t, mover, gameURI, fromB, toB)
		if a+b > 0 && a+b == lastSum {
			return a, b
		}
		lastSum = a + b
		if time.Now().After(deadline) {
			return a, b
		}
		time.Sleep(settleInterval)
	}
}

func countMoveRecordsOnce(t *testing.T, mover *harness.Player, gameURI, from, to string) int {
	t.Helper()
	recs, err := mover.RepoListRecords("app.atchess.move")
	if err != nil {
		t.Fatalf("listRecords(app.atchess.move) against %s's own PDS (%s) failed: %v", mover.Handle, mover.PDSURL, err)
	}
	count := 0
	for _, r := range recs {
		rFrom, _ := r["from"].(string)
		rTo, _ := r["to"].(string)
		if rFrom != from || rTo != to {
			continue
		}
		gameRef, _ := r["game"].(map[string]interface{})
		if uri, _ := gameRef["uri"].(string); uri == gameURI {
			count++
		}
	}
	return count
}

// TestMoveSequentialResubmit_RefusedByTurnGate_Owner pins what actually
// happens, live, when the OWNING player submits the SAME move twice
// SEQUENTIALLY (waits for the first response, then resubmits): the second
// request is refused with HTTP 403 "It is not your turn". This is NOT a
// bug and is NOT covered by atchess-1c9.113's idempotence promise --
// atchess-1c9.114's coordinator fix-pass corrected an over-broad reading
// of that promise (the original version of this test asserted 200 both
// times and failed live). atchess-1c9.113's read-back reconciliation
// lives inside RecordMove (internal/atproto/client.go) and only ever runs
// for a request that gets PAST MakeMoveHandler's turn gate
// (internal/web/service.go) -- see TestMoveConcurrency_Owner_
// SameMoveTwice_Idempotent above for the case that actually reaches it
// (both requests' turn-checks racing against the SAME snapshot).
//
// A genuinely sequential resubmission has already been answered once by
// the time it is retried: MakeMoveHandler's client.GetGame call sees the
// first move (already durable, via its cross-repo move scan) and
// correctly reports that it is no longer this player's turn -- the player
// is, in fact, asking to move twice in a row, which is illegal chess. The
// mechanism refusing this is deliberate and defensible, not a gap.
//
// The genuinely open question this does NOT resolve -- what a client
// whose first response was dropped by the network (so it never learned
// its move succeeded) should see on a good-faith retry, versus a player
// who is knowingly trying to move again -- is tracked separately as
// atchess-u3z; this test only pins today's actual, deliberate behavior.
func TestMoveSequentialResubmit_RefusedByTurnGate_Owner(t *testing.T) {
	accounts := harness.LoadAccounts(t)
	services := harness.StartServices(t, accounts)

	alice := harness.NewPlayer(t, accounts.Alice, services.AliceURL)
	bob := harness.NewPlayer(t, accounts.Bob, services.BobURL)

	// alice challenges bob, taking black herself, so bob (challenged ==
	// owner) is white and moves first -- this is required for bob's OWN
	// resubmission below to be a legal move the first time.
	gameURI := createChallengeAcceptedGame(t, alice, bob, "black")

	code1, body1 := apiPostExpectStatus(t, bob, "/api/moves", map[string]interface{}{
		"game_id": gameURI, "from": "e2", "to": "e4", "fen": startingFEN,
	})
	if code1 != http.StatusOK {
		t.Fatalf("bob's (owner's) FIRST submission of e2-e4 was expected to succeed with HTTP 200, got %d: %s", code1, body1)
	}

	code2, body2 := apiPostExpectStatus(t, bob, "/api/moves", map[string]interface{}{
		"game_id": gameURI, "from": "e2", "to": "e4", "fen": startingFEN,
	})
	if code2 != http.StatusForbidden {
		t.Errorf("bob's (owner's) SECOND, sequential resubmission of e2-e4 (after the first already succeeded) was expected to be refused by the turn gate with HTTP 403, got %d: %s", code2, body2)
	} else if !strings.Contains(body2, "It is not your turn") {
		t.Errorf("expected the turn gate's rejection body to say 'It is not your turn', got: %s", body2)
	}

	if got := countMoveRecordsFor(t, bob, gameURI, "e2", "e4"); got != 1 {
		t.Errorf("expected exactly 1 app.atchess.move record (game=%s from=e2 to=e4) in bob's (owner's/mover's) own repo -- the refused resubmission must never have reached RecordMove at all, got %d", gameURI, got)
	}
}

// TestMoveSequentialResubmit_RefusedByTurnGate_NonOwner is the non-owner
// analogue of TestMoveSequentialResubmit_RefusedByTurnGate_Owner above
// (RecordMove's CAS-protected branch never runs for a non-owning mover at
// all -- see that test's doc comment for the full explanation, which
// applies identically here regardless of ownership: MakeMoveHandler's
// turn gate runs, and refuses, before RecordMove is ever reached).
func TestMoveSequentialResubmit_RefusedByTurnGate_NonOwner(t *testing.T) {
	accounts := harness.LoadAccounts(t)
	services := harness.StartServices(t, accounts)

	alice := harness.NewPlayer(t, accounts.Alice, services.AliceURL)
	bob := harness.NewPlayer(t, accounts.Bob, services.BobURL)

	// bob challenges alice, taking black himself, so alice (challenged ==
	// owner) is white and moves first, and bob (challenger == non-owner)
	// is black and moves second -- the subject of this test.
	gameURI := createChallengeAcceptedGame(t, bob, alice, "black")

	// Setup: alice (the owner) must move first so it becomes bob's
	// (non-owner's) turn. This single call is setup, not part of what is
	// being measured, so it is required to succeed outright.
	setupCode, setupBody := apiPostExpectStatus(t, alice, "/api/moves", map[string]interface{}{
		"game_id": gameURI, "from": "e2", "to": "e4", "fen": startingFEN,
	})
	if setupCode != http.StatusOK {
		t.Fatalf("setup: alice's (owner's) move e2-e4 was expected to succeed with HTTP 200, got %d: %s", setupCode, setupBody)
	}

	code1, body1 := apiPostExpectStatus(t, bob, "/api/moves", map[string]interface{}{
		"game_id": gameURI, "from": "e7", "to": "e5", "fen": startingFEN,
	})
	if code1 != http.StatusOK {
		t.Fatalf("bob's (non-owner's) FIRST submission of e7-e5 was expected to succeed with HTTP 200, got %d: %s", code1, body1)
	}

	code2, body2 := apiPostExpectStatus(t, bob, "/api/moves", map[string]interface{}{
		"game_id": gameURI, "from": "e7", "to": "e5", "fen": startingFEN,
	})
	if code2 != http.StatusForbidden {
		t.Errorf("bob's (non-owner's) SECOND, sequential resubmission of e7-e5 (after the first already succeeded) was expected to be refused by the turn gate with HTTP 403, got %d: %s", code2, body2)
	} else if !strings.Contains(body2, "It is not your turn") {
		t.Errorf("expected the turn gate's rejection body to say 'It is not your turn', got: %s", body2)
	}

	if got := countMoveRecordsFor(t, bob, gameURI, "e7", "e5"); got != 1 {
		t.Errorf("expected exactly 1 app.atchess.move record (game=%s from=e7 to=e5) in bob's (non-owner's/mover's) own repo -- the refused resubmission must never have reached RecordMove at all, got %d", gameURI, got)
	}
}

// TestMoveConcurrency_Owner_TwoDifferentMoves is the live-server half of
// atchess-1c9.74/.112's "same player, two different moves for one turn,
// submitted concurrently" guarantee, on the OWNING player's instance (the
// game record's compare-and-swap-protected branch). Exactly one of the two
// concurrent requests may succeed (HTTP 200); the other must be rejected
// as a conflict (HTTP 409, atchess-1c9.87's sentinel mapping), and exactly
// one app.atchess.move record may exist.
//
// Unlike internal/web/move_concurrency_test.go's raceMockPDS-backed
// equivalent, there is no gate this test can install inside a real PDS to
// force both requests' reads to be simultaneous deterministically; both
// goroutines are released together via a start barrier (a closed channel)
// to maximize overlap, which is the strongest synchronization available
// against a live out-of-process server without instrumenting the PDS
// itself.
func TestMoveConcurrency_Owner_TwoDifferentMoves(t *testing.T) {
	accounts := harness.LoadAccounts(t)
	services := harness.StartServices(t, accounts)

	bob := harness.NewPlayer(t, accounts.Bob, services.BobURL)

	// A fresh third-party account guarantees this subtest's (challenger,
	// challenged) DID pair cannot collide with either of the two subtests
	// above, regardless of timing (see this file's doc comment).
	carolAccount := createThirdPartyAccount(t, accounts.Alice.PDSURL, "carol-concurrency.test", "carol-concurrency@chess.test", "carol-concurrency-pass-1")
	carol := harness.NewPlayer(t, carolAccount, services.AliceURL)

	// carol challenges bob, taking black herself, so bob (challenged ==
	// owner) is white and moves first -- required for bob's own
	// concurrent moves below to be legal.
	gameURI := createChallengeAcceptedGame(t, carol, bob, "black")

	start := make(chan struct{})
	var wg sync.WaitGroup
	var code1, code2 int
	var body1, body2 string
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		code1, body1 = apiPostExpectStatus(t, bob, "/api/moves", map[string]interface{}{
			"game_id": gameURI, "from": "e2", "to": "e4", "fen": startingFEN,
		})
	}()
	go func() {
		defer wg.Done()
		<-start
		code2, body2 = apiPostExpectStatus(t, bob, "/api/moves", map[string]interface{}{
			"game_id": gameURI, "from": "d2", "to": "d4", "fen": startingFEN,
		})
	}()
	close(start)
	wg.Wait()

	var successes, conflicts int
	for i, c := range []int{code1, code2} {
		body := body1
		if i == 1 {
			body = body2
		}
		switch c {
		case http.StatusOK:
			successes++
		case http.StatusConflict:
			conflicts++
			if !strings.Contains(body, "conflict") && !strings.Contains(body, "Failed to record move") {
				t.Errorf("expected a 409 conflict body to mention the conflict, got: %s", body)
			}
		default:
			t.Errorf("unexpected status %d for concurrent move %d: %s", c, i+1, body)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("DELIVERABLE FINDING (atchess-1c9.114): expected exactly 1 success (HTTP 200) and 1 conflict (HTTP 409) for two concurrent moves by the OWNING player against a live PDS, got codes=[%d, %d] bodies=[%q, %q]", code1, code2, body1, body2)
	}

	// Exactly one of these two moves won, so exactly one of the two counts
	// is legitimately ZERO. Settling on each separately would therefore
	// burn the poller's full deadline on the loser every single run --
	// measured at ~15s, four times this test's peers. Settle on the TOTAL
	// instead: that is the quantity the assertion actually cares about,
	// and it becomes non-zero as soon as the winner's record is visible.
	//
	// A settle is still wanted here despite both HTTP responses having
	// returned. Durability is not the question -- RecordMove's writes are
	// synchronous within the request -- but listRecords VISIBILITY can lag
	// a durable write, and this test counts via listRecords.
	e4Count, d4Count := countMoveRecordPairSettled(t, bob, gameURI, "e2", "e4", "d2", "d4")
	if e4Count+d4Count != 1 {
		t.Errorf("expected exactly 1 total app.atchess.move record between the two concurrently-attempted moves (game=%s), got e2-e4=%d d2-d4=%d (total %d)", gameURI, e4Count, d4Count, e4Count+d4Count)
	}
}

// TestMoveConcurrency_Owner_SameMoveTwice_Idempotent is the live-server
// half of atchess-1c9.113's actual promise (the coordinator's atchess-1c9.114
// fix-pass correction: that promise covers the RACING case -- two requests
// whose turn-checks overlap before either write lands -- not a sequential
// resubmit after a response was already received; see
// TestMoveSequentialResubmit_RefusedByTurnGate_Owner/_NonOwner below for
// that distinct, deliberate-and-defensible case). Two goroutines submit
// the IDENTICAL move (e2-e4) to the OWNING player's own instance,
// released together via a start barrier to maximize overlap. Both
// requests' turn-checks race against the SAME pre-move snapshot, so
// RecordMove's deterministic (game, ply) move rkey and its
// read-back-and-compare reconciliation (atchess-1c9.113) are what
// actually decide the outcome here, live, for the first time -- every
// prior test of this exact mechanism (internal/web/move_concurrency_test.go's
// *_IdenticalRetry_Idempotent tests) used only a mock PDS.
func TestMoveConcurrency_Owner_SameMoveTwice_Idempotent(t *testing.T) {
	accounts := harness.LoadAccounts(t)
	services := harness.StartServices(t, accounts)

	bob := harness.NewPlayer(t, accounts.Bob, services.BobURL)

	// A fresh third-party account guarantees this subtest's (challenger,
	// challenged) DID pair cannot collide with any other subtest in this
	// file, regardless of timing (see this file's doc comment).
	daveAccount := createThirdPartyAccount(t, accounts.Alice.PDSURL, "dave-samemove.test", "dave-samemove@chess.test", "dave-samemove-pass-1")
	dave := harness.NewPlayer(t, daveAccount, services.AliceURL)

	// dave challenges bob, taking black himself, so bob (challenged ==
	// owner) is white and moves first -- required for bob's own
	// concurrent moves below to be legal.
	gameURI := createChallengeAcceptedGame(t, dave, bob, "black")

	start := make(chan struct{})
	var wg sync.WaitGroup
	var code1, code2 int
	var body1, body2 string
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		code1, body1 = apiPostExpectStatus(t, bob, "/api/moves", map[string]interface{}{
			"game_id": gameURI, "from": "e2", "to": "e4", "fen": startingFEN,
		})
	}()
	go func() {
		defer wg.Done()
		<-start
		code2, body2 = apiPostExpectStatus(t, bob, "/api/moves", map[string]interface{}{
			"game_id": gameURI, "from": "e2", "to": "e4", "fen": startingFEN,
		})
	}()
	close(start)
	wg.Wait()

	t.Logf("same-move race results: code1=%d body1=%s | code2=%d body2=%s", code1, body1, code2, body2)

	// WHAT THIS PINS, and what it deliberately does not.
	//
	// The invariant that always holds, and the reason this subtest exists:
	// two concurrent submissions of the SAME move produce EXACTLY ONE
	// app.atchess.move record. That is atchess-1c9.113's deterministic
	// (game, ply) rkey doing its job against a real server rather than a
	// mock -- the loser's createRecord lands on the winner's rkey instead
	// of minting a second record.
	//
	// The STATUS CODES, however, are timing-dependent, and asserting a
	// strict 200/200 here would flake on a loaded runner. Traced live
	// (atchess-1c9.114's review):
	//
	//   200/200 happens when both requests read the same gameCID AND both
	//   write a byte-identical game record -- updatedAt is RFC3339, second
	//   granularity, so this needs them to land in the same second. The
	//   real PDS then short-circuits the identical-value putRecord as a
	//   no-op and never evaluates the (stale) swapRecord at all. That
	//   server behaviour is atchess-1c9.117, not something we implement.
	//
	//   200/409 happens for two ordinary reasons: the pair straddles a
	//   wall-clock second, so the records differ and the CAS IS evaluated;
	//   or ~35ms of stagger lets the loser read the fresher gameCID, so
	//   its read-back compares unequal on game.cid and reports a genuine
	//   conflict.
	//
	// Both are correct. A 409 here is benign timing, NOT a regression --
	// an earlier version of this comment said the opposite and would have
	// sent someone hunting a bug that does not exist. What WOULD be a
	// regression is a 500, an unexpected status, or more than one move
	// record; those are asserted.
	// Asserted SYMMETRICALLY, because the two goroutines are symmetric:
	// nothing makes goroutine 1 the winner. Requiring code1 == 200
	// specifically would fail the legitimate {409, 200} outcome whenever
	// the pair straddles a second (so the CAS is genuinely evaluated) AND
	// g2's request happens to land first -- measured at roughly 1 run in
	// 85, which is a false failure this file cannot afford once
	// atchess-1c9.13 makes the suite a required gate.
	okCount, conflictCount := 0, 0
	for _, c := range []int{code1, code2} {
		switch c {
		case http.StatusOK:
			okCount++
		case http.StatusConflict:
			conflictCount++
		}
	}
	if okCount+conflictCount != 2 || okCount < 1 {
		t.Errorf("two concurrent submissions of the SAME move (e2-e4) must be {200,200} (identical-value put short-circuit) or {200,409} in either order (CAS evaluated, or the loser read a fresher gameCID) -- got code1=%d code2=%d; bodies: %s | %s", code1, code2, body1, body2)
	}

	if got := countMoveRecordsFor(t, bob, gameURI, "e2", "e4"); got != 1 {
		t.Errorf("DELIVERABLE FINDING (atchess-1c9.114): expected exactly 1 app.atchess.move record (game=%s from=e2 to=e4) after two identical concurrent submissions, got %d", gameURI, got)
	}
}
