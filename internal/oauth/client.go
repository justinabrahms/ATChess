package oauth

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/rs/zerolog/log"

	"github.com/justinabrahms/atchess/internal/dpop"
)

// Scope is the OAuth scope ATChess requests. It is a CONSTANT because it was
// three separate string literals -- the client metadata we advertise, the
// authorization request, and the token exchange -- and those three drifting
// apart is a silent failure: you either break sign-in, or request more than
// you told the user you would.
//
// WHY IT IS THIS BROAD, which is a fair thing to be unhappy about.
//
// `transition:generic` is AT Protocol's transitional catch-all. On the consent
// screen it reads as "manage your profile, posts, likes and follows", "create,
// update, and delete any public data linked to your account", and "perform
// authenticated actions towards any service on your behalf". A chess app needs
// none of that. It needs to write app.atchess.* records into the signed-in
// user's repository and read them back.
//
// There is currently no narrower option. Measured 2026-08-30, both
// eurosky.social and bsky.social advertise exactly:
//
//	scopes_supported: ["atproto", "transition:email",
//	                   "transition:generic", "transition:chat.bsky"]
//
// `atproto` alone is identity and grants no repo write, and games live in the
// player's own repository -- that is the entire architecture, not an
// implementation detail. So `transition:generic` is the minimum that works,
// and the granular permissions that would let this be `repo:app.atchess.game`
// and friends are not deployed anywhere in the ecosystem yet.
//
// WHAT WE DELIBERATELY DO NOT REQUEST: `transition:email` and
// `transition:chat.bsky`. Both are offered and neither is needed.
// TestScopeRequestsNoMoreThanNeeded fails if either appears.
//
// WHEN GRANULAR SCOPES SHIP: re-check scopes_supported on a real PDS, narrow
// this to the app.atchess.* collections, and keep a fallback for servers still
// advertising only the transitional set.
const Scope = "atproto transition:generic"

type OAuthClient struct {
	clientID     string
	redirectURI  string
	privateKey   *ecdsa.PrivateKey
	publicKeyJWK map[string]interface{}
	httpClient   *http.Client

	// nonceStore caches DPoP nonces per authorization-server origin. It is
	// the same process-wide store internal/atproto uses for resource-
	// server (PDS) requests on OAuth-bound sessions -- see
	// dpop.DefaultNonceStore's doc comment for why a single shared store
	// is required rather than one per client instance.
	nonceStore *dpop.NonceStore
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
		nonceStore: dpop.DefaultNonceStore(),
	}, nil
}

// GetPublicKeyJWK returns the public key in JWK format
func (c *OAuthClient) GetPublicKeyJWK() map[string]interface{} {
	return c.publicKeyJWK
}

// nonces returns c.nonceStore, falling back to the process-wide default
// store when c.nonceStore is nil (e.g. an *OAuthClient built as a struct
// literal, as the conformance tests in this package do, rather than via
// NewOAuthClient). Never returns nil.
func (c *OAuthClient) nonces() *dpop.NonceStore {
	if c.nonceStore == nil {
		return dpop.DefaultNonceStore()
	}
	return c.nonceStore
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
	params.Set("scope", Scope)
	params.Set("code_challenge", codeChallenge)
	params.Set("code_challenge_method", "S256")

	// Include login_hint if handle is provided
	if handle != "" {
		params.Set("login_hint", handle)
	}

	return authEndpoint + "?" + params.Encode()
}

