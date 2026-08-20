package web

// Shared test-only plumbing for atchess-1c9.95: internal/atproto's
// parseServiceEndpoint and this package's getAuthorizationServer/
// getAuthServerMetadata now refuse to accept, let alone dial, any
// document/response-derived endpoint that isn't "https://<ordinary,
// portless, non-IP-literal host>" (via atproto.ValidateFetchedEndpointURL,
// which reuses the same did:web host validator atchess-1c9.69/.70/.72/.93
// built). That breaks this package's many pre-existing mock-PDS/mock-
// issuer test doubles, which stood in for "the other player's PDS" or "the
// OAuth issuer" by simply advertising their own httptest.Server's real
// http://127.0.0.1:<ephemeral-port> address as that DID document's
// serviceEndpoint (or as the OAuth callback's "iss") -- exactly the shape
// the new validation exists to reject, since production code can no longer
// tell that shape apart from a hostile one.
//
// newFakeHTTPSEndpoint keeps these tests exercising the REAL request path
// (a real TLS dial, real JSON over the wire) while advertising a realistic,
// validator-passing hostname: it starts a real local TLS listener, then
// swaps in a fake, portless hostname as the value tests embed in a DID
// document / OAuth response, and transparently redirects every outbound
// TLS dial made during the test back to the real listener.

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// fakeEndpointHostCounter makes every newFakeHTTPSEndpoint call advertise a
// DISTINCT hostname (see nextFakeEndpointHost). This matters beyond mere
// hygiene: internal/atproto's getIdentityResolver/ResolvePDS share ONE
// *identityResolver PER-PROCESS per plcDirectoryURL string
// (resolverRegistry, a package-level var, cached across every test in this
// binary), and that resolver caches a resolved DID->serviceEndpoint mapping
// for 10 minutes. A single shared constant hostname here would mean every
// test in this package that sets config.ATProto.PLCDirectoryURL resolves to
// the SAME identityResolver instance and the SAME cache keyed by DID -- so
// a test exercising e.g. "the opponent's DID resolves to an unreachable
// endpoint" could silently observe a DIFFERENT, earlier test's cached
// SUCCESSFUL resolution for that same well-known DID constant instead of
// ever actually asking ITS OWN mock server, passing for entirely the wrong
// reason (or, worse, masking the very failure it exists to prove). A
// unique hostname per test keeps each test's plcDirectoryURL -- and
// therefore its identityResolver and cache -- completely isolated.
var fakeEndpointHostCounter int64

// nextFakeEndpointHost returns a fresh, validator-passing hostname
// (atproto.ValidateFetchedEndpointURL: https, no userinfo/query/fragment/
// path, and a host that passes validateDIDWebHost -- so no port, no IP
// literal in any spelling, ASCII letters/digits/'-'/'.' only).
func nextFakeEndpointHost() string {
	return fmt.Sprintf("https://pds-mock-%d.test", atomic.AddInt64(&fakeEndpointHostCounter, 1))
}

// newFakeHTTPSEndpoint starts handler on a real local TLS listener
// (httptest.NewTLSServer, closed via t.Cleanup), then:
//
//  1. overwrites the returned *httptest.Server's exported URL field with
//     fakeEndpointHost, so a caller that embeds srv.URL in a DID document's
//     serviceEndpoint (or passes it as an OAuth "iss") gets a realistic,
//     validator-passing hostname instead of the httptest.NewTLSServer's
//     real "https://127.0.0.1:<port>" (which fails validateDIDWebHost on
//     BOTH the port and the IP-literal host);
//
//  2. installs a package-level http.DefaultTransport override -- restored
//     via t.Cleanup -- whose DialTLSContext ignores whatever host/port
//     net/http actually asked to dial and always connects to the real
//     listener address instead, skipping certificate verification (the
//     listener's certificate is self-signed).
//
// Step 2 is necessary -- rather than pointing a specific *http.Client's
// Transport at the real address -- because every production *http.Client
// on this request path (atproto.NewClientFromSession's, the shared
// identityResolver's, and this package's oauthMetadataHTTPClient) is built
// with NO explicit Transport, so each resolves http.DefaultTransport
// dynamically on every call (see net/http.Client.Send); overriding the
// package-level default is the only seam available to a test in this
// package (atproto.Client's own httpClient field is unexported and
// constructed fresh, per request, deep inside Service.clientFor, with no
// hook this package's tests can reach). Safe here because this package's
// tests never run with t.Parallel().
func newFakeHTTPSEndpoint(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(handler)
	t.Cleanup(srv.Close)

	realAddr := srv.Listener.Addr().String()
	srv.URL = nextFakeEndpointHost()

	prevTransport := http.DefaultTransport
	http.DefaultTransport = &http.Transport{
		DialTLSContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			d := &tls.Dialer{Config: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec // test-only, self-signed cert
			return d.DialContext(ctx, network, realAddr)
		},
	}
	t.Cleanup(func() { http.DefaultTransport = prevTransport })

	return srv
}
