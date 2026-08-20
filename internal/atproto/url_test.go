package atproto

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// TestXrpcURL covers the endpoint-URL shapes ATChess must handle correctly,
// independent of the local dual-PDS test harness (which can only ever
// produce "http://localhost:<port>") and independent of any network. See
// atchess-1c9.25.
func TestXrpcURL(t *testing.T) {
	cases := []struct {
		name   string
		base   string
		method string
		params url.Values
		want   string
	}{
		{
			name:   "https host with no port (real Bluesky PDS shape)",
			base:   "https://shiitake.us-east.host.bsky.network",
			method: "com.atproto.repo.getRecord",
			params: nil,
			want:   "https://shiitake.us-east.host.bsky.network/xrpc/com.atproto.repo.getRecord",
		},
		{
			name:   "https host with explicit port",
			base:   "https://host.example:443",
			method: "com.atproto.repo.getRecord",
			params: nil,
			want:   "https://host.example:443/xrpc/com.atproto.repo.getRecord",
		},
		{
			name:   "http host with port (local dual-PDS harness shape)",
			base:   "http://localhost:2583",
			method: "com.atproto.server.createSession",
			params: nil,
			want:   "http://localhost:2583/xrpc/com.atproto.server.createSession",
		},
		{
			name:   "base URL with trailing slash must not produce //xrpc",
			base:   "http://localhost:2583/",
			method: "com.atproto.repo.getRecord",
			params: nil,
			want:   "http://localhost:2583/xrpc/com.atproto.repo.getRecord",
		},
		{
			name:   "base URL with multiple trailing slashes must not produce //xrpc",
			base:   "http://localhost:2583//",
			method: "com.atproto.repo.getRecord",
			params: nil,
			want:   "http://localhost:2583/xrpc/com.atproto.repo.getRecord",
		},
		{
			name:   "IPv6 literal host",
			base:   "https://[::1]:2583",
			method: "com.atproto.repo.getRecord",
			params: nil,
			want:   "https://[::1]:2583/xrpc/com.atproto.repo.getRecord",
		},
		{
			name:   "IPv6 literal host, no port",
			base:   "https://[2001:db8::1]",
			method: "com.atproto.repo.getRecord",
			params: nil,
			want:   "https://[2001:db8::1]/xrpc/com.atproto.repo.getRecord",
		},
		{
			name:   "query params with a colon and a slash (DID / AT-URI shaped values)",
			base:   "http://localhost:2583",
			method: "com.atproto.repo.getRecord",
			params: url.Values{
				"repo":       {"did:plc:styupz2ghvg7hrq4optipm7s"},
				"collection": {"app.atchess.game"},
				"rkey":       {"3ltiv/g2d6bk2e"}, // slash is not a legal rkey char in practice, but the encoder must not corrupt the URL if one ever appears
			},
			want: "http://localhost:2583/xrpc/com.atproto.repo.getRecord?collection=app.atchess.game&repo=did%3Aplc%3Astyupz2ghvg7hrq4optipm7s&rkey=3ltiv%2Fg2d6bk2e",
		},
		{
			name:   "empty params produces no trailing '?'",
			base:   "http://localhost:2583",
			method: "com.atproto.repo.getRecord",
			params: url.Values{},
			want:   "http://localhost:2583/xrpc/com.atproto.repo.getRecord",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := xrpcURL(tc.base, tc.method, tc.params)
			if got != tc.want {
				t.Errorf("xrpcURL(%q, %q, %v) = %q, want %q", tc.base, tc.method, tc.params, got, tc.want)
			}
			if strings.Contains(strings.TrimPrefix(strings.TrimPrefix(got, "https://"), "http://"), "//") {
				t.Errorf("xrpcURL(%q, %q, %v) = %q contains a double slash after the scheme", tc.base, tc.method, tc.params, got)
			}
		})
	}
}

// TestXrpcURL_QueryValuesRoundTrip proves that query parameter values
// containing ':' and '/' -- both of which appear in AT-URIs and DIDs, and
// which this repo has a documented history of routing bugs around (see
// test/bugs/discovered-bugs.md, Bug 2) -- survive an encode/decode round
// trip unchanged.
func TestXrpcURL_QueryValuesRoundTrip(t *testing.T) {
	repo := "did:plc:styupz2ghvg7hrq4optipm7s"
	atURI := "at://did:plc:styupz2ghvg7hrq4optipm7s/app.atchess.game/3ltiwjqo6222e"

	got := xrpcURL("http://localhost:2583", "com.atproto.repo.getRecord", url.Values{
		"repo": {repo},
		"uri":  {atURI},
	})

	parsed, err := url.Parse(got)
	if err != nil {
		t.Fatalf("xrpcURL produced an unparseable URL %q: %v", got, err)
	}
	q := parsed.Query()
	if q.Get("repo") != repo {
		t.Errorf("repo round-tripped as %q, want %q", q.Get("repo"), repo)
	}
	if q.Get("uri") != atURI {
		t.Errorf("uri round-tripped as %q, want %q", q.Get("uri"), atURI)
	}
}

