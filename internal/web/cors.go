package web

import (
	"net/http"
	"net/url"

	"github.com/rs/zerolog/log"
)

// OriginChecker validates request origins against an allowlist.
type OriginChecker struct {
	allowed map[string]bool
}

// NewOriginChecker creates an OriginChecker from a list of allowed origins.
// If the list is empty, no origins are allowed (secure default).
func NewOriginChecker(origins []string) *OriginChecker {
	allowed := make(map[string]bool, len(origins))
	for _, o := range origins {
		allowed[o] = true
	}
	return &OriginChecker{allowed: allowed}
}

// IsAllowed returns true if the given origin is in the allowlist.
func (c *OriginChecker) IsAllowed(origin string) bool {
	return c.allowed[origin]
}

// CheckWebSocketOrigin returns a function suitable for websocket.Upgrader.CheckOrigin.
func (c *OriginChecker) CheckWebSocketOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		// No Origin header means same-origin request (non-browser or same-origin).
		return true
	}
	if c.IsAllowed(origin) {
		return true
	}
	log.Warn().Str("origin", origin).Msg("Rejected WebSocket connection from disallowed origin")
	return false
}

// CORSMiddleware returns HTTP middleware that sets CORS headers for allowed origins.
func (c *OriginChecker) CORSMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && c.IsAllowed(origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
		}
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Session-ID")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// AllowedOriginsFromBaseURL derives sensible default allowed origins from the server's base URL.
// Returns a list containing the base URL origin. Useful when AllowedOrigins is not explicitly configured.
func AllowedOriginsFromBaseURL(baseURL string) []string {
	if baseURL == "" {
		return nil
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil
	}
	return []string{u.Scheme + "://" + u.Host}
}
