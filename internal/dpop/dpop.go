// Package dpop implements the small set of DPoP (RFC 9449) primitives
// shared between internal/oauth (the authorization-server-facing OAuth
// client: PAR, token exchange, refresh) and internal/atproto (resource-
// server-facing XRPC requests made with an OAuth-bound session's access
// token). It exists as its own package -- rather than living in either of
// those -- specifically to avoid an import cycle: internal/atproto needs
// to sign DPoP proofs and share a nonce cache with internal/oauth's
// authorization-server-facing DPoP handling, but internal/oauth's
// conformance tests (internal/oauth/conformance_test.go) themselves import
// internal/atproto (to exercise the same handle -> DID -> PDS resolution
// chain production code uses), so internal/oauth cannot import
// internal/atproto and internal/atproto cannot import internal/oauth
// without one of those imports becoming circular in test builds. Both
// import this package instead.
package dpop

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"net/url"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// CreateProof creates a DPoP proof JWT (RFC 9449) for a request with the
// given HTTP method and URI. uri must NOT include a query string -- per
// RFC 9449 s4.2 the "htu" claim is compared without one. accessToken, when
// non-empty, is hashed into the proof's "ath" claim (s4.3) -- relevant for
// resource-server requests carrying a DPoP-bound access token; callers
// signing a PAR/token/refresh request (which has no access token yet)
// should pass "". nonce, when non-empty, is included as the proof's
// "nonce" claim, echoing a server-issued DPoP-Nonce challenge back.
//
// A fresh "iat" (now) is set on every call -- callers should call this
// again for every attempt, including retries, rather than reusing a
// previously-signed proof. This is also this package's mitigation for
// clock-skew-related proof rejection (one of atchess-1c9.12's named edge
// cases): a proof is never stale by more than the time it takes to build
// and send the one request it is used for, keeping the window between
// signing and server-side evaluation of "iat" as small as possible, rather
// than a signed-but-unsent proof from an earlier failed attempt ever being
// replayed later.
func CreateProof(privateKey *ecdsa.PrivateKey, method, uri, accessToken, nonce string) (string, error) {
	now := time.Now()

	claims := jwt.MapClaims{
		"jti": generateJTI(),
		"htm": method,
		"htu": uri,
		"iat": now.Unix(),
		"exp": now.Add(5 * time.Minute).Unix(),
	}

	if nonce != "" {
		claims["nonce"] = nonce
	}

	if accessToken != "" {
		h := sha256.Sum256([]byte(accessToken))
		claims["ath"] = base64.RawURLEncoding.EncodeToString(h[:])
	}

	token := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	token.Header["typ"] = "dpop+jwt"
	token.Header["jwk"] = map[string]interface{}{
		"kty": "EC",
		"crv": "P-256",
		"x":   base64.RawURLEncoding.EncodeToString(privateKey.PublicKey.X.Bytes()),
		"y":   base64.RawURLEncoding.EncodeToString(privateKey.PublicKey.Y.Bytes()),
	}

	return token.SignedString(privateKey)
}

func generateJTI() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// NonceStore holds the most recently observed DPoP nonce for each server
// origin an OAuth-bound client has talked to. RFC 9449 scopes nonces per
// server, not globally, and different endpoints an AT Protocol client
// talks to during a login/session lifecycle -- the PAR endpoint, the token
// endpoint, the refresh flow, and an arbitrary user's PDS acting as
// resource server -- can all live at different origins (or even be the
// same origin wearing different hats), so the store is keyed by origin
// string ("scheme://host[:port]"), not by purpose.
//
// Storing and reusing the nonce this way is what makes the common case a
// single request instead of a guaranteed-to-fail first attempt followed by
// a retry (atchess-1c9.12 step 4): once any request to an origin has seen
// a DPoP-Nonce response header, every later request (even from a brand new
// *oauth.OAuthClient or *atproto.Client -- see DefaultNonceStore) can
// include it up front.
//
// A nonce that rotates on every response (atchess-1c9.12's first edge
// case) is handled by simply overwriting the stored value every time a
// response carries a DPoP-Nonce header, success or failure alike: the
// store never tries to be "sure" a nonce is still valid, it just always
// holds the freshest one it has seen.
//
// Safe for concurrent use.
type NonceStore struct {
	mu     sync.Mutex
	nonces map[string]string
}

// NewNonceStore returns an empty, ready-to-use NonceStore.
func NewNonceStore() *NonceStore {
	return &NonceStore{nonces: make(map[string]string)}
}

// Get returns the last known nonce for origin, or "" if none is known yet.
func (s *NonceStore) Get(origin string) string {
	if origin == "" {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.nonces[origin]
}

// Set records nonce as the current nonce for origin. A no-op when origin or
// nonce is empty, so callers can pass through response headers/URLs
// unconditionally without their own nil/empty checks.
func (s *NonceStore) Set(origin, nonce string) {
	if origin == "" || nonce == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nonces[origin] = nonce
}

// OriginOf returns the "scheme://host[:port]" portion of rawURL, i.e. the
// key NonceStore uses. Returns "" if rawURL cannot be parsed into something
// with both a scheme and a host -- callers should treat that as "do not
// cache or look up a nonce for this request" rather than a hard failure,
// since DPoP nonce caching is a latency optimisation, not a correctness
// requirement (a request sent with no/a wrong nonce simply gets a 400/401
// nonce challenge back, which the normal retry path handles).
func OriginOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// defaultNonceStore is the process-wide DPoP nonce cache shared by every
// *oauth.OAuthClient and every internal/atproto *Client for OAuth-bound
// (DPoP) sessions. A single shared store -- rather than one per client
// instance -- is required because internal/web builds a brand new
// *atproto.Client per incoming HTTP request (Service.clientFor); without a
// store that outlives any single Client, every authenticated request would
// pay a failed first attempt forever, never converging to the "one
// request" common case atchess-1c9.12 step 4 asks for.
var defaultNonceStore = NewNonceStore()

// DefaultNonceStore returns the process-wide shared DPoP NonceStore.
func DefaultNonceStore() *NonceStore {
	return defaultNonceStore
}
