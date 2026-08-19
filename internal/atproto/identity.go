package atproto

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// DefaultPLCDirectoryURL is the public PLC directory used to resolve
// did:plc identifiers when no override is configured. See
// Client.SetPLCDirectoryURL and config.ATProtoConfig.PLCDirectoryURL for how
// a caller (e.g. the local dual-PDS test harness, which runs its own
// hermetic did:plc server) points this at a different directory instead.
const DefaultPLCDirectoryURL = "https://plc.directory"

// pdsCacheTTL/pdsCacheMaxEntries bound the in-memory DID->PDS resolution
// cache shared by every identityResolver (see getIdentityResolver). A
// resolution FAILURE is never cached -- only a confirmed serviceEndpoint is
// -- so a transient resolver outage or a not-yet-propagated DID is retried
// on the next call instead of sticking as a permanent failure.
const (
	pdsCacheTTL        = 10 * time.Minute
	pdsCacheMaxEntries = 512
)

type pdsCacheEntry struct {
	endpoint string
	expiry   time.Time
}

// identityResolver resolves AT Protocol DIDs to their PDS serviceEndpoint
// (atchess-1c9.10 step 1) and, as a bounded last-resort fallback for handles
// that cannot be resolved via DNS/HTTPS well-known (see resolveHandleViaPLCExport),
// scans the configured PLC directory's public /export audit log for a
// matching alsoKnownAs entry.
type identityResolver struct {
	plcDirectoryURL string
	httpClient      *http.Client

	mu    sync.Mutex
	cache map[string]pdsCacheEntry
	order []string // insertion order of cache keys, for bounded FIFO eviction
}

func newIdentityResolver(plcDirectoryURL string) *identityResolver {
	if plcDirectoryURL == "" {
		plcDirectoryURL = DefaultPLCDirectoryURL
	}
	return &identityResolver{
		plcDirectoryURL: plcDirectoryURL,
		httpClient:      &http.Client{Timeout: 10 * time.Second},
		cache:           make(map[string]pdsCacheEntry),
	}
}

var (
	resolverRegistryMu sync.Mutex
	resolverRegistry   = map[string]*identityResolver{}
)

// getIdentityResolver returns the shared identityResolver for plcDirectoryURL,
// creating one on first use. Sharing by base URL (rather than one per
// *Client) matters because internal/web builds a brand new *atproto.Client
// per authenticated request (see Service.clientFor) -- a client-scoped cache
// would never actually stay warm across a game's many moves.
func getIdentityResolver(plcDirectoryURL string) *identityResolver {
	if plcDirectoryURL == "" {
		plcDirectoryURL = DefaultPLCDirectoryURL
	}
	resolverRegistryMu.Lock()
	defer resolverRegistryMu.Unlock()
	r, ok := resolverRegistry[plcDirectoryURL]
	if !ok {
		r = newIdentityResolver(plcDirectoryURL)
		resolverRegistry[plcDirectoryURL] = r
	}
	return r
}

// ResolvePDS resolves did's PDS serviceEndpoint using the shared resolver
// for plcDirectoryURL (DefaultPLCDirectoryURL if empty). Exported so callers
// that hold a DID but no *atproto.Client (e.g. internal/web's pre-login
// OAuth discovery, which previously duplicated this logic as
// extractPDSFromDidDoc/getDidDocument) can resolve without a second
// implementation.
func ResolvePDS(ctx context.Context, did, plcDirectoryURL string) (string, error) {
	return getIdentityResolver(plcDirectoryURL).resolvePDS(ctx, did)
}

