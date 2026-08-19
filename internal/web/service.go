package web

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/justinabrahms/atchess/internal/atproto"
	"github.com/justinabrahms/atchess/internal/challenge"
	"github.com/justinabrahms/atchess/internal/chess"
	"github.com/justinabrahms/atchess/internal/config"
	"github.com/justinabrahms/atchess/internal/oauth"
	"github.com/rs/zerolog/log"
)

type Service struct {
	// serverClient is this protocol-service instance's own static
	// configured identity (from config.ATProto). It must ONLY be used by
	// genuinely unauthenticated handlers (health, public spectator reads,
	// pre-login OAuth discovery) -- never inside the `authed` router. Every
	// authenticated write handler must call clientFor(r) instead, which
	// derives a *atproto.Client from the caller's own session so records
	// land in THEIR repo, not this instance's (atchess-1c9.9; see
	// atchess-1c9.4 for the conformance test this fixes).
	serverClient   *atproto.Client
	config         *config.Config
	oauthClient    OAuthClientInterface
	challengeStore *challenge.Store
	originChecker  *OriginChecker
}

// OAuthClientInterface defines the methods we need from the OAuth client
type OAuthClientInterface interface {
	GetPublicKeyJWK() map[string]interface{}
}

func NewService(serverClient *atproto.Client, cfg *config.Config, challengeStore *challenge.Store) *Service {
	origins := cfg.Server.AllowedOrigins
	if len(origins) == 0 {
		origins = AllowedOriginsFromBaseURL(cfg.Server.BaseURL)
	}
	return &Service{
		serverClient:   serverClient,
		config:         cfg,
		challengeStore: challengeStore,
		originChecker:  NewOriginChecker(origins),
	}
}

// clientFor returns an *atproto.Client that authenticates as the caller
// identified by the current request's session (see AuthMiddleware /
// authenticatedSession), so every write it makes lands in THAT caller's own
// AT Protocol repo instead of this protocol-service instance's static
// configured identity's repo (s.serverClient). Returns an error -- which
// callers must translate into HTTP 401 -- when there is no valid session or
// its PDS URL/tokens cannot be used to build a client.
func (s *Service) clientFor(r *http.Request) (*atproto.Client, error) {
	session := authenticatedSession(r)
	if session == nil {
		return nil, fmt.Errorf("authentication required")
	}

	pdsURL := session.PDSURL
	if pdsURL == "" {
		// Should never happen for sessions minted by this service's own
		// LoginHandler (which always records PDSURL); fail closed rather
		// than silently falling back to this instance's own configured PDS,
		// which would misattribute writes exactly like the bug this method
		// exists to fix.
		return nil, fmt.Errorf("session for %s has no recorded PDS URL", session.DID)
	}

	useDPoP := session.DPoPKey != nil
	client, err := atproto.NewClientFromSession(pdsURL, session.DID, session.Handle, useDPoP, newSessionAuthenticator(session))
	if err != nil {
		return nil, fmt.Errorf("failed to build authenticated client for %s: %w", session.DID, err)
	}
	client.SetPLCDirectoryURL(s.config.ATProto.PLCDirectoryURL)
	return client, nil
}

// requireClient is a small helper for authed handlers: it calls clientFor
// and, on failure, writes an HTTP 401 (so the UI can prompt re-
// authentication) and returns ok=false. Handlers should return immediately
// when ok is false.
func (s *Service) requireClient(w http.ResponseWriter, r *http.Request) (client *atproto.Client, ok bool) {
	client, err := s.clientFor(r)
	if err != nil {
		log.Warn().Err(err).Str("path", r.URL.Path).Msg("no authenticated AT Protocol client available for request")
		http.Error(w, "Authentication required", http.StatusUnauthorized)
		return nil, false
	}
	return client, true
}

// GetOriginChecker returns the service's origin checker for use in middleware.
func (s *Service) GetOriginChecker() *OriginChecker {
	return s.originChecker
}

// SetOAuthClient sets the OAuth client for the service
func (s *Service) SetOAuthClient(oauthClient OAuthClientInterface) {
	s.oauthClient = oauthClient
}

func (s *Service) decodeGameID(encodedGameID string) (string, error) {
	// Convert URL-safe base64 back to regular base64
	base64Str := strings.ReplaceAll(encodedGameID, "-", "+")
	base64Str = strings.ReplaceAll(base64Str, "_", "/")

	// Decode base64 (padding should already be present)
	decoded, err := base64.StdEncoding.DecodeString(base64Str)
	if err != nil {
		return "", fmt.Errorf("failed to decode base64: %w", err)
	}

	return string(decoded), nil
}

