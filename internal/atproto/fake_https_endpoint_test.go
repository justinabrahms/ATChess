package atproto

// Shared test-only plumbing for atchess-1c9.95: parseServiceEndpoint now
// refuses to accept, let alone dial, any DID-document-derived
// serviceEndpoint that isn't "https://<ordinary, non-IP-literal host,
// optional port>" (via ValidateFetchedEndpointURL, which reuses
// validateHostShape -- the SSRF-relevant analysis atchess-1c9.69/.70/.72/
// .93 built for did:web hosts). That breaks this package's pre-existing
// mock-PDS test doubles (accept_challenge_test.go, derive_status_test.go),
// which stand in for "the other player's PDS" by simply advertising their
// own httptest.Server's real http://127.0.0.1:<ephemeral-port> address as
// that DID document's serviceEndpoint -- exactly the shape the new
// validation exists to reject, since production code can no longer tell
// that shape apart from a hostile one.
//
// newFakeHTTPSEndpoint keeps these tests exercising the REAL request path
// (a real TLS dial, real JSON over the wire, a real NewClient login) while
// advertising a realistic, validator-passing hostname: it starts a real
// local TLS listener, registers a fake hostname that routes to it, and a
// test embeds that fake hostname (rather than the listener's own address)
// wherever a DID document/config value is needed.
//
// newUnreachableFakeHost is the companion for atchess-1c9.51's "opponent
// PDS is unreachable" tests (atchess-1c9.95 fix-pass, reviewer-flagged):
// it registers a fake hostname that ALSO passes validation but routes to a
// real, currently-nothing-listening local address, so those tests still
// fail for a genuine connection error rather than short-circuiting at
// validation time.

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
)

// fakeEndpointHostCounter makes every fake hostname this file hands out
// DISTINCT (see nextFakeHost). This matters beyond mere hygiene:
// getIdentityResolver/ResolvePDS share ONE *identityResolver PER-PROCESS
// per plcDirectoryURL string (resolverRegistry, a package-level var,
// cached across every test in this binary -- see
// pdsCacheTTL/pdsCacheMaxEntries), and that resolver caches a resolved
// DID->serviceEndpoint mapping for 10 minutes. A single shared constant
// hostname here would mean every test in this package that calls
// client.SetPLCDirectoryURL(mock.base) resolves to the SAME
// identityResolver instance and the SAME cache keyed by DID -- so a test
// exercising e.g. "did:plc:black resolves to an unreachable endpoint" could
// silently observe a DIFFERENT, earlier test's cached SUCCESSFUL
// resolution for that same well-known DID constant instead of ever
// actually asking ITS OWN mock server, passing for entirely the wrong
// reason (or, worse, masking the very failure it exists to prove). A
// unique hostname per call keeps each test's plcDirectoryURL -- and
// therefore its identityResolver and cache -- completely isolated.
var fakeEndpointHostCounter int64

// nextFakeHost returns a fresh, validator-passing BARE hostname
// (ValidateFetchedEndpointURL/validateHostShape: ASCII letters/digits/
// '-'/'.' only, no empty label, no userinfo, no path separator, no IP
// literal in any spelling -- a port is separately fine on a
// serviceEndpoint, but this bare host never carries one itself).
func nextFakeHost() string {
	return fmt.Sprintf("pds-mock-%d.test", atomic.AddInt64(&fakeEndpointHostCounter, 1))
}

// fakeHostRoutes maps a bare fake hostname (as registered by
// newFakeHTTPSEndpoint/newUnreachableFakeHost) to the real "host:port" a
// TLS dial for it should actually reach. Looked up by
// installFakeHostTransport's DialTLSContext. A persistent, growing map
// (never pruned) rather than one swapped in/out per test: every key is
// globally unique (nextFakeHost's counter never repeats within a test
// binary run), so entries from finished tests are simply inert, not a
// correctness risk, and this lets MULTIPLE fake hosts -- e.g. one test's
// own legitimate mock PDS AND that same test's separate "unreachable
// opponent" placeholder -- resolve to two DIFFERENT real addresses
// simultaneously within a single test, which a single "redirect
// everything to the one most-recently-installed target" Transport (this
// file's pre-atchess-1c9.95-fix-pass design) could not do.
var (
	fakeHostRoutesMu      sync.Mutex
	fakeHostRoutes        = map[string]string{}
	fakeHostTransportOnce sync.Once
)

