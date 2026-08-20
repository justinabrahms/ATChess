package atproto

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newTestResolver returns an identityResolver pointed at plcDirectoryURL
// with a short-timeout httpClient, bypassing the shared package-level
// registry (getIdentityResolver) so each test gets an isolated cache.
func newTestResolver(plcDirectoryURL string) *identityResolver {
	r := newIdentityResolver(plcDirectoryURL)
	r.httpClient = &http.Client{Timeout: 5 * time.Second}
	return r
}

// TestResolvePDS_DIDPLC covers did:plc resolution via a fake PLC directory
// (httptest.Server), independent of the real network and of
// https://plc.directory specifically.
func TestResolvePDS_DIDPLC(t *testing.T) {
	const did = "did:plc:abc123example"
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/"+did {
			t.Errorf("unexpected request path %q, want %q", r.URL.Path, "/"+did)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(DIDDocument{
			ID: did,
			Service: []DIDService{
				{ID: "#atproto_pds", Type: "AtprotoPersonalDataServer", ServiceEndpoint: "http://pds-a.example:2583"},
			},
		})
	}))
	defer srv.Close()

	r := newTestResolver(srv.URL)
	got, err := r.resolvePDS(context.Background(), did)
	if err != nil {
		t.Fatalf("resolvePDS: unexpected error: %v", err)
	}
	if want := "http://pds-a.example:2583"; got != want {
		t.Errorf("resolvePDS = %q, want %q", got, want)
	}
	if requests != 1 {
		t.Errorf("expected exactly 1 request to the fake PLC directory, got %d", requests)
	}
}

// TestResolvePDS_DIDWeb covers did:web resolution via
// https://<host>/.well-known/did.json, exercised against a real (self-
// signed) TLS listener since didWebDocumentURL always builds an https://
// URL -- a plain httptest.Server (http) cannot stand in for it.
//
// This is also the REQUIRED control for
// TestFetchDIDDocument_RejectsHostileDIDWebHostsBeforeAnyRequest
// (atchess-1c9.70): it proves validateDIDWebHost is not simply rejecting
// everything, by confirming an ordinary, portless, non-IP-literal did:web
// host still resolves successfully end to end. The did:web identifier
// itself ("did:web:example.com") deliberately carries NO port -- since
// atchess-1c9.70, validateDIDWebHost unconditionally rejects any ':' in a
// did:web host -- so this test cannot simply point at the test server's own
// ephemeral port the way it used to. Instead it uses a redirecting
// transport (dialRedirectClient) that dials the real ephemeral-port TLS
// listener regardless of what host the request nominally targets, so the
// did:web string under test stays a realistic, portless production shape.
func TestResolvePDS_DIDWeb(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/did.json" {
			t.Errorf("unexpected request path %q, want /.well-known/did.json", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		// dialRedirectClient dials the real listener regardless of the
		// request's nominal host, so without this assertion the test would
		// still pass even if didWebDocumentURL built the WRONG host --
		// restoring the guarantee the pre-atchess-1c9.70 test had
		// implicitly (found during atchess-1c9.72 review).
		if r.Host != "example.com" {
			t.Errorf("request Host = %q, want %q", r.Host, "example.com")
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(DIDDocument{
			ID: "did:web:example",
			Service: []DIDService{
				{ID: "#atproto_pds", Type: "AtprotoPersonalDataServer", ServiceEndpoint: "https://pds-b.example"},
			},
		})
	}))
	defer srv.Close()

	const did = "did:web:example.com" // ordinary, portless, non-IP-literal host

	r := newTestResolver(DefaultPLCDirectoryURL) // irrelevant for did:web
	r.httpClient = dialRedirectClient(srv.Listener.Addr().String())

	got, err := r.resolvePDS(context.Background(), did)
	if err != nil {
		t.Fatalf("resolvePDS: unexpected error: %v", err)
	}
	if want := "https://pds-b.example"; got != want {
		t.Errorf("resolvePDS = %q, want %q", got, want)
	}
}

