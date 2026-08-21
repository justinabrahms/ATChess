package web

// Conformance test (originally atchess-1c9.7, moved here by atchess-1c9.83)
// that checks our OAuth client against what a real AT Protocol
// authorization server actually requires. Network-gated behind
// ATCHESS_OAUTH_CONFORMANCE=1 and skipped by default so ordinary unit runs
// and CI stay green.
//
// History: atchess-1c9.7 originally landed this in internal/oauth,
// reconstructing the handle -> DID -> PDS -> resource-server ->
// authorization-server resolution chain with local re-implementations of
// every hop except DID -> PDS (atproto.ResolvePDS), because the production
// hops for the last two steps (getAuthorizationServer,
// getAuthorizationEndpoint as it was then named) are unexported methods on
// web.Service, and internal/oauth's test files cannot import internal/web
// without an import cycle (web already imports oauth). atchess-1c9.83
// diagnosed that gap: only 1 of 4 hops ran production code. This file
// fixes that by living IN package web instead, where the unexported
// resolution helpers are directly callable:
//
//   - handle -> DID: atproto.NewClientFromSession + a stub Authenticator
//     (resolveHandleToDIDConformance below), NOT atproto.NewClient. An
//     earlier version of this file claimed handle -> DID "cannot be
//     reached without credentials, full stop" -- that was wrong.
//     NewClientFromSession is exported and takes an Authenticator
//     INTERFACE; it performs no login at construction, it just calls
//     auth.Token() once, which a stub can satisfy with a junk string. The
//     resulting *Client's ResolveHandle then runs production code for
//     real: VERIFIED (both via this stub and via a direct curl with a
//     garbage Authorization header) that bsky.social's
//     com.atproto.identity.resolveHandle does not validate the bearer
//     token for this call at all -- it is a public/unauthenticated XRPC
//     query, so the junk token is simply ignored and ResolveHandle's
//     FIRST strategy (same-PDS resolveHandle) succeeds directly. This
//     hop therefore exercises production's PRIMARY resolution strategy,
//     not a fallback -- see resolveHandleToDIDConformance's doc comment
//     for the verification detail. (An earlier draft of this comment
//     assumed the stub token would cause a 401 here and fall through to
//     the DNS-TXT strategy; that assumption was never actually confirmed
//     and turned out to be wrong -- corrected after directly probing
//     both resolveHandleSamePDS and a raw curl request.)
//   - DID -> PDS: atproto.ResolvePDS, the exact function
//     resolveOAuthEndpoints (oauth_handlers.go) calls for this hop.
//   - PDS -> resource-server metadata (authorization server URL):
//     (&Service{}).getAuthorizationServer(pdsURL) -- a zero-value Service
//     works today because this method happens not to dereference any
//     Service field, not because that is a documented contract -- see the
//     call site below for the caveat that matters if that ever changes.
//   - resource-server -> authorization-server metadata: getAuthServerMetadata
//     (a free function, no Service needed), which -- since atchess-1c9.12 --
//     already decodes and keeps the PAR-related fields (PushedAuthorization-
//     RequestEndpoint, RequirePushedAuthorizationRequests) alongside
//     AuthorizationEndpoint and TokenEndpoint, so this test reads them from
//     the SAME struct production code uses rather than a local copy.
//
// So all 4 hops now run production code end to end, each via its primary
// strategy (see above for why the handle -> DID hop is the primary
// same-PDS strategy, not a fallback). The OAuth client itself is also now
// built via the real production constructor, oauth.NewOAuthClient (see
// newConformanceOAuthClient below) rather than an unexported struct
// literal.
//
// If bsky.social's identity, PDS, or handle changes, update
// conformanceTestHandle below.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/justinabrahms/atchess/internal/atproto"
	"github.com/justinabrahms/atchess/internal/oauth"
)

// conformanceTestHandle is a public bsky.social-hosted handle used only to
// exercise the resolution chain below. No credentials of any kind are
// needed or used -- everything fetched here is public metadata.
const conformanceTestHandle = "bsky.app"

func requireOAuthConformanceGate(t *testing.T) {
	t.Helper()
	if os.Getenv("ATCHESS_OAUTH_CONFORMANCE") != "1" {
		t.Skip("skipping network-gated OAuth conformance test; set ATCHESS_OAUTH_CONFORMANCE=1 to run it (requires live network access to bsky.social)")
	}
}

// junkTokenAuthenticator is an atproto.Authenticator stub that hands back a
// fixed, invalid bearer token and never actually authenticates anything.
// It exists ONLY so atproto.NewClientFromSession -- an exported
// constructor that performs no login of its own, unlike atproto.NewClient
// -- can be used here without real credentials. Token()/ForceRefresh()
// both return the same junk string; there is nothing to refresh to,
// because there was never a real session.
type junkTokenAuthenticator struct{}

