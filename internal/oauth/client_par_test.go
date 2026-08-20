package oauth

// Unit tests for the atchess-1c9.12 production logic: Pushed Authorization
// Requests, the shared DPoP nonce-retry mechanism (postFormWithDPoPRetry),
// and the refresh_token grant. Unlike conformance_test.go, these run in the
// default (non-network-gated) suite against local httptest servers, and
// specifically cover the edge cases atchess-1c9.12 calls out: a nonce that
// rotates on every response, a nonce challenge with no DPoP-Nonce header
// (must fail without looping), retry-exactly-once, and PAR-vs-fallback
// selection.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/justinabrahms/atchess/internal/dpop"
)

func testOAuthClient(t *testing.T) *OAuthClient {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating test client key: %v", err)
	}
	return &OAuthClient{
		clientID:     "https://atchess.example.com/client-metadata.json",
		redirectURI:  "https://atchess.example.com/api/callback",
		privateKey:   priv,
		publicKeyJWK: GetPublicKeyJWK(priv),
		httpClient:   &http.Client{},
		// A fresh, test-local nonce store (rather than the shared
		// process-wide default) so tests in this file can't observe or
		// pollute each other's nonce state even though they all run in
		// the same process.
		nonceStore: dpop.NewNonceStore(),
	}
}

func testDPoPKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating DPoP key: %v", err)
	}
	return k
}

// --- PushAuthorizationRequest / BuildAuthorizationURLAuto ---

func TestPushAuthorizationRequest_Success(t *testing.T) {
	var gotForm url.Values
	var gotDPoPHeader string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parsing PAR request form: %v", err)
		}
		gotForm = r.PostForm
		gotDPoPHeader = r.Header.Get("DPoP")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"request_uri":"urn:ietf:params:oauth:request_uri:abc123","expires_in":60}`))
	}))
	defer server.Close()

	client := testOAuthClient(t)
	dpopKey := testDPoPKey(t)

	requestURI, err := client.PushAuthorizationRequest(server.URL, "https://as.example.com", "alice.test", "state123", "challenge123", dpopKey)
	if err != nil {
		t.Fatalf("PushAuthorizationRequest: %v", err)
	}
	if requestURI != "urn:ietf:params:oauth:request_uri:abc123" {
		t.Errorf("got request_uri %q, want the PAR response's request_uri", requestURI)
	}
	if got := gotForm.Get("client_id"); got != client.clientID {
		t.Errorf("PAR request client_id = %q, want %q", got, client.clientID)
	}
	if got := gotForm.Get("state"); got != "state123" {
		t.Errorf("PAR request state = %q, want %q", got, "state123")
	}
	if got := gotForm.Get("login_hint"); got != "alice.test" {
		t.Errorf("PAR request login_hint = %q, want %q", got, "alice.test")
	}
	if gotForm.Get("client_assertion") == "" {
		t.Error("PAR request did not include a client_assertion")
	}
	if gotDPoPHeader == "" {
		t.Error("PAR request did not include a DPoP proof header even though a dpopKey was supplied")
	}

	authURL := client.BuildAuthorizationURLFromRequestURI("https://as.example.com/authorize", requestURI)
	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parsing built authorization URL: %v", err)
	}
	q := parsed.Query()
	if q.Get("request_uri") != requestURI {
		t.Errorf("authorization URL request_uri = %q, want %q", q.Get("request_uri"), requestURI)
	}
	if q.Get("client_id") != client.clientID {
		t.Errorf("authorization URL client_id = %q, want %q", q.Get("client_id"), client.clientID)
	}
	// RFC 9126 s4: no other authorization parameter belongs on this URL.
	if q.Get("code_challenge") != "" || q.Get("state") != "" || q.Get("scope") != "" {
		t.Errorf("PAR-derived authorization URL must carry only client_id and request_uri, got: %s", authURL)
	}
}

// TestBuildAuthorizationURLAuto_SelectsPARWhenAdvertised is the
// PAR-vs-fallback selection unit test atchess-1c9.12 asks for: given a
// non-empty PAR endpoint, BuildAuthorizationURLAuto must use it.
func TestBuildAuthorizationURLAuto_SelectsPARWhenAdvertised(t *testing.T) {
	var parCalls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&parCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"request_uri":"urn:ietf:params:oauth:request_uri:xyz","expires_in":60}`))
	}))
	defer server.Close()

	client := testOAuthClient(t)
	dpopKey := testDPoPKey(t)

	authURL, err := client.BuildAuthorizationURLAuto("https://as.example.com/authorize", server.URL, "https://as.example.com", "alice.test", "state1", "chal1", dpopKey, true)
	if err != nil {
		t.Fatalf("BuildAuthorizationURLAuto: %v", err)
	}
	if atomic.LoadInt32(&parCalls) != 1 {
		t.Fatalf("expected exactly 1 PAR call when a PAR endpoint is advertised, got %d", parCalls)
	}
	parsed, _ := url.Parse(authURL)
	if parsed.Query().Get("request_uri") == "" {
		t.Errorf("expected a PAR-derived authorization URL (request_uri set), got: %s", authURL)
	}
}

