//go:build e2e

// Federation conformance test (atchess-1c9.5): pins the defect where every
// XRPC call site in internal/atproto/client.go (all 28 of them, per the
// verified investigation behind this bead) targets the single, statically
// configured c.pdsURL -- the PDS the protocol-service instance was itself
// bootstrapped against -- rather than resolving where a given DID's repo
// actually lives. Concretely for this test:
//
//   - ResolveHandle (internal/atproto/client.go, ~line 692) queries
//     c.pdsURL's com.atproto.identity.resolveHandle, so a caller can only
//     ever resolve handles that happen to be hosted on their OWN PDS.
//   - GetGame (internal/atproto/client.go, ~line 592-608) parses the repo
//     (DID) out of the record's at:// URI but still issues
//     com.atproto.repo.getRecord against c.pdsURL, so a caller can only
//     ever read a record that happens to live in a repo hosted on their OWN
//     PDS. This one call is reached from two different HTTP surfaces --
//     GET /api/games/{id} (GetGameHandler) directly, and POST /api/moves
//     (MakeMoveHandler) internally, before it even gets to the turn/
//     participant check -- so both surfaces go down together; it is ONE
//     root-cause defect with two externally observable symptoms, not two.
//
// This test is EXPECTED TO FAIL against today's code. Do not "fix" it by
// weakening assertions or by rerouting calls through a same-PDS shortcut --
// the failure output, and exactly how far the game gets before the
// federation breaks, is the deliverable. See atchess-1c9.10 for the actual
// fix (DID-to-PDS resolution and per-target endpoint routing).
//
// EMPIRICALLY CONFIRMED (2026-08-19, against `make test-federation-up-ci`,
// alice on PDS-A :2583 / bob on PDS-B :2584, hermetic local-plc, zero public
// DIDs minted):
//
//	$ curl 'http://localhost:2583/xrpc/com.atproto.identity.resolveHandle?handle=bob.test'
//	{"error":"InvalidRequest","message":"Unable to resolve handle"}
//
//	$ curl 'http://localhost:2583/xrpc/com.atproto.repo.getRecord?repo=<bob-did>&collection=app.atchess.game&rkey=test'
//	HTTP/1.1 400 Bad Request
//	{"error":"InvalidRequest","message":"Could not find repo: <bob-did>"}
//
// ADAPTATION NOTE -- "Bob accepts" (step 3 of the brief): there is no HTTP
// endpoint that accepts a challenge and links it to the proposed game.
// CreateGameFromChallenge (internal/atproto/client.go) exists and is
// exercised directly in test/e2e/challenge_test.go, but it is never wired
// to a route in cmd/protocol/main.go -- only plain POST /api/games
// (CreateGameHandler -> Client.CreateGame) is exposed, and that always
// creates the record in the CALLER's own repo. This test therefore emulates
// "Bob accepts" as Bob calling POST /api/games from his own
// protocol-service instance, taking the color opposite of what Alice
// requested in her challenge. This is a faithful emulation of what a real
// accept handler would do (see bobClient.CreateGameFromChallenge in
// challenge_test.go, which also creates the game in the ACCEPTING player's
// own repo) -- it is not a shortcut around the federation boundary, and it
// is called out explicitly at the point it runs, below. It IS a distinct,
// separately-notable gap (missing accept-and-link HTTP endpoint) from the
// two c.pdsURL defects above; report it as such, not as a third instance of
// the same bug.
package e2e

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/justinabrahms/atchess/test/harness"
)

// gameInfo mirrors the relevant fields of chess.Game (internal/chess/types.go)
// as returned by GET /api/games/{id}. Duplicated here rather than imported,
// matching this suite's existing convention (see healthInfo in
// ownership_test.go) of not depending on internal/... response types.
type gameInfo struct {
	ID     string `json:"id"`
	White  string `json:"white"`
	Black  string `json:"black"`
	Status string `json:"status"`
	FEN    string `json:"fen"`
}

