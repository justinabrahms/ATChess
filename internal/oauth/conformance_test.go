package oauth

// Conformance tests (originally atchess-1c9.7) that check our OAuth client
// against what a real AT Protocol authorization server actually requires.
// These are network-gated behind ATCHESS_OAUTH_CONFORMANCE=1 and skipped by
// default so ordinary unit runs and CI stay green.
//
// History: atchess-1c9.7 originally landed TestConformance_PARRequiredButNotUsed,
// pinning a real, then-unaddressed gap (PAR was advertised but not used) and
// EXPECTED it to fail until atchess-1c9.12 shipped PAR support. But it
// asserted against the plain, non-PAR BuildAuthorizationURL specifically --
// and that builder must never perform PAR (some authorization servers,
// including the local dual-PDS harness's, don't advertise a PAR endpoint at
// all, so a non-PAR path is a hard requirement). That made the assertion
// structurally unpassable even after atchess-1c9.12 added PAR support via a
// new method, BuildAuthorizationURLAuto. atchess-1c9.85 diagnosed this and
// rewrote it as TestConformance_PARIsAttemptedWhenRequired below, which
// exercises BuildAuthorizationURLAuto instead and is expected to (and does)
// PASS -- see that test's own doc comment for what it accepts as proof PAR
// was attempted.
//
// A note on how this reaches the metadata: the bead asks us to exercise the
// resolution chain the production code already implements in
// internal/web/oauth_handlers.go's resolveOAuthEndpoints (handle -> DID ->
// PDS -> resource-server -> authorization-server metadata), rather than
// hardcoding the well-known URL. We can't call resolveOAuthEndpoints
// directly: it is an unexported method on web.Service in a different
// package, its constructor (atproto.NewClient) requires real login
// credentials we deliberately don't have here, and even if we could call
// it, its last hop (getAuthorizationEndpoint) discards every metadata
// field except authorization_endpoint -- exactly the PAR fields this test
// needs are thrown away. So this test reconstructs the same chain using:
//   - a direct, unauthenticated call to com.atproto.identity.resolveHandle
//     (the same first-choice strategy production ResolveHandle tries via
//     resolveHandleSamePDS, and the one that succeeds for the public handle
//     used below -- verified manually against the live server), since
//     instantiating an *atproto.Client requires credentials we don't have;
//   - atproto.ResolvePDS, the actual exported function production code
//     calls for the DID -> PDS hop;
//   - and small local re-implementations of the PDS -> resource-server and
//     resource-server -> authorization-server hops (same URL patterns as
//     web.getAuthorizationServer / web.getAuthorizationEndpoint), because
//     those are unexported and, again, drop the fields we need to assert
//     on.
//
// If bsky.social's identity, PDS, or handle changes, update
// conformanceTestHandle below.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/justinabrahms/atchess/internal/atproto"
)

// conformanceTestHandle is a public bsky.social-hosted handle used only to
// exercise the resolution chain below. No credentials of any kind are
// needed or used -- everything fetched here is public metadata.
const conformanceTestHandle = "bsky.app"

func requireConformanceGate(t *testing.T) {
	t.Helper()
	if os.Getenv("ATCHESS_OAUTH_CONFORMANCE") != "1" {
		t.Skip("skipping network-gated OAuth conformance test; set ATCHESS_OAUTH_CONFORMANCE=1 to run it (requires live network access to bsky.social)")
	}
}

// oauthAuthServerMetadata mirrors (a superset of) the fields returned by an
// AT Protocol authorization server's
// /.well-known/oauth-authorization-server document. web.getAuthorizationEndpoint
// only extracts AuthorizationEndpoint from this document and discards
// everything else, so we re-parse it here to also see the PAR-related
// fields that assertion needs.
type oauthAuthServerMetadata struct {
	AuthorizationEndpoint              string   `json:"authorization_endpoint"`
	TokenEndpoint                      string   `json:"token_endpoint"`
	PushedAuthorizationRequestEndpoint string   `json:"pushed_authorization_request_endpoint"`
	RequirePushedAuthorizationRequests bool     `json:"require_pushed_authorization_requests"`
	DPoPSigningAlgValuesSupported      []string `json:"dpop_signing_alg_values_supported"`
}

