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
	"regexp"
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
		httpClient: &http.Client{
			Timeout:       10 * time.Second,
			CheckRedirect: refuseIdentityFetchRedirect,
		},
		cache: make(map[string]pdsCacheEntry),
	}
}

// refuseIdentityFetchRedirect is the http.Client.CheckRedirect policy shared
// by every identity-resolution fetch this package makes against a
// user-named domain: the did:plc directory (fetchDIDDocument,
// resolveHandleViaPLCExport), a did:web document (fetchDIDDocument), and
// the AT Protocol HTTPS well-known handle endpoint
// (resolveHandleViaWellKnown, which client.go's ResolveHandle deliberately
// routes through THIS SAME client -- see Client.resolver() and its call
// site -- rather than the general-purpose XRPC httpClient, precisely so
// this policy cannot drift into a second, differently-configured copy).
//
// atchess-1c9.94 (found by reviewer during atchess-1c9.93): every did:web /
// did:plc / handle host control this package has (atchess-1c9.69's handle
// grammar, .70's IP-literal rejection, .72's empty-label rule, .93's ASCII
// allowlist) validates only the host we are ABOUT to request. None of them
// survives a redirect -- Go's default http.Client follows up to 10 of
// them, so a fetch to a perfectly legitimate, allowlisted host could return
// e.g. "302 Location: http://169.254.169.254/" and be followed with zero
// further validation, defeating the entire validation stack with nothing
// more exotic than a redirect.
//
// This refuses EVERY redirect outright (option (a) from the bead) rather
// than re-validating the Location host on every hop (option (b)): neither
// a did:plc directory, a did:web document fetch, nor the AT Protocol
// .well-known/atproto-did endpoint is specified to redirect, so there is
// no legitimate case being given up here, and "no second validator to keep
// in sync with the first" is exactly the property that made .72 and .93
// possible in the first place (a second copy of the host-validation rule,
// silently drifting from the first).
//
// Deliberately NOT applied to the XRPC httpClient(s) client.go builds for
// talking to a PDS (see NewClient/NewClientWithDPoP/NewClientFromSession
// and RefreshSession): by the time any of those fire, the PDS's host has
// already been resolved (via this same, now-redirect-safe identity
// resolution, or supplied directly as trusted configuration) rather than
// being a value an attacker can steer per-request the way a did:web
// document's Location header could. Banning redirects there too could
// break a legitimate PDS's own redirect behaviour (see e.g. the e2e
// harness's proxy) for no corresponding SSRF benefit, since the request
// target is not attacker-named at that point.
func refuseIdentityFetchRedirect(req *http.Request, via []*http.Request) error {
	return fmt.Errorf("identity fetch: refusing to follow redirect to %s (did:web/did:plc/.well-known identity fetches must not redirect, atchess-1c9.94)", req.URL)
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
//
// The host segment (segments[0], after decoding) is validated by
// validateDIDWebHost -- see that function's doc comment for exactly what it
// rejects and why this must run AFTER url.PathUnescape (atchess-1c9.70).
// This is the single choke point every did:web caller shares (both
// fetchDIDDocument call sites below go through here), mirroring
// normalizeAndValidateHandle's single-choke-point approach for handles
// (atchess-1c9.69).
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
	if err := validateDIDWebHost(host); err != nil {
		return "", fmt.Errorf("invalid did:web identifier %q: %w", did, err)
	}
	if len(segments) == 1 {
		return "https://" + host + "/.well-known/did.json", nil
	}
	return "https://" + host + "/" + strings.Join(segments[1:], "/") + "/did.json", nil
}

