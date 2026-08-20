package web

// Tests for atchess-1c9.84: session.PDSURL must be the user's REAL PDS
// host, resolved from their DID document (via internal/atproto.ResolvePDS,
// reused rather than reimplemented), not assumed from the OAuth issuer
// (iss). bsky.social is the motivating counterexample: iss is
// https://bsky.social (the entryway, acting as authorization server) while
// the user's repo lives on a separate *.host.bsky.network PDS.
//
// These drive Service.OAuthCallbackHandler end to end (minus real network:
// every remote endpoint -- the issuer's authorization-server metadata/token
// endpoint and the PLC directory -- is a local httptest.Server) so a
// regression that reverts `PDSURL: pdsURL` back to `PDSURL: iss` inside the
// handler is caught here, not just in a unit test of the extracted resolver
// helper.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/justinabrahms/atchess/internal/atproto"
	"github.com/justinabrahms/atchess/internal/config"
	"github.com/justinabrahms/atchess/internal/oauth"
)

// setUpOAuthGlobalsForTest initializes the package-level oauthClient,
// sessionStore and authStore globals OAuthCallbackHandler reads directly
// (see real_handler_unauthenticated_test.go / auth_middleware_test.go for
// the same save-global/defer-restore pattern used elsewhere in this
// package), backed by a freshly generated EC key so oauth.NewOAuthClient
// doesn't need a real OAUTH_PRIVATE_KEY_PATH file on disk.
func setUpOAuthGlobalsForTest(t *testing.T) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating test OAuth key: %v", err)
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshaling test OAuth key: %v", err)
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

	client, err := oauth.NewOAuthClient("https://client.example.test/client-metadata.json", "https://client.example.test/api/callback")
	if err != nil {
		t.Fatalf("oauth.NewOAuthClient: %v", err)
	}

	oldClient, oldSessions, oldAuth := oauthClient, sessionStore, authStore
	oauthClient = client
	sessionStore = oauth.NewSessionStore()
	authStore = oauth.NewAuthorizationStore()
	t.Cleanup(func() {
		oauthClient, sessionStore, authStore = oldClient, oldSessions, oldAuth
	})
}

// fakeIssuer stands in for an OAuth issuer/authorization server: it serves
// /.well-known/oauth-authorization-server (pointing back at its own /token
// endpoint) and /token (returning a fixed, successful token response whose
// "sub" is did). Neither endpoint performs any real DPoP/client-assertion
// verification -- OAuthCallbackHandler's PDS-resolution behavior is what's
// under test here, not OAuthClient's token-exchange wire format (that's
// covered elsewhere, e.g. internal/oauth's own tests).
func fakeIssuer(t *testing.T, did string) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/.well-known/oauth-authorization-server":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{ //nolint:errcheck
				"token_endpoint": srv.URL + "/token",
			})
		case r.Method == http.MethodPost && r.URL.Path == "/token":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(oauth.TokenResponse{ //nolint:errcheck
				AccessToken:  "test-access-token",
				TokenType:    "DPoP",
				ExpiresIn:    3600,
				RefreshToken: "test-refresh-token",
				Scope:        "atproto transition:generic",
				Sub:          did,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	return srv
}

// fakePLCDirectory stands in for a did:plc directory: it serves the DID
// document at GET /{did}, with a single #atproto_pds service entry pointing
// at pdsHost.
func fakePLCDirectory(t *testing.T, did, pdsHost string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/"+did {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(atproto.DIDDocument{ //nolint:errcheck
			ID: did,
			Service: []atproto.DIDService{
				{ID: "#atproto_pds", Type: "AtprotoPersonalDataServer", ServiceEndpoint: pdsHost},
			},
		})
	}))
}

// driveOAuthCallback stores a matching AuthorizationRequest for state, then
// invokes s.OAuthCallbackHandler with code/state/iss as an OAuth callback
// redirect would supply them, returning the ResponseRecorder for the
// caller to inspect.
func driveOAuthCallback(t *testing.T, s *Service, iss string) *httptest.ResponseRecorder {
	t.Helper()

	state, err := oauth.GenerateState()
	if err != nil {
		t.Fatalf("oauth.GenerateState: %v", err)
	}
	verifier, _, err := oauth.GeneratePKCE()
	if err != nil {
		t.Fatalf("oauth.GeneratePKCE: %v", err)
	}
	dpopKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating DPoP key: %v", err)
	}
	authStore.StoreAuthorization(&oauth.AuthorizationRequest{
		State:        state,
		CodeVerifier: verifier,
		Handle:       "alice.test",
		CreatedAt:    time.Now(),
		DPoPKey:      dpopKey,
	})

	target := "/api/callback?" + url.Values{
		"code":  {"test-auth-code"},
		"state": {state},
		"iss":   {iss},
	}.Encode()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rr := httptest.NewRecorder()
	s.OAuthCallbackHandler(rr, req)
	return rr
}

// sessionIDFromRedirect extracts the "session" query parameter from a
// "/?session=<id>" Location header, as set by a successful
// OAuthCallbackHandler redirect.
func sessionIDFromRedirect(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	loc := rr.Header().Get("Location")
	if loc == "" {
		t.Fatalf("expected a Location header on redirect, got none (status %d, body %q)", rr.Code, rr.Body.String())
	}
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("parsing Location header %q: %v", loc, err)
	}
	id := u.Query().Get("session")
	if id == "" {
		t.Fatalf("Location header %q had no session query parameter", loc)
	}
	return id
}

