package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log"
)

func main() {
	// Generate new ECDSA key pair for ES256
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		log.Fatal("Failed to generate private key:", err)
	}

	// Export private key to PEM
	privKeyBytes, err := x509.MarshalECPrivateKey(privateKey)
	if err != nil {
		log.Fatal("Failed to marshal private key:", err)
	}

	privKeyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "EC PRIVATE KEY",
		Bytes: privKeyBytes,
	})

	// Generate JWK for public key
	jwk := map[string]interface{}{
		"kty": "EC",
		"crv": "P-256",
		"x":   base64.RawURLEncoding.EncodeToString(privateKey.PublicKey.X.Bytes()),
		"y":   base64.RawURLEncoding.EncodeToString(privateKey.PublicKey.Y.Bytes()),
		"use": "sig",
		"alg": "ES256",
		"kid": fmt.Sprintf("atchess-key-%d", privateKey.X.BitLen()),
	}

	jwkJSON, _ := json.MarshalIndent(jwk, "      ", "  ")

	fmt.Println("=== PRIVATE KEY (Keep this secret!) ===")
	fmt.Println("Save this to oauth-private-key.pem or set as OAUTH_PRIVATE_KEY environment variable:")
	fmt.Println()
	fmt.Print(string(privKeyPEM))
	fmt.Println()
	fmt.Println("=== PUBLIC KEY (Add to client-metadata.json) ===")
	fmt.Println("Replace the key in web/static/client-metadata.json with:")
	fmt.Println()
	fmt.Println("  \"jwks\": {")
	fmt.Println("    \"keys\": [")
	fmt.Printf("%s\n", jwkJSON)
	fmt.Println("    ]")
	fmt.Println("  }")
	fmt.Println()
	fmt.Println("=== IMPORTANT SECURITY NOTES ===")
	fmt.Println("1. NEVER commit the private key to version control")
	fmt.Println("2. Set appropriate file permissions (chmod 600) on the private key file")
	fmt.Println("3. Use environment variables or secure key management in production")
}