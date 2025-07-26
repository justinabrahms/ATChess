# DPoP (Demonstrating Proof of Possession) Implementation for AT Protocol

This package provides a complete DPoP implementation following RFC 9449 and AT Protocol requirements.

## Features

- ES256 (ECDSA P-256) key pair generation and management
- DPoP proof JWT creation with all required claims (jti, htm, htu, iat, ath)
- Secure in-memory key storage with rotation support
- JWK embedding in JWT headers
- Access token hashing for the 'ath' claim
- HTTP client integration with automatic DPoP header injection
- Proof validation and replay protection

## Usage

### Basic DPoP Manager

```go
// Create a new DPoP manager
manager, err := auth.NewDPoPManager()
if err != nil {
    log.Fatal(err)
}

// Create a DPoP proof for a request
proof, err := manager.CreateProof("POST", "https://bsky.social/xrpc/com.atproto.repo.createRecord", accessToken)
if err != nil {
    log.Fatal(err)
}

// Add DPoP header to an HTTP request
req, _ := http.NewRequest("POST", "https://bsky.social/xrpc/com.atproto.repo.createRecord", body)
err = manager.AddDPoPHeader(req, accessToken)
```

### HTTP Client with Automatic DPoP

```go
// Create a DPoP-enabled HTTP client
dpopClient := auth.NewDPoPClient(manager, func() string {
    return currentAccessToken
})

// Use the client - DPoP headers are added automatically
req, _ := http.NewRequest("POST", "https://bsky.social/xrpc/com.atproto.repo.createRecord", body)
req.Header.Set("Authorization", "DPoP " + accessToken)
resp, err := dpopClient.Do(req)
```

### Integration with AT Protocol Client

See `example_integration.go` for a complete example of integrating DPoP into an AT Protocol client.

```go
// Create a DPoP-enabled AT Protocol client
client, err := auth.NewDPoPEnabledClient("https://bsky.social", "alice.bsky.social", "password")
if err != nil {
    log.Fatal(err)
}

// Create records with automatic DPoP protection
uri, err := client.CreateRecord("app.bsky.feed.post", map[string]interface{}{
    "$type": "app.bsky.feed.post",
    "text": "Hello with DPoP!",
    "createdAt": time.Now().Format(time.RFC3339),
})
```

### Key Rotation

```go
// Rotate keys if older than 24 hours
err := manager.RotateKeyIfNeeded(24 * time.Hour)

// Get current public key as JWK
jwk := manager.GetCurrentJWK()
```

### Proof Validation

```go
// Validate a DPoP proof
err := auth.ValidateProof(dpopProof, "POST", "https://bsky.social/xrpc/com.atproto.repo.createRecord", accessToken)
if err != nil {
    // Proof is invalid
    log.Printf("Invalid proof: %v", err)
}
```

## DPoP JWT Structure

The implementation creates DPoP JWTs with the following structure:

### Header
```json
{
  "alg": "ES256",
  "typ": "dpop+jwt",
  "jwk": {
    "kty": "EC",
    "crv": "P-256",
    "x": "base64url-encoded-x-coordinate",
    "y": "base64url-encoded-y-coordinate"
  }
}
```

### Payload
```json
{
  "jti": "unique-identifier",
  "htm": "POST",
  "htu": "https://bsky.social/xrpc/com.atproto.repo.createRecord",
  "iat": 1234567890,
  "ath": "base64url-encoded-sha256-hash-of-access-token"
}
```

## Security Considerations

1. **Key Storage**: Keys are stored in memory only. For production use, consider secure key storage solutions.

2. **Key Rotation**: Implement regular key rotation (e.g., every 24 hours) to limit exposure.

3. **Replay Protection**: The implementation tracks used JTIs to prevent replay attacks within a 10-minute window.

4. **Clock Skew**: The validation allows for 30 seconds of clock skew to handle time synchronization issues.

5. **HTTPS Only**: DPoP should only be used over HTTPS connections.

## Testing

Run the comprehensive test suite:

```bash
go test ./internal/auth -v
```

The tests cover:
- DPoP proof creation and validation
- JWT signing and verification
- Key rotation
- Access token hashing
- URI normalization
- HTTP client integration