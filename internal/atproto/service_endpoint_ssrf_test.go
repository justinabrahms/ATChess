package atproto

// atchess-1c9.95: parseServiceEndpoint used to return a DID document's
// serviceEndpoint verbatim, with no scheme/host validation at all --
// xrpcURL then string-concatenates it directly into the request URL every
// later XRPC call dials. So a hostile did:web/did:plc document declaring
// e.g. "http://169.254.169.254/" (a cloud metadata address) as its
// AtprotoPersonalDataServer serviceEndpoint got that address dialed
// DIRECTLY by every caller of resolvePDS/resolveReadEndpoint -- no exotic
// host spelling or redirect required, just a value an attacker gets to
// write into a document this codebase fetches and trusts.
//
// These tests prove -- via a dial spy on the *Client's own httpClient,
// exactly as atchess-1c9.93/.94 require for this family of fix, rather
// than merely inspecting the returned error string -- that a hostile
// serviceEndpoint results in ZERO dials to the address it names, and that
// an ordinary https serviceEndpoint is completely unaffected.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"
)

// dialSpy is a minimal, thread-safe dial recorder: every DialContext/
// DialTLSContext call is recorded, then either refused (passthrough ==
// false, the default -- used by the hostile-serviceEndpoint tests) or
// delegated to the real, redirecting http.DefaultTransport
// newFakeHTTPSEndpoint installed (passthrough == true -- used by the
// legitimate-serviceEndpoint positive control, so that case can both
// RECORD the dial and actually complete it). Refusing outright in the
// hostile case (rather than e.g. delegating and then inspecting the
// result) matters specifically for a loopback/internal target: it means
// this test cannot accidentally "succeed" against whatever happens to be
// listening on 127.0.0.1 in the environment it runs in.
type dialSpy struct {
	mu          sync.Mutex
	dialed      []string
	passthrough bool
}

func (d *dialSpy) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	d.mu.Lock()
	d.dialed = append(d.dialed, addr)
	passthrough := d.passthrough
	d.mu.Unlock()
	if !passthrough {
		return nil, fmt.Errorf("dialSpy: refusing to actually dial %s", addr)
	}
	// Delegate to the real, redirecting Transport newFakeHTTPSEndpoint
	// installed as http.DefaultTransport, so the request actually
	// completes against the mock's real local listener.
	return http.DefaultTransport.(*http.Transport).DialTLSContext(ctx, network, addr)
}

func (d *dialSpy) dialedAddrs() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]string, len(d.dialed))
	copy(out, d.dialed)
	return out
}

// installDialSpy replaces client's own httpClient Transport with a
// dialSpy, capturing every DialContext/DialTLSContext address the client's
// OWN request path (getXRPC, makeRequest -- i.e. the actual data-fetch
// calls RecordMove/GetGame/etc. make against a resolved serviceEndpoint)
// attempts to reach, distinct from the identityResolver's httpClient
// (which fetches DID documents/PLC directory pages, and is deliberately
// left untouched here -- see each test's setup for why it never needs to
// reach the hostile address either).
func installDialSpy(client *Client, passthrough bool) *dialSpy {
	spy := &dialSpy{passthrough: passthrough}
	client.httpClient.Transport = &http.Transport{
		DialContext:    spy.dial,
		DialTLSContext: spy.dial,
	}
	return spy
}