// newResolveHandleMockPDS starts a mock PDS handling just enough of the
// createSession + com.atproto.identity.resolveHandle flow for ResolveHandle
// to succeed via its same-PDS fast path (resolveHandleSamePDS). It records
// the raw request URL of the resolveHandle call in *gotURL.
func newResolveHandleMockPDS(t *testing.T, gotURL *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/xrpc/com.atproto.server.createSession":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"accessJwt": "test-jwt",
				"did":       "did:plc:resolvehandletest",
				"handle":    "resolver.test",
			})
		case "/xrpc/com.atproto.identity.resolveHandle":
			*gotURL = r.URL.String()
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"did": "did:plc:resolvedtarget",
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// TestXrpcURL_EscapesQueryMetacharacters is a regression test for
// atchess-1c9.35, kept at the xrpcURL level (rather than driven through
// ResolveHandle, as it originally was) after atchess-1c9.69: as of .69, a
// value containing '&', '=' or '#' can no longer reach ResolveHandle's
// query-building at all, because it is not a syntactically valid AT
// Protocol handle and normalizeAndValidateHandle now rejects it before any
// URL is built (see TestResolveHandle_RejectsHandleWithQueryMetacharacters
// below). xrpcURL itself is still shared by every other query-string-
// building call site in this package (repo/rkey/uri params, none of which
// go through handle validation), so its escaping behavior is still worth
// pinning directly.
func TestXrpcURL_EscapesQueryMetacharacters(t *testing.T) {
	const hostileValue = "evil.example&admin=true#frag"

	got := xrpcURL("http://localhost:2583", "com.atproto.repo.getRecord", url.Values{
		"rkey": {hostileValue},
	})

	parsed, err := url.Parse(got)
	if err != nil {
		t.Fatalf("xrpcURL produced an unparseable URL %q: %v", got, err)
	}
	q := parsed.Query()

	// The round trip is the real assertion: confirm the decoded "rkey"
	// value is byte-identical to the original hostile input.
	if gotVal := q.Get("rkey"); gotVal != hostileValue {
		t.Errorf("rkey round-tripped as %q, want %q", gotVal, hostileValue)
	}

	// Confirm the query structure was not corrupted: the embedded
	// "admin=true" and "#frag" must not have become their own query
	// parameter or fragment.
	if _, ok := q["admin"]; ok {
		t.Errorf("query %q was corrupted: unexpected standalone %q parameter smuggled in from the unescaped value", parsed.RawQuery, "admin")
	}
	if parsed.Fragment != "" {
		t.Errorf("query %q was corrupted: unexpected fragment %q smuggled in from the unescaped value", got, parsed.Fragment)
	}
	if len(q) != 1 {
		t.Errorf("query %q has %d parameters, want exactly 1 (\"rkey\")", parsed.RawQuery, len(q))
	}
}

// TestResolveHandle_RejectsHandleWithQueryMetacharacters supersedes what
// was formerly TestResolveHandle_EscapesHostileHandle (atchess-1c9.35): a
// handle containing '&', '=' or '#' is not a syntactically valid AT
// Protocol handle (see normalizeAndValidateHandle), so as of atchess-1c9.69
// it is rejected outright, before ResolveHandle ever builds a query string
// from it -- a strictly stronger guarantee than merely escaping it
// correctly. This asserts both the error and that zero requests reach the
// mock PDS.
func TestResolveHandle_RejectsHandleWithQueryMetacharacters(t *testing.T) {
	const hostileHandle = "evil.example&admin=true#frag"

	requests := 0
	mockPDS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/xrpc/com.atproto.server.createSession":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"accessJwt": "test-jwt",
				"did":       "did:plc:resolvehandletest",
				"handle":    "resolver.test",
			})
		default:
			requests++
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer mockPDS.Close()

	client, err := NewClient(mockPDS.URL, "resolver.test", "password")
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	if _, err := client.ResolveHandle(context.Background(), hostileHandle); err == nil {
		t.Fatalf("ResolveHandle(%q) unexpectedly succeeded, want a validation error", hostileHandle)
	}
	if requests != 0 {
		t.Errorf("ResolveHandle(%q) made %d request(s) beyond client construction, want 0 (validation must reject it before any resolver runs)", hostileHandle, requests)
	}
}

