package auth

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
)

// JWTHeader represents the JWT header
type JWTHeader struct {
	Algorithm string                 `json:"alg"`
	Type      string                 `json:"typ"`
	JWK       *JWK                   `json:"jwk,omitempty"`
	Extra     map[string]interface{} `json:"-"`
}

// JWK represents a JSON Web Key
type JWK struct {
	KeyType string `json:"kty"`
	Curve   string `json:"crv"`
	X       string `json:"x"`
	Y       string `json:"y"`
}

// JWTClaims represents the JWT claims
type JWTClaims struct {
	// Standard claims
	Issuer    string `json:"iss,omitempty"`
	Subject   string `json:"sub,omitempty"`
	Audience  string `json:"aud,omitempty"`
	ExpiresAt int64  `json:"exp,omitempty"`
	NotBefore int64  `json:"nbf,omitempty"`
	IssuedAt  int64  `json:"iat,omitempty"`
	JTI       string `json:"jti,omitempty"`
	
	// DPoP specific claims
	HTTPMethod  string `json:"htm,omitempty"`
	HTTPURI     string `json:"htu,omitempty"`
	AccessToken string `json:"ath,omitempty"` // SHA256 hash of access token
	
	// Additional claims
	Extra map[string]interface{} `json:"-"`
}

// MarshalJSON implements custom JSON marshaling for JWTHeader
func (h *JWTHeader) MarshalJSON() ([]byte, error) {
	// Create a map with the standard fields
	m := map[string]interface{}{
		"alg": h.Algorithm,
		"typ": h.Type,
	}
	
	if h.JWK != nil {
		m["jwk"] = h.JWK
	}
	
	// Add extra fields
	for k, v := range h.Extra {
		m[k] = v
	}
	
	return json.Marshal(m)
}

// MarshalJSON implements custom JSON marshaling for JWTClaims
func (c *JWTClaims) MarshalJSON() ([]byte, error) {
	// Create a map with all fields
	m := make(map[string]interface{})
	
	if c.Issuer != "" {
		m["iss"] = c.Issuer
	}
	if c.Subject != "" {
		m["sub"] = c.Subject
	}
	if c.Audience != "" {
		m["aud"] = c.Audience
	}
	if c.ExpiresAt > 0 {
		m["exp"] = c.ExpiresAt
	}
	if c.NotBefore > 0 {
		m["nbf"] = c.NotBefore
	}
	if c.IssuedAt > 0 {
		m["iat"] = c.IssuedAt
	}
	if c.JTI != "" {
		m["jti"] = c.JTI
	}
	if c.HTTPMethod != "" {
		m["htm"] = c.HTTPMethod
	}
	if c.HTTPURI != "" {
		m["htu"] = c.HTTPURI
	}
	if c.AccessToken != "" {
		m["ath"] = c.AccessToken
	}
	
	// Add extra fields
	for k, v := range c.Extra {
		m[k] = v
	}
	
	return json.Marshal(m)
}

// GenerateES256KeyPair generates a new ECDSA P-256 key pair
func GenerateES256KeyPair() (*ecdsa.PrivateKey, error) {
	return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
}

// PrivateKeyToJWK converts an ECDSA private key to a JWK (public key only)
func PrivateKeyToJWK(key *ecdsa.PrivateKey) (*JWK, error) {
	if key.Curve != elliptic.P256() {
		return nil, fmt.Errorf("unsupported curve, expected P-256")
	}
	
	// Convert coordinates to base64url
	xBytes := key.PublicKey.X.Bytes()
	yBytes := key.PublicKey.Y.Bytes()
	
	// Pad to 32 bytes if necessary (P-256 coordinates are 32 bytes)
	if len(xBytes) < 32 {
		padded := make([]byte, 32)
		copy(padded[32-len(xBytes):], xBytes)
		xBytes = padded
	}
	if len(yBytes) < 32 {
		padded := make([]byte, 32)
		copy(padded[32-len(yBytes):], yBytes)
		yBytes = padded
	}
	
	return &JWK{
		KeyType: "EC",
		Curve:   "P-256",
		X:       base64URLEncode(xBytes),
		Y:       base64URLEncode(yBytes),
	}, nil
}

