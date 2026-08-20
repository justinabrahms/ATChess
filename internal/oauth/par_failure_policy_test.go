package oauth

// Unit tests for atchess-1c9.86's PAR-failure policy, implemented in
// BuildAuthorizationURLAuto: what happens when a PushAuthorizationRequest
// call fails, split on (a) whether the authorization server's metadata
// says it REQUIRES PAR (requirePAR) and (b) whether the failure was
// transient (network error/timeout/5xx, PARError.Transient() == true) or
// a 4xx (the server understood and rejected the request). See
// BuildAuthorizationURLAuto's doc comment for the full policy and its
// reasoning; these tests cover all four requirePAR x transient/4xx
// combinations.

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// parFailureServer returns an httptest.Server whose PAR endpoint always
// responds with statusCode (and, for 4xx-shaped failures, a plausible
// OAuth error body) -- used to simulate a definite HTTP response from the
// authorization server, as opposed to a network-level failure (see
// parUnreachableEndpoint below).
func parFailureServer(t *testing.T, statusCode int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statusCode)
		_, _ = w.Write([]byte(`{"error":"server_error"}`))
	}))
}

// parUnreachableEndpoint returns a URL that will fail with a network-level
// error (connection refused) rather than any HTTP response at all --
// exercising the StatusCode == 0 transient case.
func parUnreachableEndpoint(t *testing.T) string {
	t.Helper()
	// Bind and immediately close a listener so the port is (with very high
	// probability) refusing connections for the rest of the test.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	addr := srv.URL
	srv.Close()
	return addr
}

func assertPlainFallbackURL(t *testing.T, authURL string, expectState, expectChallenge string) {
	t.Helper()
	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parsing fallback authorization URL: %v", err)
	}
	q := parsed.Query()
	if q.Get("request_uri") != "" {
		t.Errorf("expected the PLAIN (non-PAR) authorization URL (no request_uri), got: %s", authURL)
	}
	if q.Get("state") != expectState {
		t.Errorf("fallback URL state = %q, want %q", q.Get("state"), expectState)
	}
	if q.Get("code_challenge") != expectChallenge {
		t.Errorf("fallback URL code_challenge = %q, want %q", q.Get("code_challenge"), expectChallenge)
	}
}

// --- requirePAR == true ---

// TestBuildAuthorizationURLAuto_RequirePAR_TransientFailure_HardFails pins
// the "require==true -> hard-fail on ANY PAR failure, even a transient
// one" branch: a fallback would be pointless since the server requires
// PAR and will reject the plain URL by definition.
func TestBuildAuthorizationURLAuto_RequirePAR_TransientFailure_HardFails(t *testing.T) {
	server := parFailureServer(t, http.StatusServiceUnavailable) // 503, transient
	defer server.Close()

	client := testOAuthClient(t)
	dpopKey := testDPoPKey(t)

	authURL, err := client.BuildAuthorizationURLAuto("https://as.example.com/authorize", server.URL, "https://as.example.com", "alice.test", "state1", "chal1", dpopKey, true)
	if err == nil {
		t.Fatalf("expected a hard failure when requirePAR is true and PAR failed transiently, got a URL: %s", authURL)
	}
	if authURL != "" {
		t.Errorf("expected no authorization URL on hard failure, got %q", authURL)
	}
}

// TestBuildAuthorizationURLAuto_RequirePAR_4xxFailure_HardFails pins
// "require==true -> hard-fail" for the 4xx sub-case too (both sub-cases
// of require==true must hard-fail, so this and the test above together
// cover it).
func TestBuildAuthorizationURLAuto_RequirePAR_4xxFailure_HardFails(t *testing.T) {
	server := parFailureServer(t, http.StatusBadRequest) // 400, non-transient
	defer server.Close()

	client := testOAuthClient(t)
	dpopKey := testDPoPKey(t)

	authURL, err := client.BuildAuthorizationURLAuto("https://as.example.com/authorize", server.URL, "https://as.example.com", "alice.test", "state1", "chal1", dpopKey, true)
	if err == nil {
		t.Fatalf("expected a hard failure when requirePAR is true and PAR failed with a 4xx, got a URL: %s", authURL)
	}
	if authURL != "" {
		t.Errorf("expected no authorization URL on hard failure, got %q", authURL)
	}
}

