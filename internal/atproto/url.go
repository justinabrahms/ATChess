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
// per the AT Protocol identity spec. It performs no normalisation of the
// returned endpoint (no scheme/port coercion, trailing slash left as-is) --
// callers are responsible for deciding how to join it with an XRPC method
// (see xrpcURL).
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
		return svc.ServiceEndpoint, nil
	}
	return "", fmt.Errorf("parseServiceEndpoint: no %s service entry found in DID document %q", atprotoPDSServiceType, doc.ID)
}
