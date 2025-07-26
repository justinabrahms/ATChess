package firehose_test

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/justinabrahms/atchess/internal/firehose"
	"github.com/rs/zerolog"
)

func ExampleClient() {
	// Create a logger
	logger := zerolog.New(zerolog.NewConsoleWriter()).With().Timestamp().Logger()
	
	// Define event handler
	handler := func(event firehose.Event) error {
		switch event.Type {
		case firehose.EventTypeMove:
			fmt.Printf("Chess move in game: %s\n", event.Path)
			// Process move record
			if record, ok := event.Record.(map[string]interface{}); ok {
				if gameID, ok := record["gameID"].(string); ok {
					fmt.Printf("Game ID: %s\n", gameID)
				}
			}
			
		case firehose.EventTypeChallenge:
			fmt.Printf("New chess challenge: %s\n", event.Path)
			
		case firehose.EventTypeDrawOffer:
			fmt.Printf("Draw offer in game: %s\n", event.Path)
			
		case firehose.EventTypeResignation:
			fmt.Printf("Player resigned in game: %s\n", event.Path)
			
		case firehose.EventTypeGame:
			fmt.Printf("Game update: %s\n", event.Path)
		}
		
		return nil
	}
	
	// Create client with custom options
	client := firehose.NewClient(
		handler,
		firehose.WithLogger(logger),
		// Optionally use a custom firehose URL
		// firehose.WithURL("wss://custom.firehose.example/xrpc/com.atproto.sync.subscribeRepos"),
	)
	
	// Start the client
	if err := client.Start(); err != nil {
		log.Fatalf("Failed to start firehose client: %v", err)
	}
	
	// Run for some time
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	
	<-ctx.Done()
	
	// Stop the client
	if err := client.Stop(); err != nil {
		log.Printf("Error stopping client: %v", err)
	}
}

func ExampleClient_withFiltering() {
	// Example showing how to filter events by player
	targetPlayer := "did:plc:exampleplayer"
	
	handler := func(event firehose.Event) error {
		// Only process events from our target player
		if event.Repo != targetPlayer {
			return nil
		}
		
		switch event.Type {
		case firehose.EventTypeMove:
			fmt.Printf("Player %s made a move\n", event.Repo)
			
		case firehose.EventTypeChallenge:
			fmt.Printf("Player %s created a challenge\n", event.Repo)
		}
		
		return nil
	}
	
	client := firehose.NewClient(handler)
	
	// Start and use the client...
	_ = client
}

func ExampleClient_withErrorHandling() {
	// Example showing error handling in event handler
	handler := func(event firehose.Event) error {
		// Validate event
		if event.Record == nil {
			return fmt.Errorf("event missing record data")
		}
		
		record, ok := event.Record.(map[string]interface{})
		if !ok {
			return fmt.Errorf("invalid record type")
		}
		
		// Process based on type
		switch event.Type {
		case firehose.EventTypeMove:
			gameID, ok := record["gameID"].(string)
			if !ok {
				return fmt.Errorf("move missing gameID")
			}
			
			move, ok := record["move"].(string)
			if !ok {
				return fmt.Errorf("move missing move notation")
			}
			
			fmt.Printf("Processing move %s in game %s\n", move, gameID)
			
			// Here you would typically:
			// 1. Validate the move
			// 2. Update local game state
			// 3. Notify connected clients
			// 4. Store in database
		}
		
		return nil
	}
	
	logger := zerolog.New(zerolog.NewConsoleWriter()).With().Timestamp().Logger()
	client := firehose.NewClient(handler, firehose.WithLogger(logger))
	
	// The client will log errors from the handler but continue processing
	_ = client
}