// dialRedirectClient returns an *http.Client whose every TLS connection is
// actually dialed to target, regardless of the request's nominal host --
// used to let a test exercise a realistic, portless did:web identifier
// (e.g. "did:web:example.com") while the request is really served by a
// local httptest.NewTLSServer listening on an unrelated ephemeral port.
// TLS certificate verification is skipped (this is test-only plumbing
// against a self-signed cert, never production code).
func dialRedirectClient(target string) *http.Client {
	dialer := &tls.Dialer{Config: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec // test-only, self-signed cert
	return &http.Client{
		Transport: &http.Transport{
			DialTLSContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return dialer.DialContext(ctx, network, target)
			},
		},
	}
}

// countingRoundTripper counts every outbound RoundTrip attempt and never
// lets one actually reach the network -- used to prove a hostile did:web
// identifier is rejected BEFORE fetchDIDDocument ever constructs/dispatches
// a request (atchess-1c9.70), the same "zero outbound requests" standard
// atchess-1c9.69 established for hostile handles.
type countingRoundTripper struct{ n int }

func (c *countingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	c.n++
	return nil, fmt.Errorf("countingRoundTripper: unexpected outbound request to %s", req.URL)
}

// TestFetchDIDDocument_RejectsHostileDIDWebHostsBeforeAnyRequest is the
// core regression test for atchess-1c9.70 (found during review of
// atchess-1c9.69): ResolveHandle's "if strings.HasPrefix(handle, \"did:\")"
// short-circuit returns a did:web identifier unvalidated, and
// didWebDocumentURL used to build "https://" + host + ... with no
// validation of host at all -- after url.PathUnescape-ing each
// colon-separated segment, which is exactly what lets a caller smuggle the
// ':' and '/' characters a bare segment could otherwise never contain.
//
// This proves each hostile host shape is rejected by fetchDIDDocument with
// ZERO outbound requests -- not merely that an error comes back -- via
// countingRoundTripper, which would be hit by any request that actually got
// dispatched.
func TestFetchDIDDocument_RejectsHostileDIDWebHostsBeforeAnyRequest(t *testing.T) {
	hostile := []struct {
		name string
		did  string
	}{
		{"bare IPv4 literal (direct SSRF target, e.g. cloud metadata IP shape)", "did:web:169.254.169.254"},
		// Not a plain dotted-quad, so net.ParseIP alone would miss it --
		// caught only by rejectIPLiteralSpelling's final-label-must-not-
		// start-with-a-digit rule (shared with normalizeAndValidateHandle,
		// atchess-1c9.69). glibc's inet_aton (the cgo resolver) resolves
		// this exact string to 169.254.169.254, the cloud metadata address.
		{"hex-last-octet IPv4 spelling of the cloud metadata address (bypasses a dotted-quad-only net.ParseIP check)", "did:web:169.254.169.0xfe"},
		{"percent-encoded port (%3A) on a loopback IP literal -- decodes to 127.0.0.1:8080", "did:web:127.0.0.1%3A8080"},
		{"percent-encoded port (%3A) on an otherwise ordinary host -- decodes to evil.example:8080", "did:web:evil.example%3A8080"},
		{"percent-encoded path separator (%2F) injects a path segment into the host -- decodes to evil.example/..", "did:web:evil.example%2F.."},
		{"literal userinfo (@) in the host", "did:web:attacker@evil.example"},
		// atchess-1c9.72: validateDIDWebHost rejects ANY trailing dot on a
		// did:web host outright (not an IP-specific rule) -- a did:web host
		// is part of a DID identifier, not a hostname being resolved, so
		// "did:web:example.com" and "did:web:example.com." must not be
		// allowed to name the same PDS while comparing unequal as strings
		// everywhere this codebase compares DIDs directly. This also
		// closes the concrete SSRF gap that motivated the bead: a trailing
		// dot used to make rejectIPLiteralSpelling's final label EMPTY
		// (skipping the digit check) while net.ParseIP separately refused
		// to parse the trailing dot too, so an IP-literal-shaped host with
		// a trailing dot sailed through both checks untouched. See
		// validateDIDWebHost's doc comment for the full rationale.
		{"bare IPv4 literal with a trailing dot (FQDN root-label spelling of the cloud metadata address)", "did:web:169.254.169.254."},
		{"hex-last-octet IPv4 spelling with a trailing dot", "did:web:169.254.169.0xfe."},
		{"bare IPv4 literal with multiple trailing dots", "did:web:169.254.169.254.."},
		{"ordinary (non-IP) host with a single trailing dot -- identifier-aliasing risk, not IP-specific", "did:web:example.com."},
		{"bare dot as the entire host", "did:web:."},
	}

	for _, tc := range hostile {
		t.Run(tc.name, func(t *testing.T) {
			rt := &countingRoundTripper{}
			r := newTestResolver(DefaultPLCDirectoryURL)
			r.httpClient = &http.Client{Transport: rt}

			if _, err := r.fetchDIDDocument(context.Background(), tc.did); err == nil {
				t.Fatalf("fetchDIDDocument(%q) unexpectedly succeeded, want a validation error", tc.did)
			}
			if rt.n != 0 {
				t.Errorf("fetchDIDDocument(%q) made %d outbound request(s) beyond validation, want 0 (validation must reject it before any request is dispatched)", tc.did, rt.n)
			}
		})
	}
}

