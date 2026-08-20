//go:build e2e

// Challenge delivery conformance test (atchess-1c9.6, redesigned under
// atchess-1c9.11): a challenge created by one player must become observable
// by the challenged player through the challenged player's own
// GET /api/challenge-notifications, on the challenged player's OWN
// protocol-service instance -- including a challenge issued while that
// instance was not even running yet (the offline-backfill case).
//
// DESIGN (atchess-1c9.11), summarized here so a future reader does not have
// to reconstruct it from the diff:
//
//  1. A challenge is, and only ever was, a record in its CHALLENGER's own
//     repo (app.atchess.challenge, naming the challenged DID). AT Protocol
//     never permits writing into a repo that isn't your own with your own
//     session credentials, so the OLD design's second write --
//     app.atchess.challengeNotification, created cross-repo directly into
//     the CHALLENGED player's repo -- could never succeed (verified
//     empirically: HTTP 403 AccountNotFound) and has been removed entirely,
//     not retried. See CreateChallenge (internal/atproto/client.go) and its
//     doc comment.
//
//  2. Discovery is push-based: cmd/protocol/main.go subscribes every
//     protocol-service instance directly to the com.atproto.sync.subscribeRepos
//     firehose of every PDS that might host a challenger (in this
//     two-player harness, that's simply both alice's and bob's PDSes --
//     see test/harness/services.go's StartServices), and
//     internal/firehose.EventProcessor indexes matching app.atchess.challenge
//     commits into a per-process challenge.Store CACHE, keyed by the
//     challenged DID (never a write path; see that package's doc comment).
//     GET /api/challenge-notifications serves straight from that cache.
//
//     The operator's decision for this bead (see atchess-1c9.11's notes)
//     was Jetstream (the JSON-over-websocket firehose translator) rather
//     than the raw CBOR firehose. That remains the right choice for a real
//     deployment watching the whole network, but genuine upstream Jetstream
//     (github.com/bluesky-social/jetstream, the only published module) is a
//     full-network archive service that crawls a relay via listHosts/
//     listRepos -- there is no relay in this harness (by design; see
//     atchess-1c9.24), and standing one up was judged out of this bounded
//     task's scope after actually building the real jetstream binary from
//     source and confirming its --relay-url flag expects a full crawlable
//     relay, not a bare PDS. internal/firehose (CBOR, already implemented
//     and tested pre-atchess-1c9.11) connects directly to any single PDS's
//     native subscribeRepos endpoint -- no relay required -- which is
//     exactly the same underlying wire feed Jetstream itself is built on
//     top of, just without the JSON translation hop. This test therefore
//     exercises the REAL subscription protocol against the REAL PDSes this
//     harness runs, end to end; the only thing NOT exercised is a
//     network-wide relay, which does not exist here to exercise. See
//     atchess-1c9.11's completion notes for the full tradeoff.
//
//  3. Cursor resumption across reconnects was already implemented in
//     internal/firehose.Client (LastSequence/setLastSequence,
//     pre-atchess-1c9.11) and is unchanged here.
//
//  4. Backfill on login (REDESIGNED under atchess-1c9.46 -- see that bead
//     and docs/firehose-and-backfill.md): firehose.Client no longer
//     defaults to WithCursor(0) on every process start -- that forced a
//     full replay of the ENTIRE watched PDS's commit log on every single
//     boot, which is unbounded against a production-scale host and was
//     itself the defect atchess-1c9.46 fixes. cmd/protocol/main.go now
//     persists each host's cursor across restarts and, with no persisted
//     cursor (e.g. this test's freshly built instances), starts each
//     firehose.Client at the LIVE TIP instead. The offline-delivery case
//     below is instead covered by internal/backfill: a repo-read backfill
//     run synchronously by internal/web's LoginHandler on every login,
//     which queries com.atproto.sync.listRepos +
//     com.atproto.repo.listRecords directly against every PDS named in
//     FIREHOSE_URL for app.atchess.challenge records addressed to the
//     logging-in user -- independent of the firehose entirely. This is why
//     the OfflineBackfill subtest below passes: NOT because bob's second
//     instance replays PDS-A's history via the firehose (it does not, by
//     design), but because harness.NewPlayer's login call
//     (POST /api/auth/login) triggers that repo-read backfill before the
//     login response is even returned.
//
//  5. Decline is expressed without writing to the challenger's repo: DELETE
//     /api/challenge-notifications/{key} (key = the challenge's full at://
//     URI, URL-safe-base64 encoded -- see DeclineChallengeHandler's doc
//     comment for why a bare rkey can never work here, the atchess-1c9.11
//     brief's defect 4) creates an app.atchess.challengeResponse record in
//     the RESPONDING player's own repo, referencing the challenge by
//     strongRef, then drops the challenge from that instance's local cache.
package e2e

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/justinabrahms/atchess/test/harness"
)