func (junkTokenAuthenticator) Token() (string, error)        { return "junk-not-a-real-token", nil }
func (junkTokenAuthenticator) ForceRefresh() (string, error) { return "junk-not-a-real-token", nil }

// resolveHandleToDIDConformance resolves handle -> DID via production's
// actual *atproto.Client.ResolveHandle, built with
// atproto.NewClientFromSession + junkTokenAuthenticator{} rather than
// atproto.NewClient (which performs a real password login at
// construction, and which this test must not use). NewClientFromSession
// takes an Authenticator INTERFACE and only ever calls Token() on it, so
// no credentials are needed or used here.
//
// VERIFIED (2026-08, both by calling resolveHandleSamePDS directly with
// this stub and by a raw curl carrying a garbage Authorization header)
// against the live server: bsky.social's
// com.atproto.identity.resolveHandle does NOT validate the bearer token
// for this call -- it is treated as a public/unauthenticated XRPC query,
// so the junk token here is simply ignored and ResolveHandle's FIRST
// strategy, same-PDS resolveHandle, succeeds directly and returns
// conformanceTestHandle's real DID. This hop therefore exercises
// production's PRIMARY resolution strategy, not a fallback. Do not
// reintroduce a claim that this hop falls through to DNS/well-known/PLC
// export without re-verifying -- an earlier draft of this comment assumed
// exactly that, unverified, and was wrong; see the file doc comment above
// for the correction.
func resolveHandleToDIDConformance(ctx context.Context, pdsURL, handle string) (string, error) {
	c, err := atproto.NewClientFromSession(pdsURL, "", "", false, nil, junkTokenAuthenticator{})
	if err != nil {
		return "", fmt.Errorf("constructing session client: %w", err)
	}
	return c.ResolveHandle(ctx, handle)
}

// newConformanceOAuthClient builds a real *oauth.OAuthClient via the
// production constructor, oauth.NewOAuthClient -- not an unexported struct
// literal -- backed by a freshly generated EC key supplied through
// OAUTH_PRIVATE_KEY (so no OAUTH_PRIVATE_KEY_PATH file is needed on disk;
// same technique as setUpOAuthGlobalsForTest in
// oauth_pds_resolution_test.go, duplicated here because that helper's
// fixed client_id is unsuitable for this test -- see call site). Uses a
// "*.example.com" client_id/redirect_uri: unlike ".test", bsky.social's
// live authorization server does not reject ".com" outright at the
// client_id-format check, letting the request reach a client_id-specific
// decision (invalid_client_metadata / "Forbidden hostname" for
// example.com) instead -- see the call site for exactly what that does
// and does not prove.
func newConformanceOAuthClient(t *testing.T) *oauth.OAuthClient {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating conformance OAuth key: %v", err)
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshaling conformance OAuth key: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})

	oldEnv, hadEnv := os.LookupEnv("OAUTH_PRIVATE_KEY")
	os.Setenv("OAUTH_PRIVATE_KEY", string(pemBytes))
	t.Cleanup(func() {
		if hadEnv {
			os.Setenv("OAUTH_PRIVATE_KEY", oldEnv)
		} else {
			os.Unsetenv("OAUTH_PRIVATE_KEY")
		}
	})

	client, err := oauth.NewOAuthClient("https://atchess.example.com/client-metadata.json", "https://atchess.example.com/api/callback")
	if err != nil {
		t.Fatalf("oauth.NewOAuthClient: %v", err)
	}
	return client
}

