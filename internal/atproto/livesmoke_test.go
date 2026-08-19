//go:build livesmoke

// Package atproto live smoke test for atchess-1c9.41.
//
// STRICTLY READ-ONLY against real bsky.social accounts. Never run this
// without //go:build livesmoke explicitly enabled, and never add any write
// call (createRecord/putRecord/deleteRecord) to it.
//
// Credentials are read from test/.live-accounts.json (gitignored, not
// committed). Run with:
//
//	go test -tags livesmoke ./internal/atproto/ -run TestLiveSmoke -v
package atproto

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

type liveAccount struct {
	Handle   string `json:"handle"`
	PDSURL   string `json:"pdsUrl"`
	Password string `json:"password"`
}

type liveAccounts struct {
	Alice liveAccount `json:"alice"`
	Bob   liveAccount `json:"bob"`
}

func loadLiveAccounts(t *testing.T) liveAccounts {
	t.Helper()
	path := "../../test/.live-accounts.json"
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v (this smoke test requires real bsky.social test credentials)", path, err)
	}
	var la liveAccounts
	if err := json.Unmarshal(data, &la); err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	return la
}

// TestLiveSmoke drives internal/atproto against the real public AT Protocol
// network (bsky.social + plc.directory) to find out whether our
// URL-construction/handle-resolution code, written and unit-tested only
// against the localhost dual-PDS harness, actually works against a real
// no-port https://<host>.host.bsky.network PDS shape. Read-only: it never
// calls createRecord/putRecord/deleteRecord.
func TestLiveSmoke(t *testing.T) {
	accounts := loadLiveAccounts(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// --- Step 1: handle resolution -----------------------------------
	// Use a throwaway bootstrap client pointed at the public entryway
	// purely so ResolveHandle has an httpClient/pdsURL to try the
	// same-PDS fast path against (it's expected to fail there and fall
	// through to DNS/well-known/PLC-export -- that fallthrough itself is
	// part of what we're testing).
	bootstrap := &Client{pdsURL: "https://bsky.social", httpClient: &http.Client{Timeout: 15 * time.Second}}

	aliceDID, err := bootstrap.ResolveHandle(ctx, accounts.Alice.Handle)
	if err != nil {
		t.Logf("STEP1 alice ResolveHandle FAILED: %v", err)
	} else {
		t.Logf("STEP1 alice ResolveHandle OK: did=%s", aliceDID)
	}

	bobDID, err := bootstrap.ResolveHandle(ctx, accounts.Bob.Handle)
	if err != nil {
		t.Logf("STEP1 bob ResolveHandle FAILED: %v", err)
	} else {
		t.Logf("STEP1 bob ResolveHandle OK: did=%s", bobDID)
	}

	// --- Step 1b: confirm the PLC-export gate does NOT fire against the
	// default public directory (it should fail fast with the "refusing to
	// scan" message, never actually issuing a network request).
	if _, err := bootstrap.resolver().resolveHandleViaPLCExport(ctx, accounts.Alice.Handle); err != nil {
		t.Logf("STEP1b PLC export gate check: err=%v", err)
	} else {
		t.Errorf("STEP1b PLC export gate check: UNEXPECTEDLY SUCCEEDED (should be gated off -- the default public directory must never be scanned)")
	}

	if aliceDID == "" || bobDID == "" {
		t.Fatalf("cannot continue: handle resolution failed for at least one account")
	}

	// --- Step 2: DID -> PDS resolution --------------------------------
	aliceEndpoint, err := ResolvePDS(ctx, aliceDID, "")
	if err != nil {
		t.Errorf("STEP2 alice ResolvePDS FAILED: %v", err)
	} else {
		t.Logf("STEP2 alice ResolvePDS OK: serviceEndpoint=%s", aliceEndpoint)
	}
	assertPDSEndpointShape(t, "alice", aliceEndpoint)

	bobEndpoint, err := ResolvePDS(ctx, bobDID, "")
	if err != nil {
		t.Errorf("STEP2 bob ResolvePDS FAILED: %v", err)
	} else {
		t.Logf("STEP2 bob ResolvePDS OK: serviceEndpoint=%s", bobEndpoint)
	}
	assertPDSEndpointShape(t, "bob", bobEndpoint)

	if aliceEndpoint != "" && bobEndpoint != "" && aliceEndpoint == bobEndpoint {
		t.Errorf("STEP2: alice and bob resolved to the SAME PDS endpoint %q -- expected two distinct real accounts to live on different PDS hosts", aliceEndpoint)
	}

	// --- Step 3: authenticated session (read-only handshake) ---------
	aliceClient, err := NewClient(accounts.Alice.PDSURL, accounts.Alice.Handle, accounts.Alice.Password)
	if err != nil {
		t.Fatalf("STEP3 alice NewClient (createSession) FAILED: %v", err)
	}
	if aliceClient.GetDID() != aliceDID {
		t.Errorf("STEP3 alice: session DID %s does not match handle-resolved DID %s", aliceClient.GetDID(), aliceDID)
	} else {
		t.Logf("STEP3 alice createSession OK: session.did=%s matches resolved", aliceClient.GetDID())
	}

	bobClient, err := NewClient(accounts.Bob.PDSURL, accounts.Bob.Handle, accounts.Bob.Password)
	if err != nil {
		t.Fatalf("STEP3 bob NewClient (createSession) FAILED: %v", err)
	}
	if bobClient.GetDID() != bobDID {
		t.Errorf("STEP3 bob: session DID %s does not match handle-resolved DID %s", bobClient.GetDID(), bobDID)
	} else {
		t.Logf("STEP3 bob createSession OK: session.did=%s matches resolved", bobClient.GetDID())
	}

	// --- Step 4 & 5: cross-account repo reads + URL shape -------------
	crossRead(ctx, t, "alice->bob", aliceClient, bobDID)
	crossRead(ctx, t, "bob->alice", bobClient, aliceDID)
}

// crossRead has caller's client read targetDID's repo: describeRepo and
// listRecords(app.bsky.feed.post), exercising resolveReadEndpoint + the
// unauthenticated getXRPC path against a real foreign PDS. It asserts that
// every request actually lands on the foreign leaf host (not caller's own
// PDS) and returns HTTP 200, which is what makes this a cross-PDS test
// rather than a same-PDS connectivity check.
func crossRead(ctx context.Context, t *testing.T, label string, caller *Client, targetDID string) {
	t.Helper()

	base, ownRepo, err := caller.resolveReadEndpoint(ctx, targetDID)
	if err != nil {
		t.Errorf("STEP4/5 %s resolveReadEndpoint FAILED: %v", label, err)
		return
	}
	t.Logf("STEP4/5 %s resolveReadEndpoint OK: base=%s ownRepo=%v", label, base, ownRepo)
	if ownRepo {
		t.Errorf("STEP4/5 %s resolveReadEndpoint reported ownRepo=true for a cross-account read of a distinct DID -- expected false", label)
	}

	callerHost := hostOf(t, caller.pdsURL)

	// describeRepo
	descParams := url.Values{"repo": {targetDID}}
	descURL := xrpcURL(base, "com.atproto.repo.describeRepo", descParams)
	t.Logf("STEP5 %s describeRepo URL: %s", label, descURL)
	assertForeignHost(t, label, "describeRepo", descURL, callerHost)
	if wantErr := checkURLShape(descURL, base); wantErr != "" {
		t.Errorf("STEP5 %s URL SHAPE PROBLEM: %s", label, wantErr)
	}
	resp, err := caller.getXRPC(ctx, base, ownRepo, "com.atproto.repo.describeRepo", descParams)
	if err != nil {
		t.Errorf("STEP4 %s describeRepo FAILED (transport): %v", label, err)
	} else {
		defer resp.Body.Close()
		t.Logf("STEP4 %s describeRepo HTTP %d", label, resp.StatusCode)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("STEP4 %s describeRepo: expected HTTP 200, got %d", label, resp.StatusCode)
		}
	}

	// listRecords for a real, populated collection.
	listParams := url.Values{"repo": {targetDID}, "collection": {"app.bsky.feed.post"}, "limit": {"5"}}
	listURL := xrpcURL(base, "com.atproto.repo.listRecords", listParams)
	t.Logf("STEP5 %s listRecords URL: %s", label, listURL)
	assertForeignHost(t, label, "listRecords", listURL, callerHost)
	resp2, err := caller.getXRPC(ctx, base, ownRepo, "com.atproto.repo.listRecords", listParams)
	if err != nil {
		t.Errorf("STEP4 %s listRecords FAILED (transport): %v", label, err)
		return
	}
	defer resp2.Body.Close()
	t.Logf("STEP4 %s listRecords HTTP %d", label, resp2.StatusCode)
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("STEP4 %s listRecords: expected HTTP 200, got %d", label, resp2.StatusCode)
	}

	// Also try an atchess collection, expected to simply be empty (not an
	// error) since these accounts have never written any atchess records.
	atchessParams := url.Values{"repo": {targetDID}, "collection": {"app.atchess.game"}, "limit": {"5"}}
	atchessURL := xrpcURL(base, "com.atproto.repo.listRecords", atchessParams)
	assertForeignHost(t, label, "listRecords(app.atchess.game)", atchessURL, callerHost)
	resp3, err := caller.getXRPC(ctx, base, ownRepo, "com.atproto.repo.listRecords", atchessParams)
	if err != nil {
		t.Errorf("STEP4 %s listRecords(app.atchess.game) FAILED (transport): %v", label, err)
		return
	}
	defer resp3.Body.Close()
	t.Logf("STEP4 %s listRecords(app.atchess.game) HTTP %d", label, resp3.StatusCode)
	if resp3.StatusCode != http.StatusOK {
		t.Errorf("STEP4 %s listRecords(app.atchess.game): expected HTTP 200, got %d", label, resp3.StatusCode)
	}
}