// pendingChallengeInfo mirrors internal/challenge.PendingChallenge's JSON
// shape exactly (duplicated here rather than imported, matching this
// suite's established convention of exercising only the public HTTP
// boundary -- see healthInfo in ownership_test.go).
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

// pollForChallenge polls getChallengeNotifications against player until its
// list contains an entry whose ChallengeURI == wantURI, or deadline
// elapses -- never a bare sleep-and-hope, and always bounded, so a read
// that legitimately races a very recent write gets a fair chance to
// observe it. Deliberately does NOT stop as soon as the list is merely
// non-empty: this PDS's data volume persists across repeated test runs
// (see docker-compose.dual-pds.yml), so bob's backfill-populated cache can
// easily already contain OTHER challenges from earlier runs by the time
// THIS run's challenge is polled for -- matching on the specific URI is
// the only way to avoid a false-positive pass (or, worse, a false-negative
// "not found" reported against the wrong entry) against a non-empty but
// unrelated backlog. On timeout it returns the LAST observed
// status/notifications/body plus the elapsed time, so callers can report
// both.
func pollForChallenge(t *testing.T, player *harness.Player, wantURI string, deadline time.Duration) (status int, notifications []pendingChallengeInfo, body string, elapsed time.Duration, found *pendingChallengeInfo) {
	t.Helper()
	start := time.Now()
	interval := 200 * time.Millisecond
	for {
		status, notifications, body = getChallengeNotifications(t, player)
		elapsed = time.Since(start)
		if status == http.StatusOK {
			for i := range notifications {
				if notifications[i].ChallengeURI == wantURI {
					found = &notifications[i]
					return
				}
			}
		}
		if elapsed >= deadline {
			return
		}
		time.Sleep(interval)
	}
}

// cursorFileEntry mirrors the on-disk shape of one host's entry in
// cursors.json -- internal/firehose.CursorStore's UNEXPORTED cursorEntry
// (atchess-1c9.49: {"transport":"...","seq":N}, tagged with the Transport
// it was recorded under, added when Jetstream support landed). Declared
// here, not imported, because cursorEntry itself is unexported.
type cursorFileEntry struct {
	Transport string `json:"transport"`
	Seq       int64  `json:"seq"`
}

// pollForNonEmptyCursorFile polls path (a protocol-service instance's own
// FIREHOSE_STATE_DIR/cursors.json) until it exists, parses as a non-empty
// JSON object, and returns a map of host -> persisted sequence number.
// cmd/protocol/main.go flushes cursors every firehoseCursorPersistInterval
// (5s) rather than on every processed event, so this must tolerate that
// delay rather than checking once; deadline should comfortably exceed 5s.
//
// atchess-5fs: this used to decode straight into map[string]int64, which
// can NEVER successfully unmarshal cursors.json's actual on-disk shape --
// each value is a {"transport","seq"} OBJECT, not a bare number (see
// cursorFileEntry above) -- so json.Unmarshal always returned a non-nil
// error ("cannot unmarshal object into Go value of type int64") and this
// poll ran out its FULL deadline on every single call, every run,
// regardless of how long that deadline was: not a race, not a slow flush,
// a decode-type bug introduced when atchess-1c9.49 changed the persisted
// format and this helper was never updated to match.
func pollForNonEmptyCursorFile(t *testing.T, path string, deadline time.Duration) map[string]int64 {
	t.Helper()
	start := time.Now()
	interval := 500 * time.Millisecond
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			var raw map[string]cursorFileEntry
			if jsonErr := json.Unmarshal(data, &raw); jsonErr == nil && len(raw) > 0 {
				cursors := make(map[string]int64, len(raw))
				for host, entry := range raw {
					cursors[host] = entry.Seq
				}
				return cursors
			}
		}
		if time.Since(start) >= deadline {
			if err != nil {
				t.Fatalf("cursors.json at %s never appeared/became non-empty within %s: last read error: %v", path, deadline, err)
			}
			t.Fatalf("cursors.json at %s never became non-empty within %s: last contents: %s", path, deadline, string(data))
		}
		time.Sleep(interval)
	}
}

