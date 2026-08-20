package web

// atchess-1c9.95: getAuthorizationServer (dials a resolved PDS's
// /.well-known/oauth-protected-resource) and getAuthServerMetadata (dials
// an authorization server's /.well-known/oauth-authorization-server) used
// to build their request URL directly from a value this codebase does not
// control -- a resolved pdsURL, the untrusted authorization_servers[0]
// entry of the PDS's OWN response body, or an OAuth callback's "iss" query
// parameter -- and fetch it on http.Get (http.DefaultClient): no timeout,
// no validation, and every redirect followed. A hostile/forged value
// naming an internal address (e.g. "http://169.254.169.254/", or an OAuth
// callback crafted with iss=http://169.254.169.254/) got dialed directly.
//
// These tests prove -- via a dial spy on http.DefaultTransport (both
// getAuthorizationServer/getAuthServerMetadata's oauthMetadataHTTPClient
// and every *http.Client this codebase builds with no explicit Transport
// resolve http.DefaultTransport dynamically per call, see
// fake_https_endpoint_test.go's newFakeHTTPSEndpoint for the same
// technique used elsewhere in this package's tests) -- that a hostile
// value results in ZERO dials to the address it names.

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// metadataDialSpy is a minimal, thread-safe dial recorder that refuses
// every dial it sees -- used here to prove a hostile OAuth metadata target
// is never reached, without ever completing a real connection to it (this
// matters specifically for a loopback/internal target: refusing outright
// means the test cannot accidentally "succeed" against whatever happens to
// be listening on 127.0.0.1 in the environment it runs in).
type metadataDialSpy struct {
	mu     sync.Mutex
	dialed []string
}

func (d *metadataDialSpy) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	d.mu.Lock()
	d.dialed = append(d.dialed, addr)
	d.mu.Unlock()
	return nil, fmt.Errorf("metadataDialSpy: refusing to actually dial %s", addr)
}

func (d *metadataDialSpy) dialedAddrs() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]string, len(d.dialed))
	copy(out, d.dialed)
	return out
}

// installMetadataDialSpy overrides the package-level http.DefaultTransport
// for the duration of t (restored via t.Cleanup), capturing every
// DialContext/DialTLSContext address oauthMetadataHTTPClient (or any other
// *http.Client built with no explicit Transport) attempts to reach. Safe
// here because this package's tests never run with t.Parallel() -- see
// newFakeHTTPSEndpoint's doc comment for the same reasoning applied to the
// positive-control case.
func installMetadataDialSpy(t *testing.T) *metadataDialSpy {
	t.Helper()
	spy := &metadataDialSpy{}
	prev := http.DefaultTransport
	http.DefaultTransport = &http.Transport{
		DialContext:    spy.dial,
		DialTLSContext: spy.dial,
	}
	t.Cleanup(func() { http.DefaultTransport = prev })
	return spy
}

// TestGetAuthServerMetadata_RefusesHostileURL_ZeroDials is the core
// atchess-1c9.95 regression test for the OAuth metadata site: a hostile
// authServerURL (as would arrive via an OAuth callback's attacker-supplied
// "iss" query parameter, or a malicious PDS's oauth-protected-resource
// response body) must be refused BEFORE any dial is attempted, not merely
// fail with SOME error after actually reaching the network.
func TestGetAuthServerMetadata_RefusesHostileURL_ZeroDials(t *testing.T) {
	hostile := []string{
		"http://169.254.169.254/",
		"http://127.0.0.1/",
		"https://169.254.169.254/",
		"https://127.0.0.1/",
	}

	for _, url := range hostile {
		t.Run(url, func(t *testing.T) {
			spy := installMetadataDialSpy(t)

			_, err := getAuthServerMetadata(url)
			if err == nil {
				t.Fatalf("getAuthServerMetadata(%q) unexpectedly succeeded", url)
			}
			if !strings.Contains(err.Error(), "refusing to fetch") {
				t.Errorf("expected a validation-refusal error, got: %v", err)
			}
			if dialed := spy.dialedAddrs(); len(dialed) != 0 {
				t.Fatalf("getAuthServerMetadata(%q) dialed %v, want ZERO dials", url, dialed)
			}
		})
	}
}

// TestGetAuthorizationServer_RefusesHostilePDSURL_ZeroDials covers the
// OTHER call site sharing the same validation: getAuthorizationServer,
// which dials a resolved pdsURL's oauth-protected-resource document.
func TestGetAuthorizationServer_RefusesHostilePDSURL_ZeroDials(t *testing.T) {
	hostile := []string{
		"http://169.254.169.254/",
		"http://127.0.0.1/",
		"https://169.254.169.254/",
		"https://127.0.0.1/",
	}

	s := &Service{}
	for _, url := range hostile {
		t.Run(url, func(t *testing.T) {
			spy := installMetadataDialSpy(t)

			_, err := s.getAuthorizationServer(url)
			if err == nil {
				t.Fatalf("getAuthorizationServer(%q) unexpectedly succeeded", url)
			}
			if !strings.Contains(err.Error(), "refusing to fetch") {
				t.Errorf("expected a validation-refusal error, got: %v", err)
			}
			if dialed := spy.dialedAddrs(); len(dialed) != 0 {
				t.Fatalf("getAuthorizationServer(%q) dialed %v, want ZERO dials", url, dialed)
			}
		})
	}
}

// TestOAuthCallback_HostileIssuer_RefusedBeforeAnyDial drives the actual
// production entrypoint (OAuthCallbackHandler) with an attacker-supplied
// "iss" naming an internal address -- exactly what a forged/crafted OAuth
// callback request could set it to -- and proves getTokenEndpoint(iss)
// (the FIRST thing OAuthCallbackHandler does with iss, before any other
// network call) never dials it.
func TestOAuthCallback_HostileIssuer_RefusedBeforeAnyDial(t *testing.T) {
	hostile := []string{
		"http://169.254.169.254/",
		"http://127.0.0.1/",
	}

	for _, iss := range hostile {
		t.Run(iss, func(t *testing.T) {
			setUpOAuthGlobalsForTest(t)
			spy := installMetadataDialSpy(t)

			rr := driveOAuthCallback(t, &Service{}, iss)
			if rr.Code == http.StatusFound {
				t.Fatalf("expected login to fail for hostile iss %q, got a redirect to %q", iss, rr.Header().Get("Location"))
			}
			if dialed := spy.dialedAddrs(); len(dialed) != 0 {
				t.Fatalf("OAuthCallbackHandler(iss=%q) dialed %v, want ZERO dials", iss, dialed)
			}
		})
	}
}

// TestGetAuthServerMetadata_LegitimateURL_StillResolvesAndDials is the
// required positive control: an ordinary https, non-IP-literal, well-formed
// authServerURL must still resolve and actually be dialed -- proving
// atchess-1c9.95's validation does not merely reject everything.
func TestGetAuthServerMetadata_LegitimateURL_StillResolvesAndDials(t *testing.T) {
	issuer := fakeIssuer(t, "did:plc:legit")
	defer issuer.Close()

	metadata, err := getAuthServerMetadata(issuer.URL)
	if err != nil {
		t.Fatalf("getAuthServerMetadata(%q): unexpected error: %v", issuer.URL, err)
	}
	if metadata.TokenEndpoint == "" {
		t.Errorf("expected a non-empty TokenEndpoint from a legitimate authorization server")
	}
}
