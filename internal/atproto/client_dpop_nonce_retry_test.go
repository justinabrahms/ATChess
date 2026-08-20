package atproto

// Unit tests for atchess-1c9.12's resource-server-side DPoP handling:
// Client.doRequest signing a real DPoP proof (RFC 9449) for OAuth-bound
// (dpopKey != nil) clients, and Client.makeRequest's nonce-challenge retry
// (401 + DPoP-Nonce header), as distinct from -- and NOT interfering with
// -- the pre-existing 401-triggers-ForceRefresh retry path. Style matches
// session_retry_test.go's inline httptest.NewServer fakes; fakeAuthenticator
// is defined there and reused here (same package).

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
	"strings"
	"sync/atomic"
	"testing"
)

// decodeDPoPProofUnverified base64-decodes a DPoP proof JWT's payload
// without verifying its signature -- sufficient for asserting on claims a
// proof we generated ourselves carries.
func decodeDPoPProofUnverified(t *testing.T, proof string) map[string]interface{} {
	t.Helper()
	parts := strings.Split(proof, ".")
	if len(parts) != 3 {
		t.Fatalf("DPoP proof is not a JWT (got %d dot-separated parts): %s", len(parts), proof)
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decoding DPoP proof payload: %v", err)
	}
	var claims map[string]interface{}
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatalf("unmarshaling DPoP proof payload: %v", err)
	}
	return claims
}

func newTestDPoPKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating DPoP key: %v", err)
	}
	return k
}