// validateDIDWebHost validates host -- the FIRST colon-separated segment of
// a did:web identifier, already percent-DECODED by didWebDocumentURL above
// -- before it is used to build the https:// URL a DID document is fetched
// from (atchess-1c9.70, found during review of atchess-1c9.69).
//
// This MUST run on the already-decoded value, not the raw segment:
// url.PathUnescape is exactly what lets a caller smuggle the ':' and '/'
// characters that would otherwise be structurally impossible in a bare
// colon-separated segment. For example "127.0.0.1%3A8080" decodes to
// "127.0.0.1:8080" (an IP literal with an attacker-chosen port) and
// "evil.example%2F.." decodes to "evil.example/.." (a path segment injected
// into what should be a bare hostname) -- both would otherwise sail through
// unvalidated straight into "https://" + host + ...
//
// Rejects a host that:
//   - contains any byte outside the ASCII letter/digit/'-'/'.' allowlist
//     (atchess-1c9.93) -- see didWebHostCharRE's doc comment for why this
//     is a whitelist rather than yet another blacklist entry;
//   - contains an empty label -- leading, trailing, or embedded
//     (consecutive dots), including a bare "." (atchess-1c9.72,
//     atchess-1c9.93);
//   - is an IP literal in ANY spelling (dotted-quad, hex, octal,
//     short-form, or IPv6) -- via the same rejectIPLiteralSpelling logic
//     normalizeAndValidateHandle uses for handles, so this SSRF-relevant
//     analysis is written once rather than drifting between two copies;
//   - carries userinfo ("@");
//   - specifies a port (":") -- this also catches every IPv6 literal
//     spelling, since IPv6 addresses always contain ':';
//   - contains a path separator ("/"), including one smuggled in via a
//     percent-encoded form that only becomes '/' after decoding.
//
// Deliberately NOT validated here: did:web's remaining (non-host) path
// segments. A hostile %2F in one of those can only inject an extra path
// component on the same, already-host-validated origin -- a narrower issue
// than steering the request to a different host entirely, and out of this
// bead's scope (grammar/structure validation of the HOST component only;
// see atchess-1c9.70's done-criteria).
func validateDIDWebHost(host string) error {
	if host == "" {
		return fmt.Errorf("did:web host is empty")
	}
	// A did:web host is part of a DID IDENTIFIER, not a serviceEndpoint --
	// identifiers need a single canonical form, and the AT Protocol did:web
	// spec's own host-segment grammar has no port component at all (a port
	// is smuggled in only via a hostile %3A, see didWebDocumentURL's doc
	// comment). This also catches every IPv6 literal spelling, since IPv6
	// addresses always contain ':'. Kept HERE, not in validateHostShape,
	// because a serviceEndpoint is a different animal: the AT Protocol
	// identity spec explicitly permits an "optional port number" there
	// (atchess-1c9.95 fix-pass, reviewer-flagged: reusing this rule
	// wholesale for ValidateFetchedEndpointURL rejected spec-legal,
	// self-hosted PDSes on a non-standard port for no SSRF-relevant
	// reason -- a port says nothing about whether a host is internal).
	if strings.ContainsRune(host, ':') {
		return fmt.Errorf("did:web host %q must not specify a port (and must not be an IPv6 literal)", host)
	}
	if err := validateHostShape(host); err != nil {
		return fmt.Errorf("did:web host %q is not a valid hostname: %w", host, err)
	}
	return nil
}

