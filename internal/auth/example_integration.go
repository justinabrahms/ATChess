package auth

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// DPoPEnabledClient demonstrates how to integrate DPoP into an AT Protocol client
type DPoPEnabledClient struct {
	pdsURL      string
	accessJWT   string
	did         string
	handle      string
	httpClient  *http.Client
	dpopManager *DPoPManager
}

// NewDPoPEnabledClient creates a new client with DPoP support
func NewDPoPEnabledClient(pdsURL, handle, password string) (*DPoPEnabledClient, error) {
	// Create DPoP manager
	dpopManager, err := NewDPoPManager()
	if err != nil {
		return nil, fmt.Errorf("failed to create DPoP manager: %w", err)
	}

	// Create base HTTP client
	baseClient := &http.Client{
		Timeout: 30 * time.Second,
	}

	// Create session (this would typically use DPoP for the initial auth too)
	sessionReq := map[string]interface{}{
		"identifier": handle,
		"password":   password,
	}

	reqBody, _ := json.Marshal(sessionReq)
	req, err := http.NewRequest("POST", pdsURL+"/xrpc/com.atproto.server.createSession", bytes.NewBuffer(reqBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	// Add DPoP proof for session creation (no access token yet)
	if err := dpopManager.AddDPoPHeader(req, ""); err != nil {
		return nil, fmt.Errorf("failed to add DPoP header: %w", err)
	}

	resp, err := baseClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to create session: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to create session: HTTP %d", resp.StatusCode)
	}

	var session struct {
		AccessJwt string `json:"accessJwt"`
		Did       string `json:"did"`
		Handle    string `json:"handle"`
		DPoPNonce string `json:"dpop_nonce"` // Server may provide a nonce for replay protection
	}

	if err := json.NewDecoder(resp.Body).Decode(&session); err != nil {
		return nil, fmt.Errorf("failed to decode session response: %w", err)
	}

	// Create DPoP-enabled HTTP client
	dpopClient := NewDPoPClient(dpopManager, func() string {
		return session.AccessJwt
	})

	return &DPoPEnabledClient{
		pdsURL:      pdsURL,
		accessJWT:   session.AccessJwt,
		did:         session.Did,
		handle:      session.Handle,
		httpClient:  dpopClient,
		dpopManager: dpopManager,
	}, nil
}

// CreateRecord demonstrates creating a record with DPoP
func (c *DPoPEnabledClient) CreateRecord(collection string, record interface{}) (string, error) {
	createReq := map[string]interface{}{
		"repo":       c.did,
		"collection": collection,
		"record":     record,
	}

	reqBody, _ := json.Marshal(createReq)
	req, err := http.NewRequest("POST", c.pdsURL+"/xrpc/com.atproto.repo.createRecord", bytes.NewBuffer(reqBody))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "DPoP "+c.accessJWT) // Note: DPoP scheme instead of Bearer

	// The DPoP header is automatically added by the DPoPClient transport

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to create record: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("failed to create record: HTTP %d", resp.StatusCode)
	}

	var createResp struct {
		URI string `json:"uri"`
		CID string `json:"cid"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&createResp); err != nil {
		return "", fmt.Errorf("failed to decode response: %w", err)
	}

	return createResp.URI, nil
}

// RotateKeys demonstrates key rotation
func (c *DPoPEnabledClient) RotateKeys() error {
	// Rotate keys every 24 hours
	return c.dpopManager.RotateKeyIfNeeded(24 * time.Hour)
}

// GetPublicKeyJWK returns the current public key as JWK
func (c *DPoPEnabledClient) GetPublicKeyJWK() *JWK {
	return c.dpopManager.GetCurrentJWK()
}

// Example of manual DPoP proof creation for custom requests
func (c *DPoPEnabledClient) CreateCustomDPoPProof(method, uri string) (string, error) {
	return c.dpopManager.CreateProof(method, uri, c.accessJWT)
}