// HealthHandler is unauthenticated by design and reports THIS
// protocol-service instance's own static configured (bootstrap) identity --
// never an authenticated caller's. It must not be used as a stand-in for
// "who am I" from a logged-in user's perspective; see
// GetCurrentUserHandler/AuthenticatedDID for that.
func (s *Service) HealthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status": "ok",
		"did":    s.serverClient.GetDID(),
		"handle": s.serverClient.GetHandle(),
	})
}

type CreateGameRequest struct {
	OpponentDID string `json:"opponent_did"`
	Color       string `json:"color"`
}

func (s *Service) CreateGameHandler(w http.ResponseWriter, r *http.Request) {
	var req CreateGameRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	client, ok := s.requireClient(w, r)
	if !ok {
		return
	}

	game, err := client.CreateGame(context.Background(), req.OpponentDID, req.Color)
	if err != nil {
		log.Error().Err(err).Msg("Failed to create game")
		http.Error(w, "Failed to create game", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(game)
}

type MakeMoveRequest struct {
	From      string `json:"from"`
	To        string `json:"to"`
	Promotion string `json:"promotion,omitempty"`
	FEN       string `json:"fen"`
	GameID    string `json:"game_id,omitempty"`
}

func (s *Service) MakeMoveHandler(w http.ResponseWriter, r *http.Request) {
	var req MakeMoveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	// Game ID must be provided in request body
	gameID := req.GameID
	if gameID == "" {
		http.Error(w, "game_id is required in request body", http.StatusBadRequest)
		return
	}

	client, ok := s.requireClient(w, r)
	if !ok {
		return
	}

	// Fetch authoritative game state from AT Protocol — never trust client-supplied FEN.
	// This closes the race window where two players could submit moves based on a
	// stale board position.
	game, err := client.GetGame(context.Background(), gameID)
	if err != nil {
		log.Error().Err(err).Str("gameID", gameID).Msg("Failed to fetch game for move validation")
		http.Error(w, "Failed to fetch game state", http.StatusInternalServerError)
		return
	}
	serverFEN := game.FEN

	// Verify the authenticated user is a player in this game
	authedDID := AuthenticatedDID(r)
	if authedDID != game.White && authedDID != game.Black {
		log.Warn().Str("did", authedDID).Str("gameID", gameID).Msg("User is not a player in this game")
		http.Error(w, "You are not a player in this game", http.StatusForbidden)
		return
	}

	// Log for debugging
	log.Info().Str("gameID", gameID).Str("from", req.From).Str("to", req.To).Str("serverFEN", serverFEN).Str("clientFEN", req.FEN).Str("path", r.URL.Path).Msg("MakeMoveHandler called")

	// Create chess engine from server-authoritative position
	engine, err := chess.NewEngineFromFEN(serverFEN)
	if err != nil {
		log.Error().Err(err).Str("fen", serverFEN).Msg("Invalid FEN from game record")
		http.Error(w, "Invalid game state", http.StatusInternalServerError)
		return
	}

	// Verify it's the authenticated user's turn
	activeColor := engine.GetActiveColor()
	if (activeColor == "white" && authedDID != game.White) || (activeColor == "black" && authedDID != game.Black) {
		log.Warn().Str("did", authedDID).Str("activeColor", activeColor).Str("gameID", gameID).Msg("Not this player's turn")
		http.Error(w, "It is not your turn", http.StatusForbidden)
		return
	}

	// Parse promotion
	promotion := chess.ParsePromotion(req.Promotion)

	// Make move
	moveResult, err := engine.MakeMove(req.From, req.To, promotion)
	if err != nil {
		log.Error().Err(err).Str("from", req.From).Str("to", req.To).Msg("Invalid move")
		http.Error(w, fmt.Sprintf("Invalid move: %s", err.Error()), http.StatusBadRequest)
		return
	}

	// Log move result
	log.Info().Str("gameID", gameID).Str("san", moveResult.SAN).Str("resultFEN", moveResult.FEN).Bool("check", moveResult.Check).Bool("checkmate", moveResult.Checkmate).Msg("Move executed successfully")

	// Record move in AT Protocol
	if err := client.RecordMove(context.Background(), gameID, moveResult); err != nil {
		log.Error().Err(err).Str("gameID", gameID).Msg("Failed to record move")
		http.Error(w, "Failed to record move", http.StatusInternalServerError)
		return
	}

	log.Info().Str("gameID", gameID).Msg("Move recorded in AT Protocol successfully")

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(moveResult)
}

type CreateChallengeRequest struct {
	OpponentDID string `json:"opponent_did"`
	Color       string `json:"color"`
	Message     string `json:"message,omitempty"`
}

func (s *Service) GetGameHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	encodedGameID := vars["id"]

	// Base64 decode the game ID (using URL-safe base64 decoding)
	gameID, err := s.decodeGameID(encodedGameID)
	if err != nil {
		log.Error().Err(err).Str("encodedGameID", encodedGameID).Msg("Failed to decode game ID")
		http.Error(w, "Invalid game ID", http.StatusBadRequest)
		return
	}

	// Log for debugging
	log.Info().Str("gameID", gameID).Str("encodedGameID", encodedGameID).Str("path", r.URL.Path).Msg("GetGameHandler called")

	// Fetch game from AT Protocol. This is a public, unauthenticated read
	// endpoint (spectator-style), so it legitimately uses the server's own
	// client rather than clientFor(r).
	game, err := s.serverClient.GetGame(context.Background(), gameID)
	if err != nil {
		log.Error().Err(err).Str("gameID", gameID).Msg("Failed to fetch game")
		http.Error(w, "Game not found", http.StatusNotFound)
		return
	}

	log.Info().Str("gameID", gameID).Str("fen", game.FEN).Str("status", string(game.Status)).Msg("Game fetched successfully")

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(game)
}

func (s *Service) CreateChallengeHandler(w http.ResponseWriter, r *http.Request) {
	var req CreateChallengeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	client, ok := s.requireClient(w, r)
	if !ok {
		return
	}

	// Resolve handle to DID if necessary
	opponentDID := req.OpponentDID
	if !strings.HasPrefix(opponentDID, "did:") {
		resolvedDID, err := client.ResolveHandle(context.Background(), opponentDID)
		if err != nil {
			log.Error().Err(err).Str("handle", opponentDID).Msg("Failed to resolve handle")
			http.Error(w, fmt.Sprintf("Failed to resolve handle '%s': %v", opponentDID, err), http.StatusBadRequest)
			return
		}
		opponentDID = resolvedDID
	}

	ch, err := client.CreateChallenge(context.Background(), opponentDID, req.Color, req.Message)
	if err != nil {
		log.Error().Err(err).Msg("Failed to create challenge")
		http.Error(w, "Failed to create challenge", http.StatusInternalServerError)
		return
	}

	// Index the challenge locally so the challenged player can discover it
	if s.challengeStore != nil {
		createdAt, _ := time.Parse(time.RFC3339, ch.CreatedAt)
		expiresAt, _ := time.Parse(time.RFC3339, ch.ExpiresAt)
		s.challengeStore.Add(&challenge.PendingChallenge{
			ChallengeURI:     ch.ID,
			ChallengerDID:    ch.Challenger,
			ChallengerHandle: client.GetHandle(),
			ChallengedDID:    ch.Challenged,
			Color:            ch.Color,
			Message:          ch.Message,
			ProposedGameID:   ch.ProposedGameId,
			CreatedAt:        createdAt,
			ExpiresAt:        expiresAt,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(ch)
}

func (s *Service) GetChallengeNotificationsHandler(w http.ResponseWriter, r *http.Request) {
	// Use the local challenge store for discovery (works across federation)
	if s.challengeStore != nil {
		challenges := s.challengeStore.ForPlayer(AuthenticatedDID(r))
		w.Header().Set("Content-Type", "application/json")
		if challenges == nil {
			challenges = []*challenge.PendingChallenge{}
		}
		_ = json.NewEncoder(w).Encode(challenges)
		return
	}

	// Fallback to legacy repo-based notifications (only works same-PDS)
	client, ok := s.requireClient(w, r)
	if !ok {
		return
	}
	notifications, err := client.GetChallengeNotifications(context.Background())
	if err != nil {
		log.Error().Err(err).Msg("Failed to fetch challenge notifications")
		http.Error(w, "Failed to fetch notifications", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(notifications)
}

func (s *Service) DeleteChallengeNotificationHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	notificationKey := vars["key"]

	if notificationKey == "" {
		http.Error(w, "Missing notification key", http.StatusBadRequest)
		return
	}

	client, ok := s.requireClient(w, r)
	if !ok {
		return
	}

	err := client.DeleteChallengeNotification(context.Background(), notificationKey)
	if err != nil {
		log.Error().Err(err).Str("key", notificationKey).Msg("Failed to delete notification")
		http.Error(w, "Failed to delete notification", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) OfferDrawHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		GameID  string `json:"gameId"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	client, ok := s.requireClient(w, r)
	if !ok {
		return
	}

	drawOffer, err := client.OfferDraw(context.Background(), req.GameID, req.Message)
	if err != nil {
		log.Error().Err(err).Str("gameID", req.GameID).Msg("Failed to offer draw")
		http.Error(w, "Failed to offer draw", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(drawOffer)
}

func (s *Service) RespondToDrawHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DrawOfferURI string `json:"drawOfferUri"`
		Accept       bool   `json:"accept"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	client, ok := s.requireClient(w, r)
	if !ok {
		return
	}

	err := client.RespondToDrawOffer(context.Background(), req.DrawOfferURI, req.Accept)
	if err != nil {
		log.Error().Err(err).Str("uri", req.DrawOfferURI).Msg("Failed to respond to draw offer")
		http.Error(w, "Failed to respond to draw offer", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) ResignGameHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		GameID string `json:"gameId"`
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	client, ok := s.requireClient(w, r)
	if !ok {
		return
	}

	err := client.ResignGame(context.Background(), req.GameID, req.Reason)
	if err != nil {
		log.Error().Err(err).Str("gameID", req.GameID).Msg("Failed to resign game")
		http.Error(w, "Failed to resign game", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"gameId":  req.GameID,
	})
}

func (s *Service) CheckTimeViolationHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	gameID := vars["id"]

	if gameID == "" {
		http.Error(w, "Missing game ID", http.StatusBadRequest)
		return
	}

	// Public, unauthenticated read endpoint (see cmd/protocol/main.go: this
	// route is registered on the public `api` router, not `authed`), so it
	// legitimately uses the server's own client rather than clientFor(r).
	hasViolation, violation, err := s.serverClient.CheckTimeViolation(context.Background(), gameID)
	if err != nil {
		log.Error().Err(err).Str("gameID", gameID).Msg("Failed to check time violation")
		http.Error(w, "Failed to check time violation", http.StatusInternalServerError)
		return
	}

	response := map[string]interface{}{
		"hasViolation": hasViolation,
		"violation":    violation,
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}

func (s *Service) ClaimTimeVictoryHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	gameID := vars["id"]

	if gameID == "" {
		http.Error(w, "Missing game ID", http.StatusBadRequest)
		return
	}

	client, ok := s.requireClient(w, r)
	if !ok {
		return
	}

	err := client.ClaimTimeVictory(context.Background(), gameID)
	if err != nil {
		log.Error().Err(err).Str("gameID", gameID).Msg("Failed to claim time victory")
		http.Error(w, "Failed to claim time victory", http.StatusBadRequest)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) GetTimeRemainingHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	gameID := vars["id"]

	if gameID == "" {
		http.Error(w, "Missing game ID", http.StatusBadRequest)
		return
	}

	// Public, unauthenticated read endpoint (registered on the public `api`
	// router, not `authed`), so it legitimately uses the server's own
	// client rather than clientFor(r).
	remaining, err := s.serverClient.GetTimeRemaining(context.Background(), gameID)
	if err != nil {
		log.Error().Err(err).Str("gameID", gameID).Msg("Failed to get time remaining")
		http.Error(w, "Failed to get time remaining", http.StatusInternalServerError)
		return
	}

	response := map[string]interface{}{
		"gameId":             gameID,
		"remainingSeconds":   int(remaining.Seconds()),
		"remainingFormatted": chess.FormatTimeRemaining(remaining),
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}

type AuthRequest struct {
	Handle   string `json:"handle"`
	Password string `json:"password"`
}

type AuthResponse struct {
	Success     bool   `json:"success"`
	DID         string `json:"did"`
	Handle      string `json:"handle"`
	AccessToken string `json:"accessToken"`
	Error       string `json:"error,omitempty"`
}

func (s *Service) LoginHandler(w http.ResponseWriter, r *http.Request) {
	var req AuthRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	// Validate input
	if req.Handle == "" || req.Password == "" {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(AuthResponse{
			Success: false,
			Error:   "Handle and password are required",
		})
		return
	}

	// Create a new AT Protocol client for this user
	userClient, err := atproto.NewClientWithDPoP(
		s.config.ATProto.PDSURL,
		req.Handle,
		req.Password,
		s.config.ATProto.UseDPoP,
	)
	if err != nil {
		log.Error().Err(err).Str("handle", req.Handle).Msg("Failed to authenticate user")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(AuthResponse{
			Success: false,
			Error:   "Invalid credentials or authentication failed",
		})
		return
	}
	userClient.SetPLCDirectoryURL(s.config.ATProto.PLCDirectoryURL)

	// Ensure session store is initialized for password auth
	if sessionStore == nil {
		sessionStore = oauth.NewSessionStore()
	}

	// Determine the access token's real expiry from its JWT "exp" claim so
	// EnsureFresh (internal/oauth) refreshes it proactively rather than
	// guessing; fall back to a conservative TTL if it can't be parsed.
	accessExpiresAt, ok := atproto.ParseJWTExpiry(userClient.GetAccessJWT())
	if !ok {
		accessExpiresAt = time.Now().Add(defaultSessionTokenTTL)
	}

	// Create a proper session so auth middleware works, carrying the PDS
	// URL and AT Protocol accessJwt/refreshJwt needed to build a per-user
	// *atproto.Client for every subsequent authenticated request
	// (Service.clientFor, atchess-1c9.9) instead of reusing this
	// protocol-service instance's own static configured identity.
	//
	// NOTE: DPoP is not carried across from this login client to the
	// session -- app-password sessions are always Bearer-authenticated
	// here, even if s.config.ATProto.UseDPoP is true for the login
	// handshake itself. DPoP-bound sessions (OAuth) are handled separately
	// via session.DPoPKey; see internal/web/session_auth.go.
	session := &oauth.Session{
		DID:                  userClient.GetDID(),
		Handle:               userClient.GetHandle(),
		PDSURL:               s.config.ATProto.PDSURL,
		AccessToken:          userClient.GetAccessJWT(),
		RefreshToken:         userClient.GetRefreshJWT(),
		ExpiresAt:            time.Now().Add(24 * time.Hour),
		AccessTokenExpiresAt: accessExpiresAt,
	}
	sessionID := sessionStore.CreateSession(session)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(AuthResponse{
		Success:     true,
		DID:         userClient.GetDID(),
		Handle:      userClient.GetHandle(),
		AccessToken: sessionID,
	})
}

func (s *Service) GetCurrentUserHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"did":           AuthenticatedDID(r),
		"handle":        AuthenticatedHandle(r),
		"authenticated": true,
	})
}

// ClientMetadataHandler serves the OAuth client metadata dynamically
func (s *Service) ClientMetadataHandler(w http.ResponseWriter, r *http.Request) {
	// Get the host from the request to build proper URLs
	scheme := "https"
	// Check X-Forwarded-Proto header first (set by reverse proxies like Caddy/nginx)
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		scheme = proto
	} else if r.TLS == nil {
		scheme = "http"
	}
	host := r.Host

	// Build the client metadata dynamically
	metadata := map[string]interface{}{
		"client_id":      fmt.Sprintf("%s://%s/client-metadata.json", scheme, host),
		"client_name":    "ATChess",
		"client_name#en": "ATChess - Decentralized Chess",
		"logo_uri":       "https://cdn.bsky.app/img/avatar_thumbnail/plain/did:plc:7qz7m34ck7gtzrcnailvljp5/bafkreif33s7ziwwrcctx5n4mpb63g2sphjz2p6xkn7ddx6sszq3x2s3v7m@jpeg",
		"redirect_uris": []string{
			fmt.Sprintf("%s://%s/api/callback", scheme, host),
		},
		"scope":                           "atproto transition:generic",
		"grant_types":                     []string{"authorization_code", "refresh_token"},
		"response_types":                  []string{"code"},
		"token_endpoint_auth_method":      "private_key_jwt",
		"token_endpoint_auth_signing_alg": "ES256",
		"dpop_bound_access_tokens":        true,
		"jwks":                            s.getJWKS(),
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=3600") // Cache for 1 hour
	if err := json.NewEncoder(w).Encode(metadata); err != nil {
		log.Error().Err(err).Msg("Failed to encode client metadata")
		http.Error(w, "Internal server error", http.StatusInternalServerError)
	}
}

// getJWKS returns the JSON Web Key Set for the OAuth client
func (s *Service) getJWKS() map[string]interface{} {
	// Get public key from OAuth service if available
	if s.oauthClient != nil {
		publicKeyJWK := s.oauthClient.GetPublicKeyJWK()
		return map[string]interface{}{
			"keys": []interface{}{publicKeyJWK},
		}
	}

	// Fallback to empty key set
	return map[string]interface{}{
		"keys": []interface{}{},
	}
}
