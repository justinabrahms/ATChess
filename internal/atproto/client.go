package atproto

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
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
	// First, fetch the game record to get its CID and current value
	gameCID, gameValue, err := c.getGameRecord(ctx, gameURI)
	if err != nil {
		return fmt.Errorf("failed to get game record: %w", err)
	}
	
	// Create move record
	moveRecord := map[string]interface{}{
		"$type":     "app.atchess.move",
		"createdAt": time.Now().Format(time.RFC3339),
		"game": map[string]interface{}{
			"uri": gameURI,
			"cid": gameCID,
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
	
	// Update game record with new FEN only if it's in our repository
	// Parse the game URI to get repo and rkey
	parts := strings.Split(gameURI, "/")
	if len(parts) < 5 || !strings.HasPrefix(gameURI, "at://") {
		return fmt.Errorf("invalid game URI format: %s", gameURI)
	}
	
	repo := parts[2] // The DID
	rkey := parts[4] // The record key
	
	// Only update the game record if it belongs to the current user
	if repo != c.did {
		// Game belongs to another user, we can't update it
		return nil
	}
	
	// Update the game record with new FEN and status
	gameValue["fen"] = move.FEN
	if move.Checkmate || move.Draw {
		if move.Checkmate {
			// Determine winner based on whose turn it was
			fenParts := strings.Split(move.FEN, " ")
			if len(fenParts) > 1 && fenParts[1] == "w" {
				gameValue["status"] = "black_won"
			} else {
				gameValue["status"] = "white_won"
			}
		} else if move.Draw {
			gameValue["status"] = "draw"
		}
	}
	gameValue["updatedAt"] = time.Now().Format(time.RFC3339)
	
	// Use com.atproto.repo.putRecord to update the game
	putReq := map[string]interface{}{
		"repo":       repo,
		"collection": "app.atchess.game",
		"rkey":       rkey,
		"record":     gameValue,
		"swapCid":    gameCID, // Optimistic concurrency control
	}
	
	putReqBody, _ := json.Marshal(putReq)
	putReqHTTP, err := http.NewRequest("POST", c.pdsURL+"/xrpc/com.atproto.repo.putRecord", bytes.NewBuffer(putReqBody))
	if err != nil {
		return fmt.Errorf("failed to create put request: %w", err)
	}
	
	putReqHTTP.Header.Set("Content-Type", "application/json")
	putReqHTTP.Header.Set("Authorization", "Bearer "+c.accessJWT)
	
	putResp, err := c.httpClient.Do(putReqHTTP)
	if err != nil {
		return fmt.Errorf("failed to update game record: %w", err)
	}
	defer putResp.Body.Close()
	
	if putResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(putResp.Body)
		return fmt.Errorf("failed to update game record: HTTP %d, body: %s", putResp.StatusCode, string(body))
	}
	
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

// getGameRecord fetches a game record and returns its CID and value
func (c *Client) getGameRecord(ctx context.Context, gameURI string) (string, map[string]interface{}, error) {
	// Parse the AT Protocol URI to extract repo and rkey
	// Format: at://did:plc:USER/app.atchess.game/RKEY
	parts := strings.Split(gameURI, "/")
	if len(parts) < 5 || !strings.HasPrefix(gameURI, "at://") {
		return "", nil, fmt.Errorf("invalid AT Protocol URI format: %s", gameURI)
	}
	
	repo := parts[2] // The DID
	rkey := parts[4] // The record key
	
	req, err := http.NewRequest("GET", fmt.Sprintf("%s/xrpc/com.atproto.repo.getRecord?repo=%s&collection=app.atchess.game&rkey=%s", 
		c.pdsURL, repo, rkey), nil)
	if err != nil {
		return "", nil, fmt.Errorf("failed to create request: %w", err)
	}
	
	req.Header.Set("Authorization", "Bearer "+c.accessJWT)
	
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", nil, fmt.Errorf("failed to get game record: %w", err)
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", nil, fmt.Errorf("failed to get game record: HTTP %d - %s", resp.StatusCode, string(body))
	}
	
	var getResp struct {
		URI   string                 `json:"uri"`
		CID   string                 `json:"cid"`
		Value map[string]interface{} `json:"value"`
	}
	
	if err := json.NewDecoder(resp.Body).Decode(&getResp); err != nil {
		return "", nil, fmt.Errorf("failed to decode response: %w", err)
	}
	
	return getResp.CID, getResp.Value, nil
}

func (c *Client) GetGame(ctx context.Context, gameURI string) (*chess.Game, error) {
	// Parse the AT Protocol URI to extract repo and rkey
	// Example URI: at://did:plc:example/app.atchess.game/3k2uv5...
	// We need to call com.atproto.repo.getRecord
	
	// Parse the URI to extract components
	// Format: at://did:plc:USER/app.atchess.game/RKEY
	parts := strings.Split(gameURI, "/")
	if len(parts) < 4 || !strings.HasPrefix(gameURI, "at://") {
		return nil, fmt.Errorf("invalid AT Protocol URI format: %s", gameURI)
	}
	
	repo := parts[2] // The DID
	rkey := parts[4] // The record key
	
	req, err := http.NewRequest("GET", fmt.Sprintf("%s/xrpc/com.atproto.repo.getRecord?repo=%s&collection=app.atchess.game&rkey=%s", 
		c.pdsURL, repo, rkey), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	
	req.Header.Set("Authorization", "Bearer "+c.accessJWT)
	
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to get game record: %w", err)
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to get game record: HTTP %d", resp.StatusCode)
	}
	
	var getResp struct {
		Value struct {
			Type      string `json:"$type"`
			CreatedAt string `json:"createdAt"`
			White     string `json:"white"`
			Black     string `json:"black"`
			Status    string `json:"status"`
			FEN       string `json:"fen"`
			PGN       string `json:"pgn"`
		} `json:"value"`
	}
	
	if err := json.NewDecoder(resp.Body).Decode(&getResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}
	
	return &chess.Game{
		ID:        gameURI,
		White:     getResp.Value.White,
		Black:     getResp.Value.Black,
		Status:    chess.GameStatus(getResp.Value.Status),
		FEN:       getResp.Value.FEN,
		PGN:       getResp.Value.PGN,
		CreatedAt: getResp.Value.CreatedAt,
	}, nil
}

func (c *Client) GetHandle() string {
	return c.handle
}