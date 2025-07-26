package firehose

import (
	"context"
	"fmt"
	"strings"

	"github.com/justinabrahms/atchess/internal/web"
	"github.com/rs/zerolog/log"
)

// EventProcessor handles chess events from the firehose
type EventProcessor struct {
	hub *web.Hub
	// Map of game IDs we're tracking
	trackedGames map[string]bool
	// Map of player DIDs we're tracking
	trackedPlayers map[string]bool
}

// NewEventProcessor creates a new event processor
func NewEventProcessor(hub *web.Hub) *EventProcessor {
	return &EventProcessor{
		hub:            hub,
		trackedGames:   make(map[string]bool),
		trackedPlayers: make(map[string]bool),
	}
}

// TrackGame adds a game to the tracking list
func (p *EventProcessor) TrackGame(gameID string) {
	p.trackedGames[gameID] = true
}

// UntrackGame removes a game from the tracking list
func (p *EventProcessor) UntrackGame(gameID string) {
	delete(p.trackedGames, gameID)
}

// TrackPlayer adds a player DID to the tracking list
func (p *EventProcessor) TrackPlayer(did string) {
	p.trackedPlayers[did] = true
}

// ProcessEvent handles an event from the firehose
func (p *EventProcessor) ProcessEvent(ctx context.Context, event Event) error {
	// Check if we care about this event
	if !p.shouldProcessEvent(event) {
		return nil
	}

	// Route based on event type
	switch event.Type {
	case EventTypeMove:
		return p.processMoveEvent(ctx, event)
	case EventTypeGame:
		return p.processGameEvent(ctx, event)
	case EventTypeDrawOffer:
		return p.processDrawOfferEvent(ctx, event)
	case EventTypeResignation:
		return p.processResignationEvent(ctx, event)
	case EventTypeChallengeNotification:
		return p.processChallengeNotificationEvent(ctx, event)
	default:
		log.Debug().
			Str("type", string(event.Type)).
			Str("path", event.Path).
			Msg("Ignoring unknown chess event type")
	}

	return nil
}

// shouldProcessEvent checks if we should process this event
func (p *EventProcessor) shouldProcessEvent(event Event) bool {
	// Always process if no filters are set
	if len(p.trackedGames) == 0 && len(p.trackedPlayers) == 0 {
		return true
	}

	// Check if this event is from a tracked player
	if p.trackedPlayers[event.Repo] {
		return true
	}

	// For moves and game updates, check if it's a tracked game
	if event.Type == EventTypeMove || event.Type == EventTypeGame {
		// Extract game ID from the record
		if record, ok := event.Record.(map[string]interface{}); ok {
			if gameRef := getGameReference(record); gameRef != "" {
				return p.trackedGames[gameRef]
			}
		}
	}

	return false
}

// processMoveEvent handles a chess move
func (p *EventProcessor) processMoveEvent(ctx context.Context, event Event) error {
	move, ok := event.Record.(map[string]interface{})
	if !ok {
		return fmt.Errorf("invalid move record format")
	}

	// Extract game reference
	gameRef := getGameReference(move)
	if gameRef == "" {
		return fmt.Errorf("move missing game reference")
	}

	// Send update to WebSocket clients watching this game
	update := web.GameUpdate{
		Type:   "move",
		GameID: gameRef,
		Data:   move,
	}

	p.hub.BroadcastToGame(gameRef, update)
	return nil
}

// processGameEvent handles game state updates
func (p *EventProcessor) processGameEvent(ctx context.Context, event Event) error {
	game, ok := event.Record.(map[string]interface{})
	if !ok {
		return fmt.Errorf("invalid game record format")
	}

	// Extract game ID
	gameID, ok := game["id"].(string)
	if !ok {
		return fmt.Errorf("game missing ID")
	}

	// Send update to WebSocket clients
	update := web.GameUpdate{
		Type:   "game_update",
		GameID: gameID,
		Data:   game,
	}

	p.hub.BroadcastToGame(gameID, update)

	log.Info().
		Str("type", string(event.Type)).
		Str("repo", event.Repo).
		Str("path", event.Path).
		Str("gameID", gameID).
		Msg("Processing game state update")

	return nil
}