// TestConformance_PARIsAttemptedWhenRequired verifies, against a live
// authorization server that advertises Pushed Authorization Request (PAR)
// support, that our client actually attempts PAR when it should --
// exercising BuildAuthorizationURLAuto (atchess-1c9.12's production entry
// point), not the plain BuildAuthorizationURL, which must never perform PAR
// (some authorization servers, including the local dual-PDS harness's,
// advertise no PAR endpoint at all, so a non-PAR path is a hard
// requirement, not a gap).
//
// This test cannot complete a full PAR round trip: a live authorization
// server's PAR endpoint requires fetching a resolvable client-metadata.json
// for our client_id, and hosting real, live client metadata for a test
// fixture would make a unit test depend on live infrastructure. Instead it
// grades whether a well-formed PAR request reached a DEFINITIVE response
// from the authorization server:
//
//   - "invalid_client_metadata" is accepted as proof-of-attempt: it proves
//     the AS accepted our client_id, DPoP proof and PAR request body as
//     well-formed enough to reach a client_id-specific decision, rejecting
//     only on the (deliberately unresolvable/blocklisted) client metadata
//     host -- see the call site below for why this does NOT necessarily
//     mean an outbound client-metadata.json fetch was actually issued, and
//     is exactly as far as we can prove without hosting real metadata.
//   - "use_dpop_nonce" surfacing here is a FAILURE: postFormWithDPoPRetry
//     is supposed to retry exactly once against a 400 use_dpop_nonce
//     challenge and only return that error if the second attempt is
//     ALSO challenged (or the server broke the challenge contract by
//     omitting DPoP-Nonce) -- either way, seeing it here means our nonce
//     retry mechanics did not do their job.
//   - anything else (no PAR attempt at all, a network-level failure before
//     any AS response, or a request the AS rejected for some earlier
//     reason than the client-metadata fetch) is also a FAILURE.
//
// This test was previously named TestConformance_PARRequiredButNotUsed and
// asserted (against the PLAIN builder) that PAR was NOT used -- an
// assertion atchess-1c9.7 correctly pinned as a known gap, which
// atchess-1c9.12 then closed by adding BuildAuthorizationURLAuto. But
// because that assertion targeted the plain builder specifically, and the
// plain builder must never do PAR, it could never be made to pass by any
// production change; see atchess-1c9.85 for the full diagnosis. It has been
// restructured (that comment) to grade the real PAR path instead, and
// atchess-1c9.83 then moved it here (from internal/oauth) so the resolution
// chain below runs production code instead of local copies -- see this
// file's doc comment.
func TestConformance_PARIsAttemptedWhenRequired(t *testing.T) {
	requireOAuthConformanceGate(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// handle -> DID, via production's *atproto.Client.ResolveHandle (its
	// primary same-PDS strategy) -- see file doc comment.
	did, err := resolveHandleToDIDConformance(ctx, "https://bsky.social", conformanceTestHandle)
	if err != nil {
		t.Fatalf("resolving handle %q to a DID: %v", conformanceTestHandle, err)
	}

	// DID -> PDS, via the actual exported function production code
	// (resolveOAuthEndpoints) calls for this hop.
	pdsURL, err := atproto.ResolvePDS(ctx, did, "")
	if err != nil {
		t.Fatalf("resolving PDS for %s: %v", did, err)
	}

	// PDS -> authorization server, via the actual production method
	// (resolveOAuthEndpoints calls this same method on the real Service).
	// A zero-value &Service{} works here because getAuthorizationServer
	// happens not to dereference any Service field TODAY -- that is not a
	// documented contract of the method, just its current implementation.
	// If a future change adds a field read there, this call needs a
	// properly-constructed Service (or the field needs to become
	// injectable), not a silent nil-dereference panic here.
	authServerURL, err := (&Service{}).getAuthorizationServer(pdsURL)
	if err != nil {
		t.Fatalf("resolving authorization server for PDS %s: %v", pdsURL, err)
	}

	// authorization server -> full metadata, via the actual production
	// function. getAuthServerMetadata keeps the PAR-related fields (unlike
	// the pre-atchess-1c9.12 getAuthorizationEndpoint it replaced), so this
	// test reads them from the same struct resolveOAuthEndpoints does.
	metadata, err := getAuthServerMetadata(authServerURL)
	if err != nil {
		t.Fatalf("fetching authorization server metadata from %s: %v", authServerURL, err)
	}

	parAdvertised := metadata.PushedAuthorizationRequestEndpoint != "" || metadata.RequirePushedAuthorizationRequests
	if !parAdvertised {
		t.Fatalf("test precondition failed: %s no longer advertises PAR support "+
			"(pushed_authorization_request_endpoint=%q, require_pushed_authorization_requests=%v) -- "+
			"this test needs updating, not the production client",
			authServerURL, metadata.PushedAuthorizationRequestEndpoint, metadata.RequirePushedAuthorizationRequests)
	}

	// Build a real client the same way production does (via
	// oauth.NewOAuthClient, the exported constructor -- not an unexported
	// struct literal) and see what it actually sends. Deliberately NOT
	// setUpOAuthGlobalsForTest's fixture client (oauth_pds_resolution_test.go):
	// that one uses a "*.example.test" client_id, and bsky.social's live
	// authorization server rejects ".test" TLDs outright
	// (invalid_client_id: "The client_id's TLD must not be a local
	// hostname") -- a check that runs before client_id is even used to
	// look anything up, so it proves nothing about PAR handling.
	// "*.example.com" instead gets past that check and is rejected later,
	// with invalid_client_metadata / "Forbidden hostname" -- a HOST-POLICY
	// refusal of "example.com" specifically (it looks like a blocklisted
	// placeholder domain, checked before any outbound fetch is even
	// attempted, not a DNS/connect failure from actually trying to fetch
	// it). What this DOES establish: client_id validation, the DPoP proof,
	// and the PAR request body all passed well-formedness checks and were
	// evaluated far enough to reach a client_id-specific decision --
	// that is the proof-of-attempt this test grades. It does NOT establish
	// that a client-metadata.json fetch was actually issued.
	client := newConformanceOAuthClient(t)

	verifier, challenge, err := oauth.GeneratePKCE()
	if err != nil {
		t.Fatalf("generating PKCE: %v", err)
	}
	_ = verifier
	state, err := oauth.GenerateState()
	if err != nil {
		t.Fatalf("generating state: %v", err)
	}

	// The same per-session DPoP key production generates in
	// internal/web/oauth_handlers.go before calling BuildAuthorizationURLAuto.
	dpopKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating DPoP key: %v", err)
	}

	// This is the actual production entry point (see
	// internal/web/oauth_handlers.go): it selects PAR because parEndpoint
	// (metadata.PushedAuthorizationRequestEndpoint) is non-empty, and
	// PushAuthorizationRequest actually POSTs to the live PAR endpoint.
	authURL, buildErr := client.BuildAuthorizationURLAuto(
		metadata.AuthorizationEndpoint, metadata.PushedAuthorizationRequestEndpoint,
		authServerURL, conformanceTestHandle, state, challenge, dpopKey,
		metadata.RequirePushedAuthorizationRequests)

	switch {
	case buildErr == nil:
		// A full PAR round trip succeeded outright (e.g. the AS doesn't
		// actually enforce resolving client_id's metadata for this
		// request). That's strictly stronger proof of a correct PAR
		// attempt than what we normally expect to observe here -- pass.
		parsed, perr := url.Parse(authURL)
		if perr != nil {
			t.Fatalf("BuildAuthorizationURLAuto produced an unparseable URL: %v", perr)
		}
		if parsed.Query().Get("request_uri") == "" {
			t.Errorf("BuildAuthorizationURLAuto returned no error but the resulting authorization URL has no "+
				"request_uri, meaning it silently fell back to the plain (non-PAR) builder even though a PAR "+
				"endpoint was advertised: %s", authURL)
			break
		}
		t.Logf("PASS via FULL PAR ROUND TRIP: %s accepted the request and returned a request_uri. Note this is "+
			"NOT the branch normally expected here -- it means the AS no longer rejects our unresolvable fixture "+
			"client_id, so the proof-of-attempt path below has stopped being exercised.",
			metadata.PushedAuthorizationRequestEndpoint)
	case strings.Contains(buildErr.Error(), "use_dpop_nonce"):
		// postFormWithDPoPRetry is supposed to retry exactly once against
		// a 400 use_dpop_nonce challenge and succeed (or fail for an
		// unrelated reason) on the retry. Seeing use_dpop_nonce escape
		// all the way out here means that retry did not do its job.
		t.Errorf("PAR request to %s was challenged with use_dpop_nonce and never got past it -- our DPoP "+
			"nonce retry mechanics did not work: %v", metadata.PushedAuthorizationRequestEndpoint, buildErr)
	case strings.Contains(buildErr.Error(), "invalid_client_metadata"):
		// Proof-of-attempt: the authorization server accepted our PAR
		// POST -- client_id, DPoP proof and request body -- as well-formed
		// enough to reach a client_id-specific decision, and rejected it
		// only on that decision (our fixture client_id's host is
		// deliberately unresolvable/blocklisted). That's as far as this
		// test can prove without hosting real, live client metadata --
		// which would make a unit fixture depend on live infrastructure.
		// This is a PASS. NOTE: this does not prove the AS actually issued
		// an outbound fetch to the client_id host -- "Forbidden hostname"
		// reads like a policy check against the host itself (e.g. a
		// blocklisted placeholder domain), which could equally be applied
		// before any network fetch is attempted. Either way, everything
		// UP TO client-metadata resolution was accepted, which is the
		// proof this test needs.
		//
		// Logged rather than silent: this branch and the err==nil branch
		// above are both passes but record materially different facts about
		// the live server. Without a log line a change in the AS's behaviour
		// would flip which one fires and nobody would notice.
		t.Logf("PASS via PROOF-OF-ATTEMPT: PAR POST to %s was well-formed and reached client-metadata "+
			"resolution, rejected only because the fixture client_id's host is deliberately unresolvable: %v",
			metadata.PushedAuthorizationRequestEndpoint, buildErr)
	default:
		t.Errorf("PAR request to %s (advertised by authorization server %s, "+
			"require_pushed_authorization_requests=%v) did not reach the expected definitive response -- "+
			"either PAR was never attempted, or the authorization server rejected the request for a reason "+
			"other than fetching our client metadata: %v",
			metadata.PushedAuthorizationRequestEndpoint, authServerURL,
			metadata.RequirePushedAuthorizationRequests, buildErr)
	}
}
