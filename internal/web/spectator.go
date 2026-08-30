package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/gorilla/mux"
	"github.com/justinabrahms/atchess/internal/atproto"
	"github.com/justinabrahms/atchess/internal/chess"
	"github.com/rs/zerolog/log"
)

// GameIndex represents a game available for spectating
type GameIndex struct {
	URI            string                 `json:"uri"`
	GameID         string                 `json:"gameId"`
	Players        GamePlayers            `json:"players"`
	Status         chess.GameStatus       `json:"status"`
	MoveCount      int                    `json:"moveCount"`
	LastMoveAt     *time.Time             `json:"lastMoveAt,omitempty"`
	TimeControl    map[string]interface{} `json:"timeControl,omitempty"`
	SpectatorCount int                    `json:"spectatorCount"`
	MaterialCount  chess.MaterialCount    `json:"materialCount"`
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

	// Fetch game from AT Protocol. A GetGame error wrapping
	// ErrIncompleteDerivation with a non-nil game means the record itself
	// exists but its derived status could not be fully verified (e.g. a
	// transient opponent-PDS read failure) -- not that the game is absent.
	// Render the partial game (its DerivationIncomplete flag intact, see
	// service.go's GetGameHandler for the fuller rationale) rather than
	// reporting a 404 that misdirects the spectator toward "wrong game ID"
	// instead of "status currently unverified". Any other error (or a nil
	// game) means the record genuinely could not be found.
	game, err := s.serverClient.GetGame(context.Background(), gameID)
	if err != nil && !(errors.Is(err, atproto.ErrIncompleteDerivation) && game != nil) {
		// atchess-1c9.67: GetGame failing before ever reading the record --
		// see GetGame's doc comment and GetGameHandler's fuller rationale in
		// service.go. There is no partial game to render on this path, so
		// (unlike the ErrIncompleteDerivation case above) absence must be
		// proven via ErrRecordNotFound, never inferred from a transient
		// upstream failure (ErrRecordUnavailable) or a malformed request
		// (ErrInvalidGameURI).
		switch {
		case errors.Is(err, atproto.ErrRecordNotFound):
			log.Warn().Err(err).Str("gameID", gameID).Msg("Game record not found for spectator")
			http.Error(w, "Game not found", http.StatusNotFound)
		case errors.Is(err, atproto.ErrInvalidGameURI):
			log.Warn().Err(err).Str("gameID", gameID).Msg("Malformed game URI for spectator")
			http.Error(w, "Invalid game ID", http.StatusBadRequest)
		default:
			log.Error().Err(err).Str("gameID", gameID).Msg("Failed to fetch game for spectator")
			http.Error(w, "Game status could not be retrieved; try again", http.StatusBadGateway)
		}
		return
	}
	if err != nil {
		log.Warn().Err(err).Str("gameID", gameID).Msg("Game status derivation incomplete for spectator; returning partial/unverified game")
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
		"game":          game,
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
		if err := decodeJSONBody(r, &req); err != nil {
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
			"gameId":         gameID,
			"spectatorCount": spectatorCount,
		})
	}
}

// CheckAbandonmentHandler checks if a game has been abandoned
func (s *Service) CheckAbandonmentHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	gameID := vars["id"]

	// Fetch game. As in GetGameHandler/GetSpectatorGameHandler above, an
	// error wrapping ErrIncompleteDerivation with a non-nil game means the
	// record exists but its derived Status is unproven (e.g. a transient
	// opponent-PDS read failure), not that the game is absent -- that must
	// not 404. It also must not be treated as authoritative for the
	// abandonment computation below: game.Status may be stale/wrong, so
	// neither "already ended" nor a real abandonment/canClaim verdict can
	// be trusted. Report the ambiguity explicitly instead and stop.
	game, err := s.serverClient.GetGame(context.Background(), gameID)
	if err != nil {
		if errors.Is(err, atproto.ErrIncompleteDerivation) && game != nil {
			log.Warn().Err(err).Str("gameID", gameID).Msg("Game status derivation incomplete for abandonment check; withholding verdict")
			w.Header().Set("Content-Type", "application/json")
			// Deliberately omit "abandoned" rather than reporting `false`
			// (atchess-1c9.66): a scan that could not complete cannot
			// distinguish "not abandoned" from "abandoned, but the
			// evidence for it lives in the unreachable repo". Reporting
			// `false` here would be exactly the false reassurance this
			// bead exists to remove -- an unverified negative dressed up
			// as a verified one. Callers must key off
			// "derivationIncomplete" (and the absence of "abandoned") to
			// know the abandonment state is unknown, not "no".
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"canClaim":             false,
				"derivationIncomplete": true,
				"reason":               "Game status could not be verified (one or more repos unreachable); try again",
			})
			return
		}
		// atchess-1c9.67: GetGame failing before ever reading the record --
		// see GetGame's doc comment. There is no partial game here, so
		// (unlike the ErrIncompleteDerivation branch above) absence must be
		// proven via ErrRecordNotFound, never inferred from a transient
		// upstream failure (ErrRecordUnavailable) or a malformed request
		// (ErrInvalidGameURI).
		switch {
		case errors.Is(err, atproto.ErrRecordNotFound):
			log.Warn().Err(err).Str("gameID", gameID).Msg("Game record not found for abandonment check")
			http.Error(w, "Game not found", http.StatusNotFound)
		case errors.Is(err, atproto.ErrInvalidGameURI):
			log.Warn().Err(err).Str("gameID", gameID).Msg("Malformed game URI for abandonment check")
			http.Error(w, "Invalid game ID", http.StatusBadRequest)
		default:
			log.Error().Err(err).Str("gameID", gameID).Msg("Failed to fetch game for abandonment check")
			http.Error(w, "Game status could not be retrieved; try again", http.StatusBadGateway)
		}
		return
	}

	// Only check active games
	if game.Status != chess.StatusActive {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"abandoned": false,
			"reason":    "Game already ended",
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
		"abandoned":         abandoned,
		"lastActivity":      lastActivityStr,
		"timeSinceLastMove": timeSinceLastActivity.String(),
		"timeout":           abandonmentTimeout.String(),
		"canClaim":          abandoned,
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
