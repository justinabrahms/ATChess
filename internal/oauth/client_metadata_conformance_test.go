package oauth_test

// Group B of the atchess-1c9.7 conformance tests: pure unit assertions
// (no network) that Service.ClientMetadataHandler's /client-metadata.json
// output satisfies AT Protocol client metadata requirements. This runs in
// the DEFAULT test suite (no env gate) -- see conformance_test.go in this
// package for the network-gated PAR / DPoP-nonce assertions.
//
// This lives in package oauth_test (an external test package for
// internal/oauth), not package oauth, because it needs
// github.com/justinabrahms/atchess/internal/web (for Service and
// ClientMetadataHandler), and internal/web imports internal/oauth --
// importing internal/web from an in-package (package oauth) test file
// would create an import cycle. oauth_test is a separate package from
// oauth, so no cycle exists.

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/justinabrahms/atchess/internal/web"
)

// fakeOAuthClient is a minimal stand-in satisfying web.OAuthClientInterface
// so ClientMetadataHandler serves a non-empty inline JWKS without needing a
// real OAuth private key file/env var (oauth.NewOAuthClient requires one;
// this test needs none of that machinery).
type fakeOAuthClient struct{}

func (fakeOAuthClient) GetPublicKeyJWK() map[string]interface{} {
	return map[string]interface{}{
		"kty": "EC",
		"crv": "P-256",
		"x":   "test-x-coordinate",
		"y":   "test-y-coordinate",
		"use": "sig",
		"alg": "ES256",
		"kid": "test-kid",
	}
}

// TestConformance_ClientMetadataSatisfiesATProtoRequirements drives
// Service.ClientMetadataHandler directly (no network, no real server) with
// an X-Forwarded-Proto: https header, the way a Caddy/nginx reverse proxy
// would set it in production, and checks the served document against the
// AT Protocol client metadata requirements.
func TestConformance_ClientMetadataSatisfiesATProtoRequirements(t *testing.T) {
	svc := &web.Service{}
	svc.SetOAuthClient(fakeOAuthClient{})

	const servedOrigin = "https://atchess.example.com"
	req := httptest.NewRequest("GET", servedOrigin+"/client-metadata.json", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	rr := httptest.NewRecorder()

	svc.ClientMetadataHandler(rr, req)

	if rr.Code != 200 {
		t.Fatalf("ClientMetadataHandler returned HTTP %d, body: %s", rr.Code, rr.Body.String())
	}

	var metadata map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &metadata); err != nil {
		t.Fatalf("client-metadata.json is not valid JSON: %v (body: %s)", err, rr.Body.String())
	}

	// client_id must equal the URL the document is served from.
	wantClientID := servedOrigin + "/client-metadata.json"
	if got, _ := metadata["client_id"].(string); got != wantClientID {
		t.Errorf("client_id = %q, want %q (must equal the URL client-metadata.json is served from)", got, wantClientID)
	}

	// redirect_uris must match the served origin.
	redirectURIsRaw, ok := metadata["redirect_uris"].([]interface{})
	if !ok || len(redirectURIsRaw) == 0 {
		t.Fatalf("redirect_uris missing or empty: %v", metadata["redirect_uris"])
	}
	for _, ru := range redirectURIsRaw {
		s, ok := ru.(string)
		if !ok || !strings.HasPrefix(s, servedOrigin+"/") {
			t.Errorf("redirect_uri %v does not match served origin %s", ru, servedOrigin)
		}
	}

	// token_endpoint_auth_method must be present and non-empty.
	if authMethod, _ := metadata["token_endpoint_auth_method"].(string); authMethod == "" {
		t.Errorf("token_endpoint_auth_method missing or empty: %v", metadata["token_endpoint_auth_method"])
	}

	// grant_types must be present and non-empty.
	grantTypesRaw, ok := metadata["grant_types"].([]interface{})
	if !ok || len(grantTypesRaw) == 0 {
		t.Errorf("grant_types missing or empty: %v", metadata["grant_types"])
	}

	// scope must be present and non-empty.
	if scope, _ := metadata["scope"].(string); scope == "" {
		t.Errorf("scope missing or empty: %v", metadata["scope"])
	}

	// dpop_bound_access_tokens must be true (AT Protocol requires
	// DPoP-bound tokens for confidential clients).
	if dpopBound, _ := metadata["dpop_bound_access_tokens"].(bool); !dpopBound {
		t.Errorf("dpop_bound_access_tokens = %v, want true", metadata["dpop_bound_access_tokens"])
	}

	// Must have a resolvable jwks_uri, OR an inline jwks with at least one key.
	jwksURI, hasJWKSURI := metadata["jwks_uri"].(string)
	jwksRaw, hasJWKS := metadata["jwks"].(map[string]interface{})
	switch {
	case hasJWKSURI && jwksURI != "":
		// fine: jwks_uri present. (Not fetched here -- this is a pure unit
		// test; presence/shape is all we can assert without network.)
	case hasJWKS:
		keys, ok := jwksRaw["keys"].([]interface{})
		if !ok || len(keys) == 0 {
			t.Errorf("inline jwks has no keys: %v", jwksRaw)
		}
	default:
		t.Errorf("client metadata has neither a jwks_uri nor an inline jwks: %v", metadata)
	}
}