// --- requirePAR == false ---

// TestBuildAuthorizationURLAuto_NotRequired_TransientFailure_FallsBack
// pins the one case where a fallback happens: requirePAR == false AND the
// PAR failure was transient (a 5xx here). The result must be the PLAIN
// (non-PAR) authorization URL, not an error.
func TestBuildAuthorizationURLAuto_NotRequired_TransientFailure_FallsBack(t *testing.T) {
	server := parFailureServer(t, http.StatusBadGateway) // 502, transient
	defer server.Close()

	client := testOAuthClient(t)
	dpopKey := testDPoPKey(t)

	authURL, err := client.BuildAuthorizationURLAuto("https://as.example.com/authorize", server.URL, "https://as.example.com", "alice.test", "state1", "chal1", dpopKey, false)
	if err != nil {
		t.Fatalf("expected a fallback to the plain URL (no error) when requirePAR is false and PAR failed transiently, got error: %v", err)
	}
	assertPlainFallbackURL(t, authURL, "state1", "chal1")
}

// TestBuildAuthorizationURLAuto_NotRequired_NetworkFailure_FallsBack pins
// the StatusCode == 0 transient sub-case specifically: a PAR endpoint that
// is entirely unreachable (no HTTP response at all) must also fall back
// when requirePAR is false.
func TestBuildAuthorizationURLAuto_NotRequired_NetworkFailure_FallsBack(t *testing.T) {
	unreachable := parUnreachableEndpoint(t)

	client := testOAuthClient(t)
	dpopKey := testDPoPKey(t)

	authURL, err := client.BuildAuthorizationURLAuto("https://as.example.com/authorize", unreachable, "https://as.example.com", "alice.test", "state1", "chal1", dpopKey, false)
	if err != nil {
		t.Fatalf("expected a fallback to the plain URL (no error) when requirePAR is false and the PAR endpoint is unreachable, got error: %v", err)
	}
	assertPlainFallbackURL(t, authURL, "state1", "chal1")
}

// TestBuildAuthorizationURLAuto_NotRequired_4xxFailure_HardFails pins the
// "require==false AND 4xx -> still hard-fail" branch: a 4xx means the
// authorization server understood and rejected our PAR request, which
// points at a defect in the request itself -- silently falling back
// would mask that defect rather than surface it.
func TestBuildAuthorizationURLAuto_NotRequired_4xxFailure_HardFails(t *testing.T) {
	server := parFailureServer(t, http.StatusUnauthorized) // 401, non-transient
	defer server.Close()

	client := testOAuthClient(t)
	dpopKey := testDPoPKey(t)

	authURL, err := client.BuildAuthorizationURLAuto("https://as.example.com/authorize", server.URL, "https://as.example.com", "alice.test", "state1", "chal1", dpopKey, false)
	if err == nil {
		t.Fatalf("expected a hard failure when requirePAR is false but PAR failed with a 4xx, got a URL: %s", authURL)
	}
	if authURL != "" {
		t.Errorf("expected no authorization URL on hard failure, got %q", authURL)
	}
}

// --- PARError.Transient() itself ---

// TestPARError_Transient pins the classification rule directly, since it
// is the crux of every test above: StatusCode == 0 (no response reached)
// or >= 500 is transient; any other status (in practice, 4xx) is not.
func TestPARError_Transient(t *testing.T) {
	cases := []struct {
		status int
		want   bool
	}{
		{0, true},
		{499, false},
		{500, true},
		{503, true},
		{400, false},
		{401, false},
		{404, false},
		// 408 and 429 are 4xx but ARE transient -- they do not mean our
		// request was wrong. See PARError.Transient's doc comment.
		{408, true},
		{429, true},
	}
	for _, tc := range cases {
		e := &PARError{StatusCode: tc.status, Err: errPlaceholder}
		if got := e.Transient(); got != tc.want {
			t.Errorf("PARError{StatusCode: %d}.Transient() = %v, want %v", tc.status, got, tc.want)
		}
	}
}

var errPlaceholder = &testErr{"placeholder"}

type testErr struct{ msg string }

func (e *testErr) Error() string { return e.msg }