// TestBuildAuthorizationURLAuto_FallsBackWhenNoPAREndpoint is the other
// half of the PAR-vs-fallback selection test: an empty PAR endpoint (the
// shape a server that doesn't advertise PAR -- e.g. the local dual-PDS
// harness -- produces) must fall back to the plain query-parameter URL,
// making NO network call to any PAR endpoint at all.
func TestBuildAuthorizationURLAuto_FallsBackWhenNoPAREndpoint(t *testing.T) {
	client := testOAuthClient(t)
	dpopKey := testDPoPKey(t)

	authURL, err := client.BuildAuthorizationURLAuto("https://as.example.com/authorize", "", "https://as.example.com", "alice.test", "state1", "chal1", dpopKey, false)
	if err != nil {
		t.Fatalf("BuildAuthorizationURLAuto: %v", err)
	}
	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parsing fallback authorization URL: %v", err)
	}
	q := parsed.Query()
	if q.Get("request_uri") != "" {
		t.Errorf("expected NO request_uri on the fallback URL (no PAR endpoint was given), got: %s", authURL)
	}
	if q.Get("state") != "state1" || q.Get("code_challenge") != "chal1" {
		t.Errorf("fallback URL must carry the plain query parameters directly, got: %s", authURL)
	}
}

// --- DPoP nonce retry (via ExchangeCodeForTokens, exercising
// postFormWithDPoPRetry) ---

// TestExchangeCodeForTokens_RetriesExactlyOnceOnNonceChallenge pins
// "retry-exactly-once": a single 400 use_dpop_nonce challenge (with a
// DPoP-Nonce header) is retried once and succeeds, and no more than 2
// requests are ever made.
func TestExchangeCodeForTokens_RetriesExactlyOnceOnNonceChallenge(t *testing.T) {
	var calls int32
	var secondAttemptNonceClaim string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			w.Header().Set("DPoP-Nonce", "nonce-1")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"use_dpop_nonce"}`))
			return
		}
		secondAttemptNonceClaim = dpopProofNonceClaim(t, r.Header.Get("DPoP"))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"access_token":"tok","token_type":"DPoP","expires_in":3600,"refresh_token":"rt","scope":"atproto","sub":"did:example:test"}`))
	}))
	defer server.Close()

	client := testOAuthClient(t)
	dpopKey := testDPoPKey(t)

	tokens, err := client.ExchangeCodeForTokens(server.URL, "https://as.example.com", "code123", "verifier123", dpopKey)
	if err != nil {
		t.Fatalf("ExchangeCodeForTokens: %v", err)
	}
	if tokens.AccessToken != "tok" {
		t.Errorf("got access token %q, want %q", tokens.AccessToken, "tok")
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("expected exactly 2 requests (initial + one nonce retry), got %d", got)
	}
	if secondAttemptNonceClaim != "nonce-1" {
		t.Errorf("second attempt's DPoP proof nonce claim = %q, want %q (the nonce from the first response)", secondAttemptNonceClaim, "nonce-1")
	}
}

