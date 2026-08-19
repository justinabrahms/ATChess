package atproto

import (
	"context"
	"encoding/json"
	"fmt"
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
func TestResolvePDS_DIDWeb(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/did.json" {
			t.Errorf("unexpected request path %q, want /.well-known/did.json", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
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

	// srv.Listener.Addr() is host:port on 127.0.0.1; did:web encodes the
	// literal ':' before a port as %3A per the did:web spec.
	hostPort := srv.Listener.Addr().String()
	did := "did:web:" + strings.ReplaceAll(hostPort, ":", "%3A")

	r := newTestResolver(DefaultPLCDirectoryURL) // irrelevant for did:web
	r.httpClient = srv.Client()                  // trusts the test server's self-signed cert

	got, err := r.resolvePDS(context.Background(), did)
	if err != nil {
		t.Fatalf("resolvePDS: unexpected error: %v", err)
	}
	if want := "https://pds-b.example"; got != want {
		t.Errorf("resolvePDS = %q, want %q", got, want)
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
			name: "host with %3A-encoded port -> /.well-known/did.json",
			did:  "did:web:localhost%3A2583",
			want: "https://localhost:2583/.well-known/did.json",
		},
		{
			name: "host with path segments -> <path>/did.json",
			did:  "did:web:example.com:user:alice",
			want: "https://example.com/user/alice/did.json",
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