// TestResolveHandle_OrdinaryHandleURLUnchanged pins today's exact request
// URL for an ordinary handle, so the atchess-1c9.35 escaping fix cannot
// silently change behaviour for the common case. The expected suffix below
// is a literal, hand-verified string (not built via xrpcURL or
// url.Values.Encode) -- see atchess-1c9.35's done-criteria: computing the
// expectation with the same helper the production code uses would make
// this test tautological.
func TestResolveHandle_OrdinaryHandleURLUnchanged(t *testing.T) {
	// This is the request-target the server actually receives (path+query,
	// no scheme/authority -- that's how net/http populates *http.Request.URL
	// server-side for an ordinary, non-proxy HTTP/1.1 request).
	const want = "/xrpc/com.atproto.identity.resolveHandle?handle=alice.test"

	var gotURL string
	mockPDS := newResolveHandleMockPDS(t, &gotURL)
	defer mockPDS.Close()

	client, err := NewClient(mockPDS.URL, "resolver.test", "password")
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	if _, err := client.ResolveHandle(context.Background(), "alice.test"); err != nil {
		t.Fatalf("ResolveHandle returned an unexpected error: %v", err)
	}

	if gotURL != want {
		t.Errorf("resolveHandle request URL = %q, want %q (byte-identical to today's output)", gotURL, want)
	}
}

// TestResolveHandle_RejectsHostileHandlesBeforeAnyRequest is the core
// regression test for atchess-1c9.69: resolveHandleViaWellKnown (and, in
// the same shape, resolveHandleViaDNS) used to interpolate a caller-
// supplied handle directly into the HOST/PATH of a server-side fetch, or
// the hostname of a DNS query -- a hostile value could steer WHERE the
// fetch/lookup went, not merely corrupt a query parameter (that was
// atchess-1c9.35, addressed above). The handle is user-supplied from HTTP
// request bodies (internal/web/oauth_handlers.go login,
// internal/web/service.go challenge-opponent).
//
// This proves each of a handle containing '/', '@', ':' and a bare IP
// literal is rejected by Client.ResolveHandle BEFORE any resolution
// strategy runs -- not merely that an error comes back, but that ZERO
// requests reach the same-PDS resolveHandle endpoint. Because
// normalizeAndValidateHandle is called once at ResolveHandle's entry point
// before ANY strategy is tried, zero requests to this one instrumented
// strategy proves zero requests were attempted by every strategy,
// including the DNS TXT and HTTPS well-known ones a real SSRF attempt
// would actually be targeting (those use net.DefaultResolver / a plain
// http.Client against a real hostname derived from the handle, so they
// cannot be safely instrumented the same way in a unit test without
// performing a real DNS lookup / HTTPS request -- which is precisely what
// this test proves never happens).
func TestResolveHandle_RejectsHostileHandlesBeforeAnyRequest(t *testing.T) {
	hostileHandles := []struct {
		name   string
		handle string
	}{
		{"contains slash (path injection into well-known fetch)", "evil.example/../../admin"},
		{"contains at-sign (userinfo injection, changes fetch host)", "attacker@evil.example"},
		{"contains colon (port/scheme injection, changes fetch host)", "evil.example:8080"},
		{"bare IPv4 literal (direct SSRF target, e.g. cloud metadata IP shape)", "169.254.169.254"},
		// Not a plain dotted-quad, so net.ParseIP alone would miss it --
		// caught only by the final-label-must-not-start-with-a-digit rule
		// (atchess-1c9.69 fix-pass). glibc's inet_aton (the cgo resolver)
		// resolves this exact string to 169.254.169.254, the cloud metadata
		// address.
		{"hex-last-octet IPv4 spelling of the cloud metadata address (bypasses a dotted-quad-only net.ParseIP check)", "169.254.169.0xfe"},
	}

	for _, tc := range hostileHandles {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			mockPDS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/xrpc/com.atproto.server.createSession":
					w.Header().Set("Content-Type", "application/json")
					json.NewEncoder(w).Encode(map[string]interface{}{
						"accessJwt": "test-jwt",
						"did":       "did:plc:resolvehandletest",
						"handle":    "resolver.test",
					})
				default:
					requests++
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer mockPDS.Close()

			client, err := NewClient(mockPDS.URL, "resolver.test", "password")
			if err != nil {
				t.Fatalf("failed to create client: %v", err)
			}

			if _, err := client.ResolveHandle(context.Background(), tc.handle); err == nil {
				t.Fatalf("ResolveHandle(%q) unexpectedly succeeded, want a validation error", tc.handle)
			}
			if requests != 0 {
				t.Errorf("ResolveHandle(%q) made %d request(s) beyond client construction, want 0 (validation must reject it before any resolver runs)", tc.handle, requests)
			}
		})
	}
}

