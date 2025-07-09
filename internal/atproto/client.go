package atproto

import (
	"context"
	"fmt"
	"time"

	"github.com/bluesky-social/indigo/api"
	"github.com/bluesky-social/indigo/api/atproto"
	"github.com/bluesky-social/indigo/xrpc"
	"github.com/justinabrahms/atchess/internal/chess"
)

type Client struct {
	xrpcClient *xrpc.Client
	did        string
	handle     string
}

func NewClient(pdsURL, handle, password string) (*Client, error) {
	client := &xrpc.Client{
		Host: pdsURL,
	}
	
	// Create session
	session, err := atproto.ServerCreateSession(context.Background(), client, &atproto.ServerCreateSession_Input{
		Identifier: handle,
		Password:   password,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create session: %w", err)
	}
	
	// Set auth info
	client.Auth = &xrpc.AuthInfo{
		AccessJwt:  session.AccessJwt,
		RefreshJwt: session.RefreshJwt,
		Did:        session.Did,
		Handle:     session.Handle,
	}
	
	return &Client{
		xrpcClient: client,
		did:        session.Did,
		handle:     session.Handle,
	}, nil
}

func (c *Client) CreateGame(ctx context.Context, opponentDID string, color string) (*chess.Game, error) {
	// Determine who plays white/black
	var whiteDID, blackDID string
	if color == "white" {
		whiteDID = c.did
		blackDID = opponentDID
	} else if color == "black" {
		whiteDID = opponentDID
		blackDID = c.did
	} else {
		// Random - for now just make challenger white
		whiteDID = c.did
		blackDID = opponentDID
	}
	
	// Create initial game record
	gameRecord := map[string]interface{}{
		"$type":     "app.atchess.game",
		"createdAt": time.Now().Format(time.RFC3339),
		"white":     whiteDID,
		"black":     blackDID,
		"status":    "active",
		"fen":       "rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR w KQkq - 0 1", // Starting position
		"pgn":       "",
	}
	
	// Create record in repository
	resp, err := atproto.RepoCreateRecord(ctx, c.xrpcClient, &atproto.RepoCreateRecord_Input{
		Repo:       c.did,
		Collection: "app.atchess.game",
		Record:     &gameRecord,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create game record: %w", err)
	}
	
	return &chess.Game{
		ID:        resp.Uri,
		White:     whiteDID,
		Black:     blackDID,
		Status:    chess.StatusActive,
		FEN:       gameRecord["fen"].(string),
		PGN:       "",
		CreatedAt: gameRecord["createdAt"].(string),
	}, nil
}

func (c *Client) RecordMove(ctx context.Context, gameURI string, move *chess.MoveResult) error {
	// Create move record
	moveRecord := map[string]interface{}{
		"$type":     "app.atchess.move",
		"createdAt": time.Now().Format(time.RFC3339),
		"game": map[string]interface{}{
			"uri": gameURI,
			"cid": "", // Would need to fetch game CID
		},
		"player": c.did,
		"from":   move.From,
		"to":     move.To,
		"san":    move.SAN,
		"fen":    move.FEN,
	}
	
	if move.Check {
		moveRecord["check"] = true
	}
	if move.Checkmate {
		moveRecord["checkmate"] = true
	}
	
	// Create move record
	_, err := atproto.RepoCreateRecord(ctx, c.xrpcClient, &atproto.RepoCreateRecord_Input{
		Repo:       c.did,
		Collection: "app.atchess.move",
		Record:     &moveRecord,
	})
	if err != nil {
		return fmt.Errorf("failed to create move record: %w", err)
	}
	
	// Update game record with new FEN
	// This would need proper implementation to fetch and update the game record
	
	return nil
}

func (c *Client) CreateChallenge(ctx context.Context, opponentDID, color, message string) (*chess.Challenge, error) {
	challengeRecord := map[string]interface{}{
		"$type":      "app.atchess.challenge",
		"createdAt":  time.Now().Format(time.RFC3339),
		"challenger": c.did,
		"challenged": opponentDID,
		"status":     "pending",
		"color":      color,
		"message":    message,
		"expiresAt":  time.Now().Add(24 * time.Hour).Format(time.RFC3339),
	}
	
	resp, err := atproto.RepoCreateRecord(ctx, c.xrpcClient, &atproto.RepoCreateRecord_Input{
		Repo:       c.did,
		Collection: "app.atchess.challenge",
		Record:     &challengeRecord,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create challenge record: %w", err)
	}
	
	return &chess.Challenge{
		ID:         resp.Uri,
		Challenger: c.did,
		Challenged: opponentDID,
		Status:     "pending",
		Color:      color,
		Message:    message,
		CreatedAt:  challengeRecord["createdAt"].(string),
		ExpiresAt:  challengeRecord["expiresAt"].(string),
	}, nil
}

func (c *Client) GetDID() string {
	return c.did
}

func (c *Client) GetHandle() string {
	return c.handle
}