// CreateClientAssertion creates a JWT client assertion for token requests
func (c *OAuthClient) CreateClientAssertion(issuer string) (string, error) {
	now := time.Now()
	claims := jwt.MapClaims{
		"iss": c.clientID,
		"sub": c.clientID,
		"aud": issuer, // AT Protocol expects the issuer URL, not the token endpoint
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

// ExchangeCodeForTokens exchanges an authorization code for tokens.
func (c *OAuthClient) ExchangeCodeForTokens(tokenEndpoint, issuer, code, codeVerifier string, dpopKey *ecdsa.PrivateKey) (*TokenResponse, error) {
	clientAssertion, err := c.CreateClientAssertion(issuer)
	if err != nil {
		return nil, err
	}

	resp, body, err := c.postFormWithDPoPRetry(tokenEndpoint, dpopKey, "", func() url.Values {
		data := url.Values{}
		data.Set("grant_type", "authorization_code")
		data.Set("code", code)
		data.Set("redirect_uri", c.redirectURI)
		data.Set("code_verifier", codeVerifier)
		data.Set("client_id", c.clientID)
		data.Set("client_assertion_type", "urn:ietf:params:oauth:client-assertion-type:jwt-bearer")
		data.Set("client_assertion", clientAssertion)
		return data
	})
	if err != nil {
		return nil, fmt.Errorf("token exchange failed: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token exchange failed: HTTP %d - %s", resp.StatusCode, string(body))
	}

	var tokenResp TokenResponse
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return nil, err
	}
	return &tokenResp, nil
}

// PushAuthorizationRequest performs an RFC 9126 Pushed Authorization
// Request: it POSTs the full set of authorization parameters (client_id,
// redirect_uri, response_type, scope, state, code_challenge,
// code_challenge_method, and login_hint when handle is non-empty), signed
// with a client assertion the same way the token endpoint is authenticated,
// to parEndpoint, and returns the request_uri the server hands back for use
// in the subsequent /authorize redirect (see
// BuildAuthorizationURLFromRequestURI). issuer is used as the "aud" of the
// client assertion, matching ExchangeCodeForTokens' convention -- for PAR
// this is the authorization server URL resolved via the resource/
// authorization-server metadata chain (there is no OAuth "iss" callback
// parameter yet at this point in the flow, since no redirect has happened).
//
// dpopKey, when non-nil, DPoP-binds the PAR request (and, from here
// onward, every token this authorization eventually yields) the same way
// ExchangeCodeForTokens does, including the same nonce-retry behaviour.
//
// Failures originating from the PAR exchange itself are returned as a
// *PARError, carrying the HTTP status the authorization server responded
// with (or 0 if the request never got a response at all -- a network error
// or timeout). Failures BEFORE or AFTER that exchange -- building the
// client assertion, decoding the response body, a 201 with no request_uri
// -- are returned as plain errors, which BuildAuthorizationURLAuto treats
// as non-transient and hard-fails on. See
// PARError.Transient and BuildAuthorizationURLAuto's doc comment for why
// callers need to be able to tell those apart (atchess-1c9.86).
func (c *OAuthClient) PushAuthorizationRequest(parEndpoint, issuer, handle, state, codeChallenge string, dpopKey *ecdsa.PrivateKey) (requestURI string, err error) {
	clientAssertion, err := c.CreateClientAssertion(issuer)
	if err != nil {
		return "", err
	}

	resp, body, err := c.postFormWithDPoPRetry(parEndpoint, dpopKey, "", func() url.Values {
		data := url.Values{}
		data.Set("response_type", "code")
		data.Set("client_id", c.clientID)
		data.Set("redirect_uri", c.redirectURI)
		data.Set("state", state)
		data.Set("scope", Scope)
		data.Set("code_challenge", codeChallenge)
		data.Set("code_challenge_method", "S256")
		if handle != "" {
			data.Set("login_hint", handle)
		}
		data.Set("client_assertion_type", "urn:ietf:params:oauth:client-assertion-type:jwt-bearer")
		data.Set("client_assertion", clientAssertion)
		return data
	})
	if err != nil {
		// resp can still be non-nil here even though err != nil (e.g.
		// postFormWithDPoPRetry's "server requires a DPoP nonce but
		// didn't supply one" case, which returns both) -- prefer its
		// status code when present so PARError.Transient() is exact; 0
		// (no response reached at all -- a network error/timeout) is
		// the correct default for genuinely response-less failures and
		// is itself transient.
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		return "", &PARError{StatusCode: status, Err: fmt.Errorf("pushed authorization request failed: %w", err)}
	}
	// RFC 9126 s2.2: a successful PAR response is 201 Created. Some
	// deployments have been observed returning 200; accept both, reject
	// everything else.
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return "", &PARError{
			StatusCode: resp.StatusCode,
			Err:        fmt.Errorf("pushed authorization request failed: HTTP %d - %s", resp.StatusCode, string(body)),
		}
	}

	var parResp struct {
		RequestURI string `json:"request_uri"`
		ExpiresIn  int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &parResp); err != nil {
		return "", fmt.Errorf("decoding PAR response: %w", err)
	}
	if parResp.RequestURI == "" {
		return "", fmt.Errorf("PAR response did not include a request_uri: %s", string(body))
	}
	return parResp.RequestURI, nil
}