// installFakeHostTransport installs, exactly once per test binary run
// (sync.Once), a package-level http.DefaultTransport whose DialTLSContext
// consults fakeHostRoutes rather than the address net/http nominally
// asked for. This is necessary -- rather than pointing a specific
// *Client's httpClient.Transport at the real address -- because
// NewClient/NewClientWithDPoP perform their login POST synchronously,
// inside the constructor, using an *http.Client built with no explicit
// Transport (resolving http.DefaultTransport dynamically at call time --
// see net/http.Client.Send) before a test has any *Client value to reach
// into; identityResolver's own httpClient is built the same way.
// Overriding the package-level default is therefore the only seam that
// covers BOTH the login call and every later resolution/XRPC call this
// package's mock-PDS test doubles need.
//
// Installed once and never restored (unlike this file's original
// per-call swap-and-t.Cleanup-restore design): every atproto package test
// is hermetic (see e.g. TestResolveHandleViaPLCExport_GatedAgainstDefaultDirectory's
// explicit refusal to touch the real network), so there is no legitimate
// dial in this test binary that needs net/http's REAL default dialing
// behaviour preserved -- and a persistent installation is what makes
// fakeHostRoutes' multi-target routing actually useful across the
// lifetime of a single test (see its own doc comment). Safe here because
// this package's tests never run with t.Parallel() -- see TestMain-less
// convention: nothing else mutates http.DefaultTransport in this package.
func installFakeHostTransport() {
	fakeHostTransportOnce.Do(func() {
		http.DefaultTransport = &http.Transport{
			DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				host, _, err := net.SplitHostPort(addr)
				if err != nil {
					host = addr
				}
				fakeHostRoutesMu.Lock()
				real, ok := fakeHostRoutes[host]
				fakeHostRoutesMu.Unlock()
				if !ok {
					return nil, fmt.Errorf("fake TLS transport: no route registered for %s (host %q)", addr, host)
				}
				d := &tls.Dialer{Config: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec // test-only, self-signed cert
				return d.DialContext(ctx, network, real)
			},
		}
	})
}

func registerFakeHostRoute(fakeHost, realAddr string) {
	fakeHostRoutesMu.Lock()
	defer fakeHostRoutesMu.Unlock()
	fakeHostRoutes[fakeHost] = realAddr
}

// newFakeHTTPSEndpoint starts handler on a real local TLS listener
// (httptest.NewTLSServer, closed via t.Cleanup), registers a fresh fake
// hostname routed to it, and overwrites the returned *httptest.Server's
// exported URL field with that fake hostname -- so a caller that embeds
// srv.URL in a DID document's serviceEndpoint (or passes it as pdsURL to
// NewClient) gets a realistic, validator-passing hostname instead of
// httptest.NewTLSServer's real "https://127.0.0.1:<port>" (which fails
// validateHostShape on the IP-literal host).
func newFakeHTTPSEndpoint(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	installFakeHostTransport()

	srv := httptest.NewTLSServer(handler)
	t.Cleanup(srv.Close)

	realAddr := srv.Listener.Addr().String()
	fakeHost := nextFakeHost()
	registerFakeHostRoute(fakeHost, realAddr)
	srv.URL = "https://" + fakeHost

	return srv
}

// newUnreachableFakeHost returns a validator-passing "https://<host>"
// (see nextFakeHost) whose registered route points at a real, currently
// nothing-listening local address: a TCP listener is opened just long
// enough to obtain an address, then closed immediately, so dialing it
// fails fast with connection-refused -- a genuine opponent-PDS outage
// (atchess-1c9.51), rather than failing at VALIDATION time the way a
// plain-http/IP-literal placeholder now would (atchess-1c9.95 fix-pass,
// reviewer-flagged: see deriveTestPDS.setUnreachable, the sole caller).
func newUnreachableFakeHost(t *testing.T) string {
	t.Helper()
	installFakeHostTransport()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("newUnreachableFakeHost: failed to allocate a port: %v", err)
	}
	realAddr := l.Addr().String()
	l.Close() // nothing listens here from this point on -- dials fail closed, fast

	fakeHost := nextFakeHost()
	registerFakeHostRoute(fakeHost, realAddr)
	return "https://" + fakeHost
}
