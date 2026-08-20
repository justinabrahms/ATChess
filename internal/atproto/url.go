package atproto

import (
	"fmt"
	"net/url"
	"strings"
)

// xrpcURL joins a PDS base URL and an XRPC method name into a full request
// URL, optionally appending a URL-encoded query string built from params.
//
// This exists because the AT Protocol PDS shapes ATChess must talk to are
// NOT uniform:
//   - the local dual-PDS test harness (@atproto/pds) can only ever emit
//     "http://localhost:<port>" -- any non-localhost hostname forces the PDS
//     into "https://<host>" with NO port.
//   - real Bluesky PDSes are "https://<host>" with no port (e.g.
//     "https://shiitake.us-east.host.bsky.network").
//
// xrpcURL performs no scheme/port normalisation of its own: base is used
// verbatim (after stripping any trailing slashes, so callers don't have to
// care whether their configured base URL ends in "/"). It does not assume a
// port is present, and does not upgrade/downgrade scheme.
//
// When params is nil or empty, the returned URL has no "?" suffix at all,
// which keeps this helper byte-identical to the historical
// fmt.Sprintf("%s/xrpc/%s", base, method) call sites it replaces.
func xrpcURL(base, method string, params url.Values) string {
	joined := strings.TrimRight(base, "/") + "/xrpc/" + method
	if len(params) == 0 {
		return joined
	}
	return joined + "?" + params.Encode()
}

// DIDDocument is a minimal representation of an AT Protocol DID document --
// just enough to locate the subject's PDS endpoint. See
// https://atproto.com/specs/did for the full shape.
type DIDDocument struct {
	ID      string       `json:"id"`
	Service []DIDService `json:"service"`
}

// DIDService is one entry of a DID document's "service" array.
type DIDService struct {
	ID              string `json:"id"`
	Type            string `json:"type"`
	ServiceEndpoint string `json:"serviceEndpoint"`
}

// atprotoPDSServiceType is the "type" value the AT Protocol spec assigns to
// a DID document's personal-data-server service entry.
const atprotoPDSServiceType = "AtprotoPersonalDataServer"

// parseServiceEndpoint extracts the PDS serviceEndpoint from a DID document
// by finding the service entry whose type is "AtprotoPersonalDataServer",
// per the AT Protocol identity spec. It performs no NORMALISATION of the
// returned endpoint (no scheme/port coercion, trailing slash left as-is) --
// callers are responsible for deciding how to join it with an XRPC method
// (see xrpcURL) -- but it DOES VALIDATE it via ValidateFetchedEndpointURL
// before returning (atchess-1c9.95, found by reviewer during atchess-1c9.94):
// every did:web/did:plc/handle host control this package built
// (atchess-1c9.69/.70/.72/.93/.94) validates only the host we deliberately
// look UP; none of them validated the VALUE a lookup HANDS BACK, so a
// hostile DID document could simply declare e.g.
// "http://169.254.169.254/" as its serviceEndpoint and have it dialed
// directly by every caller of resolvePDS/ResolvePDS -- no exotic host
// spelling or redirect required, just a value an attacker gets to write
// into a document ATChess fetches and trusts.
func parseServiceEndpoint(doc *DIDDocument) (string, error) {
	if doc == nil {
		return "", fmt.Errorf("parseServiceEndpoint: nil DID document")
	}
	for _, svc := range doc.Service {
		if svc.Type != atprotoPDSServiceType {
			continue
		}
		if svc.ServiceEndpoint == "" {
			return "", fmt.Errorf("parseServiceEndpoint: %s service entry has empty serviceEndpoint", atprotoPDSServiceType)
		}
		if _, err := ValidateFetchedEndpointURL(svc.ServiceEndpoint); err != nil {
			return "", fmt.Errorf("parseServiceEndpoint: %s service entry: %w", atprotoPDSServiceType, err)
		}
		return svc.ServiceEndpoint, nil
	}
	return "", fmt.Errorf("parseServiceEndpoint: no %s service entry found in DID document %q", atprotoPDSServiceType, doc.ID)
}

