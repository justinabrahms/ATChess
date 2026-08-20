package web

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/justinabrahms/atchess/internal/atproto"
	"github.com/justinabrahms/atchess/internal/oauth"
	"github.com/rs/zerolog/log"
)

// Global OAuth client and session stores
var (
	oauthClient  *oauth.OAuthClient
	sessionStore *oauth.SessionStore
	authStore    *oauth.AuthorizationStore
)

// InitializeOAuth sets up the OAuth client and stores
func InitializeOAuth(baseURL string) error {
	clientID := baseURL + "/client-metadata.json"
	redirectURI := baseURL + "/api/callback"

	client, err := oauth.NewOAuthClient(clientID, redirectURI)
	if err != nil {
		return fmt.Errorf("failed to create OAuth client: %w", err)
	}

	oauthClient = client
	sessionStore = oauth.NewSessionStore()
	authStore = oauth.NewAuthorizationStore()

	// Start session cleanup routine
	sessionStore.StartCleanupRoutine()

	// Don't update static client metadata anymore since we're serving it dynamically

	return nil
}

// GetOAuthClient returns the global OAuth client
func GetOAuthClient() *oauth.OAuthClient {
	return oauthClient
}

// updateClientMetadata updates the static client metadata with our public key
func updateClientMetadata(publicKeyJWK map[string]interface{}) {
	// In a real deployment, this would update the served client-metadata.json
	// For now, we'll log the JWK that should be added
	log.Info().Interface("jwk", publicKeyJWK).Msg("Add this JWK to client-metadata.json")
}

