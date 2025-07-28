package oauth

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type OAuthClient struct {
	clientID     string
	redirectURI  string
	privateKey   *ecdsa.PrivateKey
	publicKeyJWK map[string]interface{}
	httpClient   *http.Client
}

// NewOAuthClient creates a new OAuth client for AT Protocol
func NewOAuthClient(clientID, redirectURI string) (*OAuthClient, error) {
	// Load the private key from file or environment
	privateKey, err := LoadPrivateKey()
	if err != nil {
		return nil, fmt.Errorf("failed to load private key: %w", err)
	}

	// Create JWK representation of public key
	publicKeyJWK := GetPublicKeyJWK(privateKey)

	return &OAuthClient{
		clientID:     clientID,
		redirectURI:  redirectURI,
		privateKey:   privateKey,
		publicKeyJWK: publicKeyJWK,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}, nil
}

// GetPublicKeyJWK returns the public key in JWK format
func (c *OAuthClient) GetPublicKeyJWK() map[string]interface{} {
	return c.publicKeyJWK
}

// GeneratePKCE creates a PKCE challenge pair
func GeneratePKCE() (verifier, challenge string, err error) {
	// Generate random bytes for verifier
	verifierBytes := make([]byte, 32)
	if _, err := rand.Read(verifierBytes); err != nil {
		return "", "", err
	}
	
	verifier = base64.RawURLEncoding.EncodeToString(verifierBytes)
	
	// Create challenge by hashing verifier
	h := sha256.New()
	h.Write([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(h.Sum(nil))
	
	return verifier, challenge, nil
}

// BuildAuthorizationURL constructs the authorization URL
func (c *OAuthClient) BuildAuthorizationURL(authEndpoint, handle, state, codeChallenge string) string {
	params := url.Values{}
	params.Set("response_type", "code")
	params.Set("client_id", c.clientID)
	params.Set("redirect_uri", c.redirectURI)
	params.Set("state", state)
	params.Set("scope", "atproto transition:generic")
	params.Set("code_challenge", codeChallenge)
	params.Set("code_challenge_method", "S256")
	
	// Include login_hint if handle is provided
	if handle != "" {
		params.Set("login_hint", handle)
	}
	
	return authEndpoint + "?" + params.Encode()
}

// CreateClientAssertion creates a JWT client assertion for token requests
func (c *OAuthClient) CreateClientAssertion(tokenEndpoint string) (string, error) {
	now := time.Now()
	claims := jwt.MapClaims{
		"iss": c.clientID,
		"sub": c.clientID,
		"aud": tokenEndpoint,
		"iat": now.Unix(),
		"exp": now.Add(5 * time.Minute).Unix(),
		"jti": generateJTI(),
	}
	
	token := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	token.Header["kid"] = "is4PQCqbnUs" // Must match the kid in our JWKS
	
	signedToken, err := token.SignedString(c.privateKey)
	if err != nil {
		return "", fmt.Errorf("failed to sign assertion: %w", err)
	}
	
	return signedToken, nil
}

// ExchangeCodeForTokens exchanges an authorization code for tokens
func (c *OAuthClient) ExchangeCodeForTokens(tokenEndpoint, code, codeVerifier string, dpopKey *ecdsa.PrivateKey) (*TokenResponse, error) {
	// Create client assertion
	clientAssertion, err := c.CreateClientAssertion(tokenEndpoint)
	if err != nil {
		return nil, err
	}
	
	// Try up to 2 times (initial + 1 retry with nonce)
	var nonce string
	for attempt := 0; attempt < 2; attempt++ {
		// Prepare request
		data := url.Values{}
		data.Set("grant_type", "authorization_code")
		data.Set("code", code)
		data.Set("redirect_uri", c.redirectURI)
		data.Set("code_verifier", codeVerifier)
		data.Set("client_id", c.clientID)
		data.Set("client_assertion_type", "urn:ietf:params:oauth:client-assertion-type:jwt-bearer")
		data.Set("client_assertion", clientAssertion)
		
		req, err := http.NewRequest("POST", tokenEndpoint, strings.NewReader(data.Encode()))
		if err != nil {
			return nil, err
		}
		
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		
		// Add DPoP header if key provided
		if dpopKey != nil {
			dpopToken, err := createDPoPToken(dpopKey, "POST", tokenEndpoint, "", nonce)
			if err != nil {
				return nil, err
			}
			req.Header.Set("DPoP", dpopToken)
		}
		
		resp, err := c.httpClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		
		// Check for DPoP nonce requirement
		if resp.StatusCode == http.StatusBadRequest {
			body, _ := io.ReadAll(resp.Body)
			var errorResp struct {
				Error string `json:"error"`
				ErrorDescription string `json:"error_description"`
			}
			if err := json.Unmarshal(body, &errorResp); err == nil && errorResp.Error == "use_dpop_nonce" {
				// Extract nonce from DPoP-Nonce header and retry
				if newNonce := resp.Header.Get("DPoP-Nonce"); newNonce != "" && attempt == 0 {
					nonce = newNonce
					continue // Retry with nonce
				}
			}
			return nil, fmt.Errorf("token exchange failed: HTTP %d - %s", resp.StatusCode, string(body))
		}
		
		if resp.StatusCode != http.StatusOK {
			// Read error response
			body, _ := io.ReadAll(resp.Body)
			return nil, fmt.Errorf("token exchange failed: HTTP %d - %s", resp.StatusCode, string(body))
		}
		
		var tokenResp TokenResponse
		if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
			return nil, err
		}
		
		return &tokenResp, nil
	}
	
	return nil, fmt.Errorf("token exchange failed after retries")
}

// TokenResponse represents the OAuth token response
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
	Sub          string `json:"sub"`
}

// Helper functions

func generateKID(publicKey *ecdsa.PublicKey) string {
	// Create a deterministic key ID from public key
	keyBytes, _ := x509.MarshalPKIXPublicKey(publicKey)
	h := sha256.Sum256(keyBytes)
	return base64.RawURLEncoding.EncodeToString(h[:8])
}

func generateJTI() string {
	// Generate random JWT ID
	b := make([]byte, 16)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func createDPoPToken(privateKey *ecdsa.PrivateKey, method, uri, accessToken string, nonce string) (string, error) {
	now := time.Now()
	
	// Create DPoP JWT
	claims := jwt.MapClaims{
		"jti": generateJTI(),
		"htm": method,
		"htu": uri,
		"iat": now.Unix(),
		"exp": now.Add(5 * time.Minute).Unix(),
	}
	
	// Add nonce if provided (required by some servers)
	if nonce != "" {
		claims["nonce"] = nonce
	}
	
	// Add access token hash if provided
	if accessToken != "" {
		h := sha256.Sum256([]byte(accessToken))
		claims["ath"] = base64.RawURLEncoding.EncodeToString(h[:])
	}
	
	token := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	
	// Add JWK to header
	token.Header["typ"] = "dpop+jwt"
	token.Header["jwk"] = map[string]interface{}{
		"kty": "EC",
		"crv": "P-256",
		"x":   base64.RawURLEncoding.EncodeToString(privateKey.PublicKey.X.Bytes()),
		"y":   base64.RawURLEncoding.EncodeToString(privateKey.PublicKey.Y.Bytes()),
	}
	
	return token.SignedString(privateKey)
}

// GenerateState creates a random state parameter
func GenerateState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}