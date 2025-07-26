package firehose

import (
	"context"
	"encoding/json"
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

	switch event.Collection {
	case "app.atchess.move":
		return p.processMoveEvent(ctx, event)
	case "app.atchess.game":
		return p.processGameEvent(ctx, event)
	case "app.atchess.drawOffer":
		return p.processDrawOfferEvent(ctx, event)
	case "app.atchess.resignation":
		return p.processResignationEvent(ctx, event)
	case "app.atchess.challenge":
		return p.processChallengeEvent(ctx, event)
	case "app.atchess.challengeNotification":
		return p.processChallengeNotificationEvent(ctx, event)
	default:
		log.Debug().
			Str("collection", event.Collection).
			Msg("Ignoring unknown chess collection")
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
	if event.Collection == "app.atchess.move" || event.Collection == "app.atchess.game" {
		// Extract game ID from the record
		if gameRef, ok := getGameReference(event.Record); ok {
			return p.trackedGames[gameRef]
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
	gameRef, ok := getGameReference(move)
	if !ok {
		return fmt.Errorf("move missing game reference")
	}

	// Extract move details
	moveData := map[string]interface{}{
		"from":       move["from"],
		"to":         move["to"],
		"san":        move["san"],
		"fen":        move["fen"],
		"player":     move["player"],
		"moveNumber": move["moveNumber"],
		"check":      move["check"],
		"checkmate":  move["checkmate"],
		"createdAt":  move["createdAt"],
	}

	// Broadcast to WebSocket clients
	if p.hub != nil {
		p.hub.HandleFirehoseEvent(ctx, "move", gameRef, moveData)
	}

	log.Info().
		Str("gameID", gameRef).
		Str("san", getString(move, "san")).
		Str("player", getString(move, "player")).
		Msg("Move processed from firehose")

	return nil
}

// processGameEvent handles game state updates
func (p *EventProcessor) processGameEvent(ctx context.Context, event Event) error {
	game, ok := event.Record.(map[string]interface{})
	if !ok {
		return fmt.Errorf("invalid game record format")
	}

	// Extract game ID from URI
	gameID := extractIDFromURI(event.URI)
	if gameID != "" {
		// Track this game for future events
		p.TrackGame(gameID)
	}

	// Broadcast game update
	if p.hub != nil {
		p.hub.HandleFirehoseEvent(ctx, "game_update", gameID, game)
	}

	status := getString(game, "status")
	if status != "active" {
		log.Info().
			Str("gameID", gameID).
			Str("status", status).
			Msg("Game ended")
		
		// Stop tracking ended games
		p.UntrackGame(gameID)
	}

	return nil
}

// processDrawOfferEvent handles draw offers
func (p *EventProcessor) processDrawOfferEvent(ctx context.Context, event Event) error {
	drawOffer, ok := event.Record.(map[string]interface{})
	if !ok {
		return fmt.Errorf("invalid draw offer record format")
	}

	gameRef, ok := getGameReference(drawOffer)
	if !ok {
		return fmt.Errorf("draw offer missing game reference")
	}

	// Broadcast draw offer
	if p.hub != nil {
		p.hub.HandleFirehoseEvent(ctx, "draw_offer", gameRef, drawOffer)
	}

	log.Info().
		Str("gameID", gameRef).
		Str("offeredBy", getString(drawOffer, "offeredBy")).
		Str("status", getString(drawOffer, "status")).
		Msg("Draw offer processed")

	return nil
}

// processResignationEvent handles resignations
func (p *EventProcessor) processResignationEvent(ctx context.Context, event Event) error {
	resignation, ok := event.Record.(map[string]interface{})
	if !ok {
		return fmt.Errorf("invalid resignation record format")
	}

	gameRef, ok := getGameReference(resignation)
	if !ok {
		return fmt.Errorf("resignation missing game reference")
	}

	// Broadcast resignation
	if p.hub != nil {
		p.hub.HandleFirehoseEvent(ctx, "resignation", gameRef, resignation)
	}

	log.Info().
		Str("gameID", gameRef).
		Str("resigningPlayer", getString(resignation, "resigningPlayer")).
		Msg("Resignation processed")

	return nil
}

// processChallengeEvent handles new challenges
func (p *EventProcessor) processChallengeEvent(ctx context.Context, event Event) error {
	challenge, ok := event.Record.(map[string]interface{})
	if !ok {
		return fmt.Errorf("invalid challenge record format")
	}

	// Track players involved in challenges
	if challenger := getString(challenge, "challenger"); challenger != "" {
		p.TrackPlayer(challenger)
	}
	if challenged := getString(challenge, "challenged"); challenged != "" {
		p.TrackPlayer(challenged)
	}

	log.Debug().
		Str("challenger", getString(challenge, "challenger")).
		Str("challenged", getString(challenge, "challenged")).
		Msg("Challenge processed")

	return nil
}

// processChallengeNotificationEvent handles challenge notifications
func (p *EventProcessor) processChallengeNotificationEvent(ctx context.Context, event Event) error {
	// Challenge notifications are primarily for the UI
	// The firehose processor just logs them
	log.Debug().
		Str("repo", event.Repo).
		Msg("Challenge notification processed")
	
	return nil
}

// Helper functions

// getGameReference extracts game URI from a record
func getGameReference(record map[string]interface{}) (string, bool) {
	// Check for direct game field
	if gameRef, ok := record["game"].(map[string]interface{}); ok {
		if uri, ok := gameRef["uri"].(string); ok {
			return extractIDFromURI(uri), true
		}
	}
	
	// For game records, use the record key
	if record["$type"] == "app.atchess.game" {
		// The game ID would be in the event URI
		return "", false
	}
	
	return "", false
}

// extractIDFromURI extracts the record ID from an AT URI
// Format: at://did:plc:xxx/collection/recordID
func extractIDFromURI(uri string) string {
	parts := strings.Split(uri, "/")
	if len(parts) >= 5 {
		return parts[4]
	}
	return ""
}

// getString safely gets a string from a map
func getString(m map[string]interface{}, key string) string {
	if val, ok := m[key].(string); ok {
		return val
	}
	return ""
}

// CreateChessEventHandler creates an event handler for the firehose client
func CreateChessEventHandler(processor *EventProcessor) EventHandler {
	return func(ctx context.Context, event Event) error {
		// Only process chess-related events
		if !strings.HasPrefix(event.Collection, "app.atchess.") {
			return nil
		}

		return processor.ProcessEvent(ctx, event)
	}
}