// assertPDSEndpointShape asserts that a resolved PDS serviceEndpoint has the
// real-Bluesky no-port https shape (e.g. https://shiitake.us-east.host.bsky.network),
// as opposed to the local dual-PDS harness's http://localhost:<port> shape.
// This is the single most important claim this live smoke test makes, since
// it is the one thing the harness can never exercise for real.
func assertPDSEndpointShape(t *testing.T, label, endpoint string) {
	t.Helper()
	if endpoint == "" {
		t.Errorf("STEP2 %s: empty PDS endpoint", label)
		return
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		t.Errorf("STEP2 %s: unparseable PDS endpoint %q: %v", label, endpoint, err)
		return
	}
	if parsed.Scheme != "https" {
		t.Errorf("STEP2 %s: expected scheme \"https\", got %q in %q", label, parsed.Scheme, endpoint)
	}
	if parsed.Port() != "" {
		t.Errorf("STEP2 %s: expected NO port (real-Bluesky PDS shape), got port %q in %q", label, parsed.Port(), endpoint)
	}
	if parsed.Host == "" {
		t.Errorf("STEP2 %s: empty host in %q", label, endpoint)
	}
}

// hostOf returns the host component of raw, asserting it parses. It is used
// to compare constructed request URLs against the caller's own configured
// PDS host.
func hostOf(t *testing.T, raw string) string {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Errorf("unparseable URL %q: %v", raw, err)
		return ""
	}
	return parsed.Host
}

