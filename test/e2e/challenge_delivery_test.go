//go:build e2e

// Challenge delivery conformance test (atchess-1c9.6): pins the defect
// where a challenge created by one player is never observable by the
// challenged player through the challenged player's own
// GET /api/challenge-notifications, given this harness's
// one-instance-per-player topology (the same topology test/harness uses
// throughout this epic to model two independently-run, cross-PDS
// deployments -- see test/harness/services.go's doc comment).
//
// ROOT CAUSE, as verified by reading the code (this is worth reading in
// full before touching this file -- it is NOT the single bug the bead's
// input description names; there are two independent, stacked defects):
//
//  1. internal/atproto/client.go's CreateChallengeNotification (~line 723)
//     issues com.atproto.repo.createRecord with "repo": challengedDID
//     against c.pdsURL (the CALLER's own configured PDS) -- an attempt to
//     write into a repo AT Protocol does not permit you to write into (you
//     may only write to your own repo, on your own PDS, with your own
//     session credentials). It is invoked as a best-effort side effect of
//     CreateChallenge (client.go ~line 388) and its error is swallowed
//     (printed to stdout, never surfaced to the HTTP caller) -- so
//     POST /api/challenges reports success even though the notification
//     write below it silently failed.
//
//  2. Separately -- and this is what actually determines what THIS test
//     observes over HTTP -- internal/web/service.go's
//     GetChallengeNotificationsHandler does NOT read via
//     atproto.Client.GetChallengeNotifications (the repo-record path
//     described in the bead's input) at all, whenever a
//     *challenge.Store is configured. cmd/protocol/main.go ALWAYS
//     configures one (challenge.NewStore()), so this handler always takes
//     the s.challengeStore.ForPlayer(s.client.GetDID()) branch instead:
//     an IN-MEMORY, IN-PROCESS index (see internal/challenge/store.go's own
//     doc comment: "This solves the fundamental AT Protocol limitation
//     where you cannot write to another user's repository ... we maintain
//     a local index"). CreateChallengeHandler adds to that same in-memory
//     Store, but only on the CHALLENGER's own protocol-service instance's
//     process. test/harness starts one protocol-service instance PER
//     PLAYER (see test/harness/services.go) -- exactly modeling two
//     independently-run, cross-PDS deployments, which is this epic's whole
//     point. The challenged player's instance is a wholly separate OS
//     process with its own, empty Store. The only mechanism that could
//     bridge the two processes' Stores is the firehose
//     (internal/firehose), which test/harness unconditionally disables
//     (FIREHOSE_ENABLED=false, see test/harness/services.go) and which, even
//     enabled, requires a real relay/BGS to carry events between two
//     independent PDSes -- not present in this hermetic stack.
//
//     So: notification delivery has TWO independent, stacked failures, not
//     one. The cross-repo record write described in the bead's input is
//     itself rejected by AT Protocol (root-caused in finding 1, confirmed
//     empirically below) -- but even if it somehow succeeded, the HTTP
//     surface this test exercises (GET /api/challenge-notifications)
//     would still not consult that record, because it reads an in-process
//     Store that was never populated on the challenged player's instance.
//     atchess-1c9.11's redesign needs to account for BOTH.
//
//  3. DELETE /api/challenge-notifications/{key} ("decline") is wired only
//     to the legacy repo-record deletion path
//     (atproto.Client.DeleteChallengeNotification, which requires its
//     input to be a full "at://did/.../rkey" URI), but mux's
//     {key} route segment (and the frontend's own
//     `uriParts[uriParts.length-1]`, web/static/index.html) can only ever
//     supply the bare rkey, never a full URI. So this endpoint is
//     structurally unable to succeed regardless of federation, wired to a
//     record-index mechanism (challenge.Store) that has no delete route at
//     all. This is a third, distinct, pre-existing gap; reported below,
//     not fixed here.
//
// This test is EXPECTED TO FAIL today. Do not "fix" production code to
// make it pass -- the failure output is the deliverable. See
// atchess-1c9.11, which owns the redesign these findings feed.
package e2e

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/justinabrahms/atchess/test/harness"
)

