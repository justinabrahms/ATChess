package auth

import (
	"crypto/ecdsa"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// DPoPManager manages DPoP key pairs and proof generation
type DPoPManager struct {
	mu          sync.RWMutex
	currentKey  *ecdsa.PrivateKey
	currentJWK  *JWK
	keyRotation time.Time
	proofCache  map[string]time.Time // Track recently used JTIs to prevent replay
}

// NewDPoPManager creates a new DPoP manager
func NewDPoPManager() (*DPoPManager, error) {
	manager := &DPoPManager{
		proofCache: make(map[string]time.Time),
	}
	
	// Generate initial key pair
	if err := manager.rotateKey(); err != nil {
		return nil, err
	}
	
	// Start cleanup goroutine for proof cache
	go manager.cleanupProofCache()
	
	return manager, nil
}

// CreateProof creates a DPoP proof JWT for a request
func (m *DPoPManager) CreateProof(method, uri, accessToken string) (string, error) {
	m.mu.RLock()
	privateKey := m.currentKey
	jwk := m.currentJWK
	m.mu.RUnlock()
	
	// Generate unique JTI
	jti, err := GenerateJTI()
	if err != nil {
		return "", fmt.Errorf("failed to generate jti: %w", err)
	}
	
	// Create header with JWK
	header := &JWTHeader{
		Algorithm: "ES256",
		Type:      "dpop+jwt",
		JWK:       jwk,
	}
	
	// Create claims
	now := time.Now().Unix()
	claims := &JWTClaims{
		JTI:        jti,
		HTTPMethod: strings.ToUpper(method),
		HTTPURI:    uri,
		IssuedAt:   now,
	}
	
	// Add access token hash if provided
	if accessToken != "" {
		claims.AccessToken = HashAccessToken(accessToken)
	}
	
	// Store JTI to prevent replay
	m.mu.Lock()
	m.proofCache[jti] = time.Now()
	m.mu.Unlock()
	
	// Create and sign JWT
	return CreateJWT(header, claims, privateKey)
}

// CreateProofForRequest creates a DPoP proof for an HTTP request
func (m *DPoPManager) CreateProofForRequest(req *http.Request, accessToken string) (string, error) {
	// Extract the URI without query parameters for the htu claim
	uri := req.URL.Scheme + "://" + req.URL.Host + req.URL.Path
	
	return m.CreateProof(req.Method, uri, accessToken)
}

// AddDPoPHeader adds a DPoP header to an HTTP request
func (m *DPoPManager) AddDPoPHeader(req *http.Request, accessToken string) error {
	proof, err := m.CreateProofForRequest(req, accessToken)
	if err != nil {
		return err
	}
	
	req.Header.Set("DPoP", proof)
	return nil
}

// GetCurrentJWK returns the current public key as JWK
func (m *DPoPManager) GetCurrentJWK() *JWK {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.currentJWK
}

// RotateKeyIfNeeded rotates the key if it's older than the specified duration
func (m *DPoPManager) RotateKeyIfNeeded(maxAge time.Duration) error {
	m.mu.RLock()
	needsRotation := time.Since(m.keyRotation) > maxAge
	m.mu.RUnlock()
	
	if needsRotation {
		return m.rotateKey()
	}
	
	return nil
}

// rotateKey generates a new key pair
func (m *DPoPManager) rotateKey() error {
	privateKey, err := GenerateES256KeyPair()
	if err != nil {
		return fmt.Errorf("failed to generate key pair: %w", err)
	}
	
	jwk, err := PrivateKeyToJWK(privateKey)
	if err != nil {
		return fmt.Errorf("failed to convert key to JWK: %w", err)
	}
	
	m.mu.Lock()
	m.currentKey = privateKey
	m.currentJWK = jwk
	m.keyRotation = time.Now()
	m.mu.Unlock()
	
	return nil
}

// cleanupProofCache periodically removes old JTIs from the cache
func (m *DPoPManager) cleanupProofCache() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	
	for range ticker.C {
		m.mu.Lock()
		now := time.Now()
		for jti, timestamp := range m.proofCache {
			// Remove JTIs older than 10 minutes
			if now.Sub(timestamp) > 10*time.Minute {
				delete(m.proofCache, jti)
			}
		}
		m.mu.Unlock()
	}
}