// validateHostShape is the SSRF-relevant host-shape analysis shared by
// validateDIDWebHost (a did:web IDENTIFIER's host segment, atchess-1c9.70/
// .72/.93 -- which additionally forbids a port, see validateDIDWebHost)
// and ValidateFetchedEndpointURL (a serviceEndpoint/OAuth-metadata URL's
// host, atchess-1c9.95 -- which permits a port, per the AT Protocol
// identity spec's "optional port number" for serviceEndpoint). Extracted
// as its own function during atchess-1c9.95's fix pass so this analysis is
// written ONCE and shared by both shapes, rather than validateDIDWebHost
// being reused wholesale for a shape (serviceEndpoint) it was never
// designed to validate -- that reuse rejected spec-legal, self-hosted
// PDSes on a non-standard port with no SSRF-relevant justification (a port
// says nothing about whether a HOST is internal).
//
// Rejects a host that:
//   - contains any byte outside the ASCII letter/digit/'-'/'.' allowlist
//     (atchess-1c9.93) -- see didWebHostCharRE's doc comment for why this
//     is a whitelist rather than yet another blacklist entry. This is also
//     what closes every IPv6 literal spelling here (url.URL.Hostname()
//     strips the "[...]" brackets net/url requires around an IPv6 host,
//     e.g. "[::1]" -> "::1", so ValidateFetchedEndpointURL never sees the
//     brackets -- but "::1" itself contains ':', which this charset
//     allowlist already rejects, so no bracket-specific handling is
//     needed at all -- see TestValidateHostShape_IPv6Literal);
//   - contains an empty label -- leading, trailing, or embedded
//     (consecutive dots), including a bare "." (atchess-1c9.72,
//     atchess-1c9.93);
//   - is an IP literal in ANY spelling (dotted-quad, hex, octal,
//     short-form, or IPv6) -- via the same rejectIPLiteralSpelling logic
//     normalizeAndValidateHandle uses for handles, so this SSRF-relevant
//     analysis is written once rather than drifting between two copies;
//   - carries userinfo ("@");
//   - contains a path separator ("/"), including one smuggled in via a
//     percent-encoded form that only becomes '/' after decoding (relevant
//     to validateDIDWebHost's did:web caller; ValidateFetchedEndpointURL
//     rejects a path on the URL itself before this ever runs).
//
// Deliberately NOT validated here: a port. See each caller for how it
// handles one.
func validateHostShape(host string) error {
	if host == "" {
		return fmt.Errorf("host is empty")
	}
	// atchess-1c9.93: an ALLOWLIST, not another blacklist entry. Two
	// bypasses in a row against this function (atchess-1c9.72's trailing
	// dot, and this one) got through because a guard declined for a reason
	// unrelated to the actual threat -- rejectIPLiteralSpelling's digit
	// test inspects a specific byte, and for a hostile-but-differently-
	// spelled input that byte simply wasn't '0'-'9'. Concretely:
	// net/http's Transport re-normalises the request's URL host via IDNA
	// (idnaASCII, called from canonicalAddr inside dialConn) immediately
	// before dialing -- AFTER this validation has already run and
	// approved the raw string -- and that normalisation maps fullwidth
	// digits (U+FF10-U+FF19) to ASCII digits and several fullwidth/CJK
	// full-stop code points (U+3002, U+FF0E, U+FF61) to '.'. So
	// "did:web:169.254.169.２５４" (fullwidth digits, no ASCII digit
	// anywhere in the raw string) is actually DIALED at
	// 169.254.169.254:443 -- the cloud metadata address -- with zero DNS
	// resolution involved (it is already an IP literal by the time it is
	// dialed), so this is exploitable in CGO_ENABLED=0 production builds,
	// unlike atchess-1c9.72's variant. Restricting the character set here
	// to ASCII letters, digits, '-', and '.' makes every non-ASCII
	// spelling illegal input before any of that later normalisation ever
	// runs, rather than trying to anticipate its next output shape. This
	// also makes the fix airtight against the specific mechanism: Go's
	// idnaASCII is a no-op identity function for any string that is
	// already pure ASCII (net/http/request.go: `if ascii.Is(v) { return
	// v, nil }`), so once a host passes this allowlist, net/http's later
	// re-normalisation is GUARANTEED to leave it byte-for-byte unchanged
	// before dialing -- there is no second normalisation pass left to
	// race. An internationalised host must carry its punycode ("xn--")
	// form instead -- pure ASCII, unaffected by this rule, and accepted
	// unchanged (see TestDIDWebIDNANormalizationBypass_PunycodeStillAccepted,
	// which asserts a punycode host reaches the DIALER -- not merely that
	// it passes validation, which would be a hollow regression guard).
	if !didWebHostCharRE.MatchString(host) {
		return fmt.Errorf("host %q contains a character outside the permitted ASCII letters, digits, '-', and '.'", host)
	}
	// Reject ANY empty label under one rule -- leading, trailing, or
	// embedded (consecutive dots) -- rather than a trailing-dot-only
	// special case: a bare "." is simultaneously a leading and trailing
	// empty label, "a..b" has an embedded one, and "example.com." has a
	// trailing one, and all three are the same underlying malformation.
	// This also closes atchess-1c9.72 (an IP-literal host, e.g.
	// "169.254.169.254.", slipping past validation because a trailing dot
	// made rejectIPLiteralSpelling's final label empty) without any
	// IP-specific handling: it is simply not a valid host shape, IP-looking
	// or not, and likewise closes the embedded-empty-label shape ("a..b")
	// noted during atchess-1c9.93's review.
	for _, label := range strings.Split(host, ".") {
		if label == "" {
			return fmt.Errorf("host %q contains an empty label (a leading, trailing, or consecutive dot)", host)
		}
	}
	if strings.ContainsRune(host, '@') {
		return fmt.Errorf("host %q must not contain userinfo (\"@\")", host)
	}
	if strings.ContainsRune(host, '/') {
		return fmt.Errorf("host %q must not contain a path separator", host)
	}
	if err := rejectIPLiteralSpelling(host); err != nil {
		return fmt.Errorf("host %q is not a valid hostname: %w", host, err)
	}
	return nil
}