// TestDIDWebDocumentURL covers didWebDocumentURL's path construction
// directly (no network), including the %3A-encoded-port and multi-segment
// path shapes the did:web spec defines.
func TestDIDWebDocumentURL(t *testing.T) {
	cases := []struct {
		name    string
		did     string
		want    string
		wantErr bool
	}{
		{
			name: "bare host, no port -> /.well-known/did.json",
			did:  "did:web:example.com",
			want: "https://example.com/.well-known/did.json",
		},
		{
			// A did:web host specifying a port is rejected outright
			// (atchess-1c9.70): validateDIDWebHost bans any ':' in the
			// decoded host segment, which also covers IPv6 literals (always
			// ':'-bearing). This is a deliberate, unconditional restriction --
			// not merely "no port on an IP literal" -- see
			// validateDIDWebHost's doc comment for the full rejection list
			// and rationale.
			name:    "host with %3A-encoded port is rejected (atchess-1c9.70: no port on a did:web host)",
			did:     "did:web:localhost%3A2583",
			wantErr: true,
		},
		{
			name: "host with path segments -> <path>/did.json",
			did:  "did:web:example.com:user:alice",
			want: "https://example.com/user/alice/did.json",
		},
		{
			// atchess-1c9.72: a did:web host is part of a DID identifier,
			// not a hostname being resolved -- "example.com" and
			// "example.com." must not be allowed to name the same PDS
			// while comparing unequal as strings everywhere this codebase
			// compares DIDs directly. Reject the trailing dot outright,
			// even though it is legitimate FQDN syntax for DNS resolution
			// itself -- see validateDIDWebHost's doc comment.
			name:    "ordinary host with a single trailing dot is rejected (identifier canonical-form, not IP-specific)",
			did:     "did:web:example.com.",
			wantErr: true,
		},
		{
			name:    "ordinary host with multiple trailing dots is rejected",
			did:     "did:web:example.com..",
			wantErr: true,
		},
		{
			name:    "bare dot as the entire host is rejected",
			did:     "did:web:.",
			wantErr: true,
		},
		{
			// Regression guard: an ordinary, portless, non-dotted did:web
			// host must keep resolving normally -- this is the case the
			// trailing-dot rejection above must not disturb.
			name: "ordinary host, no trailing dot, still resolves",
			did:  "did:web:example.com",
			want: "https://example.com/.well-known/did.json",
		},
		{
			name:    "empty identifier",
			did:     "did:web:",
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := didWebDocumentURL(tc.did)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error for %q, got %q", tc.did, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("didWebDocumentURL(%q) = %q, want %q", tc.did, got, tc.want)
			}
		})
	}
}

