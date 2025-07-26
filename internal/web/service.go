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
	client *atproto.Client
	config *config.Config
}

func NewService(client *atproto.Client, config *config.Config) *Service {
	return &Service{
		client: client,
		config: config,
	}
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
	
	challenge, err := s.client.CreateChallenge(context.Background(), req.OpponentDID, req.Color, req.Message)
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