// didWebHostCharRE matches only the byte set validateDIDWebHost permits in
// a did:web host: ASCII letters, digits, '-', and '.'. This is the
// atchess-1c9.93 allowlist -- see validateDIDWebHost's doc comment for the
// full rationale. Every non-ASCII byte is refused by construction,
// including the specific fullwidth-digit and fullwidth/CJK full-stop code
// points net/http's later IDNA re-normalisation (idnaASCII) would
// otherwise be free to reinterpret as a dotted-quad IP literal after this
// validation has already run.
var didWebHostCharRE = regexp.MustCompile(`^[a-zA-Z0-9.-]+$`)

// --- Handle resolution -----------------------------------------------------
//
// resolveHandleDNSTimeout/resolveHandleWellKnownTimeout bound each
// resolution attempt so a slow/unreachable DNS server or origin cannot hang
// a request indefinitely.
const (
	resolveHandleDNSTimeout       = 5 * time.Second
	resolveHandleWellKnownTimeout = 5 * time.Second
)

// handleLabelMaxLength/handleMaxLength bound how long a single dot-separated
// label of a handle -- and the handle as a whole -- may be, per the AT
// Protocol handle grammar (https://atproto.com/specs/handle), which mirrors
// RFC 1035's domain name label/name length limits.
const (
	handleLabelMaxLength = 63
	handleMaxLength      = 253
)

// handleLabelRE matches one dot-separated label of a valid AT Protocol
// handle: ASCII letters, digits and hyphens only, and it must not start or
// end with a hyphen (so a bare "-" label is also rejected, since it both
// starts and ends with one). Being anchored and restricted to this
// character class also means no non-ASCII byte, and none of '/', '@', ':',
// whitespace, or any other URL/authority-delimiting character, can ever
// pass this check.
var handleLabelRE = regexp.MustCompile(`^[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$`)

// normalizeAndValidateHandle validates handle against the AT Protocol
// handle grammar -- dot-separated labels of ASCII letters/digits/hyphens,
// at least two labels, each label <= 63 bytes, the whole handle
// <= 253 bytes, no scheme/port/userinfo/path characters, the FINAL label
// must not start with a digit, and no bare IP literal -- and returns it
// normalized to lowercase.
//
// The final-label-must-not-start-with-a-digit rule
// (https://atproto.com/specs/handle: "the final domain label must not
// start with a digit") is not cosmetic: it is what makes a bare IP literal
// -- in ANY of the forms a resolver's address parser might accept, not
// just dotted-quad -- structurally unable to satisfy the handle grammar at
// all, closing off a whole class of bypass that a dotted-quad-only
// net.ParseIP check (below) cannot catch. glibc's resolver (which cgo's
// name resolution uses, selected automatically by Go on many nsswitch
// configs) accepts numeric-address forms far looser than dotted-quad,
// e.g.: "169.254.169.0xfe" and "0xa9.0xfe.0xa9.0xfe" both resolve to
// 169.254.169.254 (the cloud metadata address), "127.1" and "0x7f.0.0.1"
// both resolve to 127.0.0.1, and "010.010.010.010" resolves to 8.8.8.8 (its
// octets are octal). Every one of those has a final label beginning with a
// digit, so this rule rejects all of them even though net.ParseIP does
// not recognize any of those forms as an IP and would let them through.
//
// This is the single choke point called from Client.ResolveHandle
// (atchess-1c9.69) before ANY resolution strategy runs. Every strategy --
// same-PDS resolveHandle (query param), DNS TXT lookup (hostname),
// HTTPS well-known (host + path), and the PLC export scan (match target) --
// is only ever invoked with the already-validated, already-normalized
// return value, so a hostile handle is rejected before it can steer any of
// their outbound host/path/DNS-query construction.
//
// Case handling: AT Protocol handles are explicitly case-insensitive and
// the spec says implementations should normalize to lowercase
// (https://atproto.com/specs/handle#normalization-and-validation). This
// function normalizes to lowercase rather than rejecting mixed-case input,
// so a handle typed/pasted as "Alice.Bsky.Social" still resolves instead of
// failing on a cosmetic difference no real client would surface to a user.
//
// A trailing dot (e.g. "alice.test.") is deliberately rejected rather than
// stripped: it splits into a trailing empty label, which the empty-label
// check below already catches, and this project takes no position on
// FQDN-style trailing-dot handles being equivalent to their non-dotted
// form -- treating them as invalid avoids that ambiguity entirely.
func normalizeAndValidateHandle(handle string) (string, error) {
	if handle == "" {
		return "", fmt.Errorf("invalid handle %q: empty", handle)
	}
	if len(handle) > handleMaxLength {
		return "", fmt.Errorf("invalid handle %q: exceeds maximum length of %d bytes", handle, handleMaxLength)
	}

	normalized := strings.ToLower(handle)

	labels := strings.Split(normalized, ".")
	if len(labels) < 2 {
		return "", fmt.Errorf("invalid handle %q: must have at least two dot-separated labels", handle)
	}
	for _, label := range labels {
		if label == "" {
			return "", fmt.Errorf("invalid handle %q: contains an empty label (consecutive dots, or a leading/trailing dot)", handle)
		}
		if len(label) > handleLabelMaxLength {
			return "", fmt.Errorf("invalid handle %q: label %q exceeds maximum length of %d bytes", handle, label, handleLabelMaxLength)
		}
		if !handleLabelRE.MatchString(label) {
			return "", fmt.Errorf("invalid handle %q: label %q must contain only ASCII letters, digits and hyphens, and must not start or end with a hyphen", handle, label)
		}
	}

	// Per the AT Protocol spec, the FINAL label must not start with an
	// ASCII digit -- and a handle that is syntactically a bare IP literal
	// would otherwise satisfy the label grammar above (digits are valid
	// label characters) while being exactly the SSRF-relevant shape this
	// validation exists to close off. See rejectIPLiteralSpelling's doc
	// comment for the full rationale and the glibc numeric-address examples
	// this closes off. Do not remove this thinking net.ParseIP alone is
	// sufficient.
	if err := rejectIPLiteralSpelling(normalized); err != nil {
		return "", fmt.Errorf("invalid handle %q: %w", handle, err)
	}

	return normalized, nil
}