// TestResolvePDS_NoPDSServiceEntry covers a DID document that exists but has
// no AtprotoPersonalDataServer service entry -- must error clearly, naming
// the DID.
func TestResolvePDS_NoPDSServiceEntry(t *testing.T) {
	const did = "did:plc:nopdsentry"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(DIDDocument{
			ID: did,
			Service: []DIDService{
				{ID: "#other", Type: "SomeOtherServiceType", ServiceEndpoint: "https://irrelevant.example"},
			},
		})
	}))
	defer srv.Close()

	r := newTestResolver(srv.URL)
	_, err := r.resolvePDS(context.Background(), did)
	if err == nil {
		t.Fatal("expected an error for a DID document with no PDS service entry")
	}
	if !strings.Contains(err.Error(), did) {
		t.Errorf("error %q does not name the DID %q", err.Error(), did)
	}
}

// TestResolvePDS_UnsupportedMethod covers a DID method this resolver does
// not support (only did:plc and did:web are).
func TestResolvePDS_UnsupportedMethod(t *testing.T) {
	r := newTestResolver(DefaultPLCDirectoryURL)
	_, err := r.resolvePDS(context.Background(), "did:key:zSomeKey")
	if err == nil {
		t.Fatal("expected an error for an unsupported DID method")
	}
}

// TestResolvePDS_CacheExpiry proves a resolved endpoint is reused (no
// second network request) while its cache entry is fresh, and re-fetched
// once that entry's expiry has passed. It manipulates the cache's internal
// expiry directly (rather than sleeping for the real pdsCacheTTL) since this
// test lives in-package.
func TestResolvePDS_CacheExpiry(t *testing.T) {
	const did = "did:plc:cacheexpiry"
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(DIDDocument{
			ID: did,
			Service: []DIDService{
				{Type: "AtprotoPersonalDataServer", ServiceEndpoint: fmt.Sprintf("http://pds.example/%d", requests)},
			},
		})
	}))
	defer srv.Close()

	r := newTestResolver(srv.URL)

	first, err := r.resolvePDS(context.Background(), did)
	if err != nil {
		t.Fatalf("first resolvePDS: unexpected error: %v", err)
	}
	if requests != 1 {
		t.Fatalf("expected 1 request after first resolvePDS, got %d", requests)
	}

	// Still within TTL: must be served from cache, no second request.
	second, err := r.resolvePDS(context.Background(), did)
	if err != nil {
		t.Fatalf("second resolvePDS: unexpected error: %v", err)
	}
	if requests != 1 {
		t.Errorf("expected cache hit (still 1 request), got %d requests", requests)
	}
	if second != first {
		t.Errorf("cached resolvePDS returned %q, want the original %q", second, first)
	}

	// Force the cache entry to have already expired, then resolve again --
	// this must re-fetch (and get a NEW endpoint, since the fake server
	// returns a different value on each request).
	r.mu.Lock()
	entry := r.cache[did]
	entry.expiry = time.Now().Add(-1 * time.Minute)
	r.cache[did] = entry
	r.mu.Unlock()

	third, err := r.resolvePDS(context.Background(), did)
	if err != nil {
		t.Fatalf("third resolvePDS: unexpected error: %v", err)
	}
	if requests != 2 {
		t.Errorf("expected a re-fetch after cache expiry (2 requests total), got %d", requests)
	}
	if third == first {
		t.Errorf("expected a freshly re-fetched endpoint after expiry, got the stale cached value %q again", third)
	}
}

// TestResolvePDS_FailureNotCached proves a resolution failure (unreachable
// resolver, or a DID document with no PDS entry) is never cached as a
// success: the very next call must retry against the network rather than
// returning a poisoned result, and once the server starts answering
// correctly, resolution succeeds.
func TestResolvePDS_FailureNotCached(t *testing.T) {
	const did = "did:plc:failsthenrecovers"
	requests := 0
	succeed := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if !succeed {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(DIDDocument{
			ID: did,
			Service: []DIDService{
				{Type: "AtprotoPersonalDataServer", ServiceEndpoint: "http://pds.example"},
			},
		})
	}))
	defer srv.Close()

	r := newTestResolver(srv.URL)

	if _, err := r.resolvePDS(context.Background(), did); err == nil {
		t.Fatal("expected the first resolvePDS (server returning 500) to fail")
	}
	if requests != 1 {
		t.Fatalf("expected 1 request after the failed resolvePDS, got %d", requests)
	}
	r.mu.Lock()
	_, cached := r.cache[did]
	r.mu.Unlock()
	if cached {
		t.Fatal("a failed resolution must not be cached")
	}

	succeed = true
	got, err := r.resolvePDS(context.Background(), did)
	if err != nil {
		t.Fatalf("expected resolvePDS to succeed once the server recovers: %v", err)
	}
	if requests != 2 {
		t.Errorf("expected a fresh (non-cached) attempt after the earlier failure, got %d total requests", requests)
	}
	if want := "http://pds.example"; got != want {
		t.Errorf("resolvePDS = %q, want %q", got, want)
	}
}

