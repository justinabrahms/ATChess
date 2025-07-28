package web

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/gorilla/mux"
	"github.com/justinabrahms/atchess/internal/atproto"
	"github.com/justinabrahms/atchess/internal/chess"
	"github.com/justinabrahms/atchess/internal/config"
	"github.com/rs/zerolog/log"
)

type Service struct {
	client      *atproto.Client
	config      *config.Config
	oauthClient OAuthClientInterface
}

// OAuthClientInterface defines the methods we need from the OAuth client
type OAuthClientInterface interface {
	GetPublicKeyJWK() map[string]interface{}
}

func NewService(client *atproto.Client, config *config.Config) *Service {
	return &Service{
		client: client,
		config: config,
	}
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

func (s *Service) HealthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status": "ok",
		"did":    s.client.GetDID(),
		"handle": s.client.GetHandle(),
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
	
	game, err := s.client.CreateGame(context.Background(), req.OpponentDID, req.Color)
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
	
	// Log for debugging
	log.Info().Str("gameID", gameID).Str("from", req.From).Str("to", req.To).Str("fen", req.FEN).Str("path", r.URL.Path).Msg("MakeMoveHandler called")
	
	// Create chess engine from current position
	engine, err := chess.NewEngineFromFEN(req.FEN)
	if err != nil {
		log.Error().Err(err).Str("fen", req.FEN).Msg("Invalid FEN")
		http.Error(w, "Invalid FEN", http.StatusBadRequest)
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
	if err := s.client.RecordMove(context.Background(), gameID, moveResult); err != nil {
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
	
	// Fetch game from AT Protocol
	game, err := s.client.GetGame(context.Background(), gameID)
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
	
	// Resolve handle to DID if necessary
	opponentDID := req.OpponentDID
	if !strings.HasPrefix(opponentDID, "did:") {
		resolvedDID, err := s.client.ResolveHandle(context.Background(), opponentDID)
		if err != nil {
			log.Error().Err(err).Str("handle", opponentDID).Msg("Failed to resolve handle")
			http.Error(w, fmt.Sprintf("Failed to resolve handle '%s': %v", opponentDID, err), http.StatusBadRequest)
			return
		}
		opponentDID = resolvedDID
	}
	
	challenge, err := s.client.CreateChallenge(context.Background(), opponentDID, req.Color, req.Message)
	if err != nil {
		log.Error().Err(err).Msg("Failed to create challenge")
		http.Error(w, "Failed to create challenge", http.StatusInternalServerError)
		return
	}
	
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(challenge)
}

func (s *Service) GetChallengeNotificationsHandler(w http.ResponseWriter, r *http.Request) {
	notifications, err := s.client.GetChallengeNotifications(context.Background())
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
	
	err := s.client.DeleteChallengeNotification(context.Background(), notificationKey)
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
	
	drawOffer, err := s.client.OfferDraw(context.Background(), req.GameID, req.Message)
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
	
	err := s.client.RespondToDrawOffer(context.Background(), req.DrawOfferURI, req.Accept)
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
	
	err := s.client.ResignGame(context.Background(), req.GameID, req.Reason)
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
	
	hasViolation, violation, err := s.client.CheckTimeViolation(context.Background(), gameID)
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
	
	err := s.client.ClaimTimeVictory(context.Background(), gameID)
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
	
	remaining, err := s.client.GetTimeRemaining(context.Background(), gameID)
	if err != nil {
		log.Error().Err(err).Str("gameID", gameID).Msg("Failed to get time remaining")
		http.Error(w, "Failed to get time remaining", http.StatusInternalServerError)
		return
	}
	
	response := map[string]interface{}{
		"gameId": gameID,
		"remainingSeconds": int(remaining.Seconds()),
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
	
	// Return success with user info
	// Note: In production, you'd want to create a session token instead of returning the raw JWT
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(AuthResponse{
		Success:     true,
		DID:         userClient.GetDID(),
		Handle:      userClient.GetHandle(),
		AccessToken: "session_" + base64.URLEncoding.EncodeToString([]byte(userClient.GetDID())),
	})
}

func (s *Service) GetCurrentUserHandler(w http.ResponseWriter, r *http.Request) {
	// For now, return the service's configured user
	// In a real implementation, this would validate the session token
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"did":    s.client.GetDID(),
		"handle": s.client.GetHandle(),
		"authenticated": true,
	})
}

// ClientMetadataHandler serves the OAuth client metadata dynamically
func (s *Service) ClientMetadataHandler(w http.ResponseWriter, r *http.Request) {
	// Get the host from the request to build proper URLs
	scheme := "https"
	if r.TLS == nil {
		scheme = "http"
	}
	host := r.Host
	
	// Build the client metadata dynamically
	metadata := map[string]interface{}{
		"client_id": fmt.Sprintf("%s://%s/client-metadata.json", scheme, host),
		"client_name": "ATChess",
		"client_name#en": "ATChess - Decentralized Chess", 
		"logo_uri": "https://cdn.bsky.app/img/avatar_thumbnail/plain/did:plc:7qz7m34ck7gtzrcnailvljp5/bafkreif33s7ziwwrcctx5n4mpb63g2sphjz2p6xkn7ddx6sszq3x2s3v7m@jpeg",
		"redirect_uris": []string{
			fmt.Sprintf("%s://%s/api/callback", scheme, host),
		},
		"scope": "atproto transition:generic",
		"grant_types": []string{"authorization_code", "refresh_token"},
		"response_types": []string{"code"},
		"token_endpoint_auth_method": "private_key_jwt",
		"token_endpoint_auth_signing_alg": "ES256",
		"dpop_bound_access_tokens": true,
		"jwks": s.getJWKS(),
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