// resolveHandleToDIDPublic resolves handle -> DID via the same public,
// unauthenticated com.atproto.identity.resolveHandle XRPC call that
// production's resolveHandleSamePDS strategy (the first strategy
// atproto.Client.ResolveHandle tries) makes -- without needing an
// authenticated *atproto.Client, which we deliberately don't construct
// here.
func resolveHandleToDIDPublic(ctx context.Context, pdsURL, handle string) (string, error) {
	u := pdsURL + "/xrpc/com.atproto.identity.resolveHandle?handle=" + url.QueryEscape(handle)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("resolveHandle: HTTP %d", resp.StatusCode)
	}
	var out struct {
		DID string `json:"did"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.DID == "" {
		return "", fmt.Errorf("resolveHandle: empty did in response")
	}
	return out.DID, nil
}

// fetchAuthorizationServerPublic re-implements the PDS -> resource-server
// hop web.getAuthorizationServer performs (unexported, so not directly
// callable from here): GET pdsURL/.well-known/oauth-protected-resource and
// return its first authorization_servers entry.
func fetchAuthorizationServerPublic(ctx context.Context, pdsURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pdsURL+"/.well-known/oauth-protected-resource", nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("oauth-protected-resource: HTTP %d", resp.StatusCode)
	}
	var out struct {
		AuthorizationServers []string `json:"authorization_servers"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if len(out.AuthorizationServers) == 0 {
		return "", fmt.Errorf("no authorization_servers advertised")
	}
	return out.AuthorizationServers[0], nil
}

// fetchAuthServerMetadataPublic re-implements the resource-server ->
// authorization-server hop web.getAuthorizationEndpoint performs
// (unexported, and it discards every field except authorization_endpoint):
// GET authServerURL/.well-known/oauth-authorization-server and return the
// full document.
func fetchAuthServerMetadataPublic(ctx context.Context, authServerURL string) (*oauthAuthServerMetadata, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, authServerURL+"/.well-known/oauth-authorization-server", nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("oauth-authorization-server: HTTP %d", resp.StatusCode)
	}
	var out oauthAuthServerMetadata
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