// TestGetGame_RefusesHostileServiceEndpoint_ZeroDialsToInternalAddress is
// the core atchess-1c9.95 regression test: blackDID's DID document
// advertises a hostile serviceEndpoint (a cloud metadata address or
// loopback, in both a plain-http and a scheme-correct-but-IP-literal-host
// spelling), and GetGame's cross-repo read of black's repo (needed for
// terminal-event derivation, exactly as
// TestGetGame_IncompleteDerivation_OpponentUnreachable_Resigned already
// exercises for a plain connection failure) must be refused BEFORE any
// dial is attempted to that address -- not merely fail with SOME error.
//
// The dial spy runs in PASSTHROUGH mode here (see installDialSpy): white's
// OWN repo (c.pdsURL, the mock's legitimate fake hostname) is read first
// and MUST actually succeed, exactly as it would in production, so this
// test genuinely reaches the cross-repo resolution of blackDID's hostile
// serviceEndpoint rather than merely failing earlier for an unrelated
// reason. What is asserted is not "zero dials at all" (the legitimate
// white-repo read dials fine) but that NONE of the dials made ever name
// the hostile host -- proving the hostile address specifically is never
// reached, even though the (redirecting, always-succeeding) dialer beneath
// the spy would have happily connected it there if parseServiceEndpoint's
// validation had not already refused the endpoint first.
func TestGetGame_RefusesHostileServiceEndpoint_ZeroDialsToInternalAddress(t *testing.T) {
	hostile := []struct {
		name        string
		endpoint    string
		hostileHost string // the bare host that must never appear in a dialed address
	}{
		{"cloud metadata address, http scheme", "http://169.254.169.254/", "169.254.169.254"},
		{"loopback, http scheme", "http://127.0.0.1/", "127.0.0.1"},
		{"cloud metadata address, https scheme but IP-literal host", "https://169.254.169.254/", "169.254.169.254"},
		{"loopback, https scheme but IP-literal host", "https://127.0.0.1/", "127.0.0.1"},
	}

	for _, tc := range hostile {
		t.Run(tc.name, func(t *testing.T) {
			mock := newDeriveTestPDS(t)
			srv := mock.server()
			defer srv.Close()

			// blackDID's DID document resolves "successfully" -- to the
			// hostile endpoint. This is the SAME injection point
			// TestGetGame_IncompleteDerivation_OpponentUnreachable_Resigned
			// uses for a plain unreachable address; here the declared
			// endpoint is instead a live-network-relevant internal address.
			mock.setServiceEndpoint(blackDID, tc.endpoint)

			gameURI := mock.seedActiveGame(t, time.Now().Add(-time.Hour), nil)
			mock.seed(whiteDID, "app.atchess.resignation", "resign1", map[string]interface{}{
				"$type":           "app.atchess.resignation",
				"createdAt":       time.Now().Format(time.RFC3339),
				"game":            map[string]interface{}{"uri": gameURI},
				"resigningPlayer": whiteDID,
			})

			client := newDeriveTestClient(t, mock)
			spy := installDialSpy(client, true)

			game, err := client.GetGame(context.Background(), gameURI)
			if err == nil {
				t.Fatalf("GetGame unexpectedly succeeded with a hostile serviceEndpoint %q, status %q", tc.endpoint, game.Status)
			}
			if !errors.Is(err, ErrIncompleteDerivation) {
				t.Errorf("expected errors.Is(err, ErrIncompleteDerivation), got: %v", err)
			}

			for _, addr := range spy.dialedAddrs() {
				host, _, splitErr := net.SplitHostPort(addr)
				if splitErr != nil {
					host = addr
				}
				if host == tc.hostileHost {
					t.Fatalf("GetGame dialed the hostile address %q while resolving serviceEndpoint %q -- want it NEVER dialed", addr, tc.endpoint)
				}
			}
		})
	}
}

// TestGetGame_LegitimateServiceEndpoint_StillResolvesAndDials is the
// required positive control: an ordinary https, non-IP-literal,
// well-formed serviceEndpoint must still resolve and actually be dialed --
// proving atchess-1c9.95's validation does not merely reject everything.
// Reuses deriveTestPDS's own fake, redirected hostname (mock.base, set up
// by newFakeHTTPSEndpoint) as blackDID's serviceEndpoint too, so this is a
// real end-to-end dial via the SAME dial-spy mechanism the hostile-case
// tests above use -- just asserting the opposite outcome.
func TestGetGame_LegitimateServiceEndpoint_StillResolvesAndDials(t *testing.T) {
	mock := newDeriveTestPDS(t)
	srv := mock.server()
	defer srv.Close()

	gameURI := mock.seedActiveGame(t, time.Now().Add(-time.Hour), nil)
	mock.seed(whiteDID, "app.atchess.resignation", "resign1", map[string]interface{}{
		"$type":           "app.atchess.resignation",
		"createdAt":       time.Now().Format(time.RFC3339),
		"game":            map[string]interface{}{"uri": gameURI},
		"resigningPlayer": whiteDID,
	})

	client := newDeriveTestClient(t, mock)
	spy := installDialSpy(client, true)

	game, err := client.GetGame(context.Background(), gameURI)
	if err != nil {
		t.Fatalf("GetGame: unexpected error with a legitimate serviceEndpoint: %v", err)
	}
	if game.Status != "black_won" {
		t.Errorf("game.Status = %q, want %q", game.Status, "black_won")
	}
	if dialed := spy.dialedAddrs(); len(dialed) == 0 {
		t.Errorf("GetGame made zero dials against a legitimate cross-repo serviceEndpoint -- expected the black-repo read to actually be attempted")
	}
}

