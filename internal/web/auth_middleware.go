package web

import (
	"context"
	"net/http"

	"github.com/justinabrahms/atchess/internal/oauth"
	"github.com/rs/zerolog/log"
)

type contextKey string

const (
	contextKeyDID     contextKey = "authenticated_did"
	contextKeyHandle  contextKey = "authenticated_handle"
	contextKeySession contextKey = "authenticated_session"
)

// AuthMiddleware validates the session from X-Session-ID header and injects
// the authenticated user's DID and handle into the request context.
// Returns 401 if no valid session is found.
func AuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sessionStore == nil {
			log.Error().Msg("AuthMiddleware: session store not initialized")
			http.Error(w, "Authentication unavailable", http.StatusServiceUnavailable)
			return
		}

		sessionID := r.Header.Get("X-Session-ID")
		if sessionID == "" {
			http.Error(w, "Authentication required", http.StatusUnauthorized)
			return
		}

		session, err := sessionStore.GetSession(sessionID)
		if err != nil {
			log.Debug().Err(err).Msg("AuthMiddleware: invalid session")
			http.Error(w, "Invalid or expired session", http.StatusUnauthorized)
			return
		}

		ctx := context.WithValue(r.Context(), contextKeyDID, session.DID)
		ctx = context.WithValue(ctx, contextKeyHandle, session.Handle)
		ctx = context.WithValue(ctx, contextKeySession, session)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// AuthenticatedDID returns the DID of the authenticated user from the request context.
func AuthenticatedDID(r *http.Request) string {
	did, _ := r.Context().Value(contextKeyDID).(string)
	return did
}

// AuthenticatedHandle returns the handle of the authenticated user from the request context.
func AuthenticatedHandle(r *http.Request) string {
	handle, _ := r.Context().Value(contextKeyHandle).(string)
	return handle
}

// authenticatedSession returns the *oauth.Session for the current request,
// as injected by AuthMiddleware, or nil if there is none (e.g. the request
// did not go through AuthMiddleware, or no session store was configured).
// Used by Service.clientFor (atchess-1c9.9) to build a per-user
// *atproto.Client instead of reusing the protocol-service instance's static
// configured identity.
func authenticatedSession(r *http.Request) *oauth.Session {
	session, _ := r.Context().Value(contextKeySession).(*oauth.Session)
	return session
}
