package oauth

import (
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// Session represents an OAuth session with tokens and metadata
type Session struct {
	DID          string    `json:"did"`
	Handle       string    `json:"handle"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
	DPoPKey      *ecdsa.PrivateKey `json:"-"`
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
	State         string    `json:"state"`
	CodeVerifier  string    `json:"code_verifier"`
	Handle        string    `json:"handle"`
	CreatedAt     time.Time `json:"created_at"`
	DPoPKey       *ecdsa.PrivateKey `json:"-"`
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
		Alias: (*Alias)(s),
		DPoPKeyData: nil, // We don't serialize private keys
	})
}