// OAuthLoginHandler initiates the OAuth flow
func (s *Service) OAuthLoginHandler(w http.ResponseWriter, r *http.Request) {
	// Check if OAuth is initialized
	if oauthClient == nil || authStore == nil || sessionStore == nil {
		log.Error().Msg("OAuth not initialized - SERVER_BASE_URL may not be set")
		http.Error(w, "OAuth not configured. Please ensure SERVER_BASE_URL is set.", http.StatusServiceUnavailable)
		return
	}

	var req struct {
		Handle string `json:"handle"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	// Resolve handle to get PDS URL and OAuth authorization server metadata
	// (including, per atchess-1c9.12, the PAR fields the pre-atchess-1c9.12
	// resolution chain discarded).
	pdsURL, authServerURL, metadata, err := s.resolveOAuthEndpoints(req.Handle)
	if err != nil {
		log.Error().Err(err).Str("handle", req.Handle).Msg("Failed to resolve OAuth endpoints")
		http.Error(w, "Failed to resolve authentication server", http.StatusInternalServerError)
		return
	}

	// Generate PKCE parameters
	verifier, challenge, err := oauth.GeneratePKCE()
	if err != nil {
		http.Error(w, "Failed to generate PKCE", http.StatusInternalServerError)
		return
	}

	// Generate state
	state, err := oauth.GenerateState()
	if err != nil {
		http.Error(w, "Failed to generate state", http.StatusInternalServerError)
		return
	}

	// Generate the DPoP key for this session. This SAME key is used for
	// the (optional) PAR call below, the eventual token exchange, and
	// every request/refresh made with the tokens that flow from it -- DPoP
	// binds a token to the specific proof key that first requested it, so
	// the key must not change partway through.
	dpopKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		http.Error(w, "Failed to generate DPoP key", http.StatusInternalServerError)
		return
	}

	// Store authorization request
	authStore.StoreAuthorization(&oauth.AuthorizationRequest{
		State:        state,
		CodeVerifier: verifier,
		Handle:       req.Handle,
		CreatedAt:    time.Now(),
		DPoPKey:      dpopKey,
	})

	// Build the authorization URL. oauthClient.BuildAuthorizationURLAuto
	// uses Pushed Authorization Requests (RFC 9126) when -- and only when
	// -- the authorization server actually advertises a PAR endpoint: PAR
	// is not universal (e.g. the local dual-PDS test harness's
	// authorization server does not advertise one), and unconditionally
	// requiring it would break every server that doesn't support it.
	// metadata.RequirePushedAuthorizationRequests is passed through so
	// BuildAuthorizationURLAuto can apply atchess-1c9.86's PAR-failure
	// policy (hard-fail vs. fall back to the plain /authorize URL) -- see
	// that method's doc comment for the exact selection and failure-policy
	// rules.
	authURL, err := oauthClient.BuildAuthorizationURLAuto(
		metadata.AuthorizationEndpoint, metadata.PushedAuthorizationRequestEndpoint,
		authServerURL, req.Handle, state, challenge, dpopKey, metadata.RequirePushedAuthorizationRequests)
	if err != nil {
		authStore.GetAndDeleteAuthorization(state) //nolint:errcheck // best-effort cleanup of the request we just stored
		log.Error().Err(err).Str("handle", req.Handle).Str("parEndpoint", metadata.PushedAuthorizationRequestEndpoint).
			Msg("Failed to build authorization URL")
		http.Error(w, "Failed to start authorization", http.StatusInternalServerError)
		return
	}

	// Return authorization URL to client
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"authorization_url": authURL,
		"pds_url":           pdsURL,
	})
}

// OAuthCallbackHandler handles the OAuth callback
func (s *Service) OAuthCallbackHandler(w http.ResponseWriter, r *http.Request) {
	// Check if OAuth is initialized
	if oauthClient == nil || authStore == nil || sessionStore == nil {
		log.Error().Msg("OAuth not initialized - SERVER_BASE_URL may not be set")
		http.Error(w, "OAuth not configured. Please ensure SERVER_BASE_URL is set.", http.StatusServiceUnavailable)
		return
	}

	// Get parameters from query
	code := r.URL.Query().Get("code")
	state := r.URL.Query().Get("state")
	iss := r.URL.Query().Get("iss")

	if code == "" || state == "" {
		http.Error(w, "Missing code or state", http.StatusBadRequest)
		return
	}

	// Retrieve authorization request
	authReq, err := authStore.GetAndDeleteAuthorization(state)
	if err != nil {
		log.Error().Err(err).Str("state", state).Msg("Failed to retrieve authorization")
		http.Error(w, "Invalid or expired authorization", http.StatusBadRequest)
		return
	}

	// Get token endpoint from issuer
	tokenEndpoint, err := getTokenEndpoint(iss)
	if err != nil {
		log.Error().Err(err).Str("iss", iss).Msg("Failed to get token endpoint")
		http.Error(w, "Failed to get token endpoint", http.StatusInternalServerError)
		return
	}

	// Exchange code for tokens
	tokens, err := oauthClient.ExchangeCodeForTokens(tokenEndpoint, iss, code, authReq.CodeVerifier, authReq.DPoPKey)
	if err != nil {
		log.Error().
			Err(err).
			Str("tokenEndpoint", tokenEndpoint).
			Str("code", code[:10]+"...").
			Str("iss", iss).
			Msg("Failed to exchange code for tokens")
		http.Error(w, fmt.Sprintf("Failed to exchange authorization code: %v", err), http.StatusInternalServerError)
		return
	}

	// Resolve the user's REAL PDS host from their DID document rather than
	// assuming it from the OAuth issuer (atchess-1c9.84). Under the atproto
	// "transition:generic" profile a user's PDS often acts as its own
	// authorization server, making iss and the PDS host the same origin --
	// but that is not guaranteed. bsky.social is the counterexample: iss is
	// https://bsky.social (the entryway, acting as authorization server),
	// while the user's repo lives on a separate *.host.bsky.network PDS.
	// Trusting iss there would silently record the wrong host and every
	// subsequent authenticated write (via Service.clientFor) would land in
	// the wrong place. Resolved once, here, at session creation -- NOT
	// per-request in clientFor -- so this doesn't add a network round trip
	// to every authenticated call.
	pdsURL, err := resolveSessionPDS(r.Context(), tokens.Sub, s.config.ATProto.PLCDirectoryURL)
	if err != nil {
		log.Error().Err(err).Str("did", tokens.Sub).Str("iss", iss).
			Msg("Failed to resolve PDS from DID document for OAuth session; refusing to create a session with an unresolved PDS host")
		http.Error(w, fmt.Sprintf("Failed to resolve your PDS from your DID document (did=%s): %v", tokens.Sub, err), http.StatusInternalServerError)
		return
	}

	session := &oauth.Session{
		DID:                  tokens.Sub,
		Handle:               authReq.Handle,
		PDSURL:               pdsURL,
		AuthServerURL:        iss,
		AccessToken:          tokens.AccessToken,
		RefreshToken:         tokens.RefreshToken,
		ExpiresAt:            time.Now().Add(time.Duration(tokens.ExpiresIn) * time.Second),
		AccessTokenExpiresAt: time.Now().Add(time.Duration(tokens.ExpiresIn) * time.Second),
		DPoPKey:              authReq.DPoPKey,
	}

	sessionID := sessionStore.CreateSession(session)

	// Login-time repo-read challenge backfill (atchess-1c9.46) -- see
	// LoginHandler's (internal/web/service.go) identical call for why this
	// runs synchronously, before the redirect, rather than fire-and-forget.
	s.backfillChallengesOnLogin(r.Context(), tokens.Sub)

	// Redirect to main page with session
	http.Redirect(w, r, "/?session="+sessionID, http.StatusFound)
}

// GetSessionHandler returns current session info
func (s *Service) GetSessionHandler(w http.ResponseWriter, r *http.Request) {
	sessionID := r.Header.Get("X-Session-ID")
	if sessionID == "" {
		http.Error(w, "No session", http.StatusUnauthorized)
		return
	}

	session, err := sessionStore.GetSession(sessionID)
	if err != nil {
		http.Error(w, "Invalid session", http.StatusUnauthorized)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"did":           session.DID,
		"handle":        session.Handle,
		"authenticated": true,
		"expires_at":    session.ExpiresAt,
	})
}

// LogoutHandler destroys the session
func (s *Service) LogoutHandler(w http.ResponseWriter, r *http.Request) {
	sessionID := r.Header.Get("X-Session-ID")
	if sessionID != "" {
		sessionStore.DeleteSession(sessionID)
	}

	w.WriteHeader(http.StatusNoContent)
}

// Helper methods

// authServerMetadata is the subset of an AT Protocol authorization server's
// /.well-known/oauth-authorization-server document this service acts on.
// Unlike the pre-atchess-1c9.12 getAuthorizationEndpoint (which decoded and
// kept only AuthorizationEndpoint, silently discarding everything else),
// this keeps the PAR-related fields too so resolveOAuthEndpoints' caller
// can decide whether to use Pushed Authorization Requests.
type authServerMetadata struct {
	AuthorizationEndpoint              string `json:"authorization_endpoint"`
	TokenEndpoint                      string `json:"token_endpoint"`
	PushedAuthorizationRequestEndpoint string `json:"pushed_authorization_request_endpoint"`
	RequirePushedAuthorizationRequests bool   `json:"require_pushed_authorization_requests"`
}

// resolveOAuthEndpoints resolves handle all the way through to the target
// authorization server's metadata: handle -> DID -> PDS -> resource-server
// metadata -> authorization-server metadata. authServerURL is returned
// alongside metadata so callers that need to authenticate directly to the
// authorization server (e.g. a Pushed Authorization Request's client
// assertion "aud") don't have to re-derive it.
func (s *Service) resolveOAuthEndpoints(handle string) (pdsURL, authServerURL string, metadata *authServerMetadata, err error) {
	// First resolve handle to DID
	// Called before any session exists (this is how login is initiated), so
	// it legitimately uses the server's own client rather than clientFor(r).
	did, err := s.serverClient.ResolveHandle(context.Background(), handle)
	if err != nil {
		return "", "", nil, fmt.Errorf("failed to resolve handle: %w", err)
	}

	// Get DID document and extract its PDS URL. Delegates to
	// internal/atproto.ResolvePDS (did:plc via the configured PLC
	// directory, did:web via HTTPS well-known) rather than duplicating that
	// resolution logic here -- see atchess-1c9.10.
	pdsURL, err = atproto.ResolvePDS(context.Background(), did, s.config.ATProto.PLCDirectoryURL)
	if err != nil {
		return "", "", nil, fmt.Errorf("failed to resolve PDS for %s: %w", did, err)
	}

	// Get OAuth authorization server metadata
	authServerURL, err = s.getAuthorizationServer(pdsURL)
	if err != nil {
		return "", "", nil, fmt.Errorf("failed to get authorization server: %w", err)
	}

	metadata, err = getAuthServerMetadata(authServerURL)
	if err != nil {
		return "", "", nil, fmt.Errorf("failed to get authorization server metadata: %w", err)
	}

	return pdsURL, authServerURL, metadata, nil
}

// resolveSessionPDS resolves did's real PDS host from its DID document,
// via internal/atproto.ResolvePDS -- the same DID -> PDS resolution used
// elsewhere in this codebase (e.g. resolveOAuthEndpoints above), rather
// than a second implementation. Deliberately does NOT accept or fall back
// to the OAuth issuer on failure (atchess-1c9.84): recording a
// plausible-but-wrong host would surface later as a write failure far from
// this call site, looking like a permissions problem rather than a
// resolution one. Extracted as its own function (rather than inlined into
// OAuthCallbackHandler) so it can be exercised directly by tests without
// driving a full authorization-code exchange.
func resolveSessionPDS(ctx context.Context, did, plcDirectoryURL string) (string, error) {
	pdsURL, err := atproto.ResolvePDS(ctx, did, plcDirectoryURL)
	if err != nil {
		return "", fmt.Errorf("resolving PDS for %s: %w", did, err)
	}
	return pdsURL, nil
}

// oauthMetadataHTTPClientTimeout bounds every fetch oauthMetadataHTTPClient
// makes, so an unresponsive PDS/authorization-server (attacker-chosen or
// otherwise) cannot hang a login/callback request indefinitely.
const oauthMetadataHTTPClientTimeout = 10 * time.Second

// oauthMetadataHTTPClient is the client getAuthorizationServer and
// getAuthServerMetadata use to fetch OAuth resource-/authorization-server
// metadata -- atchess-1c9.95, replacing the previous bare http.Get (i.e.
// http.DefaultClient): that had no timeout at all, and followed up to 10
// redirects with no re-validation of the Location host, mirroring exactly
// the redirect gap atchess-1c9.94 closed for identity-resolution fetches
// (see refuseIdentityFetchRedirect's doc comment in internal/atproto).
// Neither an oauth-protected-resource nor an oauth-authorization-server
// metadata document is specified to redirect, so refusing every redirect
// outright gives up no legitimate case.
var oauthMetadataHTTPClient = &http.Client{
	Timeout: oauthMetadataHTTPClientTimeout,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return fmt.Errorf("oauth metadata fetch: refusing to follow redirect to %s", req.URL)
	},
}

func (s *Service) getAuthorizationServer(pdsURL string) (string, error) {
	// pdsURL was resolved from the target account's DID document (see
	// resolveOAuthEndpoints -> atproto.ResolvePDS), which atchess-1c9.95's
	// parseServiceEndpoint already validates via
	// atproto.ValidateFetchedEndpointURL before ever returning it. Validated
	// again here anyway, at the actual dial site, rather than trusting that
	// invariant to hold across every current and future caller of this
	// method -- the whole point of atchess-1c9.95 is "validate the value
	// immediately before it is dialed", not "validate it somewhere upstream
	// and hope".
	if _, err := atproto.ValidateFetchedEndpointURL(pdsURL); err != nil {
		return "", fmt.Errorf("refusing to fetch resource-server metadata: %w", err)
	}

	// Get resource server metadata
	resp, err := oauthMetadataHTTPClient.Get(pdsURL + "/.well-known/oauth-protected-resource")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var metadata struct {
		AuthorizationServers []string `json:"authorization_servers"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&metadata); err != nil {
		return "", err
	}

	if len(metadata.AuthorizationServers) == 0 {
		return "", fmt.Errorf("no authorization servers found")
	}

	return metadata.AuthorizationServers[0], nil
}