// pendingChallengeInfo mirrors internal/challenge.PendingChallenge's JSON
// shape exactly (duplicated here rather than imported, matching this
// suite's established convention of exercising only the public HTTP
// boundary -- see healthInfo in ownership_test.go). This is the type
// GetChallengeNotificationsHandler actually serializes today, given
// cmd/protocol/main.go always configures a *challenge.Store (see the
// package doc comment above, finding 2). It deliberately does NOT match
// web/static/index.html's displayChallenges(), which still expects the
// OLD atproto.ChallengeNotification shape (capitalized "URI",
// "Challenger", "ChallengerHandle", "Color" fields, no json tags) -- that
// mismatch is itself a latent, separate bug this test does not attempt to
// characterize further, noted here only so a future reader does not
// mistake this struct's field names for a typo.
type pendingChallengeInfo struct {
	ChallengeURI     string `json:"challengeUri"`
	ChallengeCID     string `json:"challengeCid"`
	ChallengerDID    string `json:"challengerDid"`
	ChallengerHandle string `json:"challengerHandle"`
	ChallengedDID    string `json:"challengedDid"`
	Color            string `json:"color"`
	Message          string `json:"message,omitempty"`
	ProposedGameID   string `json:"proposedGameId,omitempty"`
	CreatedAt        string `json:"createdAt"`
	ExpiresAt        string `json:"expiresAt"`
}

