package atproto

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/justinabrahms/atchess/internal/chess"
)

type Client struct {
	pdsURL     string
	accessJWT  string
	did        string
	handle     string
	httpClient *http.Client
}

func NewClient(pdsURL, handle, password string) (*Client, error) {
	httpClient := &http.Client{
		Timeout: 30 * time.Second,
	}
	
	// Create session
	sessionReq := map[string]interface{}{
		"identifier": handle,
		"password":   password,
	}
	
	reqBody, _ := json.Marshal(sessionReq)
	req, err := http.NewRequest("POST", pdsURL+"/xrpc/com.atproto.server.createSession", bytes.NewBuffer(reqBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	
	req.Header.Set("Content-Type", "application/json")
	
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to create session: %w", err)
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to create session: HTTP %d", resp.StatusCode)
	}
	
	var session struct {
		AccessJwt string `json:"accessJwt"`
		Did       string `json:"did"`
		Handle    string `json:"handle"`
	}
	
	if err := json.NewDecoder(resp.Body).Decode(&session); err != nil {
		return nil, fmt.Errorf("failed to decode session response: %w", err)
	}
	
	return &Client{
		pdsURL:     pdsURL,
		accessJWT:  session.AccessJwt,
		did:        session.Did,
		handle:     session.Handle,
		httpClient: httpClient,
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
	createReq := map[string]interface{}{
		"repo":       c.did,
		"collection": "app.atchess.game",
		"record":     gameRecord,
	}
	
	reqBody, _ := json.Marshal(createReq)
	req, err := http.NewRequest("POST", c.pdsURL+"/xrpc/com.atproto.repo.createRecord", bytes.NewBuffer(reqBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.accessJWT)
	
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to create game record: %w", err)
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to create game record: HTTP %d", resp.StatusCode)
	}
	
	var createResp struct {
		URI string `json:"uri"`
		CID string `json:"cid"`
	}
	
	if err := json.NewDecoder(resp.Body).Decode(&createResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}
	
	return &chess.Game{
		ID:        createResp.URI,
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
	createReq := map[string]interface{}{
		"repo":       c.did,
		"collection": "app.atchess.move",
		"record":     moveRecord,
	}
	
	reqBody, _ := json.Marshal(createReq)
	req, err := http.NewRequest("POST", c.pdsURL+"/xrpc/com.atproto.repo.createRecord", bytes.NewBuffer(reqBody))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.accessJWT)
	
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to create move record: %w", err)
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to create move record: HTTP %d", resp.StatusCode)
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
	
	createReq := map[string]interface{}{
		"repo":       c.did,
		"collection": "app.atchess.challenge",
		"record":     challengeRecord,
	}
	
	reqBody, _ := json.Marshal(createReq)
	req, err := http.NewRequest("POST", c.pdsURL+"/xrpc/com.atproto.repo.createRecord", bytes.NewBuffer(reqBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.accessJWT)
	
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to create challenge record: %w", err)
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to create challenge record: HTTP %d", resp.StatusCode)
	}
	
	var createResp struct {
		URI string `json:"uri"`
		CID string `json:"cid"`
	}
	
	if err := json.NewDecoder(resp.Body).Decode(&createResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}
	
	return &chess.Challenge{
		ID:         createResp.URI,
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