// encodeChallengeKey URL-safe-base64-encodes uri exactly the way
// DeclineChallengeHandler expects its {key} route segment to be encoded
// (Service.decodeGameID's inverse -- see that handler's doc comment).
func encodeChallengeKey(uri string) string {
	return base64.URLEncoding.EncodeToString([]byte(uri))
}

// apiDelete issues a DELETE to path on player's protocol-service instance,
// authenticated as player, and returns the raw status and body.
func apiDelete(t *testing.T, player *harness.Player, path string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, player.ProtocolURL+path, nil)
	if err != nil {
		t.Fatalf("failed to build DELETE request for %s%s: %v", player.ProtocolURL, path, err)
	}
	resp, err := player.HTTPClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE %s%s (as %s) failed: %v", player.ProtocolURL, path, player.Handle, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// TestChallengeDelivery pins atchess-1c9.6/atchess-1c9.11: alice (PDS-A)
// challenges bob (PDS-B) by handle, and bob -- authenticated as himself,
// against his own protocol-service instance and his own PDS -- sees exactly
// one pending challenge from alice, can decline it (which lands a record in
// HIS OWN repo, never alice's), and no longer sees it afterward; alice
// never sees her own outbound challenge in her own inbound list. A separate
// subtest, OfflineBackfill, additionally proves that a challenge issued
// while the challenged player's protocol-service instance was not running
// at all is still discovered once that instance starts and authenticates.
func TestChallengeDelivery(t *testing.T) {
	accounts := harness.LoadAccounts(t)
	services := harness.StartServices(t, accounts)

	alice := harness.NewPlayer(t, accounts.Alice, services.AliceURL)
	bob := harness.NewPlayer(t, accounts.Bob, services.BobURL)

	t.Logf("alice: did=%s pds=%s protocol=%s", alice.DID, alice.PDSURL, alice.ProtocolURL)
	t.Logf("bob:   did=%s pds=%s protocol=%s", bob.DID, bob.PDSURL, bob.ProtocolURL)

	// ---------------------------------------------------------------
	// Alice challenges bob BY HANDLE. Cross-PDS handle resolution
	// (atchess-1c9.5) was fixed by atchess-1c9.10; if it regresses, fall
	// back to bob's known DID (from test/.harness-accounts.json) and log
	// loudly, so a handle-resolution regression does not mask this test's
	// actual subject (delivery).
	// ---------------------------------------------------------------
	var challengeURI, challengeCID string
	t.Run("AliceChallengesBobByHandle", func(t *testing.T) {
		status, body := apiPostExpectStatus(t, alice, "/api/challenges", map[string]interface{}{
			"opponent_did": accounts.Bob.Handle,
			"color":        "white",
			"message":      "challenge-delivery test challenge",
		})
		if status != http.StatusOK {
			t.Logf("DEGRADED: challenging bob by handle (%q) returned HTTP %d, not 200 -- possible atchess-1c9.5 regression (ResolveHandle). Falling back to bob's known DID. Body: %s", accounts.Bob.Handle, status, body)
			status, body = apiPostExpectStatus(t, alice, "/api/challenges", map[string]interface{}{
				"opponent_did": accounts.Bob.DID,
				"color":        "white",
				"message":      "challenge-delivery test challenge (fallback by DID)",
			})
		}
		if status != http.StatusOK {
			t.Fatalf("challenging bob (by handle, then by DID fallback) failed: HTTP %d: %s", status, body)
		}
		var decoded map[string]interface{}
		if err := json.Unmarshal([]byte(body), &decoded); err != nil {
			t.Fatalf("challenge creation: HTTP 200 but undecodable body: %v (body: %s)", err, body)
		}
		challengeURI = recordURI(t, decoded, "POST /api/challenges (as alice)")
		t.Logf("OK: alice challenged bob -- challenge uri=%s", challengeURI)
	})
	if challengeURI == "" {
		t.Fatalf("cannot continue: no challenge was created")
	}
	challengeRkey := rkeyOf(t, challengeURI)

	// ---------------------------------------------------------------
	// Bob, authenticated as himself against his OWN protocol-service
	// instance, sees exactly one pending challenge from alice, including
	// her handle, the offered color, and the CID needed for decline's
	// strongRef.
	// ---------------------------------------------------------------
	var bobNotification pendingChallengeInfo
	t.Run("BobSeesPendingChallengeFromAlice", func(t *testing.T) {
		status, notifications, body, elapsed, found := pollForChallenge(t, bob, challengeURI, 15*time.Second)
		if status != http.StatusOK {
			t.Fatalf("GET /api/challenge-notifications (as bob, via his own instance %s) never returned HTTP 200 within %s: last status=%d body=%s", bob.ProtocolURL, elapsed, status, body)
		}
		if found == nil {
			t.Fatalf("bob (authenticated against his own protocol-service instance, %s) does not see alice's challenge (uri=%s) after polling for %s, even though it is directed at him (did=%s). His list contained %d other entr(y/ies): %+v", bob.ProtocolURL, challengeURI, elapsed, bob.DID, len(notifications), notifications)
		}
		if found.ChallengerHandle != accounts.Alice.Handle {
			t.Errorf("notification's challenger handle = %q, want alice's handle %q", found.ChallengerHandle, accounts.Alice.Handle)
		}
		if found.Color != "white" {
			t.Errorf("notification's offered color = %q, want %q", found.Color, "white")
		}
		if found.ChallengeCID == "" {
			t.Errorf("notification's ChallengeCID is empty; decline's strongRef needs it")
		}
		bobNotification = *found
		challengeCID = found.ChallengeCID
		t.Logf("OK: bob saw the notification after %s: %+v", elapsed, *found)
	})

	// ---------------------------------------------------------------
	// The challenge record lands ONLY in alice's own repo (the challenger,
	// the authenticated author of the write), never bob's (the opponent) --
	// direct-PDS corroboration, independent of the HTTP API.
	// ---------------------------------------------------------------
	t.Run("ChallengeRecordLandsOnlyInChallengersOwnRepo", func(t *testing.T) {
		assertFoundIn(t, alice, "alice (challenger)", "app.atchess.challenge", challengeRkey, map[string]*harness.Player{"bob": bob})
		assertNotFoundIn(t, bob, "bob (challenged)", "app.atchess.challenge", challengeRkey)
	})

	// ---------------------------------------------------------------
	// Alice must NOT see her own outbound challenge in her own inbound
	// list (the challenged-DID keying in challenge.Store.ForPlayer).
	// ---------------------------------------------------------------
	t.Run("AliceDoesNotSeeOwnOutboundChallenge", func(t *testing.T) {
		status, notifications, body := getChallengeNotifications(t, alice)
		if status != http.StatusOK {
			t.Fatalf("GET /api/challenge-notifications (as alice) returned HTTP %d: %s", status, body)
		}
		for _, n := range notifications {
			if n.ChallengeURI == challengeURI {
				t.Fatalf("alice sees her OWN outbound challenge (uri=%s) in her own inbound notification list: %+v", challengeURI, n)
			}
		}
		t.Logf("OK: alice's inbound list does not contain her own outbound challenge (list: %+v)", notifications)
	})

	// ---------------------------------------------------------------
	// Bob declines: DELETE /api/challenge-notifications/{key}, key = the
	// challenge's full at:// URI, URL-safe-base64 encoded (a bare rkey, as
	// web/static/index.html's declineChallenge() and mux's {key} route
	// segment naively supply, can never be resolved back to a full URI on
	// this end -- the challenge record lives in ALICE's repo, an arbitrary
	// DID bob's instance does not otherwise know; this was defect 4 from
	// atchess-1c9.11's brief).
	// ---------------------------------------------------------------
	t.Run("BobDeclines", func(t *testing.T) {
		status, body := apiDelete(t, bob, "/api/challenge-notifications/"+encodeChallengeKey(bobNotification.ChallengeURI))
		if status != http.StatusNoContent {
			t.Fatalf("DELETE /api/challenge-notifications/%s (as bob) returned HTTP %d, want 204: %s", encodeChallengeKey(bobNotification.ChallengeURI), status, body)
		}
		t.Logf("OK: bob declined alice's challenge (HTTP 204)")

		t.Run("ChallengeNoLongerInBobsList", func(t *testing.T) {
			status, notifications, body := getChallengeNotifications(t, bob)
			if status != http.StatusOK {
				t.Fatalf("GET /api/challenge-notifications (as bob, post-decline) returned HTTP %d: %s", status, body)
			}
			for _, n := range notifications {
				if n.ChallengeURI == challengeURI {
					t.Fatalf("alice's challenge (uri=%s) is still present in bob's list after declining it: %+v", challengeURI, notifications)
				}
			}
			t.Logf("OK: alice's challenge no longer appears in bob's list after decline")
		})

		// The decline record must land in BOB's (the responder's) own
		// repo, referencing the challenge by strongRef -- never in
		// alice's (the challenger's) repo, which AT Protocol would not
		// have permitted anyway.
		t.Run("DeclineRecordLandsInBobsOwnRepo", func(t *testing.T) {
			bobRecords, err := bob.RepoListRecords("app.atchess.challengeResponse")
			if err != nil {
				t.Fatalf("could not list bob's app.atchess.challengeResponse records: %v", err)
			}
			var found map[string]interface{}
			for _, rec := range bobRecords {
				challengeRef, _ := rec["challenge"].(map[string]interface{})
				if uri, _ := challengeRef["uri"].(string); uri == challengeURI {
					found = rec
					break
				}
			}
			if found == nil {
				t.Fatalf("expected an app.atchess.challengeResponse record in bob's own repo referencing challenge uri=%s, but none of his %d record(s) matched: %+v", challengeURI, len(bobRecords), bobRecords)
			}
			if response, _ := found["response"].(string); response != "declined" {
				t.Errorf("expected response=\"declined\", got %q: %+v", response, found)
			}
			challengeRef, _ := found["challenge"].(map[string]interface{})
			if cid, _ := challengeRef["cid"].(string); cid != challengeCID {
				t.Errorf("expected the decline record's challenge strongRef cid to be %q (from the notification), got %q", challengeCID, cid)
			}
			t.Logf("OK: decline record found in bob's own repo: %+v", found)

			aliceRecords, err := alice.RepoListRecords("app.atchess.challengeResponse")
			if err != nil {
				t.Fatalf("could not list alice's app.atchess.challengeResponse records: %v", err)
			}
			for _, rec := range aliceRecords {
				challengeRef, _ := rec["challenge"].(map[string]interface{})
				if uri, _ := challengeRef["uri"].(string); uri == challengeURI {
					t.Errorf("ownership violation: bob's decline record was found in ALICE's repo (the challenger's), not just bob's own: %+v", rec)
				}
			}
			t.Logf("OK: no decline record leaked into alice's (the challenger's) repo")
		})
	})

	// ---------------------------------------------------------------
	// OfflineBackfill: the offline-delivery half of atchess-1c9.11's
	// acceptance gate. A challenge issued while the challenged player's
	// protocol-service instance was not running at all must still be
	// discovered once that instance starts and authenticates -- a
	// live-only subscription would silently drop it. This uses a SECOND,
	// independently started protocol-service instance for bob (the first
	// one, `bob` above, was already running when alice's earlier challenge
	// was created, so it cannot demonstrate the offline case) that is not
	// started until after this new challenge already exists.
	// ---------------------------------------------------------------
	t.Run("OfflineBackfill_ChallengeIssuedWhileChallengedPlayersInstanceWasNotRunning", func(t *testing.T) {
		var offlineChallengeURI string
		t.Run("AliceChallengesBobWhileBobsSecondInstanceIsNotRunningYet", func(t *testing.T) {
			status, body := apiPostExpectStatus(t, alice, "/api/challenges", map[string]interface{}{
				"opponent_did": bob.DID,
				"color":        "black",
				"message":      "offline-backfill test challenge",
			})
			if status != http.StatusOK {
				t.Fatalf("challenging bob (by DID, for the offline-backfill case) failed: HTTP %d: %s", status, body)
			}
			var decoded map[string]interface{}
			if err := json.Unmarshal([]byte(body), &decoded); err != nil {
				t.Fatalf("HTTP 200 but undecodable body: %v (body: %s)", err, body)
			}
			offlineChallengeURI = recordURI(t, decoded, "POST /api/challenges (as alice, offline-backfill case)")
			t.Logf("challenge created while bob's SECOND instance does not exist yet: uri=%s", offlineChallengeURI)
		})
		if offlineChallengeURI == "" {
			t.Fatalf("cannot continue: no offline-backfill challenge was created")
		}

		t.Run("BobStartsASecondInstanceAndSeesItViaBackfill", func(t *testing.T) {
			binPath := harness.BuildProtocolBinary(t)
			plcDirectoryURL := harness.ResolvePLCDirectoryURL(t)
			firehoseURLs := harness.AllPDSFirehoseURLs(accounts)

			// This is the moment "bob's client starts" -- his second
			// protocol-service instance did not exist, and therefore was
			// not subscribed to anything, at the time the challenge above
			// was created. StartPlayerService blocks until it reports
			// healthy; its firehose.Client(s) start at the LIVE TIP (no
			// stored cursor, see atchess-1c9.46) so they do NOT see this
			// already-past challenge via the firehose at all. Discovery
			// instead happens inside harness.NewPlayer below: its call to
			// POST /api/auth/login runs internal/backfill's synchronous
			// repo-read backfill (querying PDS-A -- alice's host -- and
			// PDS-B directly, independent of the firehose) BEFORE the
			// login response is returned, which is what actually finds
			// this challenge. See this file's header comment, point 4, for
			// the full explanation of why this subtest now passes for a
			// different reason than it originally did.
			bobSecondURL, _ := harness.StartPlayerService(t, binPath, accounts.Bob, "bob-second-instance-offline-backfill", plcDirectoryURL, firehoseURLs)
			bobSecond := harness.NewPlayer(t, accounts.Bob, bobSecondURL)
			t.Logf("bob's second instance started (simulating his client coming online after being offline); login (which triggers the repo-read backfill) has already completed: %s", bobSecondURL)

			status, notifications, body, elapsed, found := pollForChallenge(t, bobSecond, offlineChallengeURI, 15*time.Second)
			if status != http.StatusOK {
				t.Fatalf("GET /api/challenge-notifications (as bob, second instance) never returned HTTP 200 within %s: last status=%d body=%s", elapsed, status, body)
			}
			if found == nil {
				t.Fatalf("bob's SECOND instance (started strictly after the challenge already existed) does not see the offline-issued challenge (uri=%s) after polling for %s -- backfill-on-login did not work. His list contained %d other entr(y/ies): %+v", offlineChallengeURI, elapsed, len(notifications), notifications)
			}
			t.Logf("OK: bob's second instance discovered the offline-issued challenge via backfill after %s: %+v", elapsed, *found)
		})
	})

	// ---------------------------------------------------------------
	// CursorPersistence (atchess-1c9.46 review fix, defect 2): each
	// instance's OWN cursors.json (under its own per-instance
	// FIREHOSE_STATE_DIR -- see Services' doc comment) must actually be
	// non-empty and correct after a full run, proving cursor persistence
	// is not being silently wiped by another instance sharing the same
	// state directory (the defect: the harness previously never set
	// FIREHOSE_STATE_DIR at all, so every instance shared one file).
	// ---------------------------------------------------------------
	t.Run("CursorPersistence_PerInstanceStateDirIsNotClobbered", func(t *testing.T) {
		// Guard 1 (atchess-1c9.46 second review fix): assert the two
		// instances were actually given DISTINCT state directories BEFORE
		// doing anything else. Without this, the checks below can pass
		// for the wrong reason -- see the next comment -- and this
		// subtest would prove nothing about "not clobbered" despite its
		// name. This alone fails immediately (not after any poll window)
		// if the per-instance FIREHOSE_STATE_DIR wiring in
		// test/harness/services.go is ever removed or reverted to a
		// single shared directory for every instance.
		if services.AliceStateDir == "" || services.BobStateDir == "" {
			t.Fatalf("services.AliceStateDir=%q services.BobStateDir=%q: per-instance FIREHOSE_STATE_DIR was not wired up (empty)", services.AliceStateDir, services.BobStateDir)
		}
		if services.AliceStateDir == services.BobStateDir {
			t.Fatalf("services.AliceStateDir == services.BobStateDir == %q: alice's and bob's instances were given the SAME state directory -- this is exactly the defect this subtest exists to catch (see Services' doc comment); everything below would be checking one shared file, not two independent ones", services.AliceStateDir)
		}

		aliceCursorsPath := filepath.Join(services.AliceStateDir, "cursors.json")
		bobCursorsPath := filepath.Join(services.BobStateDir, "cursors.json")

		aliceCursors := pollForNonEmptyCursorFile(t, aliceCursorsPath, 20*time.Second)
		bobCursors := pollForNonEmptyCursorFile(t, bobCursorsPath, 20*time.Second)

		t.Logf("alice's instance cursors.json (%s): %+v", aliceCursorsPath, aliceCursors)
		t.Logf("bob's instance cursors.json (%s): %+v", bobCursorsPath, bobCursors)

		// Guard 2: prove the two paths above resolved to two physically
		// DIFFERENT files on disk, not merely two different path strings
		// that happen to end up pointing at the same inode (e.g. via a
		// symlink) or, more to the point, two strings that our own Go
		// code computed distinctly while the actual subprocess ignored
		// FIREHOSE_STATE_DIR and both wrote to one shared default file
		// instead (in which case one or both of the reads above would
		// never have observed a non-empty file at all and already failed
		// -- see pollForNonEmptyCursorFile -- but this closes the gap for
		// any path where they don't). This is the check that actually
		// exercises "not clobbered": with content-based checks alone
		// (non-empty, no negative values), a single shared file kept
		// alive by two still-running instances self-heals within the
		// poll window above regardless of whether clobbering occurred,
		// which is exactly how this subtest previously passed even after
		// the per-instance state dir wiring was removed entirely.
		aliceInfo, err := os.Stat(aliceCursorsPath)
		if err != nil {
			t.Fatalf("os.Stat(%s) failed after successfully reading it: %v", aliceCursorsPath, err)
		}
		bobInfo, err := os.Stat(bobCursorsPath)
		if err != nil {
			t.Fatalf("os.Stat(%s) failed after successfully reading it: %v", bobCursorsPath, err)
		}
		if os.SameFile(aliceInfo, bobInfo) {
			t.Fatalf("alice's cursors.json (%s) and bob's cursors.json (%s) are the SAME underlying file on disk -- their instances are clobbering each other's persisted cursors", aliceCursorsPath, bobCursorsPath)
		}

		for _, seq := range aliceCursors {
			if seq < 0 {
				t.Errorf("alice's instance persisted a negative cursor %d (should have been deleted, not stored, by CursorStore.Store)", seq)
			}
		}
		for _, seq := range bobCursors {
			if seq < 0 {
				t.Errorf("bob's instance persisted a negative cursor %d (should have been deleted, not stored, by CursorStore.Store)", seq)
			}
		}
	})

	t.Logf("SUMMARY: challengeURI=%s bobDeclined=true", challengeURI)
}