// ValidateFetchedEndpointURL validates rawURL -- a URL taken from a
// document or response body this codebase does not control (a DID
// document's serviceEndpoint, or an OAuth resource-/authorization-server
// metadata value such as internal/web's getAuthorizationServer/
// getAuthServerMetadata) -- before it is ever dialed (atchess-1c9.95).
//
// Every existing host control in this package (atchess-1c9.69's handle
// grammar, .70's did:web host validation, .72's empty-label rule, .93's
// ASCII allowlist, .94's redirect refusal) guards the host ATChess
// deliberately chooses to look up. None of them guards the VALUE a lookup
// hands back -- so a hostile did:web/did:plc document (or a forged OAuth
// callback "iss"/response body) could simply DECLARE an internal address
// (e.g. a cloud metadata IP, or loopback) and have it dialed directly, with
// no exotic spelling or redirect needed at all.
//
// Requires:
//   - a parseable absolute URL;
//   - scheme "https". No exception is made HERE for a "local/dev" value:
//     this validator must only ever run on a value FETCHED from an
//     untrusted document/response. An OPERATOR-CONFIGURED value (e.g.
//     config.ATProtoConfig.PDSURL, which legitimately is
//     "http://localhost:3000" for the single-PDS dev stack, or the
//     dual-PDS test harness's account.PDSURL) must never be routed through
//     this function at all -- it is used directly as a Client's own pdsURL
//     (see resolveReadEndpoint's c.did shortcut), never extracted from a
//     document this codebase fetched. Drawing that line at the CALL SITE
//     (not by carving a "local" exception into this function) is
//     deliberate: an exception coded in here would apply to every caller,
//     including the ones -- parseServiceEndpoint, getAuthorizationServer,
//     getAuthServerMetadata -- for which the whole point is that the value
//     is attacker-influenced;
//   - no embedded userinfo ("user:pass@host");
//   - no query string or fragment;
//   - no path other than "" or "/" -- xrpcURL's join
//     (strings.TrimRight(base, "/") + "/xrpc/" + method[+ "?" + query])
//     assumes base is a bare origin; any other path would either be
//     silently discarded by that join or (worse) redirect the request to
//     an attacker-chosen path on an otherwise-legitimate host;
//   - a HOSTNAME (port stripped first, see below) that passes
//     validateHostShape -- the SAME shape analysis atchess-1c9.69/.70/.72/
//     .93 built for a did:web host (ASCII letters/digits/'-'/'.' only, no
//     empty label, no userinfo, no IP literal in ANY spelling, no path
//     separator), reused rather than reimplemented so this SSRF-relevant
//     analysis is written once. Unlike validateDIDWebHost (which ALSO
//     forbids a port, because a did:web host is part of a DID IDENTIFIER
//     with no port component in its grammar), a port is explicitly
//     permitted here: the AT Protocol identity spec
//     (https://atproto.com/specs/did#service-endpoints) says a
//     serviceEndpoint "must contain only the URI scheme (http or https),
//     hostname, and optional port number" -- so
//     "https://pds.example.com:8443" is spec-legal and must resolve,
//     matching a real self-hosted PDS on a non-standard port. u.Port() is
//     accepted whenever it is present at all: url.Parse itself already
//     rejects a non-numeric port ("invalid port %q after host") before
//     this function ever runs, so there is nothing further to validate --
//     a port says nothing about whether a HOST is internal, which is what
//     validateHostShape (via rejectIPLiteralSpelling) actually guards
//     against. u.Hostname() (not u.Host) is what gets validated so this
//     works correctly for both a plain host ("pds.example.com") and a
//     bracketed IPv6 literal ("[::1]", which Hostname() strips to "::1" --
//     containing ':', already rejected by validateHostShape's charset
//     allowlist, so this closes IPv6 literals with no bracket-specific
//     code at all; see TestValidateFetchedEndpointURL_IPv6LiteralRejected).
//
// Returns the parsed *url.URL on success, so a caller that needs the bare
// origin (e.g. getAuthServerMetadata) can use it directly rather than
// re-parsing.
func ValidateFetchedEndpointURL(rawURL string) (*url.URL, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid endpoint URL %q: %w", rawURL, err)
	}
	if u.Scheme != "https" {
		return nil, fmt.Errorf("invalid endpoint URL %q: scheme must be https, got %q", rawURL, u.Scheme)
	}
	if u.User != nil {
		return nil, fmt.Errorf("invalid endpoint URL %q: must not contain userinfo", rawURL)
	}
	if u.RawQuery != "" {
		return nil, fmt.Errorf("invalid endpoint URL %q: must not contain a query string", rawURL)
	}
	if u.Fragment != "" {
		return nil, fmt.Errorf("invalid endpoint URL %q: must not contain a fragment", rawURL)
	}
	if u.Path != "" && u.Path != "/" {
		return nil, fmt.Errorf("invalid endpoint URL %q: must not contain a path", rawURL)
	}
	if err := validateHostShape(u.Hostname()); err != nil {
		return nil, fmt.Errorf("invalid endpoint URL %q: %w", rawURL, err)
	}
	return u, nil
}
