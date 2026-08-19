//go:build e2e

// Ownership conformance test (atchess-1c9.4): pins the defect where
// internal/web/service.go writes every record through a single, config-
// supplied *atproto.Client (s.client) instead of one derived from the
// authenticated caller (see AuthenticatedDID in internal/web/auth_middleware.go,
// which every write handler ignores). This test is EXPECTED TO FAIL against
// today's code. Do not "fix" it by weakening assertions -- the failure output
// is the deliverable. See atchess-1c9.9 for the actual fix.
//
// IMPORTANT SUBTLETY (read before touching this file):
//
// test/harness starts one protocol-service instance PER PLAYER, and each
// instance's s.client is bootstrapped from that SAME player's own account
// (see harness.StartServices / startProtocolService: ATPROTO_HANDLE/PASSWORD
// come from the same Account the harness later logs in as). That means for
// the "obvious" flow -- alice logs into her own instance and acts -- the
// protocol-service instance's static configured identity and the
// authenticated caller's identity are ALWAYS THE SAME DID. Asserting "the
// record landed in the acting player's own repo" against that flow would
// pass today for the wrong reason: not because the code correctly derives
// authorship from AuthenticatedDID, but because in this specific harness
// topology the two identities can never diverge through a legitimate
// alice-only or bob-only flow.
//
// To get a genuinely discriminating assertion, this test provisions a THIRD
// identity ("carol") directly on alice's PDS (the same PDS alice's protocol-
// service instance is configured against) and logs carol into ALICE's
// instance via the normal public /api/auth/login endpoint. LoginHandler
// (internal/web/service.go) authenticates whatever handle+password is
// posted against the instance's configured PDS URL and mints a session for
// whatever DID comes back -- it does not require that identity to match the
// instance's own bootstrap identity. So carol is a real, independently
// authenticated caller whose DID is guaranteed to differ from alice's
// instance's static s.client identity. Any record carol's requests cause to
// be written should, if authorship were correctly attributed, land in
// CAROL's repo -- and today it does not: it lands in alice's (the server
// identity's) repo instead. That is the sharp, unambiguous failure this
// test pins.
//
// What this DOES distinguish: whether a write handler attributes a record
// to the authenticated caller (AuthenticatedDID) or to the protocol-service
// instance's static configured identity (s.client) -- exactly the bug
// atchess-1c9.9 fixes.
//
// What this does NOT distinguish: any bug in how a foreign PDS's repos are
// resolved/read (e.g. atproto.Client.GetGame hardcodes its own configured
// c.pdsURL rather than resolving the record's actual home PDS from its DID
// document), which is a related but distinct defect that can independently
// block cross-PDS reads. Where that surfaces below it is called out
// explicitly and the affected subtest is marked, not silently treated as a
// pass.
//
// NOTICE TO WHOEVER IMPLEMENTS atchess-1c9.9 (per-user atproto client):
// the following subtests currently PASS because they assert TODAY'S BUGGY
// behaviour. Once .9 correctly derives authorship from AuthenticatedDID,
// these subtests are EXPECTED to go red -- that red is correct and means the
// fix worked. As part of .9, invert their assertions (they should assert the
// record now DOES land in the authenticated caller's repo and DOES NOT land
// in the server identity's repo) or delete them outright if they become
// redundant with the "FoundInAuthenticatedAuthorRepo"/"FoundInAuthenticated-
// CreatorRepo" subtests once those start passing:
//
//   - Challenge/CHARACTERIZATION_InvertWhenAtchess1c9dot9Lands
//   - Game/CHARACTERIZATION_InvertWhenAtchess1c9dot9Lands
//   - Move/CHARACTERIZATION_AuthenticatedCreatorLockedOutOfOwnGame_InvertWhenAtchess1c9dot9Lands
//
// Additionally, Move/NonDiscriminating_FoundInMoverRepo proves nothing about
// correct attribution today (see its own comment below) -- a real,
// discriminating move-ownership assertion (analogous to the carol-based
// Challenge/Game cases) belongs in atchess-1c9.9's own acceptance criteria,
// once carol can actually participate in a game.
package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/justinabrahms/atchess/test/harness"
)