// PARError describes why a Pushed Authorization Request
// (PushAuthorizationRequest) failed, carrying enough information for a
// caller to decide whether the failure could plausibly be worked around
// (e.g. by falling back to a plain, non-PAR /authorize URL) or not. See
// BuildAuthorizationURLAuto's doc comment for the policy this type exists
// to support (atchess-1c9.86).
type PARError struct {
	// StatusCode is the HTTP status the authorization server's PAR
	// endpoint returned, or 0 if the request never reached a response at
	// all (a network error or timeout talking to the endpoint).
	StatusCode int
	Err        error
}

func (e *PARError) Error() string { return e.Err.Error() }
func (e *PARError) Unwrap() error { return e.Err }

// Transient reports whether this PAR failure is the kind a fresh attempt
// -- or a fallback to a differently-shaped request -- could plausibly
// overcome: no response was received at all (StatusCode == 0: a network
// error or timeout), or the authorization server itself failed (a 5xx).
// It is NOT transient for most 4xx: the authorization server received and
// understood the request well enough to actively reject it, so retrying
// -- or falling back to the plain /authorize URL, which is a differently
// shaped but still fallible request -- would likely be rejected the same
// way.
//
// 408 and 429 are the exceptions, and they matter. Neither says anything
// about our request being WRONG: 408 is a timeout the server noticed
// before we did, and 429 means "correct, but not right now". Treating
// them as fatal would hard-fail a login for exactly the reason the
// require=false fallback exists to survive, and /authorize is typically
// rate-limited separately from the PAR endpoint. Falling back may simply
// relocate a 429 to the redirect, but that is a cosmetic loss where
// hard-failing is a real one.
func (e *PARError) Transient() bool {
	if e.StatusCode == http.StatusRequestTimeout || e.StatusCode == http.StatusTooManyRequests {
		return true
	}
	return e.StatusCode == 0 || e.StatusCode >= 500
}

// BuildAuthorizationURLFromRequestURI builds the /authorize redirect target
// for a request_uri obtained via PushAuthorizationRequest. Per RFC 9126
// s4, only client_id and request_uri belong on this URL -- every other
// authorization parameter was already delivered (and is now referenced
// opaquely) via the PAR call, and must NOT be duplicated here.
func (c *OAuthClient) BuildAuthorizationURLFromRequestURI(authEndpoint, requestURI string) string {
	params := url.Values{}
	params.Set("client_id", c.clientID)
	params.Set("request_uri", requestURI)
	return authEndpoint + "?" + params.Encode()
}

// BuildAuthorizationURLAuto selects between a Pushed Authorization Request
// (RFC 9126) and the plain query-parameter authorization URL, based purely
// on whether parEndpoint is non-empty -- the caller is expected to pass an
// authorization server's advertised pushed_authorization_request_endpoint
// (empty string if it did not advertise one, atchess-1c9.12's explicit
// "use PAR only when the server advertises a PAR endpoint" rule). issuer
// is used as the client assertion's "aud" for the PAR call; it is unused
// when parEndpoint is empty.
//
// requirePAR should be the authorization server's advertised
// require_pushed_authorization_requests metadata field, and governs what
// happens when a PAR attempt (parEndpoint != "") fails -- this is
// atchess-1c9.86's PAR-FAILURE POLICY, decided deliberately rather than
// left as the previous unconditional hard-fail:
//
//   - requirePAR == true: HARD-FAIL on ANY PushAuthorizationRequest error,
//     transient or not. A fallback to the plain /authorize URL would be
//     pointless here -- the server has told us via metadata that it
//     REQUIRES PAR, so it will, by definition, reject a plain
//     authorization request too. Falling back would just move an
//     already-certain failure somewhere less legible (a confusing
//     authorize-endpoint rejection instead of a clear PAR error).
//   - requirePAR == false AND the failure is transient (*PARError with
//     Transient() == true: a network error/timeout, or the authorization
//     server itself erroring with a 5xx): FALL BACK to the plain
//     /authorize URL, logged at warn. The server accepts a plain request
//     by definition when it doesn't require PAR, and a momentary PAR
//     outage on the server's end shouldn't block a login that would
//     otherwise work.
//   - requirePAR == false AND the failure is NOT transient (a 4xx, or any
//     other error shape): HARD-FAIL. A 4xx means the authorization server
//     understood our PAR request well enough to actively refuse it --
//     that points at a defect in what we're sending, not at PAR being
//     temporarily unavailable, and the plain /authorize URL would likely
//     be built from the same (wrong) parameters and get refused too.
//     Falling back would silently mask that defect instead of surfacing
//     it -- exactly the class of masking atchess-1c9.76 and .85 were
//     about.
//
// In short: fall back only when availability -- not correctness -- is in
// question, and only when the server's own metadata says a fallback could
// possibly succeed.
//
// This is the single production entry point atchess-1c9.12's login handler
// (internal/web's OAuthLoginHandler) uses, and is exercised directly (with
// both a populated and an empty parEndpoint, and all four requirePAR x
// transient/4xx combinations) by this package's own unit tests -- see
// client_par_test.go -- rather than only indirectly through internal/web,
// since a full web-layer test would additionally require mocking the
// handle/DID/PDS resolution chain PAR selection itself does not depend on.
func (c *OAuthClient) BuildAuthorizationURLAuto(authEndpoint, parEndpoint, issuer, handle, state, codeChallenge string, dpopKey *ecdsa.PrivateKey, requirePAR bool) (string, error) {
	if parEndpoint == "" {
		return c.BuildAuthorizationURL(authEndpoint, handle, state, codeChallenge), nil
	}
	requestURI, err := c.PushAuthorizationRequest(parEndpoint, issuer, handle, state, codeChallenge, dpopKey)
	if err != nil {
		if !requirePAR {
			var parErr *PARError
			if errors.As(err, &parErr) && parErr.Transient() {
				log.Warn().Err(err).Str("parEndpoint", parEndpoint).
					Msg("Pushed Authorization Request failed transiently and the authorization server does not require PAR; falling back to a plain (non-PAR) authorization URL")
				return c.BuildAuthorizationURL(authEndpoint, handle, state, codeChallenge), nil
			}
		}
		return "", err
	}
	return c.BuildAuthorizationURLFromRequestURI(authEndpoint, requestURI), nil
}