// TestResolvePDS_UnreachableResolverNamesDIDAndResolver covers the edge
// case of a resolver that cannot be reached at all: the error must name
// both the DID being resolved and identify the resolver that failed.
func TestResolvePDS_UnreachableResolverNamesDIDAndResolver(t *testing.T) {
	const did = "did:plc:unreachable"
	// A closed listener: connections to it are refused immediately, no
	// real network access required.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	unreachableURL := srv.URL
	srv.Close() // now nothing is listening

	r := newTestResolver(unreachableURL)
	_, err := r.resolvePDS(context.Background(), did)
	if err == nil {
		t.Fatal("expected an error resolving against an unreachable resolver")
	}
	if !strings.Contains(err.Error(), did) {
		t.Errorf("error %q does not name the DID %q", err.Error(), did)
	}
}

// TestScanPLCExportPage covers the PLC-export-log scanning fallback used by
// ResolveHandle as a last resort (resolveHandleViaPLCExport), independent of
// any network: a matching, non-nullified, non-tombstone entry is found; a
// later (chronologically later in the page) matching entry supersedes an
// earlier one for the same handle; nullified/tombstone entries are ignored;
// and malformed lines are tolerated rather than aborting the scan.
func TestScanPLCExportPage(t *testing.T) {
	body := strings.Join([]string{
		`{"did":"did:plc:first","createdAt":"2024-01-01T00:00:00Z","nullified":false,"operation":{"type":"create","alsoKnownAs":["at://someone-else.test"]}}`,
		`not valid json, must be skipped`,
		`{"did":"did:plc:old","createdAt":"2024-01-02T00:00:00Z","nullified":false,"operation":{"type":"create","alsoKnownAs":["at://bob.test"]}}`,
		`{"did":"did:plc:tombstoned","createdAt":"2024-01-03T00:00:00Z","nullified":true,"operation":{"type":"create","alsoKnownAs":["at://bob.test"]}}`,
		`{"did":"did:plc:current","createdAt":"2024-01-04T00:00:00Z","nullified":false,"operation":{"type":"update","alsoKnownAs":["at://bob.test"]}}`,
	}, "\n")

	found, lastCreatedAt, lineCount, err := scanPLCExportPage(strings.NewReader(body), "at://bob.test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := "did:plc:current"; found != want {
		t.Errorf("found = %q, want %q (most recent non-nullified match)", found, want)
	}
	if want := "2024-01-04T00:00:00Z"; lastCreatedAt != want {
		t.Errorf("lastCreatedAt = %q, want %q", lastCreatedAt, want)
	}
	if want := 4; lineCount != want { // 5 lines, 1 malformed and skipped
		t.Errorf("lineCount = %d, want %d", lineCount, want)
	}
}

// TestScanPLCExportPage_NoMatch covers the not-found case.
func TestScanPLCExportPage_NoMatch(t *testing.T) {
	body := `{"did":"did:plc:someone","createdAt":"2024-01-01T00:00:00Z","nullified":false,"operation":{"type":"create","alsoKnownAs":["at://someone-else.test"]}}`
	found, _, _, err := scanPLCExportPage(strings.NewReader(body), "at://bob.test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if found != "" {
		t.Errorf("found = %q, want empty (no match)", found)
	}
}