// processDrawOfferEvent handles draw offers
func (p *EventProcessor) processDrawOfferEvent(ctx context.Context, event Event) error {
	drawOffer, ok := event.Record.(map[string]interface{})
	if !ok {
		return fmt.Errorf("invalid draw offer record format")
	}

	gameRef := getGameReference(drawOffer)
	if gameRef == "" {
		return fmt.Errorf("draw offer missing game reference")
	}

	update := web.GameUpdate{
		Type:   "draw_offer",
		GameID: gameRef,
		Data:   drawOffer,
	}

	p.hub.BroadcastToGame(gameRef, update)
	return nil
}

// processResignationEvent handles resignations
func (p *EventProcessor) processResignationEvent(ctx context.Context, event Event) error {
	resignation, ok := event.Record.(map[string]interface{})
	if !ok {
		return fmt.Errorf("invalid resignation record format")
	}

	gameRef := getGameReference(resignation)
	if gameRef == "" {
		return fmt.Errorf("resignation missing game reference")
	}

	update := web.GameUpdate{
		Type:   "resignation",
		GameID: gameRef,
		Data:   resignation,
	}

	p.hub.BroadcastToGame(gameRef, update)
	return nil
}

// processChallengeNotificationEvent handles challenge notifications
func (p *EventProcessor) processChallengeNotificationEvent(ctx context.Context, event Event) error {
	notification, ok := event.Record.(map[string]interface{})
	if !ok {
		return fmt.Errorf("invalid challenge notification record format")
	}

	// Send to the challenged player
	update := web.GameUpdate{
		Type: "challenge_notification",
		Data: notification,
	}

	// The repo is the challenged player's DID
	p.hub.BroadcastToPlayer(event.Repo, update)
	return nil
}

// isGameTracked checks if we're tracking this game
func (p *EventProcessor) isGameTracked(event Event) bool {
	// Check if it's a game-related event
	if event.Type == EventTypeGame ||
		event.Type == EventTypeMove ||
		event.Type == EventTypeDrawOffer ||
		event.Type == EventTypeResignation {
		record, ok := event.Record.(map[string]interface{})
		if !ok {
			return false
		}
		if gameRef := getGameReference(record); gameRef != "" {
			return p.trackedGames[gameRef]
		}
	}
	return false
}

// isPlayerInvolved checks if a tracked player is involved
func (p *EventProcessor) isPlayerInvolved(event Event) bool {
	// The repo is always one of the players
	if p.trackedPlayers[event.Repo] {
		return true
	}

	// For games, check both players
	if event.Type == EventTypeGame {
		game, ok := event.Record.(map[string]interface{})
		if !ok {
			return false
		}
		if white, ok := game["white"].(string); ok && p.trackedPlayers[white] {
			return true
		}
		if black, ok := game["black"].(string); ok && p.trackedPlayers[black] {
			return true
		}
	}

	return false
}

// getGameReference extracts game reference from various record types
func getGameReference(record map[string]interface{}) string {
	// Try direct game field
	if game, ok := record["game"].(map[string]interface{}); ok {
		if ref, ok := game["$link"].(string); ok {
			return ref
		}
	}

	// Try game ID field
	if id, ok := record["id"].(string); ok {
		return id
	}

	// Try gameId field
	if id, ok := record["gameId"].(string); ok {
		return id
	}

	return ""
}

// CreateChessEventHandler creates an event handler for the firehose client
func CreateChessEventHandler(processor *EventProcessor) EventHandler {
	return func(event Event) error {
		// Only process chess-related events
		if !strings.Contains(event.Path, "app.atchess") {
			return nil
		}

		// Process the event
		return processor.ProcessEvent(context.Background(), event)
	}
}