// getChallengeNotifications performs a single GET /api/challenge-notifications
// against player's own protocol-service instance, authenticated as player,
// and returns the raw status, decoded list (nil on non-200 or a decode
// failure), and the raw body for diagnostics.
func getChallengeNotifications(t *testing.T, player *harness.Player) (status int, notifications []pendingChallengeInfo, body string) {
	t.Helper()
	resp, err := player.HTTPClient.Get(player.ProtocolURL + "/api/challenge-notifications")
	if err != nil {
		t.Fatalf("GET %s/api/challenge-notifications (as %s) failed: %v", player.ProtocolURL, player.Handle, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	body = string(raw)
	status = resp.StatusCode
	if status != http.StatusOK {
		return
	}
	if err := json.Unmarshal(raw, &notifications); err != nil {
		t.Fatalf("GET %s/api/challenge-notifications (as %s) returned HTTP 200 but an undecodable body: %v (body: %s)", player.ProtocolURL, player.Handle, err, body)
	}
	return
}

// pollChallengeNotifications polls getChallengeNotifications against player
// until it returns at least one notification or deadline elapses -- never a
// bare sleep-and-hope, and always bounded, so a read that legitimately
// races a very recent write gets a fair chance to observe it. On timeout it
// returns the LAST observed status/notifications/body (which may reflect a
// genuine, permanent delivery failure rather than a transient race), along
// with the elapsed time, so callers can report both.
func pollChallengeNotifications(t *testing.T, player *harness.Player, deadline time.Duration) (status int, notifications []pendingChallengeInfo, body string, elapsed time.Duration) {
	t.Helper()
	start := time.Now()
	interval := 200 * time.Millisecond
	for {
		status, notifications, body = getChallengeNotifications(t, player)
		elapsed = time.Since(start)
		if status == http.StatusOK && len(notifications) > 0 {
			return
		}
		if elapsed >= deadline {
			return
		}
		time.Sleep(interval)
	}
}

// createSessionRaw authenticates directly against pdsURL via the raw
// com.atproto.server.createSession XRPC endpoint (bypassing both
// test/harness's protocol-service session and internal/atproto.Client),
// so this test can replicate -- and independently capture the verbatim PDS
// response to -- the exact cross-repo write CreateChallengeNotification
// (internal/atproto/client.go) performs internally as a swallowed,
// unobservable side effect of POST /api/challenges.
func createSessionRaw(t *testing.T, pdsURL, handle, password string) (accessJwt, did string) {
	t.Helper()
	reqBody, _ := json.Marshal(map[string]string{"identifier": handle, "password": password})
	resp, err := http.Post(pdsURL+"/xrpc/com.atproto.server.createSession", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatalf("createSession against %s for %s failed: %v", pdsURL, handle, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("createSession against %s for %s returned HTTP %d: %s", pdsURL, handle, resp.StatusCode, string(body))
	}
	var session struct {
		AccessJwt string `json:"accessJwt"`
		Did       string `json:"did"`
	}
	if err := json.Unmarshal(body, &session); err != nil || session.AccessJwt == "" || session.Did == "" {
		t.Fatalf("createSession against %s for %s returned HTTP 200 but no usable session (body: %s, err: %v)", pdsURL, handle, string(body), err)
	}
	return session.AccessJwt, session.Did
}

// rawCreateRecordAttempt issues com.atproto.repo.createRecord against
// pdsURL, authenticated with accessJwt, for the given repo/collection/
// record -- the exact same shape of call CreateChallengeNotification makes
// -- and returns the verbatim HTTP status and response body. It never
// fails the test itself: capturing the actual response (success, auth
// rejection, or something else entirely) is the point.
func rawCreateRecordAttempt(t *testing.T, pdsURL, accessJwt, repo, collection string, record map[string]interface{}) (status int, body string) {
	t.Helper()
	reqBody, err := json.Marshal(map[string]interface{}{
		"repo":       repo,
		"collection": collection,
		"record":     record,
	})
	if err != nil {
		t.Fatalf("failed to marshal createRecord request: %v", err)
	}
	req, err := http.NewRequest("POST", pdsURL+"/xrpc/com.atproto.repo.createRecord", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatalf("failed to build createRecord request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessJwt)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("createRecord request to %s (repo=%s, collection=%s) failed: %v", pdsURL, repo, collection, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// TestChallengeDelivery pins atchess-1c9.6: alice (PDS-A) challenges bob
// (PDS-B) by handle, and bob -- authenticated as himself, against his own
// protocol-service instance and his own PDS -- must see exactly one
// pending challenge from alice, be able to decline it, and no longer see
// it afterward; alice must never see her own outbound challenge in her own
// inbound list. See the package doc comment above for the two (three,
// counting decline) independent defects this is expected to surface.
func TestChallengeDelivery(t *testing.T) {
	accounts := harness.LoadAccounts(t)
	services := harness.StartServices(t, accounts)

	alice := harness.NewPlayer(t, accounts.Alice, services.AliceURL)
	bob := harness.NewPlayer(t, accounts.Bob, services.BobURL)

	t.Logf("alice: did=%s pds=%s protocol=%s", alice.DID, alice.PDSURL, alice.ProtocolURL)
	t.Logf("bob:   did=%s pds=%s protocol=%s", bob.DID, bob.PDSURL, bob.ProtocolURL)

	// ---------------------------------------------------------------
	// Step 2: alice challenges bob BY HANDLE. Handle resolution across
	// PDSes was independently pinned as broken in atchess-1c9.5
	// (ResolveHandle queries c.pdsURL, alice's own PDS, which cannot
	// resolve a handle hosted on bob's PDS). Per this bead's brief, use
	// the same degraded-fallback technique: if resolving by handle fails,
	// fall back to bob's known DID (from test/.harness-accounts.json) and
	// log loudly, so that failure does not mask the delivery defect this
	// test exists to pin.
	// ---------------------------------------------------------------
	var challengeURI string
	handleResolutionOK := false
	t.Run("AliceChallengesBobByHandle_PassesEvenIfDegradedToDIDFallback_SeeAtchess1c9dot5", func(t *testing.T) {
		status, body := apiPostExpectStatus(t, alice, "/api/challenges", map[string]interface{}{
			"opponent_did": accounts.Bob.Handle,
			"color":        "white",
			"message":      "challenge-delivery test challenge",
		})
		if status != http.StatusOK {
			t.Logf("DEGRADED: challenging bob by handle (%q) failed (HTTP %d: %s) -- consistent with the atchess-1c9.5 handle-resolution defect (ResolveHandle queries alice's own PDS, %s, which cannot resolve a handle hosted on bob's PDS, %s). Falling back to bob's known DID.", accounts.Bob.Handle, status, body, alice.PDSURL, accounts.Bob.PDSURL)
			return
		}
		var decoded map[string]interface{}
		if err := json.Unmarshal([]byte(body), &decoded); err != nil {
			t.Fatalf("challenge-by-handle: HTTP 200 but undecodable body: %v (body: %s)", err, body)
		}
		challengeURI = recordURI(t, decoded, "POST /api/challenges (as alice, by handle)")
		handleResolutionOK = true
		t.Logf("OK: alice challenged bob by handle %q -- challenge uri=%s", accounts.Bob.Handle, challengeURI)
	})

	if !handleResolutionOK {
		t.Logf("DEGRADED FALLBACK: using bob's known DID (%s) directly instead of his handle (%q), because AliceChallengesBobByHandle failed above", accounts.Bob.DID, accounts.Bob.Handle)
		resp := apiPost(t, alice, "/api/challenges", map[string]interface{}{
			"opponent_did": accounts.Bob.DID,
			"color":        "white",
			"message":      "challenge-delivery test challenge (fallback by DID)",
		})
		challengeURI = recordURI(t, resp, "POST /api/challenges (as alice, fallback by DID)")
		t.Logf("challenge created via DID fallback: uri=%s", challengeURI)
	}

	if challengeURI == "" {
		t.Fatalf("cannot continue: no challenge record was created by either the handle path or the DID fallback")
	}
	challengeRkey := rkeyOf(t, challengeURI)

	// ---------------------------------------------------------------
	// Step 3/4: THE CENTRAL ASSERTION. Bob, authenticated as himself
	// against his OWN protocol-service instance, should see exactly one
	// pending challenge from alice, including her handle and the offered
	// color. Bounded poll to 15s; on timeout, FAIL reporting elapsed time
	// and the last observed (empty) body -- never sleep-and-hope.
	// ---------------------------------------------------------------
	var bobSawNotification bool
	var bobNotification pendingChallengeInfo
	t.Run("BobSeesPendingChallengeFromAlice", func(t *testing.T) {
		status, notifications, body, elapsed := pollChallengeNotifications(t, bob, 15*time.Second)
		if status != http.StatusOK {
			t.Errorf("GET /api/challenge-notifications (as bob, via his own instance %s) never returned HTTP 200 within %s: last status=%d body=%s", bob.ProtocolURL, elapsed, status, body)
			return
		}
		if len(notifications) == 0 {
			t.Errorf("challenge delivery defect confirmed: bob (authenticated against his own protocol-service instance, %s) sees ZERO pending challenges after polling for %s, even though alice created one (uri=%s) directed at him (did=%s). Last response body: %s.\n"+
				"Root cause (see this file's package doc comment for the full trace): GetChallengeNotificationsHandler (internal/web/service.go) reads bob's own in-PROCESS challenge.Store, which was never populated -- the challenge was added to ALICE's instance's own in-memory Store (by CreateChallengeHandler, on her process only), and the firehose that could otherwise bridge the two processes' Stores is disabled in this harness (FIREHOSE_ENABLED=false) and requires a real relay even when enabled. Independently, the record-based delivery path (atproto.Client.CreateChallengeNotification, invoked as a best-effort side effect of CreateChallenge) is expected to fail outright because it attempts to write into bob's repo via alice's own PDS -- see the CrossRepoWriteDiagnostic and DirectPDSCorroboration subtests below for the verbatim evidence.",
				bob.ProtocolURL, elapsed, challengeURI, bob.DID, body)
			return
		}
		if len(notifications) != 1 {
			t.Errorf("bob sees %d pending challenges, want exactly 1 (uri=%s): %+v", len(notifications), challengeURI, notifications)
		}
		var found *pendingChallengeInfo
		for i := range notifications {
			if notifications[i].ChallengeURI == challengeURI {
				found = &notifications[i]
				break
			}
		}
		if found == nil {
			t.Errorf("bob's pending-challenge list does not contain alice's challenge (uri=%s); got: %+v", challengeURI, notifications)
			return
		}
		if found.ChallengerHandle != accounts.Alice.Handle {
			t.Errorf("notification's challenger handle = %q, want alice's handle %q", found.ChallengerHandle, accounts.Alice.Handle)
		}
		if found.Color != "white" {
			t.Errorf("notification's offered color = %q, want %q", found.Color, "white")
		}
		bobSawNotification = true
		bobNotification = *found
		t.Logf("UNEXPECTED (not today's known defect): bob saw the notification after %s: %+v", elapsed, *found)
	})

	// ---------------------------------------------------------------
	// Determine what actually happens to the cross-repo write:
	// independently replicate the exact call
	// atproto.Client.CreateChallengeNotification makes internally (and
	// whose error is silently swallowed by CreateChallenge), so its
	// verbatim PDS response is captured and reported rather than merely
	// inferred from the empty notification list above.
	// ---------------------------------------------------------------
	t.Run("CrossRepoWriteDiagnostic", func(t *testing.T) {
		aliceAccessJwt, aliceDID := createSessionRaw(t, accounts.Alice.PDSURL, accounts.Alice.Handle, accounts.Alice.Password)
		if aliceDID != alice.DID {
			t.Fatalf("createSessionRaw returned did=%s, want alice's known did=%s", aliceDID, alice.DID)
		}
		notificationRecord := map[string]interface{}{
			"$type":     "app.atchess.challengeNotification",
			"createdAt": time.Now().Format(time.RFC3339),
			"challenge": map[string]interface{}{
				"uri": challengeURI,
				"cid": "bafyreicrossrepowritediagnosticplaceholder",
			},
			"challenger":       alice.DID,
			"challengerHandle": accounts.Alice.Handle,
			"color":            "white",
			"expiresAt":        time.Now().Add(24 * time.Hour).Format(time.RFC3339),
		}
		// This is EXACTLY what CreateChallengeNotification (internal/atproto/client.go)
		// does: POST com.atproto.repo.createRecord to the CALLER's own PDS
		// (alice's, here) with "repo" set to the OTHER player's DID (bob's).
		status, body := rawCreateRecordAttempt(t, accounts.Alice.PDSURL, aliceAccessJwt, bob.DID, "app.atchess.challengeNotification", notificationRecord)
		t.Logf("VERBATIM PDS RESPONSE to cross-repo createRecord (POST %s/xrpc/com.atproto.repo.createRecord, authenticated as alice did=%s, repo=bob's did=%s, collection=app.atchess.challengeNotification): HTTP %d: %s",
			accounts.Alice.PDSURL, alice.DID, bob.DID, status, body)
		if status == http.StatusOK {
			t.Errorf("SURPRISING: the cross-repo write UNEXPECTEDLY SUCCEEDED (HTTP 200: %s) -- alice's PDS (%s) accepted a createRecord call naming bob's DID (%s, hosted on a DIFFERENT PDS, %s) as the repo. This contradicts the bead's stated theory that AT Protocol categorically rejects cross-repo writes; report this prominently rather than assuming rejection. Investigate WHERE the record actually landed (see DirectPDSCorroboration below) before designing atchess-1c9.11's fix.", body, alice.DID, bob.DID, bob.PDSURL)
			return
		}
		t.Errorf("cross-repo write REJECTED as expected, pinning finding 1 from this file's package doc comment: alice's own PDS (%s) refused to createRecord into a repo not her own (bob's did=%s, actually hosted on %s) with HTTP %d: %s", accounts.Alice.PDSURL, bob.DID, bob.PDSURL, status, body)
	})

	// ---------------------------------------------------------------
	// Step 5: decline. Since the central assertion above establishes that
	// bob never sees a real notification through the API (this is the
	// expected, defect-confirming outcome), there is no genuine
	// notification key to decline. Rather than silently skip step 5 or
	// fabricate a false pass, this subtest documents that fact and then
	// still exercises DELETE /api/challenge-notifications/{key} with the
	// best available candidate key (the challenge's own rkey, matching
	// what web/static/index.html's declineChallenge() would extract from a
	// notification URI), to capture and report its actual behavior. This
	// subtest is EXPECTED TO FAIL and must not be mistaken for coverage of
	// a working decline flow -- see its name.
	// ---------------------------------------------------------------
	t.Run("Decline_CANNOT_BE_GENUINELY_EXERCISED_NoNotificationDelivered", func(t *testing.T) {
		if bobSawNotification {
			// Only reachable if a fix has landed and delivery now works;
			// exercise the real decline flow for real.
			status, body := apiPostExpectStatus(t, bob, "/api/challenge-notifications/"+rkeyOf(t, bobNotification.ChallengeURI), map[string]interface{}{})
			t.Logf("decline attempt (real notification present): HTTP %d: %s", status, body)
			return
		}
		t.Errorf("cannot genuinely test decline: bob was never delivered a real challenge notification (see BobSeesPendingChallengeFromAlice above), so there is no legitimate notification key to decline. Attempting DELETE with the best available candidate key (the original challenge's rkey, %q) purely to document actual behavior, not as a substitute for real coverage.", challengeRkey)

		req, err := http.NewRequest(http.MethodDelete, bob.ProtocolURL+"/api/challenge-notifications/"+challengeRkey, nil)
		if err != nil {
			t.Fatalf("failed to build DELETE request: %v", err)
		}
		resp, err := bob.HTTPClient.Do(req)
		if err != nil {
			t.Fatalf("DELETE %s/api/challenge-notifications/%s (as bob) failed: %v", bob.ProtocolURL, challengeRkey, err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		t.Logf("DELETE /api/challenge-notifications/%s (as bob, candidate key = original challenge's rkey): HTTP %d: %s -- this exercises finding 3 from the package doc comment (DeleteChallengeNotificationHandler requires a FULL at:// URI in the route's {key} segment, which mux can never supply, since {key} is a single non-slash path segment; expect this to fail structurally, independent of federation)", challengeRkey, resp.StatusCode, string(body))

		// Whatever DELETE did or didn't do, re-confirm bob's list is still
		// empty of alice's challenge -- consistent either way, since it was
		// never there to begin with.
		status, notifications, listBody := getChallengeNotifications(t, bob)
		if status == http.StatusOK {
			for _, n := range notifications {
				if n.ChallengeURI == challengeURI {
					t.Errorf("alice's challenge (uri=%s) is now present in bob's list post-DELETE, which is inconsistent with it never having been delivered pre-DELETE: %+v", challengeURI, notifications)
				}
			}
		} else {
			t.Logf("post-DELETE list check: HTTP %d: %s", status, listBody)
		}
	})

	// ---------------------------------------------------------------
	// Step 6: alice must NOT see her own outbound challenge in her own
	// inbound list.
	// ---------------------------------------------------------------
	t.Run("AliceDoesNotSeeOwnOutboundChallenge", func(t *testing.T) {
		status, notifications, body := getChallengeNotifications(t, alice)
		if status != http.StatusOK {
			t.Errorf("GET /api/challenge-notifications (as alice) returned HTTP %d: %s", status, body)
			return
		}
		for _, n := range notifications {
			if n.ChallengeURI == challengeURI {
				t.Errorf("alice sees her OWN outbound challenge (uri=%s) in her own inbound notification list: %+v", challengeURI, n)
				return
			}
		}
		t.Logf("OK: alice's inbound list does not contain her own outbound challenge (list: %+v)", notifications)
	})

	// ---------------------------------------------------------------
	// Step 7: direct-PDS corroboration, independent of the HTTP API.
	// Distinguishes "never written" from "written to the wrong repo" from
	// "written correctly but read from the wrong place" by looking at
	// app.atchess.challengeNotification records directly on both alice's
	// and bob's PDSes.
	// ---------------------------------------------------------------
	t.Run("DirectPDSCorroboration", func(t *testing.T) {
		aliceRecords, aliceErr := alice.RepoListRecords("app.atchess.challengeNotification")
		bobRecords, bobErr := bob.RepoListRecords("app.atchess.challengeNotification")

		if aliceErr != nil {
			t.Logf("alice's repo (%s, did=%s): listRecords(app.atchess.challengeNotification) error: %v", alice.PDSURL, alice.DID, aliceErr)
		} else {
			t.Logf("alice's repo (%s, did=%s): %d app.atchess.challengeNotification record(s): %+v", alice.PDSURL, alice.DID, len(aliceRecords), aliceRecords)
		}
		if bobErr != nil {
			t.Logf("bob's repo (%s, did=%s): listRecords(app.atchess.challengeNotification) error: %v", bob.PDSURL, bob.DID, bobErr)
		} else {
			t.Logf("bob's repo (%s, did=%s): %d app.atchess.challengeNotification record(s): %+v", bob.PDSURL, bob.DID, len(bobRecords), bobRecords)
		}

		switch {
		case len(aliceRecords) == 0 && len(bobRecords) == 0:
			t.Errorf("VERDICT: app.atchess.challengeNotification records exist in NEITHER alice's nor bob's repo -- this is bug flavor #1, \"never written\" (consistent with CreateChallengeNotification's cross-repo createRecord call being rejected outright by AT Protocol before any record is persisted anywhere; see CrossRepoWriteDiagnostic above for the verbatim rejection). This is the CENTRAL failure atchess-1c9.11 must redesign around: there is no record, anywhere, for a read-side fix alone to find.")
		case len(aliceRecords) > 0 && len(bobRecords) == 0:
			t.Errorf("VERDICT: app.atchess.challengeNotification record(s) landed in ALICE's repo (the challenger, not bob) -- this is bug flavor #2, \"written to the wrong repo\" (consistent with the cross-repo createRecord call, sent to alice's own PDS naming bob as the repo, being silently reinterpreted by that PDS as writing to the AUTHENTICATED caller's own repo -- i.e. alice's -- rather than the named repo argument). atchess-1c9.11 could potentially fix this by having a read path check the CHALLENGER's repo too, though that is unusual AT Protocol design (records should live with their subject, not their author, for this use case).")
		case len(bobRecords) > 0:
			t.Errorf("SURPRISING VERDICT: app.atchess.challengeNotification record(s) exist in BOB's repo -- this would be bug flavor #3, \"written correctly but read from the wrong place\" (GetChallengeNotifications, internal/atproto/client.go, does list from c.did -- bob's own repo when bob is the caller -- so if the record is really there, the repo-record read path SHOULD find it; the observed failure in BobSeesPendingChallengeFromAlice must then be explained by GetChallengeNotificationsHandler preferring the empty challenge.Store branch over this working path, per finding 2 in the package doc comment). This would mean atchess-1c9.11 might only need to fix the HANDLER's branch selection, not the write path.")
		}
	})

	t.Logf("SUMMARY: challengeURI=%s handleResolutionOK=%v bobSawNotificationViaAPI=%v", challengeURI, handleResolutionOK, bobSawNotification)
}