// TestOAuthCallback_PDSURLResolvedFromDIDDocument_NotIssuer is the direct
// regression test for atchess-1c9.84: the issuer and the user's real PDS
// are DIFFERENT hosts (as on bsky.social), and session.PDSURL must end up
// holding the resolved PDS, not iss.
func TestOAuthCallback_PDSURLResolvedFromDIDDocument_NotIssuer(t *testing.T) {
	setUpOAuthGlobalsForTest(t)

	const did = "did:plc:alice1234567"
	const resolvedPDS = "https://alice.host.bsky.network"

	issuer := fakeIssuer(t, did)
	defer issuer.Close()
	plc := fakePLCDirectory(t, did, resolvedPDS)
	defer plc.Close()

	if issuer.URL == resolvedPDS {
		t.Fatal("test setup bug: issuer and resolved PDS must differ")
	}

	s := &Service{config: &config.Config{ATProto: config.ATProtoConfig{PLCDirectoryURL: plc.URL}}}

	rr := driveOAuthCallback(t, s, issuer.URL)
	if rr.Code != http.StatusFound {
		t.Fatalf("expected 302 redirect, got %d: %s", rr.Code, rr.Body.String())
	}

	sessionID := sessionIDFromRedirect(t, rr)
	session, err := sessionStore.GetSession(sessionID)
	if err != nil {
		t.Fatalf("expected the created session to be retrievable: %v", err)
	}

	if session.PDSURL != resolvedPDS {
		t.Errorf("session.PDSURL = %q, want the resolved PDS %q (NOT the issuer %q)", session.PDSURL, resolvedPDS, issuer.URL)
	}
	if session.PDSURL == issuer.URL {
		t.Errorf("session.PDSURL equals the issuer (%q) -- this is exactly the atchess-1c9.84 bug: it must be the resolved PDS instead", issuer.URL)
	}
}

// TestOAuthCallback_PDSResolutionFailure_FailsLoginNoSession covers PLC
// unreachable / DID document has no #atproto_pds entry (fakePLCDirectory's
// 404-for-any-other-DID branch stands in for either): login must fail
// closed, with NO session created -- not a session recorded with iss as a
// silent fallback.
func TestOAuthCallback_PDSResolutionFailure_FailsLoginNoSession(t *testing.T) {
	setUpOAuthGlobalsForTest(t)

	const did = "did:plc:bob-unresolvable"

	issuer := fakeIssuer(t, did)
	defer issuer.Close()
	// PLC directory that has no record of this DID at all (404 for
	// everything), simulating "PLC unreachable / malformed / DID unknown".
	plc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer plc.Close()

	s := &Service{config: &config.Config{ATProto: config.ATProtoConfig{PLCDirectoryURL: plc.URL}}}

	rr := driveOAuthCallback(t, s, issuer.URL)

	if rr.Code == http.StatusFound {
		t.Fatalf("expected login to fail (non-redirect status), got a redirect to %q", rr.Header().Get("Location"))
	}
	if rr.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 on resolution failure, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), did) {
		t.Errorf("expected the error body to name the DID %q so the cause is legible, got: %s", did, rr.Body.String())
	}

	// No session was created: the handler never reached CreateSession
	// (proven above by the non-redirect status/body), so there is no
	// session ID to have been minted or stored in sessionStore at all.
}

// TestOAuthCallback_IssuerAndPDSCoincide_StillWorks covers the ordinary
// "transition:generic" case: the user's PDS legitimately acts as its own
// authorization server, so iss and the resolved PDS are the SAME host.
// This must keep working.
func TestOAuthCallback_IssuerAndPDSCoincide_StillWorks(t *testing.T) {
	setUpOAuthGlobalsForTest(t)

	const did = "did:plc:carol7654321"

	issuer := fakeIssuer(t, did)
	defer issuer.Close()
	plc := fakePLCDirectory(t, did, issuer.URL)
	defer plc.Close()

	s := &Service{config: &config.Config{ATProto: config.ATProtoConfig{PLCDirectoryURL: plc.URL}}}

	rr := driveOAuthCallback(t, s, issuer.URL)
	if rr.Code != http.StatusFound {
		t.Fatalf("expected 302 redirect, got %d: %s", rr.Code, rr.Body.String())
	}

	sessionID := sessionIDFromRedirect(t, rr)
	session, err := sessionStore.GetSession(sessionID)
	if err != nil {
		t.Fatalf("expected the created session to be retrievable: %v", err)
	}
	if session.PDSURL != issuer.URL {
		t.Errorf("session.PDSURL = %q, want %q (issuer and resolved PDS coincide in this case)", session.PDSURL, issuer.URL)
	}
}

// TestResolveSessionPDS_WrapsErrorWithDID is a narrower unit test of the
// extracted resolver helper itself: on failure, the returned error must
// name the DID, so the cause is legible in a log (per atchess-1c9.84).
func TestResolveSessionPDS_WrapsErrorWithDID(t *testing.T) {
	plc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer plc.Close()

	const did = "did:plc:doesnotexist"
	_, err := resolveSessionPDS(context.Background(), did, plc.URL)
	if err == nil {
		t.Fatal("expected an error resolving a DID the fake PLC directory has no record of")
	}
	if !strings.Contains(err.Error(), did) {
		t.Errorf("expected error to name the DID %q, got: %v", did, err)
	}
}