// TestGetIdentityResolver_SharedByBaseURL proves the shared resolver
// registry returns the SAME *identityResolver instance for the same
// plcDirectoryURL (so its cache is actually shared across the many
// short-lived *Clients internal/web constructs per request) and a
// DIFFERENT instance for a different plcDirectoryURL.
func TestGetIdentityResolver_SharedByBaseURL(t *testing.T) {
	a1 := getIdentityResolver("http://example-a.invalid")
	a2 := getIdentityResolver("http://example-a.invalid")
	if a1 != a2 {
		t.Error("expected the same *identityResolver for the same plcDirectoryURL")
	}
	b := getIdentityResolver("http://example-b.invalid")
	if a1 == b {
		t.Error("expected a different *identityResolver for a different plcDirectoryURL")
	}
	empty := getIdentityResolver("")
	def := getIdentityResolver(DefaultPLCDirectoryURL)
	if empty != def {
		t.Error("expected an empty plcDirectoryURL to resolve to the same instance as DefaultPLCDirectoryURL")
	}
}

// Compile-time sanity that ResolvePDS (the exported, package-level
// entrypoint used by internal/web in place of its former
// extractPDSFromDidDoc/getDidDocument duplication) has the expected shape.
var _ func(context.Context, string, string) (string, error) = ResolvePDS

// TestResolvePDS_Exported exercises the exported ResolvePDS wrapper end to
// end against a fake PLC directory.
func TestResolvePDS_Exported(t *testing.T) {
	const did = "did:plc:exportedwrapper"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(DIDDocument{
			ID: did,
			Service: []DIDService{
				{Type: "AtprotoPersonalDataServer", ServiceEndpoint: "http://pds.example"},
			},
		})
	}))
	defer srv.Close()

	got, err := ResolvePDS(context.Background(), did, srv.URL)
	if err != nil {
		t.Fatalf("ResolvePDS: unexpected error: %v", err)
	}
	if want := "http://pds.example"; got != want {
		t.Errorf("ResolvePDS = %q, want %q", got, want)
	}
}

// TestResolveHandleViaPLCExport_GatedAgainstDefaultDirectory covers
// atchess-1c9.10 review gap 2: the PLC-export fallback must refuse to scan
// the real, public https://plc.directory (a multi-million-operation ledger
// a bounded scan can never usefully search) and must do so WITHOUT making
// any network call -- a mistyped/deleted real handle must fail fast, not
// after a ~10s scan. This test proves no request is ever sent by pointing
// the resolver's httpClient at a server that fails the test if it receives
// any request at all, while the resolver is configured with
// DefaultPLCDirectoryURL.
func TestResolveHandleViaPLCExport_GatedAgainstDefaultDirectory(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected network request to %s -- the PLC export fallback must not fire against the default directory", r.URL)
	}))
	defer srv.Close()

	r := newTestResolver(DefaultPLCDirectoryURL)
	// Point the plcDirectoryURL field back at the default sentinel value
	// (newTestResolver/newIdentityResolver already do this for an empty or
	// DefaultPLCDirectoryURL input), but use srv's client so a request, if
	// one were mistakenly sent, would be observable rather than silently
	// timing out against a real DNS lookup for "plc.directory".
	r.httpClient = srv.Client()

	_, err := r.resolveHandleViaPLCExport(context.Background(), "someone.test")
	if err == nil {
		t.Fatal("expected an error: the PLC export fallback must refuse to run against the default directory")
	}
	if !strings.Contains(err.Error(), "skipped") {
		t.Errorf("error %q does not clearly indicate the scan was skipped/gated", err.Error())
	}
}

// TestResolveHandleViaPLCExport_RunsAgainstNonDefaultDirectory proves the
// gate in TestResolveHandleViaPLCExport_GatedAgainstDefaultDirectory does
// not disable the fallback entirely -- it must still function against a
// configured non-default (e.g. local/test) PLC directory, which is what the
// dual-PDS test harness relies on for its non-DNS-resolvable ".test"
// handles.
func TestResolveHandleViaPLCExport_RunsAgainstNonDefaultDirectory(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, `{"did":"did:plc:foundme","createdAt":"2024-01-01T00:00:00Z","nullified":false,"operation":{"type":"create","alsoKnownAs":["at://someone.test"]}}`)
	}))
	defer srv.Close()

	r := newTestResolver(srv.URL)
	got, err := r.resolveHandleViaPLCExport(context.Background(), "someone.test")
	if err != nil {
		t.Fatalf("unexpected error against a configured non-default directory: %v", err)
	}
	if want := "did:plc:foundme"; got != want {
		t.Errorf("resolveHandleViaPLCExport = %q, want %q", got, want)
	}
}