// moveResultInfo mirrors the relevant fields of chess.MoveResult as returned
// by POST /api/moves.
type moveResultInfo struct {
	SAN       string `json:"san"`
	FEN       string `json:"fen"`
	Check     bool   `json:"check"`
	Checkmate bool   `json:"checkmate"`
}

// encodeGameID is the exact inverse of Service.decodeGameID
// (internal/web/service.go): URL-safe base64 (as GetGameHandler's route
// expects in the {id} path segment) of the raw at:// record URI.
func encodeGameID(gameURI string) string {
	std := base64.StdEncoding.EncodeToString([]byte(gameURI))
	std = strings.ReplaceAll(std, "+", "-")
	std = strings.ReplaceAll(std, "/", "_")
	return std
}

// getGameStatus performs a single, unauthenticated GET /api/games/{id}
// against baseURL (one player's own protocol-service instance) and returns
// the raw status code, the decoded game (nil if the status was not 200),
// and the raw response body for diagnostics.
func getGameStatus(t *testing.T, baseURL, gameURI string) (int, *gameInfo, string) {
	t.Helper()
	encoded := encodeGameID(gameURI)
	resp, err := http.Get(baseURL + "/api/games/" + encoded)
	if err != nil {
		t.Fatalf("GET %s/api/games/%s failed: %v", baseURL, encoded, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, nil, string(body)
	}
	var g gameInfo
	if err := json.Unmarshal(body, &g); err != nil {
		t.Fatalf("GET %s/api/games/%s returned HTTP 200 but undecodable body: %v (body: %s)", baseURL, encoded, err, string(body))
	}
	return resp.StatusCode, &g, string(body)
}

// pollGameStatusOK polls getGameStatus against baseURL until it returns
// HTTP 200 or deadline elapses, so a read that legitimately races a very
// recent write (e.g. reading a record immediately after the createRecord
// call that produced it) gets a fair chance to observe it -- never a bare
// sleep-and-hope, and always bounded. On timeout it returns the LAST
// observed status/body/game (which may be a genuine, permanent federation
// failure rather than a transient race) along with the elapsed time, so
// callers can report both.
func pollGameStatusOK(t *testing.T, baseURL, gameURI string, deadline time.Duration) (status int, game *gameInfo, body string, elapsed time.Duration) {
	t.Helper()
	start := time.Now()
	interval := 200 * time.Millisecond
	for {
		status, game, body = getGameStatus(t, baseURL, gameURI)
		elapsed = time.Since(start)
		if status == http.StatusOK {
			return
		}
		if elapsed >= deadline {
			return
		}
		time.Sleep(interval)
	}
}

// TestFederation pins atchess-1c9.5: a full cross-PDS game between Alice
// (PDS-A) and Bob (PDS-B), using ONLY the public HTTP boundary (one
// protocol-service instance per player, per test/harness). It is expected
// to fail for a federation reason. Each property is its own t.Run so a
// failure at one step does not hide the others; where a step cannot proceed
// on its own output, it falls back to known-good data (e.g. Bob's DID from
// test/.harness-accounts.json) so later steps still get exercised, and logs
// that fallback loudly so it is never mistaken for a clean pass.
func TestFederation(t *testing.T) {
	accounts := harness.LoadAccounts(t)
	services := harness.StartServices(t, accounts)

	alice := harness.NewPlayer(t, accounts.Alice, services.AliceURL)
	bob := harness.NewPlayer(t, accounts.Bob, services.BobURL)

	t.Logf("alice: did=%s pds=%s protocol=%s", alice.DID, alice.PDSURL, alice.ProtocolURL)
	t.Logf("bob:   did=%s pds=%s protocol=%s", bob.DID, bob.PDSURL, bob.ProtocolURL)

	// ---------------------------------------------------------------
	// Step 2: handle resolution across PDSes.
	//
	// Alice's protocol-service instance is configured against her own PDS
	// (PDS-A). We ask it to resolve bob's HANDLE (not his DID) via the only
	// HTTP surface that invokes ResolveHandle: POST /api/challenges with a
	// non-"did:"-prefixed opponent_did (CreateChallengeHandler,
	// internal/web/service.go). If resolution works, this call also
	// produces our "Alice challenges Bob" record (step 3) as a side effect;
	// if it fails, no challenge record is created (CreateChallengeHandler
	// returns 400 before ever calling CreateChallenge), so step 3 below
	// falls back to using bob's DID directly.
	// ---------------------------------------------------------------
	handleResolutionOK := false
	resolvedBobDID := ""
	t.Run("HandleResolution", func(t *testing.T) {
		status, body := apiPostExpectStatus(t, alice, "/api/challenges", map[string]interface{}{
			"opponent_did": accounts.Bob.Handle,
			"color":        "white",
			"message":      "federation handle-resolution probe",
		})
		if status != http.StatusOK {
			t.Errorf("handle resolution failed: alice's protocol-service instance (%s), whose configured AT Protocol client queries its OWN PDS (%s), tried to resolve bob's handle %q -- which is hosted on PDS %s (bob's actual DID: %s) -- and got HTTP %d: %s.\n"+
				"Root cause: atproto.Client.ResolveHandle (internal/atproto/client.go) unconditionally issues com.atproto.identity.resolveHandle against c.pdsURL instead of discovering the handle's actual home PDS.\n"+
				"Handle being resolved: %q (bob's DID: %s). PDS actually queried: %s. PDS that should have been queried (bob's home PDS): %s.",
				services.AliceURL, alice.PDSURL, accounts.Bob.Handle, accounts.Bob.PDSURL, accounts.Bob.DID, status, body,
				accounts.Bob.Handle, accounts.Bob.DID, alice.PDSURL, accounts.Bob.PDSURL)
			return
		}
		var decoded map[string]interface{}
		if err := json.Unmarshal([]byte(body), &decoded); err != nil {
			t.Fatalf("handle resolution: POST /api/challenges returned HTTP 200 but an undecodable body: %v (body: %s)", err, body)
		}
		challenged, _ := decoded["Challenged"].(string)
		if challenged != accounts.Bob.DID {
			t.Errorf("handle resolution: resolved bob's handle %q to %q, want his actual DID %q", accounts.Bob.Handle, challenged, accounts.Bob.DID)
			return
		}
		handleResolutionOK = true
		resolvedBobDID = challenged
		t.Logf("OK: alice resolved bob's handle %q to %q", accounts.Bob.Handle, challenged)
	})

	if !handleResolutionOK {
		resolvedBobDID = accounts.Bob.DID
		t.Logf("DEGRADED FALLBACK: HandleResolution failed above; the rest of this test uses bob's KNOWN DID (%s) from test/.harness-accounts.json directly, bypassing handle resolution entirely, so the remaining federation properties can still be exercised", resolvedBobDID)
	}

	// ---------------------------------------------------------------
	// Step 3: challenge across PDSes. Alice challenges Bob (using his DID
	// directly -- see fallback note above -- so this step is not itself
	// blocked by a HandleResolution failure), then "Bob accepts" (emulated;
	// see the ADAPTATION NOTE in the package doc comment).
	// ---------------------------------------------------------------
	var challengeURI string
	t.Run("Challenge", func(t *testing.T) {
		if !handleResolutionOK {
			t.Logf("DEGRADED: using bob's known DID (%s) instead of his handle, because HandleResolution failed above", resolvedBobDID)
		}
		resp := apiPost(t, alice, "/api/challenges", map[string]interface{}{
			"opponent_did": resolvedBobDID,
			"color":        "white",
			"message":      "federation test challenge",
		})
		challengeURI = recordURI(t, resp, "POST /api/challenges (as alice, federation test)")
		t.Logf("challenge created: uri=%s (alice=%s challenges bob=%s for white)", challengeURI, alice.DID, resolvedBobDID)
	})

	var gameURI string
	t.Run("Accept_EMULATED_NoAcceptEndpoint_SeeAtchess1c9dot29", func(t *testing.T) {
		t.Log("DEGRADED/ADAPTED: no HTTP endpoint exists to accept a challenge and link it to the proposed game -- CreateGameFromChallenge (internal/atproto/client.go) is never routed in cmd/protocol/main.go, only plain POST /api/games is. Emulating 'Bob accepts' as bob calling POST /api/games from his OWN protocol-service instance, taking black (the color opposite of what alice requested). This is a faithful analogue of the real accept flow (see bobClient.CreateGameFromChallenge in test/e2e/challenge_test.go, which likewise creates the game in the ACCEPTING player's own repo) but is a separate, distinct API-completeness gap from the two c.pdsURL routing defects this test otherwise pins -- do not count it as a third instance of the same bug.")
		resp := apiPost(t, bob, "/api/games", map[string]interface{}{
			"opponent_did": alice.DID,
			"color":        "black",
		})
		gameURI = recordURI(t, resp, "POST /api/games (as bob, emulated accept)")
		white, _ := resp["white"].(string)
		black, _ := resp["black"].(string)
		t.Logf("game created: uri=%s white=%s black=%s -- record lives in BOB's repo on his PDS (%s), because CreateGameHandler always writes to the authenticated caller's configured repo/PDS (c.did/c.pdsURL), never the opponent's", gameURI, white, black, bob.PDSURL)
	})

	if gameURI == "" {
		t.Fatalf("cannot continue past Accept: no game record was created (see the Accept subtest's own failure output above)")
	}

	// ---------------------------------------------------------------
	// Step 4: shared game state. Both players read the SAME game record,
	// each strictly through their OWN protocol-service instance / PDS, via
	// the public (unauthenticated) GET /api/games/{id}.
	// ---------------------------------------------------------------
	t.Run("SharedGameState", func(t *testing.T) {
		t.Run("BobOwnPDS", func(t *testing.T) {
			// This read may legitimately race the createRecord call that
			// just produced it -- bounded poll, never a bare sleep.
			status, g, body, elapsed := pollGameStatusOK(t, bob.ProtocolURL, gameURI, 5*time.Second)
			if status != http.StatusOK {
				t.Errorf("bob (the game record's home PDS, %s) could not read the game he just created, via his OWN protocol-service instance (%s), even after polling for %s: HTTP %d: %s", bob.PDSURL, bob.ProtocolURL, elapsed, status, body)
				return
			}
			t.Logf("OK: bob's own instance reads the game fine (after %s): fen=%q status=%q", elapsed, g.FEN, g.Status)
		})
		t.Run("AliceForeignPDS", func(t *testing.T) {
			status, g, body, elapsed := pollGameStatusOK(t, alice.ProtocolURL, gameURI, 5*time.Second)
			if status != http.StatusOK {
				t.Errorf("federation defect confirmed: alice's protocol-service instance (%s), configured against her OWN PDS (%s), could not read the game record that actually lives on bob's PDS (%s, repo=%s), even after polling for %s: HTTP %d: %s.\n"+
					"Root cause: atproto.Client.GetGame (internal/atproto/client.go) unconditionally issues com.atproto.repo.getRecord against c.pdsURL instead of resolving the record URI's own repo DID to its actual home PDS.\n"+
					"DID being resolved: %q. PDS actually queried: %s. PDS that should have been queried: %s.",
					alice.ProtocolURL, alice.PDSURL, bob.PDSURL, bob.DID, elapsed, status, body,
					bob.DID, alice.PDSURL, bob.PDSURL)
				return
			}
			t.Logf("unexpected (not today's known bug): alice's instance could read the foreign-PDS game record: fen=%q status=%q", g.FEN, g.Status)
		})
	})

	// ---------------------------------------------------------------
	// Step 6: negative assertion, isolated from the move sequence below so
	// it gives a clean signal independent of whatever the move sequence
	// does or does not manage to land. It is white's turn (starting
	// position); bob is black. Bob's own instance CAN read this game (see
	// SharedGameState/BobOwnPDS above -- it lives in his own repo), so this
	// specifically exercises MakeMoveHandler's turn check, not the
	// cross-PDS read defect.
	// ---------------------------------------------------------------
	t.Run("OutOfTurnRejected", func(t *testing.T) {
		status, body := apiPostExpectStatus(t, bob, "/api/moves", map[string]interface{}{
			"game_id": gameURI,
			"from":    "e7",
			"to":      "e5",
			"fen":     "",
		})
		if status >= 200 && status < 300 {
			t.Errorf("SECURITY: bob (black) was allowed to move e7-e5 when it is white's turn: HTTP %d: %s", status, body)
			return
		}
		if status < 400 || status >= 500 {
			t.Errorf("bob's out-of-turn move was rejected, but not with a 4xx status as required: HTTP %d: %s", status, body)
			return
		}
		t.Logf("OK: bob's out-of-turn move correctly rejected with HTTP %d: %s", status, body)
	})

	// ---------------------------------------------------------------
	// Step 5: play Fool's mate to completion, alternating which player's
	// session submits each move. Each half-move is its own subtest so a
	// failure at move N does not hide whether moves N+1..5 were attempted.
	// The request body's "fen" field is deliberately left blank: the server
	// never trusts client-supplied FEN (MakeMoveHandler fetches the
	// authoritative position itself via GetGame), so it plays no role here.
	// ---------------------------------------------------------------
	type halfMove struct {
		mover      *harness.Player
		moverLabel string
		from, to   string
		san        string // expected SAN, for diagnostic logging only
	}
	moves := []halfMove{
		{alice, "alice(white)", "e2", "e4", "e4"},
		{bob, "bob(black)", "e7", "e5", "e5"},
		{alice, "alice(white)", "d1", "h5", "Qh5"},
		{bob, "bob(black)", "e8", "e7", "Ke7"},
		{alice, "alice(white)", "h5", "e5", "Qxe5#"},
	}

	successfulMoves := 0
	checkmateSeen := false
	var checkmateFENFromMover string

	t.Run("PlayFoolsMate", func(t *testing.T) {
		for i, m := range moves {
			i, m := i, m
			t.Run(fmt.Sprintf("Move%d_%s_%s-%s", i+1, m.moverLabel, m.from, m.to), func(t *testing.T) {
				status, body := apiPostExpectStatus(t, m.mover, "/api/moves", map[string]interface{}{
					"game_id": gameURI,
					"from":    m.from,
					"to":      m.to,
					"fen":     "",
				})
				if status != http.StatusOK {
					cascading := strings.Contains(strings.ToLower(body), "not your turn")
					if cascading {
						t.Errorf("move %d (%s %s-%s, expected SAN %s) rejected with HTTP %d: %s -- this is a CASCADING consequence of an earlier move in this sequence never landing (so the server still thinks it is someone else's turn), not itself a fresh instance of the cross-PDS GetGame defect. See earlier Move subtests in this run for the actual federation failure.", i+1, m.moverLabel, m.from, m.to, m.san, status, body)
					} else {
						t.Errorf("move %d (%s %s-%s, expected SAN %s) rejected with HTTP %d: %s -- consistent with the cross-PDS GetGame defect (see SharedGameState/AliceForeignPDS above): %s's protocol-service instance (%s, own PDS %s) cannot read the game record, which lives on %s's PDS (%s)", i+1, m.moverLabel, m.from, m.to, m.san, status, body, m.moverLabel, m.mover.ProtocolURL, m.mover.PDSURL, bob.Handle, bob.PDSURL)
					}
					return
				}
				successfulMoves++
				var mr moveResultInfo
				if err := json.Unmarshal([]byte(body), &mr); err != nil {
					t.Fatalf("move %d: HTTP 200 but undecodable body: %v (body: %s)", i+1, err, body)
				}
				t.Logf("move %d recorded: %s (check=%v checkmate=%v) fen=%q", i+1, mr.SAN, mr.Check, mr.Checkmate, mr.FEN)
				if mr.Checkmate {
					checkmateSeen = true
					checkmateFENFromMover = mr.FEN
				}

				// After each move that DID land, both players should
				// observe the same FEN and side-to-move, each reading
				// through their own PDS/service. Bounded poll: a
				// same-instance write-then-read could in principle race.
				t.Run("BothViewsAgree", func(t *testing.T) {
					aliceStatus, aliceGame, aliceBody, aliceElapsed := pollGameStatusOK(t, alice.ProtocolURL, gameURI, 5*time.Second)
					bobStatus, bobGame, bobBody, bobElapsed := pollGameStatusOK(t, bob.ProtocolURL, gameURI, 5*time.Second)
					if aliceStatus != http.StatusOK || bobStatus != http.StatusOK {
						t.Errorf("cannot compare views after move %d: alice's view (via %s, after %s) status=%d body=%s; bob's view (via %s, after %s) status=%d body=%s", i+1, alice.ProtocolURL, aliceElapsed, aliceStatus, aliceBody, bob.ProtocolURL, bobElapsed, bobStatus, bobBody)
						return
					}
					if aliceGame.FEN != bobGame.FEN {
						t.Errorf("FEN disagreement after move %d: alice's view (via her PDS %s)=%q, bob's view (via his PDS %s)=%q", i+1, alice.PDSURL, aliceGame.FEN, bob.PDSURL, bobGame.FEN)
						return
					}
					aliceSideToMove := sideToMove(aliceGame.FEN)
					bobSideToMove := sideToMove(bobGame.FEN)
					if aliceSideToMove != bobSideToMove {
						t.Errorf("side-to-move disagreement after move %d: alice's view=%q, bob's view=%q (fen=%q)", i+1, aliceSideToMove, bobSideToMove, aliceGame.FEN)
						return
					}
					t.Logf("OK: both views agree after move %d: fen=%q side-to-move=%q", i+1, aliceGame.FEN, aliceSideToMove)
				})
			})
		}
	})

	// ---------------------------------------------------------------
	// Final state: checkmate, correct winner, from BOTH players' views.
	// ---------------------------------------------------------------
	t.Run("CheckmateBothViews", func(t *testing.T) {
		if !checkmateSeen {
			t.Errorf("game never reached checkmate: only %d/%d scripted half-moves were accepted by the server (see the PlayFoolsMate subtests above for exactly which move failed first, and why). Cannot verify final status or winner from either player's view.", successfulMoves, len(moves))
			return
		}
		aliceStatus, aliceGame, aliceBody, aliceElapsed := pollGameStatusOK(t, alice.ProtocolURL, gameURI, 5*time.Second)
		bobStatus, bobGame, bobBody, bobElapsed := pollGameStatusOK(t, bob.ProtocolURL, gameURI, 5*time.Second)
		if aliceStatus != http.StatusOK || bobStatus != http.StatusOK {
			t.Errorf("checkmate was reported by the final move (fen=%q) but the final game state could not be re-read from both views: alice (after %s) status=%d body=%s; bob (after %s) status=%d body=%s", checkmateFENFromMover, aliceElapsed, aliceStatus, aliceBody, bobElapsed, bobStatus, bobBody)
			return
		}
		if aliceGame.Status != "white_won" {
			t.Errorf("alice's view: expected status=white_won after Qxe5#, got %q (fen=%q)", aliceGame.Status, aliceGame.FEN)
		}
		if bobGame.Status != "white_won" {
			t.Errorf("bob's view: expected status=white_won after Qxe5#, got %q (fen=%q)", bobGame.Status, bobGame.FEN)
		}
		if aliceGame.Status == "white_won" && bobGame.Status == "white_won" {
			t.Logf("OK: both views agree the game ended white_won by checkmate")
		}
	})

	t.Logf("SUMMARY: %d/%d scripted fool's-mate half-moves landed successfully; checkmate reached=%v", successfulMoves, len(moves), checkmateSeen)
}

// sideToMove extracts the active-color field (the 2nd space-delimited
// field) from a FEN string, for comparing "whose turn" two independently
// fetched game views agree on. Returns "" (which will simply fail to equal
// any real side) if fen is not well-formed enough to have one.
func sideToMove(fen string) string {
	parts := strings.Fields(fen)
	if len(parts) < 2 {
		return ""
	}
	return parts[1]
}
