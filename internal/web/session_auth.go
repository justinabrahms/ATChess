package web

import (
	"fmt"
	"time"

	"github.com/justinabrahms/atchess/internal/atproto"
	"github.com/justinabrahms/atchess/internal/oauth"
)

// defaultSessionTokenTTL is the fallback lifetime assumed for a refreshed
// app-password session's access token when its expiry cannot be parsed
// directly out of the JWT (see atproto.ParseJWTExpiry). Conservative on
// purpose: refreshing a bit early is harmless, refreshing too late means a
// request fails with 401.
const defaultSessionTokenTTL = 50 * time.Minute

// sessionAuthenticator adapts an *oauth.Session to atproto.Authenticator,
// refreshing app-password sessions via atproto.RefreshSession. It is what
// lets Service.clientFor build a *atproto.Client that acts as the
// authenticated caller (atchess-1c9.9) instead of the protocol-service
// instance's static configured identity.
//
// OAuth (DPoP-bound) sessions are refreshed via oauthClient.RefreshTokens
// (atchess-1c9.12), which performs the refresh_token grant DPoP-bound to
// session.DPoPKey -- the same key used at login, since DPoP binds a token
// to its original proof key for the token's whole lifetime, not just its
// initial issuance.
type sessionAuthenticator struct {
	session *oauth.Session
}

// newSessionAuthenticator returns an atproto.Authenticator backed by
// session. session must not be nil.
func newSessionAuthenticator(session *oauth.Session) *sessionAuthenticator {
	return &sessionAuthenticator{session: session}
}

// Token implements atproto.Authenticator.
func (a *sessionAuthenticator) Token() (string, error) {
	return a.session.EnsureFresh(a.refreshFunc())
}

// ForceRefresh implements atproto.Authenticator.
func (a *sessionAuthenticator) ForceRefresh() (string, error) {
	return a.session.ForceRefresh(a.refreshFunc())
}

func (a *sessionAuthenticator) refreshFunc() oauth.RefreshFunc {
	session := a.session
	return func(refreshToken string) (string, string, time.Time, error) {
		if session.DPoPKey != nil {
			return refreshOAuthSession(session, refreshToken)
		}
		if refreshToken == "" {
			return "", "", time.Time{}, fmt.Errorf("session has no refresh token available")
		}
		if session.PDSURL == "" {
			return "", "", time.Time{}, fmt.Errorf("session has no PDS URL recorded, cannot refresh")
		}

		accessJWT, refreshJWT, err := atproto.RefreshSession(session.PDSURL, refreshToken)
		if err != nil {
			return "", "", time.Time{}, err
		}

		expiresAt, ok := atproto.ParseJWTExpiry(accessJWT)
		if !ok {
			expiresAt = time.Now().Add(defaultSessionTokenTTL)
		}
		return accessJWT, refreshJWT, expiresAt, nil
	}
}

// refreshOAuthSession performs the OAuth refresh_token grant for a
// DPoP-bound (session.DPoPKey != nil) session, via the global oauthClient
// (initialized by InitializeOAuth). session.AuthServerURL holds the OAuth
// issuer recorded at login time (see OAuthCallbackHandler) and is used for
// BOTH the token endpoint's discovery origin and the client assertion's
// "aud" -- deliberately NOT session.PDSURL (atchess-1c9.84): on
// bsky.social the two are different hosts (iss is https://bsky.social, the
// entryway acting as authorization server, while PDSURL is the user's
// *.host.bsky.network PDS, which does not serve
// /.well-known/oauth-authorization-server at all). Fails closed (rather
// than looping or silently reusing a stale token) when any of the OAuth
// client, the session's issuer, or its refresh token is unavailable.
func refreshOAuthSession(session *oauth.Session, refreshToken string) (string, string, time.Time, error) {
	if oauthClient == nil {
		return "", "", time.Time{}, fmt.Errorf("OAuth client not initialized, cannot refresh OAuth session")
	}
	if refreshToken == "" {
		return "", "", time.Time{}, fmt.Errorf("OAuth session has no refresh token available")
	}
	if session.AuthServerURL == "" {
		return "", "", time.Time{}, fmt.Errorf("OAuth session has no recorded authorization-server issuer, cannot refresh")
	}

	tokenEndpoint, err := getTokenEndpoint(session.AuthServerURL)
	if err != nil {
		return "", "", time.Time{}, fmt.Errorf("resolving token endpoint for OAuth refresh: %w", err)
	}

	tokens, err := oauthClient.RefreshTokens(tokenEndpoint, session.AuthServerURL, refreshToken, session.DPoPKey)
	if err != nil {
		return "", "", time.Time{}, fmt.Errorf("OAuth token refresh failed: %w", err)
	}

	expiresAt, ok := atproto.ParseJWTExpiry(tokens.AccessToken)
	if !ok {
		expiresAt = time.Now().Add(time.Duration(tokens.ExpiresIn) * time.Second)
	}
	return tokens.AccessToken, tokens.RefreshToken, expiresAt, nil
}