// TestNormalizeAndValidateHandle is table-driven coverage of the AT
// Protocol handle grammar enforced by normalizeAndValidateHandle
// (atchess-1c9.69), independent of any resolver/network. See
// TestResolveHandle_RejectsHostileHandlesBeforeAnyRequest and
// TestResolveHandle_ValidHandlesStillResolve (url_test.go) for the
// end-to-end proof that this is actually wired into Client.ResolveHandle
// and blocks real requests.
func TestNormalizeAndValidateHandle(t *testing.T) {
	cases := []struct {
		name    string
		handle  string
		want    string // expected normalized return value; ignored if wantErr
		wantErr bool
	}{
		{name: "empty", handle: "", wantErr: true},
		{name: "single label with no dot", handle: "alice", wantErr: true},
		{name: "leading hyphen in a label", handle: "-alice.test", wantErr: true},
		{name: "trailing hyphen in a label", handle: "alice-.test", wantErr: true},
		{name: "label consisting of a single hyphen", handle: "-.test", wantErr: true},
		{name: "consecutive dots (empty label)", handle: "alice..test", wantErr: true},
		{name: "leading dot (empty label)", handle: ".alice.test", wantErr: true},
		{name: "trailing dot (empty label)", handle: "alice.test.", wantErr: true},
		{name: "label over 63 bytes", handle: strings.Repeat("a", 64) + ".test", wantErr: true},
		{name: "label at exactly 63 bytes is allowed", handle: strings.Repeat("a", 63) + ".test", want: strings.Repeat("a", 63) + ".test"},
		{name: "total length over 253 bytes", handle: strings.Repeat("a", 250) + "." + strings.Repeat("b", 10) + ".test", wantErr: true},
		{name: "contains a slash", handle: "alice.test/evil", wantErr: true},
		{name: "contains an at-sign", handle: "alice@evil.test", wantErr: true},
		{name: "contains a colon", handle: "alice.test:8080", wantErr: true},
		{name: "contains whitespace", handle: "alice test.example", wantErr: true},
		{name: "bare IPv4 literal", handle: "127.0.0.1", wantErr: true},
		// ":" is not in handleLabelRE's character class, so this is actually
		// rejected at the label-regexp step (the colons never even survive
		// strings.Split into label candidates that look IP-shaped) -- NOT by
		// the net.ParseIP check below it, despite the name suggesting
		// otherwise. Named for what actually rejects it, per the
		// atchess-1c9.69 fix-pass review: any IPv6 form contains ':', which
		// dies at the regexp first.
		{name: "IPv6 literal is rejected by the label-character regexp (colon is not a valid label character), not by net.ParseIP", handle: "::1", wantErr: true},

		// The following five are all rejected by the final-label-must-not-
		// start-with-a-digit rule (see normalizeAndValidateHandle's doc
		// comment) -- NOT by net.ParseIP, which only recognizes plain
		// dotted-quad/IPv6 syntax and would let every one of these through.
		// Each resolves to a real, sensitive address under glibc's
		// (cgo-backed) numeric-address parsing, which Go selects
		// automatically on nsswitch configs carrying mdns/resolve entries
		// (plausible on the Ubuntu 24.04 deploy host).
		{name: "hex-last-octet form of the cloud metadata address (glibc inet_aton resolves this to 169.254.169.254)", handle: "169.254.169.0xfe", wantErr: true},
		{name: "all-hex-octet form of the cloud metadata address (glibc inet_aton resolves this to 169.254.169.254)", handle: "0xa9.0xfe.0xa9.0xfe", wantErr: true},
		{name: "short-form loopback (glibc inet_aton resolves this to 127.0.0.1)", handle: "127.1", wantErr: true},
		{name: "hex-first-octet form of loopback (glibc inet_aton resolves this to 127.0.0.1)", handle: "0x7f.0.0.1", wantErr: true},
		{name: "octal-octet form (glibc inet_aton resolves this to 8.8.8.8)", handle: "010.010.010.010", wantErr: true},

		{name: "ordinary two-label handle", handle: "alice.test", want: "alice.test"},
		{name: "ordinary three-label handle", handle: "alice.bsky.social", want: "alice.bsky.social"},
		{name: "handle with a hyphen mid-label is allowed", handle: "my-handle.test", want: "my-handle.test"},
		{name: "uppercase is normalized to lowercase, not rejected", handle: "Alice.Bsky.Social", want: "alice.bsky.social"},
		{name: "punycode (IDN) final label is allowed", handle: "xn--80ak6aa92e.com", want: "xn--80ak6aa92e.com"},
		{name: "digits in a non-final label are allowed (only the FINAL label may not start with a digit)", handle: "user123.example.com", want: "user123.example.com"},
		// Deliberate choice: the AT Protocol spec's final-label rule only
		// forbids the final label from STARTING with a digit -- a digit
		// elsewhere in the final label (not in the leading position) is
		// still spec-legal, so this is accepted, not rejected.
		{name: "digit in the final label that is not its first character is allowed", handle: "example.c0m", want: "example.c0m"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizeAndValidateHandle(tc.handle)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("normalizeAndValidateHandle(%q) = %q, nil; want an error", tc.handle, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeAndValidateHandle(%q) returned an unexpected error: %v", tc.handle, err)
			}
			if got != tc.want {
				t.Errorf("normalizeAndValidateHandle(%q) = %q, want %q", tc.handle, got, tc.want)
			}
		})
	}
}