// TestValidateFetchedEndpointURL_PortHandling is the atchess-1c9.95
// fix-pass regression test: the original implementation reused
// validateDIDWebHost wholesale, which unconditionally rejects any ':' in
// the host -- correct for a did:web host (part of a DID IDENTIFIER, whose
// grammar has no port component at all), but wrong for a serviceEndpoint,
// which the AT Protocol identity spec explicitly permits an "optional port
// number" on (https://atproto.com/specs/did#service-endpoints). That bug
// made "https://pds.example.com:8443" -- a spec-legal, real self-hosted
// PDS shape -- unable to federate with this codebase at all. This proves
// the split (validateHostShape, shared; validateDIDWebHost, did:web-only,
// still forbids a port) fixes that without reopening any IP-literal-with-
// a-port bypass.
func TestValidateFetchedEndpointURL_PortHandling(t *testing.T) {
	cases := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{"ordinary host with a non-standard port is ACCEPTED (spec-legal self-hosted PDS shape)", "https://pds.example.com:8443", false},
		{"ordinary host with a non-standard port and trailing slash is ACCEPTED", "https://pds.example.com:8443/", false},
		{"ordinary host with no port is still ACCEPTED (unaffected by the split)", "https://pds.example.com", false},
		{"IPv4 literal WITH a port is still REJECTED -- a port does not launder an internal address", "https://169.254.169.254:8443", true},
		{"IPv4 literal with no port is still REJECTED", "https://169.254.169.254", true},
		{"loopback with a port is still REJECTED", "https://127.0.0.1:8443", true},
		{"IPv6 literal (bracketed, with a port) is still REJECTED", "https://[::1]:8443", true},
		{"IPv6 literal (bracketed, no port) is still REJECTED -- Hostname() strips the brackets to \"::1\", which the charset allowlist alone rejects (no bracket-specific code)", "https://[::1]/", true},
		{"non-numeric port is REJECTED by url.Parse itself before this function ever runs", "https://pds.example.com:abc/", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ValidateFetchedEndpointURL(tc.url)
			if tc.wantErr && err == nil {
				t.Fatalf("ValidateFetchedEndpointURL(%q) = nil, want an error", tc.url)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("ValidateFetchedEndpointURL(%q) returned an unexpected error: %v", tc.url, err)
			}
		})
	}
}

// TestParseServiceEndpoint_PortAccepted proves the fix end to end (not
// merely at the ValidateFetchedEndpointURL unit-test level): a DID
// document advertising a serviceEndpoint on a non-standard https port
// resolves successfully via parseServiceEndpoint/resolvePDS.
func TestParseServiceEndpoint_PortAccepted(t *testing.T) {
	const want = "https://pds.example.com:8443"
	doc := &DIDDocument{
		ID: "did:plc:example",
		Service: []DIDService{
			{ID: "#atproto_pds", Type: "AtprotoPersonalDataServer", ServiceEndpoint: want},
		},
	}
	got, err := parseServiceEndpoint(doc)
	if err != nil {
		t.Fatalf("parseServiceEndpoint: unexpected error for a spec-legal serviceEndpoint with a port: %v", err)
	}
	if got != want {
		t.Errorf("parseServiceEndpoint = %q, want %q", got, want)
	}
}

// TestValidateDIDWebHost_StillRejectsPort_AfterSplit is the explicit
// atchess-1c9.95 fix-pass regression guard for validateDIDWebHost's own
// behaviour post-split: TestValidateDIDWebHost (identity_test.go, an
// atchess-1c9.72/.93-pinned test, left byte-for-byte unmodified) already
// covers this case, but this test additionally proves the SAME host string
// that validateDIDWebHost still rejects for a port is ACCEPTED by
// ValidateFetchedEndpointURL -- i.e. the split actually produced two
// different, independently-correct answers for the two different shapes,
// not one relaxed-for-everyone validator.
func TestValidateDIDWebHost_StillRejectsPort_AfterSplit(t *testing.T) {
	const host = "pds.example.com:8443"
	if err := validateDIDWebHost(host); err == nil {
		t.Errorf("validateDIDWebHost(%q) = nil, want an error (a did:web host must never carry a port)", host)
	}
	if _, err := ValidateFetchedEndpointURL("https://" + host); err != nil {
		t.Errorf("ValidateFetchedEndpointURL(%q) returned an unexpected error (a serviceEndpoint MAY carry a port): %v", "https://"+host, err)
	}
}