// TestExchangeCodeForTokens_NonceChallengeWithoutHeader_FailsWithoutLooping
// pins the "401/400 with no nonce header -> fail, do not loop" edge case:
// a use_dpop_nonce challenge that (against spec) carries no DPoP-Nonce
// header must return an error after exactly ONE request, never retrying
// blindly.
func TestExchangeCodeForTokens_NonceChallengeWithoutHeader_FailsWithoutLooping(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		// Deliberately NO DPoP-Nonce header.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"use_dpop_nonce"}`))
	}))
	defer server.Close()

	client := testOAuthClient(t)
	dpopKey := testDPoPKey(t)

	_, err := client.ExchangeCodeForTokens(server.URL, "https://as.example.com", "code123", "verifier123", dpopKey)
	if err == nil {
		t.Fatal("expected an error when the server issues a use_dpop_nonce challenge with no DPoP-Nonce header, got nil")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected EXACTLY 1 request (no retry loop) when the nonce challenge carries no DPoP-Nonce header, got %d", got)
	}
}

// TestExchangeCodeForTokens_RotatingNonce_UpdatesStoreEvenOnFinalFailure
// pins the "nonce rotates on every response" edge case: if the server
// issues a DIFFERENT nonce on every response (so the retry also fails),
// postFormWithDPoPRetry must still (a) stop after exactly one retry (not
// loop forever chasing the rotating nonce) and (b) leave the nonce store
// holding the LATEST nonce it saw, so the next independent call converges
// immediately instead of repeating the same failure.
func TestExchangeCodeForTokens_RotatingNonce_UpdatesStoreEvenOnFinalFailure(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		// A fresh nonce on every single response, forever -- the store
		// can never "catch up" mid-call.
		w.Header().Set("DPoP-Nonce", "rotating-nonce-"+string(rune('A'+n-1)))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"use_dpop_nonce"}`))
	}))
	defer server.Close()

	client := testOAuthClient(t)
	dpopKey := testDPoPKey(t)

	_, err := client.ExchangeCodeForTokens(server.URL, "https://as.example.com", "code123", "verifier123", dpopKey)
	if err == nil {
		t.Fatal("expected an error: the server never accepts any nonce we send")
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("expected EXACTLY 2 requests (initial + one retry, even though the nonce keeps rotating), got %d", got)
	}

	origin := dpop.OriginOf(server.URL)
	if got := client.nonces().Get(origin); got != "rotating-nonce-B" {
		t.Errorf("nonce store for %s = %q, want %q (the nonce from the SECOND/last response observed)", origin, got, "rotating-nonce-B")
	}
}

// TestExchangeCodeForTokens_ReusesStoredNonceOnFirstAttempt pins "so the
// common path is one request not two" (atchess-1c9.12 step 4): once the
// shared nonce store already holds a nonce for an origin, a fresh call
// against that origin must succeed on its FIRST attempt.
func TestExchangeCodeForTokens_ReusesStoredNonceOnFirstAttempt(t *testing.T) {
	var calls int32
	var firstAttemptNonceClaim string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		firstAttemptNonceClaim = dpopProofNonceClaim(t, r.Header.Get("DPoP"))
		if firstAttemptNonceClaim != "known-good-nonce" {
			w.Header().Set("DPoP-Nonce", "known-good-nonce")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"use_dpop_nonce"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"access_token":"tok","token_type":"DPoP","expires_in":3600,"refresh_token":"rt","scope":"atproto","sub":"did:example:test"}`))
	}))
	defer server.Close()

	client := testOAuthClient(t)
	dpopKey := testDPoPKey(t)
	client.nonces().Set(dpop.OriginOf(server.URL), "known-good-nonce")

	_, err := client.ExchangeCodeForTokens(server.URL, "https://as.example.com", "code123", "verifier123", dpopKey)
	if err != nil {
		t.Fatalf("ExchangeCodeForTokens: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected exactly 1 request (nonce was already known), got %d", got)
	}
}

// --- refresh_token grant ---