// TestConformance_PARIsAttemptedWhenRequired verifies, against a live
// authorization server that advertises Pushed Authorization Request (PAR)
// support, that our client actually attempts PAR when it should --
// exercising BuildAuthorizationURLAuto (atchess-1c9.12's production entry
// point), not the plain BuildAuthorizationURL, which must never perform PAR
// (some authorization servers, including the local dual-PDS harness's,
// advertise no PAR endpoint at all, so a non-PAR path is a hard
// requirement, not a gap).
//
// This test cannot complete a full PAR round trip: a live authorization
// server's PAR endpoint requires fetching a resolvable client-metadata.json
// for our client_id, and hosting real, live client metadata for a test
// fixture would make a unit test depend on live infrastructure. Instead it
// grades whether a well-formed PAR request reached a DEFINITIVE response
// from the authorization server:
//
//   - "invalid_client_metadata" is accepted as proof-of-attempt: it proves
//     the AS accepted our PAR request as well-formed enough to go try to
//     fetch our (deliberately unresolvable) client metadata -- exactly as
//     far as we can prove without hosting real metadata.
//   - "use_dpop_nonce" surfacing here is a FAILURE: postFormWithDPoPRetry
//     is supposed to retry exactly once against a 400 use_dpop_nonce
//     challenge and only return that error if the second attempt is
//     ALSO challenged (or the server broke the challenge contract by
//     omitting DPoP-Nonce) -- either way, seeing it here means our nonce
//     retry mechanics did not do their job.
//   - anything else (no PAR attempt at all, a network-level failure before
//     any AS response, or a request the AS rejected for some earlier
//     reason than the client-metadata fetch) is also a FAILURE.
//
// This test was previously named TestConformance_PARRequiredButNotUsed and
// asserted (against the PLAIN builder) that PAR was NOT used -- an
// assertion atchess-1c9.7 correctly pinned as a known gap, which
// atchess-1c9.12 then closed by adding BuildAuthorizationURLAuto. But
// because that assertion targeted the plain builder specifically, and the
// plain builder must never do PAR, it could never be made to pass by any
// production change; see atchess-1c9.85 for the full diagnosis. It has been
// restructured (this comment) to grade the real PAR path instead.
func TestConformance_PARIsAttemptedWhenRequired(t *testing.T) {
	requireConformanceGate(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// handle -> DID
	did, err := resolveHandleToDIDPublic(ctx, "https://bsky.social", conformanceTestHandle)
	if err != nil {
		t.Fatalf("resolving handle %q to a DID: %v", conformanceTestHandle, err)
	}

	// DID -> PDS, via the actual exported function production code
	// (resolveOAuthEndpoints) calls for this hop.
	pdsURL, err := atproto.ResolvePDS(ctx, did, "")
	if err != nil {
		t.Fatalf("resolving PDS for %s: %v", did, err)
	}

	// PDS -> authorization server.
	authServerURL, err := fetchAuthorizationServerPublic(ctx, pdsURL)
	if err != nil {
		t.Fatalf("resolving authorization server for PDS %s: %v", pdsURL, err)
	}

	// authorization server -> full metadata (kept in full, unlike
	// web.getAuthorizationEndpoint, so we can see the PAR fields).
	metadata, err := fetchAuthServerMetadataPublic(ctx, authServerURL)
	if err != nil {
		t.Fatalf("fetching authorization server metadata from %s: %v", authServerURL, err)
	}

	parAdvertised := metadata.PushedAuthorizationRequestEndpoint != "" || metadata.RequirePushedAuthorizationRequests
	if !parAdvertised {
		t.Fatalf("test precondition failed: %s no longer advertises PAR support "+
			"(pushed_authorization_request_endpoint=%q, require_pushed_authorization_requests=%v) -- "+
			"this test needs updating, not the production client",
			authServerURL, metadata.PushedAuthorizationRequestEndpoint, metadata.RequirePushedAuthorizationRequests)
	}

	// Build a real client the same way production does and see what it
	// actually sends.
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating test client key: %v", err)
	}
	client := &OAuthClient{
		clientID:     "https://atchess.example.com/client-metadata.json",
		redirectURI:  "https://atchess.example.com/api/callback",
		privateKey:   privateKey,
		publicKeyJWK: GetPublicKeyJWK(privateKey),
		httpClient:   &http.Client{Timeout: 10 * time.Second},
	}

	verifier, challenge, err := GeneratePKCE()
	if err != nil {
		t.Fatalf("generating PKCE: %v", err)
	}
	_ = verifier
	state, err := GenerateState()
	if err != nil {
		t.Fatalf("generating state: %v", err)
	}

	// The same per-session DPoP key production generates in
	// internal/web/oauth_handlers.go before calling BuildAuthorizationURLAuto.
	dpopKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating DPoP key: %v", err)
	}

	// This is the actual production entry point (see
	// internal/web/oauth_handlers.go): it selects PAR because parEndpoint
	// (metadata.PushedAuthorizationRequestEndpoint) is non-empty, and
	// PushAuthorizationRequest actually POSTs to the live PAR endpoint.
	authURL, buildErr := client.BuildAuthorizationURLAuto(
		metadata.AuthorizationEndpoint, metadata.PushedAuthorizationRequestEndpoint,
		authServerURL, conformanceTestHandle, state, challenge, dpopKey,
		metadata.RequirePushedAuthorizationRequests)

	switch {
	case buildErr == nil:
		// A full PAR round trip succeeded outright (e.g. the AS doesn't
		// actually enforce resolving client_id's metadata for this
		// request). That's strictly stronger proof of a correct PAR
		// attempt than what we normally expect to observe here -- pass.
		parsed, perr := url.Parse(authURL)
		if perr != nil {
			t.Fatalf("BuildAuthorizationURLAuto produced an unparseable URL: %v", perr)
		}
		if parsed.Query().Get("request_uri") == "" {
			t.Errorf("BuildAuthorizationURLAuto returned no error but the resulting authorization URL has no "+
				"request_uri, meaning it silently fell back to the plain (non-PAR) builder even though a PAR "+
				"endpoint was advertised: %s", authURL)
			break
		}
		t.Logf("PASS via FULL PAR ROUND TRIP: %s accepted the request and returned a request_uri. Note this is "+
			"NOT the branch normally expected here -- it means the AS no longer rejects our unresolvable fixture "+
			"client_id, so the proof-of-attempt path below has stopped being exercised.",
			metadata.PushedAuthorizationRequestEndpoint)
	case strings.Contains(buildErr.Error(), "use_dpop_nonce"):
		// postFormWithDPoPRetry is supposed to retry exactly once against
		// a 400 use_dpop_nonce challenge and succeed (or fail for an
		// unrelated reason) on the retry. Seeing use_dpop_nonce escape
		// all the way out here means that retry did not do its job.
		t.Errorf("PAR request to %s was challenged with use_dpop_nonce and never got past it -- our DPoP "+
			"nonce retry mechanics did not work: %v", metadata.PushedAuthorizationRequestEndpoint, buildErr)
	case strings.Contains(buildErr.Error(), "invalid_client_metadata"):
		// Proof-of-attempt: the authorization server accepted our PAR
		// POST as well-formed enough to go try to fetch client metadata
		// for our (deliberately unresolvable, non-live) client_id, and
		// rejected it only because that fetch failed. That's as far as
		// this test can prove without hosting real, live client metadata
		// -- which would make a unit fixture depend on live
		// infrastructure. This is a PASS.
		//
		// Logged rather than silent: this branch and the err==nil branch
		// above are both passes but record materially different facts about
		// the live server. Without a log line a change in the AS's behaviour
		// would flip which one fires and nobody would notice.
		t.Logf("PASS via PROOF-OF-ATTEMPT: PAR POST to %s was well-formed and reached the client-metadata "+
			"fetch stage, rejected only because the fixture client_id is deliberately unresolvable: %v",
			metadata.PushedAuthorizationRequestEndpoint, buildErr)
	default:
		t.Errorf("PAR request to %s (advertised by authorization server %s, "+
			"require_pushed_authorization_requests=%v) did not reach the expected definitive response -- "+
			"either PAR was never attempted, or the authorization server rejected the request for a reason "+
			"other than fetching our client metadata: %v",
			metadata.PushedAuthorizationRequestEndpoint, authServerURL,
			metadata.RequirePushedAuthorizationRequests, buildErr)
	}
}

