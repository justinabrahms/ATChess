package atproto

import (
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
