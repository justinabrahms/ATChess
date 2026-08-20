package atproto

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// alwaysRedirectDialer is a dial spy purpose-built for redirect tests: its
// FIRST DialTLSContext call is always routed to realAddr (a real, local
// httptest.NewTLSServer listener) regardless of the address net/http
// actually asked for -- this is dialRedirectClient's technique
// (internal/atproto/identity_test.go, atchess-1c9.70/.93), needed because
// the did:web/handle host under test is a realistic string like
// "example.com" or a handle's own name, not the test server's own
// ephemeral-port address.
//
// Every call AFTER the first -- which can only happen if the client
// followed a redirect the first response returned -- is refused outright
// and its address recorded, rather than silently redirected to realAddr
// too: that is what lets a test prove SPECIFICALLY which address a
// followed redirect would have dialed (atchess-1c9.94's "assert the
// address actually dialed via DialContext/DialTLSContext" requirement),
// and that it is never reached at all once the CheckRedirect fix is in
// place. dialPlain (for a plain-HTTP redirect target, e.g.
// "http://169.254.169.254/") always refuses and records -- a did:web or
// AT Protocol .well-known identity fetch never legitimately makes a first
// request over plain HTTP, so any call to it is necessarily a followed
// redirect.
type alwaysRedirectDialer struct {
	realAddr string

	mu     sync.Mutex
	dialed []string
}

func (d *alwaysRedirectDialer) dialTLS(ctx context.Context, network, addr string) (net.Conn, error) {
	d.mu.Lock()
	d.dialed = append(d.dialed, addr)
	first := len(d.dialed) == 1
	d.mu.Unlock()
	if first {
		dialer := &tls.Dialer{Config: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec // test-only, self-signed cert
		return dialer.DialContext(ctx, network, d.realAddr)
	}
	return nil, fmt.Errorf("alwaysRedirectDialer: refusing to dial redirect target %s over TLS (a did:web/.well-known identity fetch must never follow a redirect)", addr)
}

func (d *alwaysRedirectDialer) dialPlain(ctx context.Context, network, addr string) (net.Conn, error) {
	d.mu.Lock()
	d.dialed = append(d.dialed, addr)
	d.mu.Unlock()
	return nil, fmt.Errorf("alwaysRedirectDialer: refusing to dial redirect target %s over plain HTTP (a did:web/.well-known identity fetch must never follow a redirect)", addr)
}

func (d *alwaysRedirectDialer) dialedAddrs() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]string, len(d.dialed))
	copy(out, d.dialed)
	return out
}

// newRedirectSpyResolver returns an identityResolver built via the REAL
// newIdentityResolver constructor -- so its httpClient carries the actual
// production CheckRedirect policy (refuseIdentityFetchRedirect), not a
// test-reimplemented stand-in -- with only its Transport swapped for an
// alwaysRedirectDialer routed at realAddr.
func newRedirectSpyResolver(realAddr string) (*identityResolver, *alwaysRedirectDialer) {
	r := newIdentityResolver(DefaultPLCDirectoryURL)
	spy := &alwaysRedirectDialer{realAddr: realAddr}
	r.httpClient.Transport = &http.Transport{
		DialContext:    spy.dialPlain,
		DialTLSContext: spy.dialTLS,
	}
	return r, spy
}

// TestFetchDIDDocument_RefusesRedirectToInternalAddress is the core
// atchess-1c9.94 regression test: a did:web host that has already passed
// every existing host-validation check (atchess-1c9.69/.70/.72/.93) can
// still steer the fetch to an arbitrary address -- including a cloud
// metadata / loopback address no validation ever inspected -- simply by
// having its server respond with a redirect. This proves, via the
// alwaysRedirectDialer dial spy (NOT the URL string, which would never
// reveal this), that the internal redirect target is NEVER dialed once
// r.httpClient carries the production CheckRedirect policy.
//
// PROVING THIS RED: with newIdentityResolver's CheckRedirect assignment
// temporarily removed, this test fails with dialed == [realAddr,
// "<internal-target>:80"] -- i.e. the internal address IS dialed. See the
// atchess-1c9.94 implementation report for the captured red output.
func TestFetchDIDDocument_RefusesRedirectToInternalAddress(t *testing.T) {
	internalTargets := []string{
		"http://169.254.169.254/", // cloud metadata address
		"http://127.0.0.1/",       // loopback
	}

	for _, target := range internalTargets {
		t.Run(target, func(t *testing.T) {
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, target, http.StatusFound)
			}))
			defer srv.Close()

			r, spy := newRedirectSpyResolver(srv.Listener.Addr().String())

			const did = "did:web:example.com" // ordinary, allowlisted, non-IP-literal host
			if _, err := r.fetchDIDDocument(context.Background(), did); err == nil {
				t.Fatalf("fetchDIDDocument(%q) unexpectedly succeeded, want a redirect-refused error", did)
			}

			dialed := spy.dialedAddrs()
			if len(dialed) != 1 {
				t.Fatalf("fetchDIDDocument(%q) dialed %v, want exactly 1 address (the redirect target %q must NEVER be dialed)", did, dialed, target)
			}
		})
	}
}