// resolvePDS resolves did's PDS serviceEndpoint, consulting the cache first.
func (r *identityResolver) resolvePDS(ctx context.Context, did string) (string, error) {
	if did == "" {
		return "", fmt.Errorf("resolvePDS: empty DID")
	}
	if endpoint, ok := r.cacheGet(did); ok {
		return endpoint, nil
	}
	doc, err := r.fetchDIDDocument(ctx, did)
	if err != nil {
		return "", fmt.Errorf("resolving PDS for %s: %w", did, err)
	}
	endpoint, err := parseServiceEndpoint(doc)
	if err != nil {
		return "", fmt.Errorf("resolving PDS for %s: %w", did, err)
	}
	r.cachePut(did, endpoint)
	return endpoint, nil
}

func (r *identityResolver) cacheGet(did string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.cache[did]
	if !ok {
		return "", false
	}
	if time.Now().After(entry.expiry) {
		delete(r.cache, did)
		r.removeOrderLocked(did)
		return "", false
	}
	return entry.endpoint, true
}

func (r *identityResolver) cachePut(did, endpoint string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.cache[did]; !exists {
		for len(r.order) >= pdsCacheMaxEntries {
			oldest := r.order[0]
			r.order = r.order[1:]
			delete(r.cache, oldest)
		}
		r.order = append(r.order, did)
	}
	r.cache[did] = pdsCacheEntry{endpoint: endpoint, expiry: time.Now().Add(pdsCacheTTL)}
}

func (r *identityResolver) removeOrderLocked(did string) {
	for i, d := range r.order {
		if d == did {
			r.order = append(r.order[:i], r.order[i+1:]...)
			return
		}
	}
}

// fetchDIDDocument retrieves did's DID document: did:plc via
// {plcDirectoryURL}/{did}; did:web via https://{host}/.well-known/did.json
// (with any %3A-encoded port and additional path segments decoded per the
// did:web spec). Any other method is rejected -- ATChess only ever deals in
// did:plc (production) and did:web (test/self-hosted) identities.
func (r *identityResolver) fetchDIDDocument(ctx context.Context, did string) (*DIDDocument, error) {
	switch {
	case strings.HasPrefix(did, "did:plc:"):
		u := strings.TrimRight(r.plcDirectoryURL, "/") + "/" + did
		doc, err := r.fetchJSON(ctx, u)
		if err != nil {
			return nil, fmt.Errorf("did:plc resolver %s: %w", r.plcDirectoryURL, err)
		}
		return doc, nil
	case strings.HasPrefix(did, "did:web:"):
		u, err := didWebDocumentURL(did)
		if err != nil {
			return nil, fmt.Errorf("did:web resolver: %w", err)
		}
		doc, err := r.fetchJSON(ctx, u)
		if err != nil {
			return nil, fmt.Errorf("did:web resolver %s: %w", u, err)
		}
		return doc, nil
	default:
		return nil, fmt.Errorf("unsupported DID method for %q (only did:plc and did:web are supported)", did)
	}
}

func (r *identityResolver) fetchJSON(ctx context.Context, u string) (*DIDDocument, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("building request to %s: %w", u, err)
	}
	resp, err := r.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching %s: %w", u, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024))
		return nil, fmt.Errorf("fetching %s: HTTP %d: %s", u, resp.StatusCode, string(body))
	}
	var doc DIDDocument
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, fmt.Errorf("decoding DID document from %s: %w", u, err)
	}
	return &doc, nil
}

// didWebDocumentURL converts a did:web identifier into the https URL its DID
// document is published at, per https://w3c-ccg.github.io/did-method-web/:
// colons after "did:web:" separate path segments (with a %3A-encoded port
// staying attached to the host segment), and when there is no explicit path
// the document lives at /.well-known/did.json; otherwise it lives at
// <path>/did.json.
func didWebDocumentURL(did string) (string, error) {
	id := strings.TrimPrefix(did, "did:web:")
	if id == "" {
		return "", fmt.Errorf("empty did:web identifier in %q", did)
	}
	segments := strings.Split(id, ":")
	for i, seg := range segments {
		decoded, err := url.PathUnescape(seg)
		if err != nil {
			return "", fmt.Errorf("invalid did:web segment %q in %q: %w", seg, did, err)
		}
		if decoded == "" {
			return "", fmt.Errorf("empty did:web path segment in %q", did)
		}
		segments[i] = decoded
	}
	host := segments[0]
	if len(segments) == 1 {
		return "https://" + host + "/.well-known/did.json", nil
	}
	return "https://" + host + "/" + strings.Join(segments[1:], "/") + "/did.json", nil
}