func TestRefreshTokens_Success(t *testing.T) {
	var gotForm url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parsing refresh request form: %v", err)
		}
		gotForm = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"access_token":"newtok","token_type":"DPoP","expires_in":3600,"refresh_token":"newrt","scope":"atproto","sub":"did:example:test"}`))
	}))
	defer server.Close()

	client := testOAuthClient(t)
	dpopKey := testDPoPKey(t)

	tokens, err := client.RefreshTokens(server.URL, "https://as.example.com", "oldrt", dpopKey)
	if err != nil {
		t.Fatalf("RefreshTokens: %v", err)
	}
	if tokens.AccessToken != "newtok" || tokens.RefreshToken != "newrt" {
		t.Errorf("got tokens %+v, want access_token=newtok refresh_token=newrt", tokens)
	}
	if got := gotForm.Get("grant_type"); got != "refresh_token" {
		t.Errorf("grant_type = %q, want refresh_token", got)
	}
	if got := gotForm.Get("refresh_token"); got != "oldrt" {
		t.Errorf("refresh_token = %q, want oldrt", got)
	}
	if gotForm.Get("client_assertion") == "" {
		t.Error("refresh request did not include a client_assertion")
	}
}

func TestRefreshTokens_EmptyRefreshTokenFailsImmediately(t *testing.T) {
	client := testOAuthClient(t)
	dpopKey := testDPoPKey(t)
	_, err := client.RefreshTokens("https://as.example.com/token", "https://as.example.com", "", dpopKey)
	if err == nil {
		t.Fatal("expected an error for an empty refresh token")
	}
}

// TestExchangeCodeForTokens_NonceRetry_SignsDistinctJTIPerAttempt pins
// atchess-1c9.86 item 3: postFormWithDPoPRetry (client.go ~337) signs a
// fresh DPoP proof on every attempt, including the retry, rather than
// reusing a proof built for an earlier attempt -- which is what prevents
// stale-iat rejection under clock skew (see dpop.CreateProof's doc
// comment). jti, not iat, is the primary signal here: RFC 9449 requires a
// proof's jti be unique, and unlike iat it doesn't depend on wall-clock
// granularity -- a regression that hoists/reuses the proof would still be
// caught even if both attempts complete within the same second.
func TestExchangeCodeForTokens_NonceRetry_SignsDistinctJTIPerAttempt(t *testing.T) {
	var firstJTI, secondJTI string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims, err := decodeJWTPayloadUnverified(r.Header.Get("DPoP"))
		if err != nil {
			t.Fatalf("decoding DPoP proof: %v", err)
		}
		jti, _ := claims["jti"].(string)
		if firstJTI == "" {
			firstJTI = jti
			w.Header().Set("DPoP-Nonce", "nonce-1")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"use_dpop_nonce"}`))
			return
		}
		secondJTI = jti
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"access_token":"tok","token_type":"DPoP","expires_in":3600,"refresh_token":"rt","scope":"atproto","sub":"did:example:test"}`))
	}))
	defer server.Close()

	client := testOAuthClient(t)
	dpopKey := testDPoPKey(t)

	_, err := client.ExchangeCodeForTokens(server.URL, "https://as.example.com", "code123", "verifier123", dpopKey)
	if err != nil {
		t.Fatalf("ExchangeCodeForTokens: %v", err)
	}
	if firstJTI == "" || secondJTI == "" {
		t.Fatalf("expected both attempts to carry a jti claim, got first=%q second=%q", firstJTI, secondJTI)
	}
	if firstJTI == secondJTI {
		t.Errorf("both attempts' DPoP proofs carried the SAME jti (%q) -- the proof was reused/hoisted instead of freshly signed per attempt", firstJTI)
	}
}

// dpopProofNonceClaim decodes proof (a DPoP JWT) and returns its "nonce"
// claim, or "" if absent. Built on this package's own
// decodeJWTPayloadUnverified (conformance_test.go) rather than duplicating
// JWT-payload decoding a third time in this file.
func dpopProofNonceClaim(t *testing.T, proof string) string {
	t.Helper()
	if proof == "" {
		return ""
	}
	claims, err := decodeJWTPayloadUnverified(proof)
	if err != nil {
		t.Fatalf("decoding DPoP proof: %v", err)
	}
	nonce, _ := claims["nonce"].(string)
	return nonce
}
