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
	// requiring it would break every server that doesn't support it. See
	// that method's doc comment for the exact selection rule.
	authURL, err := oauthClient.BuildAuthorizationURLAuto(
		metadata.AuthorizationEndpoint, metadata.PushedAuthorizationRequestEndpoint,
		authServerURL, req.Handle, state, challenge, dpopKey)
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

	// Create session. PDSURL is set to the OAuth issuer: under the atproto
	// "transition:generic" OAuth profile a user's PDS acts as its own
	// authorization server, so iss is typically the same origin records
	// must be written to -- but this is provisional. Actually wiring OAuth
	// sessions through Service.clientFor (refresh, DPoP-bound requests,
	// etc.) is atchess-1c9.12's job; recording PDSURL here just avoids
	// leaving that bead with a session shape that has no PDS URL at all.
	session := &oauth.Session{
		DID:                  tokens.Sub,
		Handle:               authReq.Handle,
		PDSURL:               iss,
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

func (s *Service) getAuthorizationServer(pdsURL string) (string, error) {
	// Get resource server metadata
	resp, err := http.Get(pdsURL + "/.well-known/oauth-protected-resource")
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
// this function's callers (resolveOAuthEndpoints and getTokenEndpoint)
// ensure that.
func getAuthServerMetadata(authServerURL string) (*authServerMetadata, error) {
	resp, err := http.Get(authServerURL + "/.well-known/oauth-authorization-server")
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
// for a subsequent refresh, an OAuth session's recorded PDSURL -- see
// refreshOAuthSession in session_auth.go, which is the other caller of
// this function) -- normalized down to its bare origin, matching how the
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