// --- Handle resolution -----------------------------------------------------
//
// resolveHandleDNSTimeout/resolveHandleWellKnownTimeout bound each
// resolution attempt so a slow/unreachable DNS server or origin cannot hang
// a request indefinitely.
const (
	resolveHandleDNSTimeout       = 5 * time.Second
	resolveHandleWellKnownTimeout = 5 * time.Second
)

// resolveHandleViaDNS looks up the "_atproto.<handle>" TXT record per the AT
// Protocol handle resolution spec (https://atproto.com/specs/handle) and
// extracts the "did=" value. Returns a non-nil error if no such record
// exists or none of its values are well-formed.
func resolveHandleViaDNS(ctx context.Context, handle string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, resolveHandleDNSTimeout)
	defer cancel()
	records, err := net.DefaultResolver.LookupTXT(ctx, "_atproto."+handle)
	if err != nil {
		return "", fmt.Errorf("DNS TXT lookup for _atproto.%s: %w", handle, err)
	}
	for _, rec := range records {
		if did, ok := strings.CutPrefix(rec, "did="); ok && did != "" {
			return did, nil
		}
	}
	return "", fmt.Errorf("DNS TXT record(s) for _atproto.%s present but none started with \"did=\"", handle)
}

// resolveHandleViaWellKnown fetches https://<handle>/.well-known/atproto-did
// per the AT Protocol handle resolution spec using httpClient. The response
// body is expected to be the DID as UTF-8 plain text (surrounding
// whitespace tolerated).
func resolveHandleViaWellKnown(ctx context.Context, httpClient *http.Client, handle string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, resolveHandleWellKnownTimeout)
	defer cancel()
	u := "https://" + handle + "/.well-known/atproto-did"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", fmt.Errorf("building request to %s: %w", u, err)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetching %s: %w", u, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetching %s: HTTP %d", u, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8*1024))
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", u, err)
	}
	did := strings.TrimSpace(string(body))
	if !strings.HasPrefix(did, "did:") {
		return "", fmt.Errorf("%s did not return a DID (got %q)", u, did)
	}
	return did, nil
}

// plcExportPageSize/plcExportMaxPages/plcExportBudget bound
// resolveHandleViaPLCExport -- see its doc comment.
const (
	plcExportPageSize = 1000
	plcExportMaxPages = 20
	plcExportBudget   = 10 * time.Second
)