// CreateJWT creates and signs a JWT with ES256
func CreateJWT(header *JWTHeader, claims *JWTClaims, privateKey *ecdsa.PrivateKey) (string, error) {
	// Encode header
	headerJSON, err := json.Marshal(header)
	if err != nil {
		return "", fmt.Errorf("failed to marshal header: %w", err)
	}
	headerEncoded := base64URLEncode(headerJSON)
	
	// Encode claims
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("failed to marshal claims: %w", err)
	}
	claimsEncoded := base64URLEncode(claimsJSON)
	
	// Create signing input
	signingInput := headerEncoded + "." + claimsEncoded
	
	// Sign with ES256
	hash := sha256.Sum256([]byte(signingInput))
	r, s, err := ecdsa.Sign(rand.Reader, privateKey, hash[:])
	if err != nil {
		return "", fmt.Errorf("failed to sign: %w", err)
	}
	
	// Convert signature to bytes (r and s are 32 bytes each for P-256)
	rBytes := r.Bytes()
	sBytes := s.Bytes()
	
	// Pad to 32 bytes if necessary
	if len(rBytes) < 32 {
		padded := make([]byte, 32)
		copy(padded[32-len(rBytes):], rBytes)
		rBytes = padded
	}
	if len(sBytes) < 32 {
		padded := make([]byte, 32)
		copy(padded[32-len(sBytes):], sBytes)
		sBytes = padded
	}
	
	// Concatenate r and s
	signature := append(rBytes, sBytes...)
	signatureEncoded := base64URLEncode(signature)
	
	// Create JWT
	jwt := signingInput + "." + signatureEncoded
	
	return jwt, nil
}

// VerifyJWT verifies a JWT signature using the embedded JWK
func VerifyJWT(token string) (*JWTHeader, *JWTClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, nil, fmt.Errorf("invalid JWT format")
	}
	
	// Decode header
	headerJSON, err := base64URLDecode(parts[0])
	if err != nil {
		return nil, nil, fmt.Errorf("failed to decode header: %w", err)
	}
	
	var header JWTHeader
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		return nil, nil, fmt.Errorf("failed to unmarshal header: %w", err)
	}
	
	// Verify algorithm
	if header.Algorithm != "ES256" {
		return nil, nil, fmt.Errorf("unsupported algorithm: %s", header.Algorithm)
	}
	
	// Extract public key from JWK
	if header.JWK == nil {
		return nil, nil, fmt.Errorf("missing JWK in header")
	}
	
	publicKey, err := JWKToPublicKey(header.JWK)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to extract public key: %w", err)
	}
	
	// Decode claims
	claimsJSON, err := base64URLDecode(parts[1])
	if err != nil {
		return nil, nil, fmt.Errorf("failed to decode claims: %w", err)
	}
	
	var claims JWTClaims
	if err := json.Unmarshal(claimsJSON, &claims); err != nil {
		return nil, nil, fmt.Errorf("failed to unmarshal claims: %w", err)
	}
	
	// Decode signature
	signature, err := base64URLDecode(parts[2])
	if err != nil {
		return nil, nil, fmt.Errorf("failed to decode signature: %w", err)
	}
	
	if len(signature) != 64 {
		return nil, nil, fmt.Errorf("invalid signature length: expected 64 bytes, got %d", len(signature))
	}
	
	// Extract r and s from signature
	r := new(big.Int).SetBytes(signature[:32])
	s := new(big.Int).SetBytes(signature[32:])
	
	// Verify signature
	signingInput := parts[0] + "." + parts[1]
	hash := sha256.Sum256([]byte(signingInput))
	
	if !ecdsa.Verify(publicKey, hash[:], r, s) {
		return nil, nil, fmt.Errorf("signature verification failed")
	}
	
	return &header, &claims, nil
}

// JWKToPublicKey converts a JWK to an ECDSA public key
func JWKToPublicKey(jwk *JWK) (*ecdsa.PublicKey, error) {
	if jwk.KeyType != "EC" || jwk.Curve != "P-256" {
		return nil, fmt.Errorf("unsupported key type or curve")
	}
	
	xBytes, err := base64URLDecode(jwk.X)
	if err != nil {
		return nil, fmt.Errorf("failed to decode x coordinate: %w", err)
	}
	
	yBytes, err := base64URLDecode(jwk.Y)
	if err != nil {
		return nil, fmt.Errorf("failed to decode y coordinate: %w", err)
	}
	
	x := new(big.Int).SetBytes(xBytes)
	y := new(big.Int).SetBytes(yBytes)
	
	return &ecdsa.PublicKey{
		Curve: elliptic.P256(),
		X:     x,
		Y:     y,
	}, nil
}

// HashAccessToken creates a SHA256 hash of an access token for the 'ath' claim
func HashAccessToken(accessToken string) string {
	hash := sha256.Sum256([]byte(accessToken))
	return base64URLEncode(hash[:])
}

// GenerateJTI generates a unique JWT ID
func GenerateJTI() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64URLEncode(b), nil
}

// base64URLEncode encodes data to base64url format (no padding)
func base64URLEncode(data []byte) string {
	return strings.TrimRight(base64.URLEncoding.EncodeToString(data), "=")
}

// base64URLDecode decodes data from base64url format
func base64URLDecode(s string) ([]byte, error) {
	// Add padding if necessary
	switch len(s) % 4 {
	case 2:
		s += "=="
	case 3:
		s += "="
	}
	return base64.URLEncoding.DecodeString(s)
}