// ValidateProof validates a DPoP proof
func ValidateProof(proof string, method, uri, accessToken string) error {
	// Verify JWT signature and extract claims
	header, claims, err := VerifyJWT(proof)
	if err != nil {
		return fmt.Errorf("failed to verify JWT: %w", err)
	}
	
	// Verify header type
	if header.Type != "dpop+jwt" {
		return fmt.Errorf("invalid typ header: expected dpop+jwt, got %s", header.Type)
	}
	
	// Verify required claims
	if claims.JTI == "" {
		return fmt.Errorf("missing jti claim")
	}
	
	if claims.HTTPMethod == "" {
		return fmt.Errorf("missing htm claim")
	}
	
	if claims.HTTPURI == "" {
		return fmt.Errorf("missing htu claim")
	}
	
	if claims.IssuedAt == 0 {
		return fmt.Errorf("missing iat claim")
	}
	
	// Verify HTTP method matches
	if !strings.EqualFold(claims.HTTPMethod, method) {
		return fmt.Errorf("htm claim mismatch: expected %s, got %s", method, claims.HTTPMethod)
	}
	
	// Verify URI matches (normalize both URIs)
	expectedURI := normalizeURI(uri)
	actualURI := normalizeURI(claims.HTTPURI)
	if expectedURI != actualURI {
		return fmt.Errorf("htu claim mismatch: expected %s, got %s", expectedURI, actualURI)
	}
	
	// Verify access token hash if provided
	if accessToken != "" {
		expectedHash := HashAccessToken(accessToken)
		if claims.AccessToken != expectedHash {
			return fmt.Errorf("ath claim mismatch")
		}
	}
	
	// Verify proof is not too old (5 minutes)
	now := time.Now().Unix()
	if now-claims.IssuedAt > 300 {
		return fmt.Errorf("proof too old: issued at %d, now %d", claims.IssuedAt, now)
	}
	
	// Verify proof is not from the future (allow 30 seconds clock skew)
	if claims.IssuedAt > now+30 {
		return fmt.Errorf("proof from future: issued at %d, now %d", claims.IssuedAt, now)
	}
	
	return nil
}

// normalizeURI normalizes a URI for comparison
func normalizeURI(uri string) string {
	// Remove trailing slashes
	uri = strings.TrimRight(uri, "/")
	
	// Remove default ports
	uri = strings.Replace(uri, ":80/", "/", 1)
	uri = strings.Replace(uri, ":443/", "/", 1)
	
	// Ensure the URI ends without default port
	if strings.HasSuffix(uri, ":80") {
		uri = uri[:len(uri)-3]
	}
	if strings.HasSuffix(uri, ":443") {
		uri = uri[:len(uri)-4]
	}
	
	return uri
}

// DPoPInterceptor is an HTTP round tripper that automatically adds DPoP headers
type DPoPInterceptor struct {
	Manager   *DPoPManager
	Transport http.RoundTripper
	GetToken  func() string // Function to get the current access token
}

// RoundTrip implements http.RoundTripper
func (d *DPoPInterceptor) RoundTrip(req *http.Request) (*http.Response, error) {
	// Clone the request to avoid modifying the original
	req = req.Clone(req.Context())
	
	// Get current access token
	accessToken := ""
	if d.GetToken != nil {
		accessToken = d.GetToken()
	}
	
	// Add DPoP header
	if err := d.Manager.AddDPoPHeader(req, accessToken); err != nil {
		return nil, fmt.Errorf("failed to add DPoP header: %w", err)
	}
	
	// Use default transport if none provided
	transport := d.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	
	return transport.RoundTrip(req)
}

// NewDPoPClient creates an HTTP client with automatic DPoP support
func NewDPoPClient(manager *DPoPManager, getToken func() string) *http.Client {
	return &http.Client{
		Transport: &DPoPInterceptor{
			Manager:  manager,
			GetToken: getToken,
		},
	}
}