// resolveHandleViaPLCExport is a bounded, best-effort last resort for
// handles that cannot be resolved via DNS or HTTPS well-known (both of
// which require the handle to be a real, DNS-resolvable domain) -- notably
// the local dual-PDS test harness's ".test" handles, which per RFC 2606 are
// permanently reserved and never DNS-resolvable. The did:plc method's
// directory server publishes its full operation log at GET /export
// (paginated via ?after=<createdAt>&count=<n>; see
// https://github.com/did-method-plc/did-method-plc), which both the real
// https://plc.directory and this project's local-plc test double expose.
// This scans that log chronologically for an operation whose alsoKnownAs
// includes "at://<handle>", bounded by plcExportMaxPages/plcExportBudget so
// an unmatched handle fails fast rather than scanning a large production
// ledger indefinitely -- this path is not how real handle resolution scales
// and is only reached after DNS/well-known have already failed.
//
// GATED to non-default directories only (atchess-1c9.10 review gap 2): the
// real https://plc.directory's /export log is a multi-million-operation,
// permanently-append-only ledger with no way to seek near its end -- the
// first page (no "after" cursor) starts at its chronological beginning
// (2022-11-17), so a scan bounded by plcExportMaxPages/plcExportBudget can
// only ever see the OLDEST ~20k operations and can essentially never match
// a real handle. Every mistyped or deleted real handle would otherwise pay
// up to plcExportMaxPages * plcExportPageSize requests (~10MB) and up to
// plcExportBudget of latency for zero possible benefit. This fallback is
// only useful (and only fires) against a configured non-default directory
// -- i.e. the local dual-PDS test harness's hermetic did:plc server, whose
// entire tiny log is genuinely scannable -- so a real user's mistyped
// handle fails fast here instead.
func (r *identityResolver) resolveHandleViaPLCExport(ctx context.Context, handle string) (string, error) {
	if r.plcDirectoryURL == DefaultPLCDirectoryURL {
		return "", fmt.Errorf("PLC export scan skipped: no local/test PLC directory configured (ATPROTO_PLC_DIRECTORY_URL); refusing to scan the public %s (a multi-million-operation ledger a bounded scan can never usefully search)", DefaultPLCDirectoryURL)
	}

	ctx, cancel := context.WithTimeout(ctx, plcExportBudget)
	defer cancel()

	target := "at://" + handle
	after := ""
	for page := 0; page < plcExportMaxPages; page++ {
		u := fmt.Sprintf("%s/export?count=%d", strings.TrimRight(r.plcDirectoryURL, "/"), plcExportPageSize)
		if after != "" {
			u += "&after=" + url.QueryEscape(after)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return "", fmt.Errorf("building PLC export request to %s: %w", u, err)
		}
		resp, err := r.httpClient.Do(req)
		if err != nil {
			return "", fmt.Errorf("fetching %s: %w", u, err)
		}
		found, lastCreatedAt, lineCount, scanErr := scanPLCExportPage(resp.Body, target)
		resp.Body.Close()
		if scanErr != nil {
			return "", fmt.Errorf("reading PLC export from %s: %w", u, scanErr)
		}
		if found != "" {
			return found, nil
		}
		if lineCount < plcExportPageSize || lastCreatedAt == "" || lastCreatedAt == after {
			break // reached the end of the log
		}
		after = lastCreatedAt
	}
	return "", fmt.Errorf("handle %q not found in PLC export log at %s within %d page(s)", handle, r.plcDirectoryURL, plcExportMaxPages)
}

// plcExportEntry is the shape of one newline-delimited-JSON row of a did:plc
// directory's /export audit log -- just enough of it to reverse-map a
// handle to the DID that most recently claimed it.
type plcExportEntry struct {
	DID       string `json:"did"`
	CreatedAt string `json:"createdAt"`
	Nullified bool   `json:"nullified"`
	Operation struct {
		Type        string   `json:"type"`
		AlsoKnownAs []string `json:"alsoKnownAs"`
	} `json:"operation"`
}

// scanPLCExportPage scans one page of newline-delimited PLC export JSON for
// an operation whose alsoKnownAs contains target, returning the DID of the
// LAST (most recent) matching, non-nullified, non-tombstone entry seen (a
// handle can be reassigned over time, and export is chronological), plus the
// final entry's createdAt (for pagination) and how many well-formed lines
// were read. A malformed line is skipped rather than aborting the scan.
func scanPLCExportPage(body io.Reader, target string) (found, lastCreatedAt string, lineCount int, err error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var entry plcExportEntry
		if jsonErr := json.Unmarshal(line, &entry); jsonErr != nil {
			continue
		}
		lineCount++
		lastCreatedAt = entry.CreatedAt
		if entry.Nullified || entry.Operation.Type == "plc_tombstone" {
			continue
		}
		for _, aka := range entry.Operation.AlsoKnownAs {
			if aka == target {
				found = entry.DID
			}
		}
	}
	return found, lastCreatedAt, lineCount, scanner.Err()
}
