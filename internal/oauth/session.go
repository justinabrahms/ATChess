package oauth

import (
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// Session represents an authenticated session with tokens and metadata.
// It covers both app-password logins (PDSURL + AccessToken/RefreshToken are
// the AT Protocol accessJwt/refreshJwt from com.atproto.server.createSession,
// DPoPKey is nil) and OAuth logins (AccessToken/RefreshToken are OAuth
// tokens, DPoPKey is the DPoP key they are bound to). See
// internal/web/session_auth.go (sessionAuthenticator) for how this is used
// to build a per-user *atproto.Client (atchess-1c9.9) and internal/atproto
// for NewClientFromSession/RefreshSession.
type Session struct {
	DID    string `json:"did"`
	Handle string `json:"handle"`

	// PDSURL is the user's actual PDS host (resolved from their DID
	// document, atchess-1c9.84), used for XRPC calls -- both the resource-
	// server host app writes/reads hit, AND (for app-password sessions
	// only, which have no separate authorization server) the refresh
	// endpoint. See AuthServerURL below for the OAuth case, where these two
	// are NOT always the same host.
	PDSURL string `json:"pds_url"`

	// AuthServerURL is the OAuth authorization server's origin -- the "iss"
	// value from the callback, recorded once at login (OAuthCallbackHandler)
	// -- used ONLY for OAuth token-endpoint discovery and refresh (see
	// internal/web/session_auth.go's refreshOAuthSession). Empty for
	// app-password sessions (DPoPKey == nil), which have no authorization
	// server distinct from their PDS.
	//
	// This is deliberately a separate field from PDSURL rather than reusing
	// it: under the atproto "transition:generic" profile a user's PDS often
	// also acts as its own authorization server, so the two coincide for
	// self-hosted/most PDSes -- but NOT for bsky.social, where iss is
	// https://bsky.social (the entryway) while the user's repo lives on a
	// separate *.host.bsky.network PDS. Collapsing the two into one field
	// previously broke OAuth refresh against bsky.social: the token-endpoint
	// discovery request landed on the PDS host, which does not serve
	// /.well-known/oauth-authorization-server (404).
	AuthServerURL string `json:"auth_server_url"`

	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`

	// ExpiresAt is the SESSION's overall lifetime (e.g. "log this user out
	// after 24h"), checked by SessionStore.GetSession. It is set once at
	// login and is intentionally NOT extended by EnsureFresh/ForceRefresh --
	// refreshing the underlying access token keeps the session usable, but
	// does not itself extend how long the session is considered valid.
	ExpiresAt time.Time `json:"expires_at"`

	// AccessTokenExpiresAt is the underlying AT Protocol/OAuth access
	// token's own expiry (parsed from its JWT "exp" claim where possible;
	// see atproto.ParseJWTExpiry), distinct from ExpiresAt above. This is
	// what EnsureFresh/ForceRefresh consult to decide whether the token
	// needs refreshing before use.
	AccessTokenExpiresAt time.Time `json:"access_token_expires_at"`

	DPoPKey *ecdsa.PrivateKey `json:"-"`

	// mu guards AccessToken/RefreshToken/AccessTokenExpiresAt against
	// concurrent refreshes (see EnsureFresh/ForceRefresh). Deliberately
	// unexported so it is never copied or serialized; Session must always
	// be handled by pointer.
	mu sync.Mutex `json:"-"`
}

// RefreshFunc performs the actual token-refresh call given the session's
// current refresh token, returning a new access token, (optionally) a new
// refresh token, and the new access token's expiry. Kept as a caller-
// supplied function so this package stays free of AT Protocol/OAuth HTTP
// specifics -- internal/web supplies the real implementation, backed by
// atproto.RefreshSession for app-password sessions.
type RefreshFunc func(refreshToken string) (accessToken, newRefreshToken string, expiresAt time.Time, err error)

// tokenExpirySkew is how long before a session's recorded ExpiresAt
// EnsureFresh proactively refreshes it, so a request is never raced against
// the PDS's own expiry check.
const tokenExpirySkew = 30 * time.Second

// EnsureFresh returns a currently-valid access token, refreshing via
// refreshFn first if the session is at or within tokenExpirySkew of
// AccessTokenExpiresAt. Concurrent callers for the same session serialize
// on s.mu, so at most one refresh call happens per expiry even under
// concurrent requests -- the edge case explicitly called out in
// atchess-1c9.9's brief.
func (s *Session) EnsureFresh(refreshFn RefreshFunc) (string, error) {
	return s.refresh(refreshFn, false)
}

// ForceRefresh unconditionally refreshes the session's access token via
// refreshFn, ignoring AccessTokenExpiresAt. Used when the PDS itself has
// just rejected a request with 401 -- which can happen even before our
// locally-tracked expiry arrives (e.g. server-side revocation) -- so the
// caller can refresh once and retry rather than looping.
func (s *Session) ForceRefresh(refreshFn RefreshFunc) (string, error) {
	return s.refresh(refreshFn, true)
}

func (s *Session) refresh(refreshFn RefreshFunc, force bool) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !force && time.Now().Before(s.AccessTokenExpiresAt.Add(-tokenExpirySkew)) {
		return s.AccessToken, nil
	}
	if refreshFn == nil {
		return "", fmt.Errorf("session token needs refresh but no refresh function is available")
	}
	access, newRefresh, expiresAt, err := refreshFn(s.RefreshToken)
	if err != nil {
		return "", fmt.Errorf("failed to refresh session token: %w", err)
	}
	s.AccessToken = access
	if newRefresh != "" {
		s.RefreshToken = newRefresh
	}
	s.AccessTokenExpiresAt = expiresAt
	return s.AccessToken, nil
}

// SessionStore manages OAuth sessions
type SessionStore struct {
	sessions map[string]*Session // map session ID to session
	mu       sync.RWMutex
}

// NewSessionStore creates a new session store
func NewSessionStore() *SessionStore {
	return &SessionStore{
		sessions: make(map[string]*Session),
	}
}

// CreateSession stores a new session and returns a session ID
func (s *SessionStore) CreateSession(session *Session) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Generate session ID
	sessionID := generateJTI()
	s.sessions[sessionID] = session

	return sessionID
}

// GetSession retrieves a session by ID
func (s *SessionStore) GetSession(sessionID string) (*Session, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	session, exists := s.sessions[sessionID]
	if !exists {
		return nil, fmt.Errorf("session not found")
	}

	// Check if session is expired
	if time.Now().After(session.ExpiresAt) {
		return nil, fmt.Errorf("session expired")
	}

	return session, nil
}

// DeleteSession removes a session
func (s *SessionStore) DeleteSession(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.sessions, sessionID)
}

// CleanupExpiredSessions removes all expired sessions
func (s *SessionStore) CleanupExpiredSessions() {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	for id, session := range s.sessions {
		if now.After(session.ExpiresAt) {
			delete(s.sessions, id)
		}
	}
}

// StartCleanupRoutine starts a goroutine that periodically cleans up expired sessions
func (s *SessionStore) StartCleanupRoutine() {
	go func() {
		ticker := time.NewTicker(15 * time.Minute)
		defer ticker.Stop()

		for range ticker.C {
			s.CleanupExpiredSessions()
		}
	}()
}

// AuthorizationRequest represents an in-progress OAuth authorization
type AuthorizationRequest struct {
	State        string            `json:"state"`
	CodeVerifier string            `json:"code_verifier"`
	Handle       string            `json:"handle"`
	CreatedAt    time.Time         `json:"created_at"`
	DPoPKey      *ecdsa.PrivateKey `json:"-"`
}

// AuthorizationStore manages pending authorization requests
type AuthorizationStore struct {
	requests map[string]*AuthorizationRequest // map state to request
	mu       sync.RWMutex
}

// NewAuthorizationStore creates a new authorization store
func NewAuthorizationStore() *AuthorizationStore {
	return &AuthorizationStore{
		requests: make(map[string]*AuthorizationRequest),
	}
}

// StoreAuthorization stores a pending authorization request
func (a *AuthorizationStore) StoreAuthorization(req *AuthorizationRequest) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.requests[req.State] = req
}

// GetAndDeleteAuthorization retrieves and removes an authorization request
func (a *AuthorizationStore) GetAndDeleteAuthorization(state string) (*AuthorizationRequest, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	req, exists := a.requests[state]
	if !exists {
		return nil, fmt.Errorf("authorization request not found")
	}

	// Check if request is too old (15 minutes)
	if time.Since(req.CreatedAt) > 15*time.Minute {
		delete(a.requests, state)
		return nil, fmt.Errorf("authorization request expired")
	}

	delete(a.requests, state)
	return req, nil
}

// MarshalJSON custom marshaller to handle private key serialization
func (s *Session) MarshalJSON() ([]byte, error) {
	type Alias Session
	return json.Marshal(&struct {
		*Alias
		DPoPKeyData []byte `json:"dpop_key_data,omitempty"`
	}{
		Alias:       (*Alias)(s),
		DPoPKeyData: nil, // We don't serialize private keys
	})
}