// rejectIPLiteralSpelling returns a non-nil error if s could be interpreted
// by some resolver as a numeric IP address, in ANY spelling a real-world
// resolver might accept -- not just what net.ParseIP itself recognizes.
// Shared by normalizeAndValidateHandle (handles, atchess-1c9.69) and
// validateDIDWebHost (did:web hosts, atchess-1c9.70) so this SSRF-relevant
// analysis is written once instead of two copies that can silently drift
// apart.
//
// The final-label-must-not-start-with-a-digit check below
// (https://atproto.com/specs/handle: "the final domain label must not
// start with a digit") is not cosmetic: it is what makes a bare IP literal
// -- in ANY of the forms a resolver's address parser might accept, not
// just dotted-quad -- structurally unable to satisfy the handle grammar at
// all, closing off a whole class of bypass that a dotted-quad-only
// net.ParseIP check (below) cannot catch. glibc's resolver (which cgo's
// name resolution uses, selected automatically by Go on many nsswitch
// configs) accepts numeric-address forms far looser than dotted-quad,
// e.g.: "169.254.169.0xfe" and "0xa9.0xfe.0xa9.0xfe" both resolve to
// 169.254.169.254 (the cloud metadata address), "127.1" and "0x7f.0.0.1"
// both resolve to 127.0.0.1, and "010.010.010.010" resolves to 8.8.8.8 (its
// octets are octal). Every one of those has a final label beginning with a
// digit, so this rule rejects all of them even though net.ParseIP does
// not recognize any of those forms as an IP and would let them through.
//
// net.ParseIP is still checked too, as a belt-and-braces guard for plain
// dotted-quad/IPv6 literals that (for a handle) would already be caught by
// the label-character regexp or the final-label rule above in every case
// this project has identified -- kept as defense in depth in case that
// ever stops being true for some caller of this shared helper.
func rejectIPLiteralSpelling(s string) error {
	labels := strings.Split(s, ".")
	finalLabel := labels[len(labels)-1]
	if finalLabel != "" && finalLabel[0] >= '0' && finalLabel[0] <= '9' {
		return fmt.Errorf("final label %q must not start with a digit (a numeric-address spelling in some resolvers)", finalLabel)
	}
	if net.ParseIP(s) != nil {
		return fmt.Errorf("%q is a bare IP literal", s)
	}
	return nil
}

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