// TestValidateDIDWebHost exercises validateDIDWebHost directly (no
// network), covering the trailing-dot identifier-canonical-form rule
// (atchess-1c9.72) alongside the pre-existing checks it must not disturb.
// A did:web host is part of a DID identifier, not a hostname being
// resolved: "example.com" and "example.com." must not both validate,
// since callers all over this codebase compare DID strings directly for
// ownership/participant checks, and allowing the dot would let the same
// PDS be named by two strings that compare unequal.
func TestValidateDIDWebHost(t *testing.T) {
	cases := []struct {
		name    string
		host    string
		wantErr bool
	}{
		{name: "empty host", host: "", wantErr: true},

		// The core atchess-1c9.72 bug and its variants: any trailing dot
		// is rejected outright, IP-shaped or not.
		{name: "bare IPv4 literal with a trailing dot", host: "169.254.169.254.", wantErr: true},
		{name: "hex-last-octet IPv4 spelling with a trailing dot", host: "169.254.169.0xfe.", wantErr: true},
		{name: "ordinary (non-IP) host with a single trailing dot", host: "example.com.", wantErr: true},
		{name: "multiple trailing dots on an IP literal", host: "169.254.169.254..", wantErr: true},
		{name: "multiple trailing dots on an ordinary host", host: "example.com..", wantErr: true},
		{name: "bare dot as the entire host", host: ".", wantErr: true},

		// Regression guards: ordinary, non-dotted hosts must be unaffected.
		{name: "ordinary host, no trailing dot, is accepted", host: "example.com", wantErr: false},
		{name: "bare IPv4 literal, no trailing dot, still rejected as an IP literal (pre-existing atchess-1c9.70 behavior)", host: "169.254.169.254", wantErr: true},

		// Pre-existing checks this fix must not disturb.
		{name: "userinfo (@) is still rejected", host: "attacker@evil.example", wantErr: true},
		{name: "port (:) is still rejected", host: "evil.example:8080", wantErr: true},
		{name: "path separator (/) is still rejected", host: "evil.example/..", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateDIDWebHost(tc.host)
			if tc.wantErr && err == nil {
				t.Fatalf("validateDIDWebHost(%q) = nil, want an error", tc.host)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("validateDIDWebHost(%q) returned an unexpected error: %v", tc.host, err)
			}
		})
	}
}