// getAuthServerMetadata fetches and decodes an authorization server's
// /.well-known/oauth-authorization-server document. authServerURL must
// already be a bare origin (scheme://host[:port]), with no path -- both of
// this function's callers (resolveOAuthEndpoints's authServerURL, itself
// taken from the PDS's oauth-protected-resource RESPONSE BODY above, and
// getTokenEndpoint's issuer, taken from an OAuth callback's untrusted "iss"
// query parameter) ensure that. Neither value is something this codebase
// chose or validated upstream -- atchess-1c9.95 -- so it is validated here,
// at the actual dial site, via the SAME atproto.ValidateFetchedEndpointURL
// parseServiceEndpoint uses (https, no userinfo/query/fragment/path, and a
// host that passes the shared did:web host validator).
func getAuthServerMetadata(authServerURL string) (*authServerMetadata, error) {
	if _, err := atproto.ValidateFetchedEndpointURL(authServerURL); err != nil {
		return nil, fmt.Errorf("refusing to fetch authorization-server metadata: %w", err)
	}

	resp, err := oauthMetadataHTTPClient.Get(authServerURL + "/.well-known/oauth-authorization-server")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("oauth-authorization-server metadata: HTTP %d", resp.StatusCode)
	}

	var metadata authServerMetadata
	if err := json.NewDecoder(resp.Body).Decode(&metadata); err != nil {
		return nil, err
	}

	return &metadata, nil
}

// getTokenEndpoint resolves issuer's token_endpoint from its authorization
// server metadata. issuer is the OAuth "iss" value from the callback (or,
// for a subsequent refresh, an OAuth session's recorded AuthServerURL --
// see refreshOAuthSession in session_auth.go, which is the other caller of
// this function; NOT session.PDSURL, which may be a different host --
// atchess-1c9.84) -- normalized down to its bare origin, matching how the
// AT Protocol OAuth profile locates .well-known/oauth-authorization-server
// (irrespective of any path component on issuer itself). Deliberately a
// free function, not a *Service method (despite living next to Service's
// OAuth handlers): it needs no Service state, and refreshOAuthSession has
// none available (session_auth.go's refreshFunc closures are built from a
// bare *oauth.Session, not a request/Service).
func getTokenEndpoint(issuer string) (string, error) {
	u, err := url.Parse(issuer)
	if err != nil {
		return "", err
	}
	origin := fmt.Sprintf("%s://%s", u.Scheme, u.Host)

	metadata, err := getAuthServerMetadata(origin)
	if err != nil {
		return "", err
	}

	return metadata.TokenEndpoint, nil
}
