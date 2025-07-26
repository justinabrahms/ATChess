package web

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/gorilla/mux"
	"github.com/justinabrahms/atchess/internal/chess"
	"github.com/rs/zerolog/log"
)

// GameIndex represents a game available for spectating
type GameIndex struct {
	URI           string            `json:"uri"`
	GameID        string            `json:"gameId"`
	Players       GamePlayers       `json:"players"`
	Status        chess.GameStatus  `json:"status"`
	MoveCount     int               `json:"moveCount"`
	LastMoveAt    *time.Time        `json:"lastMoveAt,omitempty"`
	TimeControl   map[string]interface{} `json:"timeControl,omitempty"`
	SpectatorCount int              `json:"spectatorCount"`
	MaterialCount chess.MaterialCount `json:"materialCount"`
}

type GamePlayers struct {
	White PlayerInfo `json:"white"`
	Black PlayerInfo `json:"black"`
}

type PlayerInfo struct {
	DID    string `json:"did"`
	Handle string `json:"handle"`
}

// GetActiveGamesHandler returns a list of active games for spectating
func (s *Service) GetActiveGamesHandler(w http.ResponseWriter, r *http.Request) {
	// In a real implementation, this would query indexed games from a database
	// For now, we'll use the firehose processor's tracked games
	
	// TODO: Implement proper game indexing service
	// This is a placeholder that returns an empty list
	games := []GameIndex{}
	
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"games": games,
		"total": len(games),
	})
}

// GetSpectatorGameHandler returns game data optimized for spectators
func (s *Service) GetSpectatorGameHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	gameID := vars["id"]
	
	if gameID == "" {
		http.Error(w, "Missing game ID", http.StatusBadRequest)
		return
	}
	
	// Fetch game from AT Protocol
	game, err := s.client.GetGame(context.Background(), gameID)
	if err != nil {
		log.Error().Err(err).Str("gameID", gameID).Msg("Failed to fetch game for spectator")
		http.Error(w, "Game not found", http.StatusNotFound)
		return
	}
	
	// Get material count
	engine, err := chess.NewEngineFromFEN(game.FEN)
	var materialCount chess.MaterialCount
	if err != nil {
		log.Error().Err(err).Str("fen", game.FEN).Msg("Failed to load FEN for material count")
		// Use zero material count on error
		materialCount = chess.MaterialCount{White: 0, Black: 0}
	} else {
		materialCount = engine.GetMaterialCount()
	}
	
	// TODO: Get moves from AT Protocol when move records are implemented
	// For now, moves are parsed from PGN in the engine
	
	// Prepare spectator response
	response := map[string]interface{}{
		"game": game,
		"materialCount": materialCount,
	}
	
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}

// UpdateSpectatorCountHandler updates the spectator count for a game
func (s *Service) UpdateSpectatorCountHandler(hub *Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		gameID := vars["id"]
		
		var req struct {
			Action string `json:"action"` // "join" or "leave"
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}
		
		// Get current spectator count from WebSocket hub
		hub.mu.RLock()
		spectatorCount := 0
		if clients, ok := hub.gameClients[gameID]; ok {
			spectatorCount = len(clients)
		}
		hub.mu.RUnlock()
		
		// Broadcast spectator count update
		hub.BroadcastGameUpdate(GameUpdate{
			GameID: gameID,
			Type:   "spectator_count",
			Data: map[string]interface{}{
				"count": spectatorCount,
			},
		})
		
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"gameId": gameID,
			"spectatorCount": spectatorCount,
		})
	}
}

// CheckAbandonmentHandler checks if a game has been abandoned
func (s *Service) CheckAbandonmentHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	gameID := vars["id"]
	
	// Fetch game
	game, err := s.client.GetGame(context.Background(), gameID)
	if err != nil {
		http.Error(w, "Game not found", http.StatusNotFound)
		return
	}
	
	// Only check active games
	if game.Status != chess.StatusActive {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"abandoned": false,
			"reason": "Game already ended",
		})
		return
	}
	
	// TODO: Get last move from AT Protocol when move records are implemented
	// For now, use game creation time as last activity
	lastActivityStr := game.CreatedAt
	lastActivityTime, err := time.Parse(time.RFC3339, lastActivityStr)
	if err != nil {
		log.Error().Err(err).Msg("Failed to parse activity time")
		http.Error(w, "Invalid timestamp", http.StatusInternalServerError)
		return
	}
	
	// Default abandonment timeout: 3 days for correspondence
	abandonmentTimeout := 3 * 24 * time.Hour
	timeSinceLastActivity := time.Since(lastActivityTime)
	
	abandoned := timeSinceLastActivity > abandonmentTimeout
	
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"abandoned": abandoned,
		"lastActivity": lastActivityStr,
		"timeSinceLastMove": timeSinceLastActivity.String(),
		"timeout": abandonmentTimeout.String(),
		"canClaim": abandoned,
	})
}

// ClaimAbandonedGameHandler allows a player to claim victory in an abandoned game
func (s *Service) ClaimAbandonedGameHandler(w http.ResponseWriter, r *http.Request) {
	// TODO: Implement claim logic that:
	// 1. Get gameID from request: vars := mux.Vars(r); gameID := vars["id"]
	// 2. Verifies abandonment
	// 3. Updates game status to winner
	// 4. Creates a system move or note about abandonment
	
	http.Error(w, "Not implemented", http.StatusNotImplemented)
}