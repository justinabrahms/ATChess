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
// OAuth (DPoP-bound) sessions are not refreshable here yet -- signing a
// DPoP-bound refresh request is atchess-1c9.12's job -- so refreshFunc fails
// closed for them rather than silently reusing a stale token.
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
			// OAuth sessions are DPoP-bound; refreshing them requires
			// signing a DPoP-bound token request, which is
			// atchess-1c9.12's job. Fail closed rather than silently
			// reusing a stale/expired token.
			return "", "", time.Time{}, fmt.Errorf("OAuth session token refresh is not yet implemented (see atchess-1c9.12)")
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