// TestDoRequest_DPoPBound_SignsRealProof pins the core resource-server gap
// atchess-1c9.12 fixes: prior to this bead, an OAuth-bound
// (NewClientFromSession(..., useDPoP=true, ...)) client set
// "Authorization: DPoP <token>" but attached NO "DPoP" proof header at
// all -- which is not a valid DPoP request (RFC 9449 s4). This asserts the
// proof is now present and its claims are correct: htm/htu match the
// request, and ath is the access token's hash (RFC 9449 s4.3).
func TestDoRequest_DPoPBound_SignsRealProof(t *testing.T) {
	var gotDPoPHeader, gotAuthHeader string
	mockPDS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotDPoPHeader = r.Header.Get("DPoP")
		gotAuthHeader = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"uri":"at://x","cid":"y"}`))
	}))
	defer mockPDS.Close()

	dpopKey := newTestDPoPKey(t)
	auth := &fakeAuthenticator{token: "test-access-token"}
	client, err := NewClientFromSession(mockPDS.URL, "did:plc:test", "test.handle", true, dpopKey, auth)
	if err != nil {
		t.Fatalf("NewClientFromSession: %v", err)
	}

	resp, err := client.makeRequest("POST", mockPDS.URL+"/xrpc/some.method?foo=bar", []byte(`{}`))
	if err != nil {
		t.Fatalf("makeRequest: %v", err)
	}
	defer resp.Body.Close()

	if gotAuthHeader != "DPoP test-access-token" {
		t.Errorf("Authorization header = %q, want %q", gotAuthHeader, "DPoP test-access-token")
	}
	if gotDPoPHeader == "" {
		t.Fatal("expected a DPoP proof header, got none")
	}
	claims := decodeDPoPProofUnverified(t, gotDPoPHeader)
	if got, _ := claims["htm"].(string); got != "POST" {
		t.Errorf("proof htm = %q, want POST", got)
	}
	// htu must NOT include the query string (RFC 9449 s4.2).
	if got, _ := claims["htu"].(string); got != mockPDS.URL+"/xrpc/some.method" {
		t.Errorf("proof htu = %q, want %q (no query string)", got, mockPDS.URL+"/xrpc/some.method")
	}
	if _, ok := claims["ath"]; !ok {
		t.Error("proof is missing the ath (access token hash) claim")
	}
	if _, ok := claims["jti"]; !ok {
		t.Error("proof is missing the jti claim")
	}
}

// TestMakeRequest_DPoP_NonceChallenge_RetriesExactlyOnceWithoutForceRefresh
// pins the resource-server nonce-challenge retry: a 401 carrying a
// DPoP-Nonce header must be retried exactly once with a proof carrying that
// nonce, WITHOUT calling the Authenticator's ForceRefresh -- the access
// token was never the problem, only the missing/stale nonce was.
func TestMakeRequest_DPoP_NonceChallenge_RetriesExactlyOnceWithoutForceRefresh(t *testing.T) {
	var attempts int32
	var secondAttemptNonceClaim string
	mockPDS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n == 1 {
			w.Header().Set("DPoP-Nonce", "resource-nonce-1")
			w.Header().Set("WWW-Authenticate", `DPoP error="use_dpop_nonce"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		secondAttemptNonceClaim, _ = decodeDPoPProofUnverified(t, r.Header.Get("DPoP"))["nonce"].(string)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"uri":"at://x","cid":"y"}`))
	}))
	defer mockPDS.Close()

	dpopKey := newTestDPoPKey(t)
	auth := &fakeAuthenticator{token: "test-access-token", refreshToken: "should-not-be-used"}
	client, err := NewClientFromSession(mockPDS.URL, "did:plc:test", "test.handle", true, dpopKey, auth)
	if err != nil {
		t.Fatalf("NewClientFromSession: %v", err)
	}

	resp, err := client.makeRequest("POST", mockPDS.URL+"/xrpc/some.method", []byte(`{}`))
	if err != nil {
		t.Fatalf("makeRequest: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected the retry to succeed with 200, got %d", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&attempts); got != 2 {
		t.Fatalf("expected exactly 2 attempts (initial + one nonce retry), got %d", got)
	}
	if secondAttemptNonceClaim != "resource-nonce-1" {
		t.Errorf("retry's DPoP proof nonce claim = %q, want %q", secondAttemptNonceClaim, "resource-nonce-1")
	}
	if got := atomic.LoadInt32(&auth.refreshCalls); got != 0 {
		t.Errorf("expected ForceRefresh NOT to be called for a nonce-only challenge, but it was called %d time(s)", got)
	}
}

// TestMakeRequest_DPoP_401WithoutNonceHeader_FallsThroughToForceRefresh
// pins the "401 with no DPoP-Nonce header -> do not loop" edge case at the
// resource-server layer specifically: such a 401 must NOT be treated as a
// nonce challenge (there is nothing to retry with) and must fall through
// to the pre-existing ForceRefresh-and-retry-once behaviour instead of
// being retried a second, unbounded way on top of it.
func TestMakeRequest_DPoP_401WithoutNonceHeader_FallsThroughToForceRefresh(t *testing.T) {
	var attempts int32
	mockPDS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		// No DPoP-Nonce header at all.
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer mockPDS.Close()

	dpopKey := newTestDPoPKey(t)
	auth := &fakeAuthenticator{token: "stale-token", refreshToken: "still-stale-token"}
	client, err := NewClientFromSession(mockPDS.URL, "did:plc:test", "test.handle", true, dpopKey, auth)
	if err != nil {
		t.Fatalf("NewClientFromSession: %v", err)
	}

	resp, err := client.makeRequest("GET", mockPDS.URL+"/xrpc/some.method", nil)
	if err != nil {
		t.Fatalf("makeRequest returned an error instead of a response: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected the persistent 401 to surface as-is, got %d", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&attempts); got != 2 {
		t.Fatalf("expected EXACTLY 2 attempts total (initial + one ForceRefresh retry, no nonce-loop on top), got %d", got)
	}
	if got := atomic.LoadInt32(&auth.refreshCalls); got != 1 {
		t.Errorf("expected exactly 1 ForceRefresh call, got %d", got)
	}
}

// TestDoRequest_DPoP_ReusesNonceStoredByAnotherClientInstance pins
// atchess-1c9.12 step 4 ("store nonce per authorization-server origin and
// reuse it") specifically for the resource-server path, where
// internal/web builds a BRAND NEW *atproto.Client per incoming HTTP
// request (Service.clientFor) -- so the nonce store must be shared across
// Client instances, not private to one, for the "one request" common case
// to ever be reached in production.
func TestDoRequest_DPoP_ReusesNonceStoredByAnotherClientInstance(t *testing.T) {
	var attempts int32
	mockPDS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		nonce, _ := decodeDPoPProofUnverified(t, r.Header.Get("DPoP"))["nonce"].(string)
		if nonce != "shared-nonce" {
			w.Header().Set("DPoP-Nonce", "shared-nonce")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"uri":"at://x","cid":"y"}`))
	}))
	defer mockPDS.Close()

	dpopKey := newTestDPoPKey(t)

	// First Client instance pays the failed first attempt and learns the
	// nonce (which lands in the shared, process-wide dpop.DefaultNonceStore).
	auth1 := &fakeAuthenticator{token: "tok1"}
	client1, err := NewClientFromSession(mockPDS.URL, "did:plc:test", "test.handle", true, dpopKey, auth1)
	if err != nil {
		t.Fatalf("NewClientFromSession (client1): %v", err)
	}
	resp1, err := client1.makeRequest("GET", mockPDS.URL+"/xrpc/some.method", nil)
	if err != nil {
		t.Fatalf("client1.makeRequest: %v", err)
	}
	resp1.Body.Close()
	if got := atomic.LoadInt32(&attempts); got != 2 {
		t.Fatalf("client1: expected 2 attempts (it didn't know the nonce yet), got %d", got)
	}

	// A brand new Client instance (as clientFor would build per-request)
	// against the SAME origin should now succeed on its very first attempt.
	atomic.StoreInt32(&attempts, 0)
	auth2 := &fakeAuthenticator{token: "tok2"}
	client2, err := NewClientFromSession(mockPDS.URL, "did:plc:test", "test.handle", true, dpopKey, auth2)
	if err != nil {
		t.Fatalf("NewClientFromSession (client2): %v", err)
	}
	resp2, err := client2.makeRequest("GET", mockPDS.URL+"/xrpc/some.method", nil)
	if err != nil {
		t.Fatalf("client2.makeRequest: %v", err)
	}
	defer resp2.Body.Close()

	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Fatalf("client2 (fresh instance, same origin): expected exactly 1 attempt reusing the shared nonce, got %d", got)
	}
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("expected client2's single attempt to succeed, got HTTP %d", resp2.StatusCode)
	}
}

