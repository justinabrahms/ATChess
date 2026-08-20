package web

// Regression test for the review finding on atchess-1c9.84's fix pass:
// oauth.Session originally had only PDSURL, which the atchess-1c9.84 change
// repointed at the user's resolved PDS host -- correct for XRPC, but it was
// also the ONLY place the OAuth issuer/authorization-server origin was
// recorded, and refreshOAuthSession (session_auth.go) used that same field
// to discover the token endpoint and as the client assertion's "aud". On
// bsky.social the PDS host does not serve
// /.well-known/oauth-authorization-server at all (only the entryway,
// https://bsky.social, does) -- so refresh broke silently, invisible until
// the first refresh roughly an hour after login.
//
// This test drives refreshOAuthSession directly with the issuer and PDS
// pointed at two DIFFERENT httptest servers, and asserts BOTH the
// token-endpoint discovery request AND the client assertion's "aud" claim
// land on the issuer/AuthServerURL server -- never the PDS server, which
// is asserted to receive zero requests.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/justinabrahms/atchess/internal/oauth"
)

// decodeJWTPayload base64url-decodes the (unverified -- this test only
// needs to inspect the claims the client itself constructed, not validate
// a signature) middle segment of a compact JWT, and unmarshals it as JSON
// into out.
func decodeJWTPayload(t *testing.T, token string, out interface{}) {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("expected a 3-segment compact JWT, got %d segments: %q", len(parts), token)
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("base64url-decoding JWT payload: %v", err)
	}
	if err := json.Unmarshal(payload, out); err != nil {
		t.Fatalf("unmarshaling JWT payload %q: %v", payload, err)
	}
}

// TestRefreshOAuthSession_UsesAuthServerURL_NotPDSURL is the targeted
// regression test: issuer (AuthServerURL) and PDSURL are different hosts,
// and refresh must hit ONLY the issuer -- for both token-endpoint discovery
// and the client assertion's "aud" -- never the PDS.
func TestRefreshOAuthSession_UsesAuthServerURL_NotPDSURL(t *testing.T) {
	setUpOAuthGlobalsForTest(t)

	var issuerURL string // set right after the server below starts, before any request can be made
	var assertionAud string
	var tokenRequests, discoveryRequests int

	// atchess-1c9.95: issuer.URL is used as session.AuthServerURL, which
	// getTokenEndpoint/getAuthServerMetadata now validate (https, no port,
	// non-IP host) before dialing it -- see newFakeHTTPSEndpoint for why
	// this can no longer be the httptest server's own
	// http://127.0.0.1:<port> address.
	issuer := newFakeHTTPSEndpoint(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/.well-known/oauth-authorization-server":
			discoveryRequests++
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{ //nolint:errcheck
				"token_endpoint": issuerURL + "/token",
			})
		case r.Method == http.MethodPost && r.URL.Path == "/token":
			tokenRequests++
			if err := r.ParseForm(); err != nil {
				t.Fatalf("parsing refresh token request form: %v", err)
			}
			var claims struct {
				Aud string `json:"aud"`
			}
			decodeJWTPayload(t, r.FormValue("client_assertion"), &claims)
			assertionAud = claims.Aud

			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(oauth.TokenResponse{ //nolint:errcheck
				AccessToken:  "new-access-token",
				TokenType:    "DPoP",
				ExpiresIn:    3600,
				RefreshToken: "new-refresh-token",
				Sub:          "did:plc:refreshuser",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer issuer.Close()
	issuerURL = issuer.URL

	var pdsRequests int
	pds := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pdsRequests++
		http.NotFound(w, r) // the PDS host does NOT serve OAuth AS metadata
	}))
	defer pds.Close()

	dpopKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating DPoP key: %v", err)
	}
	session := &oauth.Session{
		DID:           "did:plc:refreshuser",
		Handle:        "refreshuser.test",
		PDSURL:        pds.URL,
		AuthServerURL: issuer.URL,
		RefreshToken:  "old-refresh-token",
		DPoPKey:       dpopKey,
	}

	accessToken, refreshToken, _, err := refreshOAuthSession(session, "old-refresh-token")
	if err != nil {
		t.Fatalf("refreshOAuthSession: unexpected error: %v", err)
	}
	if accessToken != "new-access-token" || refreshToken != "new-refresh-token" {
		t.Errorf("refreshOAuthSession returned (%q, %q), want (\"new-access-token\", \"new-refresh-token\")", accessToken, refreshToken)
	}

	if discoveryRequests != 1 {
		t.Errorf("expected exactly 1 token-endpoint discovery request to the issuer, got %d", discoveryRequests)
	}
	if tokenRequests != 1 {
		t.Errorf("expected exactly 1 refresh token request to the issuer, got %d", tokenRequests)
	}
	if pdsRequests != 0 {
		t.Errorf("expected ZERO requests to the PDS host during OAuth refresh, got %d -- refresh must talk only to the authorization server (issuer), never the PDS", pdsRequests)
	}
	if assertionAud != issuer.URL {
		t.Errorf("client assertion \"aud\" = %q, want the issuer %q (NOT the PDS %q)", assertionAud, issuer.URL, pds.URL)
	}
}