// healthInfo mirrors the relevant fields of GET /api/health's response
// (internal/web/service.go HealthHandler): the DID and handle the protocol-
// service instance itself is configured to act as.
type healthInfo struct {
	DID    string `json:"did"`
	Handle string `json:"handle"`
}

func getHealth(t *testing.T, baseURL string) healthInfo {
	t.Helper()
	resp, err := http.Get(baseURL + "/api/health")
	if err != nil {
		t.Fatalf("GET %s/api/health failed: %v", baseURL, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s/api/health returned HTTP %d: %s", baseURL, resp.StatusCode, string(body))
	}
	var h healthInfo
	if err := json.Unmarshal(body, &h); err != nil {
		t.Fatalf("failed to decode /api/health response from %s: %v (body: %s)", baseURL, err, string(body))
	}
	if h.DID == "" {
		t.Fatalf("/api/health from %s returned an empty did (body: %s)", baseURL, string(body))
	}
	return h
}

// createThirdPartyAccount provisions (idempotently) an account directly on
// pdsURL via the raw com.atproto.server.createAccount XRPC endpoint --
// deliberately bypassing scripts/create-dual-pds-accounts.sh, which only
// ever provisions alice and bob. This is how the test constructs "carol": a
// real, independently authenticated identity that lives on the SAME PDS as
// one of the harness's protocol-service instances but is NOT the identity
// that instance was bootstrapped with.
func createThirdPartyAccount(t *testing.T, pdsURL, handle, email, password string) harness.Account {
	t.Helper()

	reqBody, _ := json.Marshal(map[string]string{
		"email":    email,
		"handle":   handle,
		"password": password,
	})
	resp, err := http.Post(pdsURL+"/xrpc/com.atproto.server.createAccount", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatalf("createAccount request to %s for %s failed: %v", pdsURL, handle, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusOK {
		var created struct {
			DID string `json:"did"`
		}
		if err := json.Unmarshal(body, &created); err != nil || created.DID == "" {
			t.Fatalf("createAccount(%s) against %s returned HTTP 200 but no usable did (body: %s, err: %v)", handle, pdsURL, string(body), err)
		}
		return harness.Account{Handle: handle, DID: created.DID, PDSURL: pdsURL, Password: password}
	}

	// Idempotency: if the account already exists (e.g. a prior run left it
	// behind because the PDS data volume was not reset), verify the
	// password still works and reuse it rather than failing the whole test.
	if resp.StatusCode == http.StatusBadRequest && strings.Contains(strings.ToLower(string(body)), "already taken") {
		sessReqBody, _ := json.Marshal(map[string]string{"identifier": handle, "password": password})
		sessResp, err := http.Post(pdsURL+"/xrpc/com.atproto.server.createSession", "application/json", bytes.NewReader(sessReqBody))
		if err != nil {
			t.Fatalf("createSession fallback to %s for existing account %s failed: %v", pdsURL, handle, err)
		}
		defer sessResp.Body.Close()
		sessBody, _ := io.ReadAll(sessResp.Body)
		if sessResp.StatusCode != http.StatusOK {
			t.Fatalf("account %s already exists on %s but its password does not match the harness's (createSession HTTP %d: %s); reset the PDS data volume (make test-federation-down) and retry", handle, pdsURL, sessResp.StatusCode, string(sessBody))
		}
		var sess struct {
			DID string `json:"did"`
		}
		if err := json.Unmarshal(sessBody, &sess); err != nil || sess.DID == "" {
			t.Fatalf("createSession(%s) against %s returned HTTP 200 but no usable did (body: %s, err: %v)", handle, pdsURL, string(sessBody), err)
		}
		return harness.Account{Handle: handle, DID: sess.DID, PDSURL: pdsURL, Password: password}
	}

	t.Fatalf("createAccount(%s) against %s returned HTTP %d: %s", handle, pdsURL, resp.StatusCode, string(body))
	return harness.Account{} // unreachable
}

// apiPost POSTs a JSON body to path on player's protocol-service instance,
// authenticated as player, and decodes the response into a
// map[string]interface{} so this test does not need to import internal/...
// response types (matching the harness's own policy of exercising only the
// public HTTP boundary). Fails the test loudly on transport errors or a
// non-200 response, including the response body so a bad request is
// diagnosable without re-running.
func apiPost(t *testing.T, player *harness.Player, path string, reqBody interface{}) map[string]interface{} {
	t.Helper()
	b, err := json.Marshal(reqBody)
	if err != nil {
		t.Fatalf("failed to marshal request body for POST %s%s: %v", player.ProtocolURL, path, err)
	}
	resp, err := player.HTTPClient.Post(player.ProtocolURL+path, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST %s%s (as %s) failed: %v", player.ProtocolURL, path, player.Handle, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST %s%s (as %s, did=%s) returned HTTP %d: %s", player.ProtocolURL, path, player.Handle, player.DID, resp.StatusCode, string(respBody))
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal(respBody, &decoded); err != nil {
		t.Fatalf("failed to decode POST %s%s response as JSON: %v (body: %s)", player.ProtocolURL, path, err, string(respBody))
	}
	return decoded
}

// apiPostExpectStatus is like apiPost but is used where a non-200 response
// is the expectation being tested; it returns the raw status code and body
// instead of failing the test on a non-200.
func apiPostExpectStatus(t *testing.T, player *harness.Player, path string, reqBody interface{}) (int, string) {
	t.Helper()
	b, err := json.Marshal(reqBody)
	if err != nil {
		t.Fatalf("failed to marshal request body for POST %s%s: %v", player.ProtocolURL, path, err)
	}
	resp, err := player.HTTPClient.Post(player.ProtocolURL+path, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST %s%s (as %s) failed: %v", player.ProtocolURL, path, player.Handle, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(respBody)
}

// recordURI pulls a URI-shaped identifier out of a decoded JSON response,
// trying both "id"/"ID" (the two casings used across this codebase's
// response types -- chess.Game has a `json:"id"` tag, chess.Challenge has
// no json tags at all so Go's default marshaling capitalizes the field to
// "ID") so this test does not silently misparse one or the other.
func recordURI(t *testing.T, decoded map[string]interface{}, context string) string {
	t.Helper()
	for _, key := range []string{"id", "ID"} {
		if v, ok := decoded[key]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	t.Fatalf("%s: response had neither a usable \"id\" nor \"ID\" field: %+v", context, decoded)
	return ""
}

// rkeyOf extracts the record key (final path segment) from an AT Protocol
// URI of the form at://did/collection/rkey.
func rkeyOf(t *testing.T, uri string) string {
	t.Helper()
	parts := strings.Split(uri, "/")
	if len(parts) < 5 || !strings.HasPrefix(uri, "at://") {
		t.Fatalf("not a well-formed at:// record URI: %q", uri)
	}
	return parts[len(parts)-1]
}

// assertFoundIn asserts that collection/rkey exists in expected's own repo
// (read directly from expected's PDS, bypassing the protocol service). On
// failure it searches every repo in others too, so the failure message
// names the expected repo DID, every repo actually searched, and where (if
// anywhere) the record was actually found -- diagnosable from CI output
// alone.
func assertFoundIn(t *testing.T, expected *harness.Player, expectedLabel, collection, rkey string, others map[string]*harness.Player) {
	t.Helper()
	if rec, err := expected.RepoGetRecord(collection, rkey); err == nil {
		t.Logf("OK: %s/%s found in %s's repo (did=%s) as expected: %+v", collection, rkey, expectedLabel, expected.DID, rec)
		return
	} else {
		var found []string
		var foundWhere string
		searched := []string{fmt.Sprintf("%s (did=%s, pds=%s): not found (%v)", expectedLabel, expected.DID, expected.PDSURL, err)}
		for label, p := range others {
			if rec2, err2 := p.RepoGetRecord(collection, rkey); err2 == nil {
				found = append(found, fmt.Sprintf("%s (did=%s)", label, p.DID))
				foundWhere = fmt.Sprintf("%+v", rec2)
				searched = append(searched, fmt.Sprintf("%s (did=%s, pds=%s): FOUND", label, p.DID, p.PDSURL))
			} else {
				searched = append(searched, fmt.Sprintf("%s (did=%s, pds=%s): not found (%v)", label, p.DID, p.PDSURL, err2))
			}
		}
		if len(found) > 0 {
			t.Errorf("ownership violation: expected %s/%s in %s's repo (did=%s) -- the AUTHENTICATED author -- but it is NOT there. It was instead found in: %s (value: %s). Repos searched:\n  %s",
				collection, rkey, expectedLabel, expected.DID, strings.Join(found, ", "), foundWhere, strings.Join(searched, "\n  "))
		} else {
			t.Errorf("ownership check inconclusive: expected %s/%s in %s's repo (did=%s) but it was not found there, and not found in any other searched repo either. Repos searched:\n  %s",
				collection, rkey, expectedLabel, expected.DID, strings.Join(searched, "\n  "))
		}
	}
}

// assertNotFoundIn asserts that collection/rkey does NOT exist in player's
// own repo, naming what was (if anything) actually found there on failure.
func assertNotFoundIn(t *testing.T, player *harness.Player, label, collection, rkey string) {
	t.Helper()
	rec, err := player.RepoGetRecord(collection, rkey)
	if err != nil {
		t.Logf("OK: %s/%s correctly absent from %s's repo (did=%s): %v", collection, rkey, label, player.DID, err)
		return
	}
	t.Errorf("ownership violation: %s/%s should NOT be in %s's repo (did=%s) but was found there: %+v", collection, rkey, label, player.DID, rec)
}

// TestOwnership pins atchess-1c9.4: every app.atchess.* record must land in
// its AUTHENTICATED AUTHOR's own AT Protocol repo, never in the protocol-
// service instance's static configured identity's repo and never in a
// counterparty's or bystander's repo. This is expected to FAIL today; see
// the package doc comment above for exactly what is and is not
// distinguishable given this harness's topology.
func TestOwnership(t *testing.T) {
	accounts := harness.LoadAccounts(t)
	services := harness.StartServices(t, accounts)

	alice := harness.NewPlayer(t, accounts.Alice, services.AliceURL)
	bob := harness.NewPlayer(t, accounts.Bob, services.BobURL)

	aliceHealth := getHealth(t, services.AliceURL)
	bobHealth := getHealth(t, services.BobURL)
	t.Logf("alice's protocol-service instance (%s) reports server identity did=%s handle=%s", services.AliceURL, aliceHealth.DID, aliceHealth.Handle)
	t.Logf("bob's protocol-service instance (%s) reports server identity did=%s handle=%s", services.BobURL, bobHealth.DID, bobHealth.Handle)

	// Document (and fail loudly, rather than silently assume) the harness
	// property this whole test's design depends on: each instance's static
	// configured identity is bootstrapped as its own player, so alice's own
	// instance's server identity == alice's own DID, and likewise for bob.
	// This is exactly why an alice-only or bob-only flow can never, by
	// itself, distinguish "authored by the authenticated caller" from
	// "authored by the server's static identity" -- see carol below.
	if aliceHealth.DID != alice.DID {
		t.Fatalf("harness assumption violated: alice's protocol-service instance server identity (%s) does not equal alice's own DID (%s) -- the carol-based discriminating assertions below assume these match", aliceHealth.DID, alice.DID)
	}
	if bobHealth.DID != bob.DID {
		t.Fatalf("harness assumption violated: bob's protocol-service instance server identity (%s) does not equal bob's own DID (%s)", bobHealth.DID, bob.DID)
	}

	// carol: a third, independently authenticated identity, provisioned
	// directly on alice's PDS and logged into ALICE's protocol-service
	// instance. Her DID is guaranteed to differ from that instance's static
	// server identity (alice's own DID) -- this is the sharpest available
	// discriminant given this harness's one-instance-per-player topology.
	carolAccount := createThirdPartyAccount(t, accounts.Alice.PDSURL, "carol.test", "carol@chess.test", "carol-harness-pass-1")
	carol := harness.NewPlayer(t, carolAccount, services.AliceURL)
	if carol.DID == aliceHealth.DID {
		t.Fatalf("test setup bug: carol's DID (%s) collided with alice's server identity DID (%s); cannot use carol as a discriminating identity", carol.DID, aliceHealth.DID)
	}
	t.Logf("carol provisioned on alice's PDS (%s), did=%s, logged into alice's protocol-service instance (%s) whose server identity is did=%s", accounts.Alice.PDSURL, carol.DID, services.AliceURL, aliceHealth.DID)

	others := func(exclude ...*harness.Player) map[string]*harness.Player {
		all := map[string]*harness.Player{"alice": alice, "bob": bob, "carol": carol}
		labelOf := map[*harness.Player]string{alice: "alice", bob: "bob", carol: "carol"}
		for _, ex := range exclude {
			delete(all, labelOf[ex])
		}
		return all
	}

	// ---------------------------------------------------------------
	// Challenge ownership
	// ---------------------------------------------------------------
	t.Run("Challenge", func(t *testing.T) {
		// carol (authenticated on alice's instance) challenges bob. If
		// authorship were correctly derived from AuthenticatedDID, this
		// record would land in carol's own repo.
		resp := apiPost(t, carol, "/api/challenges", map[string]interface{}{
			"opponent_did": bob.DID,
			"color":        "white",
			"message":      "ownership-test challenge",
		})
		uri := recordURI(t, resp, "POST /api/challenges (as carol)")
		rkey := rkeyOf(t, uri)
		t.Logf("challenge created: uri=%s rkey=%s", uri, rkey)

		t.Run("FoundInAuthenticatedAuthorRepo", func(t *testing.T) {
			assertFoundIn(t, carol, "carol (authenticated author)", "app.atchess.challenge", rkey, others(carol))
		})
		t.Run("NotInOpponentRepo", func(t *testing.T) {
			assertNotFoundIn(t, bob, "bob (opponent)", "app.atchess.challenge", rkey)
		})
		t.Run("CHARACTERIZATION_InvertWhenAtchess1c9dot9Lands", func(t *testing.T) {
			// Confirmatory/diagnostic: shows exactly where the record
			// actually went, so the misroute above is not just "missing"
			// but demonstrably redirected to the server's own identity.
			//
			// THIS SUBTEST ASSERTS TODAY'S BUGGY BEHAVIOUR ON PURPOSE. It is
			// a characterization test, not a correctness gate. See the
			// "NOTICE TO WHOEVER IMPLEMENTS atchess-1c9.9" section in this
			// file's package doc comment.
			rec, err := alice.RepoGetRecord("app.atchess.challenge", rkey)
			if err != nil {
				t.Errorf("IF YOU ARE IMPLEMENTING atchess-1c9.9: this red is EXPECTED and CORRECT. It means the fix worked. Invert or delete this subtest as part of .9.\n\n"+
					"(diagnostic detail: expected the misrouted record in the server identity's repo (alice, did=%s) but it is not there either (getRecord: %v) -- if you are NOT implementing .9 and are seeing this, it means the record is missing entirely from BOTH the authenticated author's repo AND the server identity's repo, which would be a separate data-loss concern)", aliceHealth.DID, err)
				return
			}
			t.Logf("CONFIRMED misroute (characterization, expected to invert once atchess-1c9.9 lands): challenge authored by carol (did=%s) via alice's protocol-service instance landed in that instance's static server identity's repo (alice, did=%s) instead: %+v", carol.DID, aliceHealth.DID, rec)
		})
	})

	// ---------------------------------------------------------------
	// Game ownership
	// ---------------------------------------------------------------
	var carolCreatedGameURI string
	t.Run("Game", func(t *testing.T) {
		resp := apiPost(t, carol, "/api/games", map[string]interface{}{
			"opponent_did": bob.DID,
			"color":        "white",
		})
		uri := recordURI(t, resp, "POST /api/games (as carol)")
		carolCreatedGameURI = uri
		rkey := rkeyOf(t, uri)
		t.Logf("game created: uri=%s rkey=%s", uri, rkey)

		t.Run("FoundInAuthenticatedCreatorRepo", func(t *testing.T) {
			assertFoundIn(t, carol, "carol (authenticated creator)", "app.atchess.game", rkey, others(carol))
		})
		t.Run("NotInOpponentRepo", func(t *testing.T) {
			assertNotFoundIn(t, bob, "bob (opponent)", "app.atchess.game", rkey)
		})
		t.Run("CHARACTERIZATION_InvertWhenAtchess1c9dot9Lands", func(t *testing.T) {
			// THIS SUBTEST ASSERTS TODAY'S BUGGY BEHAVIOUR ON PURPOSE. It is
			// a characterization test, not a correctness gate. See the
			// "NOTICE TO WHOEVER IMPLEMENTS atchess-1c9.9" section in this
			// file's package doc comment.
			rec, err := alice.RepoGetRecord("app.atchess.game", rkey)
			if err != nil {
				t.Errorf("IF YOU ARE IMPLEMENTING atchess-1c9.9: this red is EXPECTED and CORRECT. It means the fix worked. Invert or delete this subtest as part of .9.\n\n"+
					"(diagnostic detail: expected the misrouted record in the server identity's repo (alice, did=%s) but it is not there either (getRecord: %v) -- if you are NOT implementing .9 and are seeing this, it means the record is missing entirely from BOTH the authenticated author's repo AND the server identity's repo, which would be a separate data-loss concern)", aliceHealth.DID, err)
				return
			}
			white, _ := rec["white"].(string)
			black, _ := rec["black"].(string)
			t.Logf("CONFIRMED misroute (characterization, expected to invert once atchess-1c9.9 lands): game authored by carol (did=%s) via alice's protocol-service instance landed in alice's repo (did=%s) instead, AND lists white=%s black=%s -- carol is not even recorded as a player in the game she created", carol.DID, aliceHealth.DID, white, black)
		})
	})

	// ---------------------------------------------------------------
	// Move ownership
	// ---------------------------------------------------------------
	t.Run("Move", func(t *testing.T) {
		// This sub-case uses alice's OWN instance with alice as both the
		// authenticated caller and the game's white player -- a legitimate
		// full move flow, but NOT a discriminating case (see package doc):
		// alice's instance's server identity already equals alice, so this
		// cannot, by itself, tell "derived from AuthenticatedDID" apart
		// from "derived from s.client". It is included because it is
		// literally what the brief for this bead specifies, and it does
		// still verify the record isn't duplicated into bob's or a
		// bystander's repo. Expect this sub-case to PASS today.
		gameResp := apiPost(t, alice, "/api/games", map[string]interface{}{
			"opponent_did": bob.DID,
			"color":        "white",
		})
		gameURI := recordURI(t, gameResp, "POST /api/games (as alice)")
		t.Logf("alice/bob game for move test: uri=%s", gameURI)

		moveResp := apiPost(t, alice, "/api/moves", map[string]interface{}{
			"game_id": gameURI,
			"from":    "e2",
			"to":      "e4",
			"fen":     "rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR w KQkq - 0 1",
		})
		san, _ := moveResp["san"].(string)
		t.Logf("alice's move recorded: san=%s", san)

		// The move response (chess.MoveResult) carries no record id/uri, so
		// find the created app.atchess.move record by content instead. The
		// match is scoped to THIS game's URI (record["game"]["uri"]), not
		// just from/to, because "carol" reuses accounts across runs on a
		// persisted PDS volume (see createThirdPartyAccount) and bob may
		// have played e2->e4 in some other game in a prior or concurrent
		// run; matching by move coordinates alone could find a stale or
		// unrelated record and produce a false pass/fail.
		findMove := func(t *testing.T, p *harness.Player, forGameURI string) (map[string]interface{}, bool) {
			t.Helper()
			recs, err := p.RepoListRecords("app.atchess.move")
			if err != nil {
				return nil, false
			}
			for _, r := range recs {
				from, _ := r["from"].(string)
				to, _ := r["to"].(string)
				if from != "e2" || to != "e4" {
					continue
				}
				gameRef, _ := r["game"].(map[string]interface{})
				gameURI, _ := gameRef["uri"].(string)
				if gameURI == forGameURI {
					return r, true
				}
			}
			return nil, false
		}

		// NonDiscriminating: alice is both the authenticated caller AND
		// this instance's static server identity (see the harness-topology
		// caveat in the package doc comment), so this subtest cannot tell
		// "attributed to AuthenticatedDID" apart from "attributed to
		// s.client" -- it is expected to PASS today for a reason that
		// proves nothing about correct attribution. Do not read a PASS here
		// as move-ownership coverage; it only confirms the record isn't
		// ALSO duplicated into bob's or carol's repo (see the two subtests
		// below). A real discriminating move assertion belongs to
		// atchess-1c9.9 (see the package doc comment).
		t.Run("NonDiscriminating_FoundInMoverRepo", func(t *testing.T) {
			rec, ok := findMove(t, alice, gameURI)
			if !ok {
				t.Errorf("ownership violation (or non-discriminating pass masking a deeper issue): expected an app.atchess.move record for game=%s with from=e2 to=e4 in alice's own repo (did=%s, the moving player), but listRecords(app.atchess.move) against alice's PDS (%s) did not contain it", gameURI, alice.DID, alice.PDSURL)
				return
			}
			t.Logf("NON-DISCRIMINATING PASS: move record found in alice's (mover's) repo: %+v -- this proves NOTHING about correct attribution, because alice-the-authenticated-caller and alice-the-server-identity are the SAME DID in this sub-case; it only confirms the record was not also duplicated elsewhere (see NotInOpponentRepo/NotInThirdPartyRepo below)", rec)
		})
		t.Run("NotInOpponentRepo", func(t *testing.T) {
			if _, ok := findMove(t, bob, gameURI); ok {
				t.Errorf("ownership violation: move record (game=%s from=e2 to=e4) unexpectedly found in bob's repo (did=%s), the opponent, not the mover", gameURI, bob.DID)
				return
			}
			t.Logf("OK: move record correctly absent from bob's (opponent's) repo")
		})
		t.Run("NotInThirdPartyRepo", func(t *testing.T) {
			if _, ok := findMove(t, carol, gameURI); ok {
				t.Errorf("ownership violation: move record (game=%s from=e2 to=e4) unexpectedly found in carol's repo (did=%s), an unrelated third party", gameURI, carol.DID)
				return
			}
			t.Logf("OK: move record correctly absent from carol's (third party's) repo")
		})

		// Downstream consequence of the Game subtest's failure: carol
		// created a game via alice's instance believing herself to be a
		// participant, but (per Game/CHARACTERIZATION_InvertWhenAtchess1c9dot9Lands
		// above) the resulting record lists alice/bob as white/black, not
		// carol. MakeMoveHandler's turn/participant check DOES correctly
		// consult AuthenticatedDID (internal/web/service.go
		// MakeMoveHandler: `authedDID := AuthenticatedDID(r)`), so carol is
		// correctly rejected here -- but only because the game she created
		// was never actually attributed to her in the first place.
		//
		// THIS SUBTEST ASSERTS TODAY'S BUGGY BEHAVIOUR ON PURPOSE. It is a
		// characterization test, not a correctness gate. Once
		// atchess-1c9.9 lands, carol will be correctly attributed as white
		// in the game she created and this move will succeed (HTTP 200),
		// not fail with 403. See the "NOTICE TO WHOEVER IMPLEMENTS
		// atchess-1c9.9" section in this file's package doc comment.
		t.Run("CHARACTERIZATION_AuthenticatedCreatorLockedOutOfOwnGame_InvertWhenAtchess1c9dot9Lands", func(t *testing.T) {
			if carolCreatedGameURI == "" {
				t.Skip("Game subtest did not produce a game URI to probe (see its own failure output)")
			}
			status, body := apiPostExpectStatus(t, carol, "/api/moves", map[string]interface{}{
				"game_id": carolCreatedGameURI,
				"from":    "e2",
				"to":      "e4",
				"fen":     "rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR w KQkq - 0 1",
			})
			if status != http.StatusForbidden {
				t.Errorf("IF YOU ARE IMPLEMENTING atchess-1c9.9: this red is EXPECTED and CORRECT. It means the fix worked -- carol is now correctly attributed as a participant (white) in the game she created, so her move succeeds instead of being rejected. Invert or delete this subtest as part of .9.\n\n"+
					"(diagnostic detail: expected HTTP 403 (carol rejected as a non-participant in the game she herself created, since it was actually attributed to alice/bob), got HTTP %d: %s)", status, body)
				return
			}
			t.Logf("CONFIRMED downstream consequence (characterization, expected to invert once atchess-1c9.9 lands): carol authenticated and created a game via POST /api/games, but cannot ever move in it (HTTP 403: %s) because CreateGameHandler attributed it to the server identity (alice), not to her", body)
		})
	})
}
