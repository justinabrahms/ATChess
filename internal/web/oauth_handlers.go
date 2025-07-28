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
	"strings"
	"time"

	"github.com/justinabrahms/atchess/internal/oauth"
	"github.com/rs/zerolog/log"
)

// Global OAuth client and session stores
var (
	oauthClient *oauth.OAuthClient
	sessionStore *oauth.SessionStore
	authStore *oauth.AuthorizationStore
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
	
	// Resolve handle to get PDS URL and OAuth endpoints
	pdsURL, authEndpoint, err := s.resolveOAuthEndpoints(req.Handle)
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
	
	// Generate DPoP key for this session
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
	
	// Build authorization URL
	authURL := oauthClient.BuildAuthorizationURL(authEndpoint, req.Handle, state, challenge)
	
	// Return authorization URL to client
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"authorization_url": authURL,
		"pds_url": pdsURL,
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
	tokenEndpoint, err := s.getTokenEndpoint(iss)
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
	
	// Create session
	session := &oauth.Session{
		DID:          tokens.Sub,
		Handle:       authReq.Handle,
		AccessToken:  tokens.AccessToken,
		RefreshToken: tokens.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(tokens.ExpiresIn) * time.Second),
		DPoPKey:      authReq.DPoPKey,
	}
	
	sessionID := sessionStore.CreateSession(session)
	
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
		"did":    session.DID,
		"handle": session.Handle,
		"authenticated": true,
		"expires_at": session.ExpiresAt,
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

func (s *Service) resolveOAuthEndpoints(handle string) (pdsURL, authEndpoint string, err error) {
	// First resolve handle to DID
	did, err := s.client.ResolveHandle(context.Background(), handle)
	if err != nil {
		return "", "", fmt.Errorf("failed to resolve handle: %w", err)
	}
	
	// Get DID document to find PDS
	didDoc, err := s.getDidDocument(did)
	if err != nil {
		return "", "", fmt.Errorf("failed to get DID document: %w", err)
	}
	
	// Extract PDS URL from DID document
	pdsURL = s.extractPDSFromDidDoc(didDoc)
	if pdsURL == "" {
		return "", "", fmt.Errorf("no PDS URL in DID document")
	}
	
	// Get OAuth authorization server metadata
	authServerURL, err := s.getAuthorizationServer(pdsURL)
	if err != nil {
		return "", "", fmt.Errorf("failed to get authorization server: %w", err)
	}
	
	// Get authorization endpoint from metadata
	authEndpoint, err = s.getAuthorizationEndpoint(authServerURL)
	if err != nil {
		return "", "", fmt.Errorf("failed to get authorization endpoint: %w", err)
	}
	
	return pdsURL, authEndpoint, nil
}

func (s *Service) getDidDocument(did string) (map[string]interface{}, error) {
	// For did:plc, use PLC directory
	if strings.HasPrefix(did, "did:plc:") {
		resp, err := http.Get(fmt.Sprintf("https://plc.directory/%s", did))
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		
		var doc map[string]interface{}
		if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
			return nil, err
		}
		
		return doc, nil
	}
	
	// For did:web, resolve via HTTPS
	if strings.HasPrefix(did, "did:web:") {
		// Implementation for did:web resolution
		return nil, fmt.Errorf("did:web not yet implemented")
	}
	
	return nil, fmt.Errorf("unsupported DID method")
}

func (s *Service) extractPDSFromDidDoc(doc map[string]interface{}) string {
	// Look for atproto_pds service
	services, ok := doc["service"].([]interface{})
	if !ok {
		return ""
	}
	
	for _, svc := range services {
		service, ok := svc.(map[string]interface{})
		if !ok {
			continue
		}
		
		if service["id"] == "#atproto_pds" {
			endpoint, _ := service["serviceEndpoint"].(string)
			return endpoint
		}
	}
	
	return ""
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

func (s *Service) getAuthorizationEndpoint(authServerURL string) (string, error) {
	// Get authorization server metadata
	resp, err := http.Get(authServerURL + "/.well-known/oauth-authorization-server")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	
	var metadata struct {
		AuthorizationEndpoint string `json:"authorization_endpoint"`
	}
	
	if err := json.NewDecoder(resp.Body).Decode(&metadata); err != nil {
		return "", err
	}
	
	return metadata.AuthorizationEndpoint, nil
}

func (s *Service) getTokenEndpoint(issuer string) (string, error) {
	// Parse issuer URL
	u, err := url.Parse(issuer)
	if err != nil {
		return "", err
	}
	
	// Get authorization server metadata
	metadataURL := fmt.Sprintf("%s://%s/.well-known/oauth-authorization-server", u.Scheme, u.Host)
	resp, err := http.Get(metadataURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	
	var metadata struct {
		TokenEndpoint string `json:"token_endpoint"`
	}
	
	if err := json.NewDecoder(resp.Body).Decode(&metadata); err != nil {
		return "", err
	}
	
	return metadata.TokenEndpoint, nil
}