// TestFetchDIDDocument_RefusesRedirectToExternalHost pins the atchess-1c9.94
// design choice: option (a) (refuse ALL redirects) rather than option (b)
// (re-validate the Location host on every hop). A redirect to another
// perfectly ordinary, non-internal external host must ALSO be refused --
// not just an internal-looking one -- since neither did:web nor the AT
// Protocol .well-known handle endpoint is specified to redirect at all.
func TestFetchDIDDocument_RefusesRedirectToExternalHost(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://attacker.example/", http.StatusFound)
	}))
	defer srv.Close()

	r, spy := newRedirectSpyResolver(srv.Listener.Addr().String())

	const did = "did:web:example.com"
	if _, err := r.fetchDIDDocument(context.Background(), did); err == nil {
		t.Fatalf("fetchDIDDocument(%q) unexpectedly succeeded, want a redirect-refused error", did)
	}

	dialed := spy.dialedAddrs()
	if len(dialed) != 1 {
		t.Fatalf("fetchDIDDocument(%q) dialed %v, want exactly 1 address (a redirect to an external host must ALSO be refused)", did, dialed)
	}
}

// TestFetchDIDDocument_NoRedirectStillWorks is the required control for the
// two tests above: it proves the production CheckRedirect policy
// (refuseIdentityFetchRedirect) does not disturb an ordinary,
// non-redirecting did:web fetch -- CheckRedirect is only ever invoked by
// net/http when a response IS a redirect, so this also demonstrates the
// fix has zero effect on the golden path.
func TestFetchDIDDocument_NoRedirectStillWorks(t *testing.T) {
	const wantEndpoint = "https://pds-b.example"
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(DIDDocument{
			ID: "did:web:example.com",
			Service: []DIDService{
				{ID: "#atproto_pds", Type: "AtprotoPersonalDataServer", ServiceEndpoint: wantEndpoint},
			},
		})
	}))
	defer srv.Close()

	r, spy := newRedirectSpyResolver(srv.Listener.Addr().String())

	doc, err := r.fetchDIDDocument(context.Background(), "did:web:example.com")
	if err != nil {
		t.Fatalf("fetchDIDDocument: unexpected error: %v", err)
	}
	endpoint, err := parseServiceEndpoint(doc)
	if err != nil {
		t.Fatalf("parseServiceEndpoint: unexpected error: %v", err)
	}
	if endpoint != wantEndpoint {
		t.Errorf("resolved serviceEndpoint = %q, want %q", endpoint, wantEndpoint)
	}
	if dialed := spy.dialedAddrs(); len(dialed) != 1 {
		t.Errorf("dialed %v, want exactly 1 address for a non-redirecting fetch", dialed)
	}
}

// TestResolveHandleViaWellKnown_RefusesRedirect proves the same
// CheckRedirect protection covers resolveHandleViaWellKnown -- the OTHER
// user-named-domain fetch atchess-1c9.94 is about (Client.ResolveHandle now
// deliberately calls this with c.resolver().httpClient, NOT the
// general-purpose XRPC c.httpClient, precisely so this test's client --
// built the exact same way via newIdentityResolver -- is representative of
// what production actually uses).
func TestResolveHandleViaWellKnown_RefusesRedirect(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/", http.StatusFound)
	}))
	defer srv.Close()

	r, spy := newRedirectSpyResolver(srv.Listener.Addr().String())

	if _, err := resolveHandleViaWellKnown(context.Background(), r.httpClient, "alice.test"); err == nil {
		t.Fatalf("resolveHandleViaWellKnown unexpectedly succeeded, want a redirect-refused error")
	}
	if dialed := spy.dialedAddrs(); len(dialed) != 1 {
		t.Fatalf("resolveHandleViaWellKnown dialed %v, want exactly 1 address (the internal redirect target must NEVER be dialed)", dialed)
	}
}