// assertForeignHost asserts that constructedURL's host differs from
// callerHost (the requesting client's own configured pdsURL host). This is
// what makes crossRead a genuine cross-PDS test: if this assertion is
// missing, a regression that silently routes reads back to the caller's own
// PDS (e.g. resolveReadEndpoint always returning the caller's endpoint)
// would still produce HTTP 200s and pass.
func assertForeignHost(t *testing.T, label, step, constructedURL, callerHost string) {
	t.Helper()
	h := hostOf(t, constructedURL)
	if h == "" {
		return
	}
	if h == callerHost {
		t.Errorf("STEP4/5 %s %s: constructed URL host %q matches caller's OWN pdsURL host -- expected a foreign (cross-PDS) host; URL was %s", label, step, h, constructedURL)
	}
}

// checkURLShape genuinely validates that xrpcURL's output for a real,
// no-port https PDS base has NOT been corrupted: no injected port, no
// literal double-slash immediately before "xrpc" (both are the exact shapes
// atchess-1c9.41's Mutation B injects to prove this test can fail), and the
// URL's scheme matches base's scheme.
func checkURLShape(u, base string) string {
	if strings.Contains(u, "//xrpc") {
		return fmt.Sprintf("literal double-slash immediately before \"xrpc\" in %q", u)
	}
	parsed, err := url.Parse(u)
	if err != nil {
		return fmt.Sprintf("unparseable URL %q: %v", u, err)
	}
	if parsed.Host == "" {
		return fmt.Sprintf("empty host in %q", u)
	}
	if parsed.Port() != "" {
		return fmt.Sprintf("unexpected port %q injected into %q", parsed.Port(), u)
	}
	baseParsed, err := url.Parse(base)
	if err != nil {
		return fmt.Sprintf("unparseable base URL %q: %v", base, err)
	}
	if parsed.Scheme != baseParsed.Scheme {
		return fmt.Sprintf("scheme changed: base scheme %q, constructed URL scheme %q in %q", baseParsed.Scheme, parsed.Scheme, u)
	}
	return ""
}
