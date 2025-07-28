package oauth

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
)

// HardcodedPrivateKeyPEM is the private key that matches the public key in client-metadata.json
// In production, this should be loaded from a secure key store
const HardcodedPrivateKeyPEM = `-----BEGIN EC PRIVATE KEY-----
MHcCAQEEIERqaaHFAOyCFycC43305X979BXxIriGXx2Du1aGTZGBoAoGCCqGSM49
AwEHoUQDQgAECEpzKqfuY+lCAj07Hmu2i0Wu6GLnf7jis90IyHFVc87voqatfq3a
P83bH8pcquaxIT0cTwtY3O7u7wTYw3NSog==
-----END EC PRIVATE KEY-----`

// LoadHardcodedKey loads the hardcoded private key
func LoadHardcodedKey() (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(HardcodedPrivateKeyPEM))
	if block == nil {
		return nil, fmt.Errorf("failed to parse PEM block")
	}

	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("failed to parse EC private key: %w", err)
	}

	return key, nil
}

// GetPublicKeyJWK returns the JWK representation of the public key
func GetPublicKeyJWK(privateKey *ecdsa.PrivateKey) map[string]interface{} {
	return map[string]interface{}{
		"kty": "EC",
		"crv": "P-256",
		"x":   base64.RawURLEncoding.EncodeToString(privateKey.PublicKey.X.Bytes()),
		"y":   base64.RawURLEncoding.EncodeToString(privateKey.PublicKey.Y.Bytes()),
		"use": "sig",
		"alg": "ES256",
		"kid": "is4PQCqbnUs",
	}
}

// ParseJWKToPublicKey parses a JWK to an ECDSA public key
func ParseJWKToPublicKey(jwk map[string]interface{}) (*ecdsa.PublicKey, error) {
	xStr, ok := jwk["x"].(string)
	if !ok {
		return nil, fmt.Errorf("missing x coordinate")
	}
	
	yStr, ok := jwk["y"].(string)
	if !ok {
		return nil, fmt.Errorf("missing y coordinate")
	}
	
	xBytes, err := base64.RawURLEncoding.DecodeString(xStr)
	if err != nil {
		return nil, fmt.Errorf("failed to decode x: %w", err)
	}
	
	yBytes, err := base64.RawURLEncoding.DecodeString(yStr)
	if err != nil {
		return nil, fmt.Errorf("failed to decode y: %w", err)
	}
	
	x := new(big.Int).SetBytes(xBytes)
	y := new(big.Int).SetBytes(yBytes)
	
	return &ecdsa.PublicKey{
		Curve: elliptic.P256(),
		X:     x,
		Y:     y,
	}, nil
}