// TestCreateGame_PersistentDPoPNonceChallenge_RotatingNonce_ExactlyTwoAttempts
// pins atchess-1c9.86 item 2: makeRequest (~334-357) is structurally safe
// against a resource server that NEVER accepts our nonce -- it returns a
// FRESH DPoP-Nonce on every single response, so a naive "retry while we
// got a new nonce" implementation would spin forever. The current code is
// non-recursive and each branch returns, making at most one extra request
// -- this test pins that so a future refactor toward a retry loop can't
// silently reintroduce an unbounded loop here.
//
// CreateGame (rather than calling makeRequest directly, as the other
// tests in this file do) is used specifically so a genuine error()
// propagates to the caller -- makeRequest itself just returns the second
// (still-401) response with err == nil; it's the caller's status-code
// check that turns a persistent 401 into an actual Go error.
func TestCreateGame_PersistentDPoPNonceChallenge_RotatingNonce_ExactlyTwoAttempts(t *testing.T) {
	var attempts int32
	mockPDS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		// A DIFFERENT nonce on every single response -- the retry can
		// never converge on a nonce the server will accept, so if the
		// implementation ever starts looping "while we got a fresh
		// nonce", this server will never stop feeding it one.
		w.Header().Set("DPoP-Nonce", fmt.Sprintf("resource-nonce-%d", n))
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer mockPDS.Close()

	dpopKey := newTestDPoPKey(t)
	auth := &fakeAuthenticator{token: "test-access-token", refreshToken: "should-not-be-used"}
	client, err := NewClientFromSession(mockPDS.URL, "did:plc:test", "test.handle", true, dpopKey, auth)
	if err != nil {
		t.Fatalf("NewClientFromSession: %v", err)
	}

	_, err = client.CreateGame(context.Background(), "did:plc:opponent", "white")
	if err == nil {
		t.Fatal("expected an error from a persistent 401 (rotating nonce), got nil")
	}
	if got := atomic.LoadInt32(&attempts); got != 2 {
		t.Fatalf("expected EXACTLY 2 attempts (no unbounded retry loop chasing the rotating nonce), got %d", got)
	}
	if got := atomic.LoadInt32(&auth.refreshCalls); got != 0 {
		t.Errorf("expected ForceRefresh NOT to be called (every 401 carried a DPoP-Nonce, so the nonce-challenge branch -- not the ForceRefresh branch -- handles it), got %d call(s)", got)
	}
}

// TestMakeRequest_DPoP_NonceChallenge_SignsDistinctJTIPerAttempt pins
// atchess-1c9.86 item 3 at the resource-server layer: doRequest
// (~internal/atproto/client.go:405) signs a fresh DPoP proof on every
// call, including the retry, rather than reusing one built for an earlier
// attempt -- which is what prevents stale-iat rejection under clock skew.
// jti is asserted (rather than iat) because RFC 9449 requires it be
// unique per proof and, unlike iat, it doesn't depend on wall-clock
// granularity -- so this test can't flake by completing both attempts
// within the same second.
func TestMakeRequest_DPoP_NonceChallenge_SignsDistinctJTIPerAttempt(t *testing.T) {
	var firstJTI, secondJTI string
	mockPDS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		jti, _ := decodeDPoPProofUnverified(t, r.Header.Get("DPoP"))["jti"].(string)
		if firstJTI == "" {
			firstJTI = jti
			w.Header().Set("DPoP-Nonce", "resource-nonce-1")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		secondJTI = jti
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"uri":"at://x","cid":"y"}`))
	}))
	defer mockPDS.Close()

	dpopKey := newTestDPoPKey(t)
	auth := &fakeAuthenticator{token: "test-access-token"}
	client, err := NewClientFromSession(mockPDS.URL, "did:plc:test", "test.handle", true, dpopKey, auth)
	if err != nil {
		t.Fatalf("NewClientFromSession: %v", err)
	}

	resp, err := client.makeRequest("POST", mockPDS.URL+"/xrpc/some.method", []byte(`{}`))
	if err != nil {
		t.Fatalf("makeRequest: %v", err)
	}
	defer resp.Body.Close()

	if firstJTI == "" || secondJTI == "" {
		t.Fatalf("expected both attempts to carry a jti claim, got first=%q second=%q", firstJTI, secondJTI)
	}
	if firstJTI == secondJTI {
		t.Errorf("both attempts' DPoP proofs carried the SAME jti (%q) -- the proof was reused/hoisted instead of freshly signed per attempt", firstJTI)
	}
}