// TestConformance_DPoPNonceRetry pins two related findings about
// createDPoPToken / token-endpoint DPoP nonce handling:
//
//  1. createDPoPToken DOES include a "nonce" claim when one is supplied
//     (this sub-assertion is expected to PASS).
//  2. ExchangeCodeForTokens DOES correctly retry once, and succeed, when the
//     token endpoint responds with the AT Protocol / RFC 9449 §8.2
//     authorization-server nonce challenge shape: HTTP 400 with
//     error=="use_dpop_nonce" and a DPoP-Nonce response header (this
//     sub-assertion is also expected to PASS -- see the comment just above
//     it for why).
func TestConformance_DPoPNonceRetry(t *testing.T) {
	requireConformanceGate(t)

	// --- Sub-assertion 1: createDPoPToken includes the nonce claim. ---
	dpopKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating DPoP key: %v", err)
	}
	const wantNonce = "conformance-test-nonce-abc123"
	tokenStr, err := createDPoPToken(dpopKey, "POST", "https://example.com/token", "", wantNonce)
	if err != nil {
		t.Fatalf("createDPoPToken: %v", err)
	}
	claims, err := decodeJWTPayloadUnverified(tokenStr)
	if err != nil {
		t.Fatalf("decoding DPoP proof payload: %v", err)
	}
	if got, _ := claims["nonce"].(string); got != wantNonce {
		t.Errorf("createDPoPToken(nonce=%q) produced a proof whose payload has nonce=%q, want %q", wantNonce, got, wantNonce)
	}

	// --- Sub-assertion 2: a 400 use_dpop_nonce challenge (with a
	// DPoP-Nonce header) triggers exactly one retry that then succeeds.
	//
	// An earlier version of this test asserted a 401 as the trigger. That
	// was wrong and has been corrected: RFC 9449 splits DPoP nonce
	// challenges by which party issues them -- an authorization server's
	// token endpoint (what ExchangeCodeForTokens calls) signals a required
	// nonce with HTTP 400 + error=="use_dpop_nonce" (§8.2), while only a
	// resource server signals it with HTTP 401 + WWW-Authenticate: DPoP
	// (§9). The AT Protocol OAuth spec likewise tells clients to expect
	// and retry on "HTTP 400 errors" carrying use_dpop_nonce. A live probe
	// against https://bsky.social/oauth/token confirms this: it returns
	// 400, never 401, for this challenge. internal/oauth/client.go already
	// implements the 400/use_dpop_nonce retry correctly, which is exactly
	// what this sub-assertion (now) verifies. Do not change the trigger
	// back to 401 -- that would reintroduce a defect this test doesn't
	// have and doesn't exist in the code. (The real resource-server-side
	// 401 gap, if any, is tracked separately by atchess-1c9.82; it is out
	// of scope for this token-endpoint test.)
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			w.Header().Set("DPoP-Nonce", "server-issued-nonce-xyz")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"use_dpop_nonce","error_description":"nonce required"}`))
			return
		}
		// A correctly-retrying client arrives here on its second attempt
		// with a DPoP proof carrying the issued nonce.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"access_token":"tok","token_type":"DPoP","expires_in":3600,"refresh_token":"rt","scope":"atproto","sub":"did:example:test"}`))
	}))
	defer server.Close()

	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating client key: %v", err)
	}
	client := &OAuthClient{
		clientID:     "https://atchess.example.com/client-metadata.json",
		redirectURI:  "https://atchess.example.com/api/callback",
		privateKey:   privateKey,
		publicKeyJWK: GetPublicKeyJWK(privateKey),
		httpClient:   &http.Client{Timeout: 10 * time.Second},
	}

	_, err = client.ExchangeCodeForTokens(server.URL, "https://atchess.example.com/issuer", "test-code", "test-verifier", dpopKey)
	if err != nil {
		t.Errorf("expected ExchangeCodeForTokens to retry after a 400 use_dpop_nonce response carrying a "+
			"DPoP-Nonce header and succeed on retry, but it returned an error instead: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("expected exactly 2 requests to the token endpoint (initial attempt + one nonce retry) "+
			"after a 400 use_dpop_nonce challenge, got %d", got)
	}
}

// decodeJWTPayloadUnverified base64-decodes a JWT's payload segment without
// verifying its signature. Sufficient for asserting on claims we produced
// ourselves in-process; do not reuse this for anything that trusts an
// externally-supplied token.
func decodeJWTPayloadUnverified(tokenStr string) (map[string]interface{}, error) {
	parts := strings.Split(tokenStr, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("not a JWT (expected 3 dot-separated parts, got %d)", len(parts))
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decoding payload segment: %w", err)
	}
	var claims map[string]interface{}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, fmt.Errorf("unmarshaling payload: %w", err)
	}
	return claims, nil
}