// RefreshTokens exchanges a refresh_token for a new access/refresh token
// pair via the OAuth refresh_token grant, DPoP-bound with dpopKey. dpopKey
// must be the SAME key that was used to obtain the tokens being refreshed
// -- DPoP binds a refresh token to the proof key that first requested it
// for the token's entire lifetime, not just its initial issuance -- so
// callers must persist and reuse the original session's DPoP key rather
// than generating a new one per refresh.
func (c *OAuthClient) RefreshTokens(tokenEndpoint, issuer, refreshToken string, dpopKey *ecdsa.PrivateKey) (*TokenResponse, error) {
	if refreshToken == "" {
		return nil, fmt.Errorf("refresh token is empty")
	}
	clientAssertion, err := c.CreateClientAssertion(issuer)
	if err != nil {
		return nil, err
	}

	resp, body, err := c.postFormWithDPoPRetry(tokenEndpoint, dpopKey, "", func() url.Values {
		data := url.Values{}
		data.Set("grant_type", "refresh_token")
		data.Set("refresh_token", refreshToken)
		data.Set("client_id", c.clientID)
		data.Set("client_assertion_type", "urn:ietf:params:oauth:client-assertion-type:jwt-bearer")
		data.Set("client_assertion", clientAssertion)
		return data
	})
	if err != nil {
		return nil, fmt.Errorf("refresh_token request failed: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("refresh_token request failed: HTTP %d - %s", resp.StatusCode, string(body))
	}

	var tokenResp TokenResponse
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return nil, err
	}
	return &tokenResp, nil
}

