package auth

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestDPoPManager(t *testing.T) {
	manager, err := NewDPoPManager()
	if err != nil {
		t.Fatalf("Failed to create DPoP manager: %v", err)
	}

	// Test proof creation
	proof, err := manager.CreateProof("POST", "https://example.com/xrpc/com.atproto.repo.createRecord", "test-access-token")
	if err != nil {
		t.Fatalf("Failed to create proof: %v", err)
	}

	// Verify proof format (should have 3 parts)
	parts := strings.Split(proof, ".")
	if len(parts) != 3 {
		t.Errorf("Invalid JWT format: expected 3 parts, got %d", len(parts))
	}

	// Test proof validation
	err = ValidateProof(proof, "POST", "https://example.com/xrpc/com.atproto.repo.createRecord", "test-access-token")
	if err != nil {
		t.Errorf("Failed to validate proof: %v", err)
	}

	// Test with wrong method
	err = ValidateProof(proof, "GET", "https://example.com/xrpc/com.atproto.repo.createRecord", "test-access-token")
	if err == nil {
		t.Error("Expected validation to fail with wrong method")
	}

	// Test with wrong URI
	err = ValidateProof(proof, "POST", "https://example.com/different", "test-access-token")
	if err == nil {
		t.Error("Expected validation to fail with wrong URI")
	}

	// Test with wrong access token
	err = ValidateProof(proof, "POST", "https://example.com/xrpc/com.atproto.repo.createRecord", "wrong-token")
	if err == nil {
		t.Error("Expected validation to fail with wrong access token")
	}
}

func TestDPoPHTTPClient(t *testing.T) {
	manager, err := NewDPoPManager()
	if err != nil {
		t.Fatalf("Failed to create DPoP manager: %v", err)
	}

	// Create HTTP request
	req, err := http.NewRequest("POST", "https://example.com/xrpc/com.atproto.repo.createRecord", nil)
	if err != nil {
		t.Fatalf("Failed to create request: %v", err)
	}

	// Add DPoP header
	err = manager.AddDPoPHeader(req, "test-access-token")
	if err != nil {
		t.Fatalf("Failed to add DPoP header: %v", err)
	}

	// Check header exists
	dpopHeader := req.Header.Get("DPoP")
	if dpopHeader == "" {
		t.Error("DPoP header not set")
	}

	// Validate the generated proof
	err = ValidateProof(dpopHeader, "POST", "https://example.com/xrpc/com.atproto.repo.createRecord", "test-access-token")
	if err != nil {
		t.Errorf("Failed to validate generated proof: %v", err)
	}
}

func TestJWTCreationAndVerification(t *testing.T) {
	// Generate key pair
	privateKey, err := GenerateES256KeyPair()
	if err != nil {
		t.Fatalf("Failed to generate key pair: %v", err)
	}

	// Convert to JWK
	jwk, err := PrivateKeyToJWK(privateKey)
	if err != nil {
		t.Fatalf("Failed to convert to JWK: %v", err)
	}

	// Create header
	header := &JWTHeader{
		Algorithm: "ES256",
		Type:      "dpop+jwt",
		JWK:       jwk,
	}

	// Create claims
	jti, _ := GenerateJTI()
	claims := &JWTClaims{
		JTI:         jti,
		HTTPMethod:  "POST",
		HTTPURI:     "https://example.com/api",
		IssuedAt:    time.Now().Unix(),
		AccessToken: HashAccessToken("test-token"),
	}

	// Create JWT
	jwt, err := CreateJWT(header, claims, privateKey)
	if err != nil {
		t.Fatalf("Failed to create JWT: %v", err)
	}

	// Verify JWT
	verifiedHeader, verifiedClaims, err := VerifyJWT(jwt)
	if err != nil {
		t.Fatalf("Failed to verify JWT: %v", err)
	}

	// Check header
	if verifiedHeader.Algorithm != "ES256" {
		t.Errorf("Wrong algorithm: %s", verifiedHeader.Algorithm)
	}
	if verifiedHeader.Type != "dpop+jwt" {
		t.Errorf("Wrong type: %s", verifiedHeader.Type)
	}

	// Check claims
	if verifiedClaims.JTI != jti {
		t.Errorf("Wrong JTI: %s", verifiedClaims.JTI)
	}
	if verifiedClaims.HTTPMethod != "POST" {
		t.Errorf("Wrong HTTP method: %s", verifiedClaims.HTTPMethod)
	}
	if verifiedClaims.HTTPURI != "https://example.com/api" {
		t.Errorf("Wrong HTTP URI: %s", verifiedClaims.HTTPURI)
	}
}

func TestKeyRotation(t *testing.T) {
	manager, err := NewDPoPManager()
	if err != nil {
		t.Fatalf("Failed to create DPoP manager: %v", err)
	}

	// Get initial JWK
	jwk1 := manager.GetCurrentJWK()

	// Force key rotation
	err = manager.RotateKeyIfNeeded(0) // 0 duration forces rotation
	if err != nil {
		t.Fatalf("Failed to rotate key: %v", err)
	}

	// Get new JWK
	jwk2 := manager.GetCurrentJWK()

	// Verify keys are different
	if jwk1.X == jwk2.X && jwk1.Y == jwk2.Y {
		t.Error("Key rotation didn't generate new key")
	}
}

func TestAccessTokenHash(t *testing.T) {
	token := "test-access-token"
	hash1 := HashAccessToken(token)
	hash2 := HashAccessToken(token)

	// Hashes should be consistent
	if hash1 != hash2 {
		t.Error("Hash function not consistent")
	}

	// Hash should be base64url encoded
	if strings.Contains(hash1, "+") || strings.Contains(hash1, "/") || strings.Contains(hash1, "=") {
		t.Error("Hash not properly base64url encoded")
	}

	// Different tokens should have different hashes
	hash3 := HashAccessToken("different-token")
	if hash1 == hash3 {
		t.Error("Different tokens produced same hash")
	}
}

func TestURINormalization(t *testing.T) {
	tests := []struct {
		uri1     string
		uri2     string
		expected bool
	}{
		{"https://example.com/api", "https://example.com/api", true},
		{"https://example.com/api/", "https://example.com/api", true},
		{"https://example.com:443/api", "https://example.com/api", true},
		{"http://example.com:80/api", "http://example.com/api", true},
		{"https://example.com/api", "https://example.com/different", false},
		{"https://example.com/api", "http://example.com/api", false},
	}

	for _, test := range tests {
		norm1 := normalizeURI(test.uri1)
		norm2 := normalizeURI(test.uri2)
		result := norm1 == norm2

		if result != test.expected {
			t.Errorf("URI normalization failed for %s and %s: expected %v, got %v",
				test.uri1, test.uri2, test.expected, result)
		}
	}
}