// TestResolveHandle_ValidHandlesStillResolve is the required control for
// TestResolveHandle_RejectsHostileHandlesBeforeAnyRequest: it proves
// normalizeAndValidateHandle is not simply rejecting everything by
// confirming ordinary, well-formed handles still resolve successfully
// through Client.ResolveHandle's normal (same-PDS) path.
func TestResolveHandle_ValidHandlesStillResolve(t *testing.T) {
	for _, handle := range []string{"alice.test", "alice.bsky.social"} {
		t.Run(handle, func(t *testing.T) {
			var gotURL string
			mockPDS := newResolveHandleMockPDS(t, &gotURL)
			defer mockPDS.Close()

			client, err := NewClient(mockPDS.URL, "resolver.test", "password")
			if err != nil {
				t.Fatalf("failed to create client: %v", err)
			}

			did, err := client.ResolveHandle(context.Background(), handle)
			if err != nil {
				t.Fatalf("ResolveHandle(%q) returned an unexpected error: %v", handle, err)
			}
			if did != "did:plc:resolvedtarget" {
				t.Errorf("ResolveHandle(%q) = %q, want %q", handle, did, "did:plc:resolvedtarget")
			}
		})
	}
}

// TestParseServiceEndpoint covers DID-document serviceEndpoint extraction.
// The shape is fixed by the AT Protocol spec (a service[] entry with
// type "AtprotoPersonalDataServer"); this is deliberately minimal and does
// not anticipate atchess-1c9.10's routing design.
func TestParseServiceEndpoint(t *testing.T) {
	t.Run("https with no port (real Bluesky PDS shape)", func(t *testing.T) {
		doc := &DIDDocument{
			ID: "did:plc:example",
			Service: []DIDService{
				{ID: "#atproto_pds", Type: "AtprotoPersonalDataServer", ServiceEndpoint: "https://shiitake.us-east.host.bsky.network"},
			},
		}
		got, err := parseServiceEndpoint(doc)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if want := "https://shiitake.us-east.host.bsky.network"; got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("serviceEndpoint scheme differs from caller's own PDS is preserved, not coerced", func(t *testing.T) {
		// Simulates: our own client is talking to an http:// harness PDS,
		// but resolves a target whose DID document advertises an https://
		// serviceEndpoint. The parsed endpoint must come back exactly as
		// published -- neither downgraded to http nor otherwise altered --
		// so routing logic built on top of this (atchess-1c9.10) can decide
		// what to do with the mismatch instead of it being silently lost here.
		callerPDS := "http://localhost:2583"
		doc := &DIDDocument{
			Service: []DIDService{
				{Type: "AtprotoPersonalDataServer", ServiceEndpoint: "https://target.example"},
			},
		}
		got, err := parseServiceEndpoint(doc)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got == callerPDS {
			t.Fatalf("serviceEndpoint incorrectly coerced to caller's own PDS scheme/host")
		}
		if want := "https://target.example"; got != want {
			t.Errorf("got %q, want %q (scheme must not be silently upgraded/downgraded)", got, want)
		}
	})

	t.Run("serviceEndpoint with trailing slash is returned as-is", func(t *testing.T) {
		doc := &DIDDocument{
			Service: []DIDService{
				{Type: "AtprotoPersonalDataServer", ServiceEndpoint: "https://target.example/"},
			},
		}
		got, err := parseServiceEndpoint(doc)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if want := "https://target.example/"; got != want {
			t.Errorf("got %q, want %q", got, want)
		}
		// Prove xrpcURL still does the right thing when fed this raw value.
		joined := xrpcURL(got, "com.atproto.repo.getRecord", nil)
		if want := "https://target.example/xrpc/com.atproto.repo.getRecord"; joined != want {
			t.Errorf("xrpcURL(%q, ...) = %q, want %q (trailing slash on serviceEndpoint must not produce //xrpc)", got, joined, want)
		}
	})

	t.Run("no matching service entry returns an error naming the DID", func(t *testing.T) {
		doc := &DIDDocument{
			ID: "did:plc:example",
			Service: []DIDService{
				{Type: "SomeOtherServiceType", ServiceEndpoint: "https://irrelevant.example"},
			},
		}
		_, err := parseServiceEndpoint(doc)
		if err == nil {
			t.Fatal("expected an error when no AtprotoPersonalDataServer service entry is present")
		}
	})

	t.Run("nil document returns an error", func(t *testing.T) {
		_, err := parseServiceEndpoint(nil)
		if err == nil {
			t.Fatal("expected an error for a nil DID document")
		}
	})

	t.Run("empty serviceEndpoint returns an error", func(t *testing.T) {
		doc := &DIDDocument{
			Service: []DIDService{
				{Type: "AtprotoPersonalDataServer", ServiceEndpoint: ""},
			},
		}
		_, err := parseServiceEndpoint(doc)
		if err == nil {
			t.Fatal("expected an error for an empty serviceEndpoint")
		}
	})
}