// postFormWithDPoPRetry POSTs an application/x-www-form-urlencoded body
// (rebuilt via buildData on every attempt, so callers whose form includes a
// nonce-independent value can keep it constant, though none of this
// package's callers currently need per-attempt variation) to endpoint.
//
// When dpopKey is nil, the request carries no DPoP header at all -- the
// server is assumed not to require it.
//
// When dpopKey is non-nil, every attempt is DPoP-signed. The shared
// process-wide nonce store (see DefaultNonceStore) is consulted before the
// FIRST attempt, so a server whose nonce this process already knows gets
// it on the very first request rather than a guaranteed-to-fail one
// (atchess-1c9.12 step 4 -- "so the common path is one request not two").
// Every response, success or failure, that carries a DPoP-Nonce header
// updates the store for endpoint's origin -- including on the first,
// nonce-less attempt -- which is what keeps a nonce that rotates on every
// response (atchess-1c9.12's first edge case) from ever going stale in the
// store for longer than one request.
//
// If the server responds with the AS-side nonce challenge shape (RFC 9449
// s8.2: HTTP 400 + JSON body {"error":"use_dpop_nonce"}), this retries
// EXACTLY ONCE with a freshly-signed proof carrying the nonce from that
// response's DPoP-Nonce header. A 400 use_dpop_nonce response that (against
// spec) carries no DPoP-Nonce header is NOT retried -- there is nothing to
// retry with -- and is returned as an error immediately (atchess-1c9.12's
// second edge case: "do not loop"). A second consecutive nonce challenge
// (e.g. a nonce that rotated again between the retry being built and
// received) is likewise not retried again; its (still fresh) nonce was
// already captured into the store above for the NEXT call to pick up.
//
// accessToken, when non-empty, is included as the DPoP proof's "ath" claim
// (RFC 9449 s4.3) -- relevant for resource-server requests carrying a
// bearer/DPoP access token; none of this package's own callers (PAR, token
// exchange, refresh) have an access token yet, so they all pass "".
func (c *OAuthClient) postFormWithDPoPRetry(endpoint string, dpopKey *ecdsa.PrivateKey, accessToken string, buildData func() url.Values) (*http.Response, []byte, error) {
	origin := dpop.OriginOf(endpoint)
	var nonce string
	if dpopKey != nil {
		nonce = c.nonces().Get(origin)
	}

	var lastResp *http.Response
	var lastBody []byte

	for attempt := 0; attempt < 2; attempt++ {
		data := buildData()
		req, err := http.NewRequest("POST", endpoint, strings.NewReader(data.Encode()))
		if err != nil {
			return nil, nil, err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

		if dpopKey != nil {
			// Fresh iat on every attempt (including the retry) -- this is
			// also this package's answer to the "clock skew" edge case:
			// a proof is never reused past the moment it was signed, so
			// the window between signing and the server evaluating "iat"
			// is always as small as network latency allows, rather than
			// being inflated by a prior failed attempt's round trip.
			proof, perr := createDPoPToken(dpopKey, "POST", endpoint, accessToken, nonce)
			if perr != nil {
				return nil, nil, perr
			}
			req.Header.Set("DPoP", proof)
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return nil, nil, err
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		lastResp, lastBody = resp, body

		if dpopKey != nil {
			if newNonce := resp.Header.Get("DPoP-Nonce"); newNonce != "" {
				c.nonces().Set(origin, newNonce)
			}
		}

		if resp.StatusCode == http.StatusBadRequest && dpopKey != nil && attempt == 0 {
			var errorResp struct {
				Error string `json:"error"`
			}
			if json.Unmarshal(body, &errorResp) == nil && errorResp.Error == "use_dpop_nonce" {
				challengeNonce := resp.Header.Get("DPoP-Nonce")
				if challengeNonce == "" {
					return resp, body, fmt.Errorf("server requires a DPoP nonce (use_dpop_nonce) but did not supply one via the DPoP-Nonce header; refusing to retry without one")
				}
				nonce = challengeNonce
				continue
			}
		}

		return resp, body, nil
	}

	return lastResp, lastBody, nil
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

// CreateDPoPProof creates a DPoP proof JWT (RFC 9449) for a request with
// the given HTTP method and URI (which must NOT include a query string --
// per RFC 9449 s4.2 the "htu" claim is compared without one), optionally
// binding it to accessToken (via the "ath" claim, for resource-server
// requests carrying a DPoP-bound access token) and/or a server-supplied
// nonce. A thin exported wrapper around internal/dpop.CreateProof, which is
// where this package's own PAR/token/refresh DPoP signing (createDPoPToken
// below) also delegates to -- see internal/dpop's package doc comment for
// why the actual implementation lives there rather than here.
func CreateDPoPProof(privateKey *ecdsa.PrivateKey, method, uri, accessToken, nonce string) (string, error) {
	return dpop.CreateProof(privateKey, method, uri, accessToken, nonce)
}

// createDPoPToken is this package's original (pre-atchess-1c9.12) internal
// name for CreateDPoPProof; kept as a thin alias -- rather than renaming
// every call site -- because internal/oauth/conformance_test.go calls it
// directly by this unexported name and must not be edited (it is this
// bead's specification, not implementation detail).
func createDPoPToken(privateKey *ecdsa.PrivateKey, method, uri, accessToken string, nonce string) (string, error) {
	return dpop.CreateProof(privateKey, method, uri, accessToken, nonce)
}

// GenerateState creates a random state parameter
func GenerateState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
