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
// rewrote it as TestConformance_PARIsAttemptedWhenRequired, which exercises
// BuildAuthorizationURLAuto instead and is expected to (and does) PASS.
//
// atchess-1c9.83 then moved TestConformance_PARIsAttemptedWhenRequired out
// of this file into internal/web/oauth_conformance_test.go: that test
// reaches its authorization-server metadata by walking the SAME
// handle -> DID -> PDS -> resource-server -> authorization-server
// resolution chain production code implements in
// internal/web/oauth_handlers.go's resolveOAuthEndpoints, and the last two
// hops of that chain (getAuthorizationServer, getAuthServerMetadata) are
// unexported functions on package web. This package's test files cannot
// import internal/web to reach them: web already imports internal/oauth,
// so the reverse import from an internal (same-package) oauth test file
// would be a cycle. Living in package web instead lets that test call
// those hops directly, as production code, rather than re-implementing
// them locally -- see that file's doc comment for the full hop-by-hop
// breakdown of what does and does not run production code.
//
// TestConformance_DPoPNonceRetry below has no such constraint (it only
// exercises this package's own unexported internals: createDPoPToken and a
// struct literal *OAuthClient), so it stays here.
//
// If bsky.social's identity, PDS, or handle changes, update
// internal/web/oauth_conformance_test.go's conformanceTestHandle.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func requireConformanceGate(t *testing.T) {
	t.Helper()
	if os.Getenv("ATCHESS_OAUTH_CONFORMANCE") != "1" {
		t.Skip("skipping network-gated OAuth conformance test; set ATCHESS_OAUTH_CONFORMANCE=1 to run it (requires live network access to bsky.social)")
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
