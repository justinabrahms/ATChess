package atproto

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/justinabrahms/atchess/internal/auth"
	"github.com/justinabrahms/atchess/internal/chess"
)

type Client struct {
	pdsURL      string
	accessJWT   string
	did         string
	handle      string
	httpClient  *http.Client
	dpopManager *auth.DPoPManager
	useDPoP     bool
}

// generateGameID creates a deterministic record key for a game based on challenge parameters
func generateGameID(challengerDID, challengedDID string, timestamp time.Time) string {
	// Create deterministic input from challenge parameters
	input := fmt.Sprintf("%s:%s:%d", challengerDID, challengedDID, timestamp.Unix())
	
	// Hash the input
	hash := sha256.Sum256([]byte(input))
	
	// Encode to base32 and take first 13 characters (similar to TID length)
	encoder := base32.StdEncoding.WithPadding(base32.NoPadding)
	encoded := encoder.EncodeToString(hash[:8])
	
	// Convert to lowercase and add prefix to distinguish from auto-generated TIDs
	return "ch" + strings.ToLower(encoded)[:11]
}

// NewClient creates a new AT Protocol client without DPoP support
func NewClient(pdsURL, handle, password string) (*Client, error) {
	return NewClientWithDPoP(pdsURL, handle, password, false)
}

// NewClientWithDPoP creates a new AT Protocol client with optional DPoP support
func NewClientWithDPoP(pdsURL, handle, password string, useDPoP bool) (*Client, error) {
	var httpClient *http.Client
	var dpopManager *auth.DPoPManager

	if useDPoP {
		// Create DPoP manager
		manager, err := auth.NewDPoPManager()
		if err != nil {
			return nil, fmt.Errorf("failed to create DPoP manager: %w", err)
		}
		dpopManager = manager
		
		// Create a DPoP-enabled HTTP client
		// We'll set up the token getter after authentication
		httpClient = &http.Client{
			Timeout: 30 * time.Second,
		}
	} else {
		httpClient = &http.Client{
			Timeout: 30 * time.Second,
		}
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
	
	client := &Client{
		pdsURL:      pdsURL,
		accessJWT:   session.AccessJwt,
		did:         session.Did,
		handle:      session.Handle,
		httpClient:  httpClient,
		dpopManager: dpopManager,
		useDPoP:     useDPoP,
	}

	// If using DPoP, update the HTTP client to use the interceptor
	if useDPoP {
		client.httpClient = auth.NewDPoPClient(dpopManager, func() string {
			return client.accessJWT
		})
	}

	return client, nil
}

// GetDID returns the authenticated user's DID
func (c *Client) GetDID() string {
	return c.did
}

// makeRequest is a helper method to create and execute HTTP requests with proper authentication
func (c *Client) makeRequest(method, url string, body []byte) (*http.Response, error) {
	req, err := http.NewRequest(method, url, bytes.NewBuffer(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	
	req.Header.Set("Content-Type", "application/json")
	
	// Set authorization header based on whether DPoP is enabled
	if c.useDPoP {
		req.Header.Set("Authorization", "DPoP "+c.accessJWT)
	} else {
		req.Header.Set("Authorization", "Bearer "+c.accessJWT)
	}
	
	return c.httpClient.Do(req)
}

// CreateGameFromChallenge creates a game record using a specific rkey and challenge reference
func (c *Client) CreateGameFromChallenge(ctx context.Context, opponentDID, color, rkey, challengeURI, challengeCID string) (*chess.Game, error) {
	return c.createGame(ctx, opponentDID, color, &rkey, challengeURI, challengeCID)
}

func (c *Client) CreateGame(ctx context.Context, opponentDID string, color string) (*chess.Game, error) {
	return c.createGame(ctx, opponentDID, color, nil, "", "")
}

func (c *Client) createGame(ctx context.Context, opponentDID, color string, rkey *string, challengeURI, challengeCID string) (*chess.Game, error) {
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
	
	// Add challenge reference if provided
	if challengeURI != "" {
		gameRecord["challenge"] = map[string]interface{}{
			"uri": challengeURI,
			"cid": challengeCID,
		}
	}
	
	// Create record in repository
	createReq := map[string]interface{}{
		"repo":       c.did,
		"collection": "app.atchess.game",
		"record":     gameRecord,
	}
	
	// Add explicit rkey if provided
	if rkey != nil {
		createReq["rkey"] = *rkey
	}
	
	reqBody, _ := json.Marshal(createReq)
	resp, err := c.makeRequest("POST", c.pdsURL+"/xrpc/com.atproto.repo.createRecord", reqBody)
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
	resp, err := c.makeRequest("POST", c.pdsURL+"/xrpc/com.atproto.repo.createRecord", reqBody)
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
	putResp, err := c.makeRequest("POST", c.pdsURL+"/xrpc/com.atproto.repo.putRecord", putReqBody)
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
	createdAt := time.Now()
	proposedGameID := generateGameID(c.did, opponentDID, createdAt)
	
	challengeRecord := map[string]interface{}{
		"$type":         "app.atchess.challenge",
		"createdAt":     createdAt.Format(time.RFC3339),
		"challenger":    c.did,
		"challenged":    opponentDID,
		"status":        "pending",
		"color":         color,
		"proposedGameId": proposedGameID,
		"message":       message,
		"expiresAt":     createdAt.Add(24 * time.Hour).Format(time.RFC3339),
	}
	
	createReq := map[string]interface{}{
		"repo":       c.did,
		"collection": "app.atchess.challenge",
		"record":     challengeRecord,
	}
	
	reqBody, _ := json.Marshal(createReq)
	resp, err := c.makeRequest("POST", c.pdsURL+"/xrpc/com.atproto.repo.createRecord", reqBody)
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
	
	// Try to create a notification in the challenged player's repository
	// This is best-effort - it may fail if we can't write to their repo
	timeControl := map[string]interface{}{
		"type":        "correspondence",
		"daysPerMove": 3,
	}
	
	// Attempt to create notification but don't fail the challenge creation if it fails
	notificationErr := c.CreateChallengeNotification(ctx, opponentDID, createResp.URI, createResp.CID, c.handle, color, message, timeControl)
	if notificationErr != nil {
		// Log the error but don't fail the challenge creation
		// In a real implementation, you might want to log this properly
		fmt.Printf("Warning: Could not create challenge notification: %v\n", notificationErr)
	}
	
	return &chess.Challenge{
		ID:             createResp.URI,
		Challenger:     c.did,
		Challenged:     opponentDID,
		Status:         "pending",
		Color:          color,
		ProposedGameId: proposedGameID,
		Message:        message,
		CreatedAt:      challengeRecord["createdAt"].(string),
		ExpiresAt:      challengeRecord["expiresAt"].(string),
	}, nil
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
	
	url := fmt.Sprintf("%s/xrpc/com.atproto.repo.getRecord?repo=%s&collection=app.atchess.game&rkey=%s", 
		c.pdsURL, repo, rkey)
	resp, err := c.makeRequest("GET", url, nil)
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
	
	url := fmt.Sprintf("%s/xrpc/com.atproto.repo.getRecord?repo=%s&collection=app.atchess.game&rkey=%s", 
		c.pdsURL, repo, rkey)
	resp, err := c.makeRequest("GET", url, nil)
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
			TimeControl *struct {
				Type        string `json:"type"`
				Initial     int    `json:"initial"`
				Increment   int    `json:"increment"`
				DaysPerMove int    `json:"daysPerMove"`
			} `json:"timeControl"`
		} `json:"value"`
	}
	
	if err := json.NewDecoder(resp.Body).Decode(&getResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}
	
	var timeControl *chess.TimeControl
	if getResp.Value.TimeControl != nil {
		timeControl = &chess.TimeControl{
			Type:        getResp.Value.TimeControl.Type,
			DaysPerMove: getResp.Value.TimeControl.DaysPerMove,
			Initial:     getResp.Value.TimeControl.Initial,
			Increment:   getResp.Value.TimeControl.Increment,
		}
	}
	
	return &chess.Game{
		ID:          gameURI,
		White:       getResp.Value.White,
		Black:       getResp.Value.Black,
		Status:      chess.GameStatus(getResp.Value.Status),
		FEN:         getResp.Value.FEN,
		PGN:         getResp.Value.PGN,
		TimeControl: timeControl,
		CreatedAt:   getResp.Value.CreatedAt,
	}, nil
}

func (c *Client) GetHandle() string {
	return c.handle
}

// CreateChallengeNotification creates a notification in the challenged player's repository
func (c *Client) CreateChallengeNotification(ctx context.Context, challengedDID, challengeURI, challengeCID, challengerHandle, color, message string, timeControl map[string]interface{}) error {
	// Calculate expiration time (24 hours from now)
	expiresAt := time.Now().Add(24 * time.Hour)
	
	// Create notification record
	notificationRecord := map[string]interface{}{
		"$type":     "app.atchess.challengeNotification",
		"createdAt": time.Now().Format(time.RFC3339),
		"challenge": map[string]interface{}{
			"uri": challengeURI,
			"cid": challengeCID,
		},
		"challenger":       c.did,
		"challengerHandle": challengerHandle,
		"color":           color,
		"expiresAt":       expiresAt.Format(time.RFC3339),
	}
	
	// Add optional fields
	if message != "" {
		notificationRecord["message"] = message
	}
	
	if timeControl != nil {
		notificationRecord["timeControl"] = timeControl
	}
	
	// Create record in challenged player's repository
	createReq := map[string]interface{}{
		"repo":       challengedDID,
		"collection": "app.atchess.challengeNotification",
		"record":     notificationRecord,
	}
	
	reqBody, _ := json.Marshal(createReq)
	resp, err := c.makeRequest("POST", c.pdsURL+"/xrpc/com.atproto.repo.createRecord", reqBody)
	if err != nil {
		return fmt.Errorf("failed to create challenge notification: %w", err)
	}
	defer resp.Body.Close()
	
	// Handle expected error cases
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized {
		// We don't have permission to write to the challenged player's repo
		// This is expected in many cases (different PDS, privacy settings, etc.)
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("cannot write to challenged player's repository: HTTP %d - %s", resp.StatusCode, string(body))
	}
	
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed to create challenge notification: HTTP %d - %s", resp.StatusCode, string(body))
	}
	
	return nil
}

// GetChallengeNotifications retrieves pending challenge notifications for the current user
func (c *Client) GetChallengeNotifications(ctx context.Context) ([]*ChallengeNotification, error) {
	// List records in the challengeNotification collection
	url := fmt.Sprintf("%s/xrpc/com.atproto.repo.listRecords?repo=%s&collection=app.atchess.challengeNotification&limit=100",
		c.pdsURL, c.did)
	resp, err := c.makeRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to list challenge notifications: %w", err)
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("failed to list challenge notifications: HTTP %d - %s", resp.StatusCode, string(body))
	}
	
	var listResp struct {
		Records []struct {
			URI   string `json:"uri"`
			CID   string `json:"cid"`
			Value struct {
				Type      string `json:"$type"`
				CreatedAt string `json:"createdAt"`
				Challenge struct {
					URI string `json:"uri"`
					CID string `json:"cid"`
				} `json:"challenge"`
				Challenger       string                 `json:"challenger"`
				ChallengerHandle string                 `json:"challengerHandle"`
				Color            string                 `json:"color"`
				Message          string                 `json:"message"`
				ExpiresAt        string                 `json:"expiresAt"`
				TimeControl      map[string]interface{} `json:"timeControl"`
			} `json:"value"`
		} `json:"records"`
	}
	
	if err := json.NewDecoder(resp.Body).Decode(&listResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}
	
	// Filter out expired notifications and convert to our type
	var notifications []*ChallengeNotification
	now := time.Now()
	
	for _, record := range listResp.Records {
		// Parse expiration time
		expiresAt, err := time.Parse(time.RFC3339, record.Value.ExpiresAt)
		if err != nil {
			continue // Skip if we can't parse the expiration
		}
		
		// Skip expired notifications
		if expiresAt.Before(now) {
			continue
		}
		
		notification := &ChallengeNotification{
			URI:              record.URI,
			CID:              record.CID,
			CreatedAt:        record.Value.CreatedAt,
			ChallengeURI:     record.Value.Challenge.URI,
			ChallengeCID:     record.Value.Challenge.CID,
			Challenger:       record.Value.Challenger,
			ChallengerHandle: record.Value.ChallengerHandle,
			Color:            record.Value.Color,
			Message:          record.Value.Message,
			ExpiresAt:        record.Value.ExpiresAt,
			TimeControl:      record.Value.TimeControl,
		}
		
		notifications = append(notifications, notification)
	}
	
	return notifications, nil
}

// ChallengeNotification represents a challenge notification record
type ChallengeNotification struct {
	URI              string
	CID              string
	CreatedAt        string
	ChallengeURI     string
	ChallengeCID     string
	Challenger       string
	ChallengerHandle string
	Color            string
	Message          string
	ExpiresAt        string
	TimeControl      map[string]interface{}
}

// DeleteChallengeNotification removes a challenge notification from the user's repository
func (c *Client) DeleteChallengeNotification(ctx context.Context, notificationURI string) error {
	// Parse the URI to extract repo and rkey
	// Format: at://did:plc:USER/app.atchess.challengeNotification/RKEY
	parts := strings.Split(notificationURI, "/")
	if len(parts) < 5 || !strings.HasPrefix(notificationURI, "at://") {
		return fmt.Errorf("invalid notification URI format: %s", notificationURI)
	}
	
	repo := parts[2] // The DID
	rkey := parts[4] // The record key
	
	// Verify this notification belongs to the current user
	if repo != c.did {
		return fmt.Errorf("cannot delete notification from another user's repository")
	}
	
	// Delete the record
	deleteReq := map[string]interface{}{
		"repo":       repo,
		"collection": "app.atchess.challengeNotification",
		"rkey":       rkey,
	}
	
	reqBody, _ := json.Marshal(deleteReq)
	resp, err := c.makeRequest("POST", c.pdsURL+"/xrpc/com.atproto.repo.deleteRecord", reqBody)
	if err != nil {
		return fmt.Errorf("failed to delete notification: %w", err)
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed to delete notification: HTTP %d - %s", resp.StatusCode, string(body))
	}
	
	return nil
}

// OfferDraw creates a draw offer record for a game
func (c *Client) OfferDraw(ctx context.Context, gameID string, message string) (*DrawOffer, error) {
	// First, fetch the game record to get its CID
	gameCID, gameValue, err := c.getGameRecord(ctx, gameID)
	if err != nil {
		return nil, fmt.Errorf("failed to get game record: %w", err)
	}
	
	// Verify the game is active
	if status, ok := gameValue["status"].(string); ok && status != "active" {
		return nil, fmt.Errorf("cannot offer draw in a game with status: %s", status)
	}
	
	// Create draw offer record
	drawOfferRecord := map[string]interface{}{
		"$type":     "app.atchess.drawOffer",
		"createdAt": time.Now().Format(time.RFC3339),
		"game": map[string]interface{}{
			"uri": gameID,
			"cid": gameCID,
		},
		"offeredBy": c.did,
		"status":    "pending",
	}
	
	// Add optional message
	if message != "" {
		drawOfferRecord["message"] = message
	}
	
	// Create record in repository
	createReq := map[string]interface{}{
		"repo":       c.did,
		"collection": "app.atchess.drawOffer",
		"record":     drawOfferRecord,
	}
	
	reqBody, _ := json.Marshal(createReq)
	resp, err := c.makeRequest("POST", c.pdsURL+"/xrpc/com.atproto.repo.createRecord", reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to create draw offer record: %w", err)
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("failed to create draw offer record: HTTP %d - %s", resp.StatusCode, string(body))
	}
	
	var createResp struct {
		URI string `json:"uri"`
		CID string `json:"cid"`
	}
	
	if err := json.NewDecoder(resp.Body).Decode(&createResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}
	
	return &DrawOffer{
		URI:       createResp.URI,
		CID:       createResp.CID,
		CreatedAt: drawOfferRecord["createdAt"].(string),
		GameURI:   gameID,
		GameCID:   gameCID,
		OfferedBy: c.did,
		Message:   message,
		Status:    "pending",
	}, nil
}

// RespondToDrawOffer accepts or declines a draw offer
func (c *Client) RespondToDrawOffer(ctx context.Context, drawOfferURI string, accept bool) error {
	// Parse the draw offer URI to extract repo and rkey
	parts := strings.Split(drawOfferURI, "/")
	if len(parts) < 5 || !strings.HasPrefix(drawOfferURI, "at://") {
		return fmt.Errorf("invalid draw offer URI format: %s", drawOfferURI)
	}
	
	repo := parts[2] // The DID
	rkey := parts[4] // The record key
	
	// Get the draw offer record
	url := fmt.Sprintf("%s/xrpc/com.atproto.repo.getRecord?repo=%s&collection=app.atchess.drawOffer&rkey=%s", 
		c.pdsURL, repo, rkey)
	resp, err := c.makeRequest("GET", url, nil)
	if err != nil {
		return fmt.Errorf("failed to get draw offer record: %w", err)
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed to get draw offer record: HTTP %d - %s", resp.StatusCode, string(body))
	}
	
	var getResp struct {
		URI   string                 `json:"uri"`
		CID   string                 `json:"cid"`
		Value map[string]interface{} `json:"value"`
	}
	
	if err := json.NewDecoder(resp.Body).Decode(&getResp); err != nil {
		return fmt.Errorf("failed to decode response: %w", err)
	}
	
	// Verify the draw offer is still pending
	if status, ok := getResp.Value["status"].(string); ok && status != "pending" {
		return fmt.Errorf("draw offer is not pending, current status: %s", status)
	}
	
	// Get the game reference
	gameRef, ok := getResp.Value["game"].(map[string]interface{})
	if !ok {
		return fmt.Errorf("invalid game reference in draw offer")
	}
	gameURI, ok := gameRef["uri"].(string)
	if !ok {
		return fmt.Errorf("missing game URI in draw offer")
	}
	
	// Update the draw offer record
	getResp.Value["status"] = "accepted"
	if !accept {
		getResp.Value["status"] = "declined"
	}
	getResp.Value["respondedAt"] = time.Now().Format(time.RFC3339)
	getResp.Value["respondedBy"] = c.did
	
	// Update the draw offer record
	putReq := map[string]interface{}{
		"repo":       repo,
		"collection": "app.atchess.drawOffer",
		"rkey":       rkey,
		"record":     getResp.Value,
		"swapCid":    getResp.CID,
	}
	
	putReqBody, _ := json.Marshal(putReq)
	putResp, err := c.makeRequest("POST", c.pdsURL+"/xrpc/com.atproto.repo.putRecord", putReqBody)
	if err != nil {
		return fmt.Errorf("failed to update draw offer record: %w", err)
	}
	defer putResp.Body.Close()
	
	if putResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(putResp.Body)
		return fmt.Errorf("failed to update draw offer record: HTTP %d - %s", putResp.StatusCode, string(body))
	}
	
	// If the draw was accepted, update the game status
	if accept {
		// Get the game record
		gameCID, gameValue, err := c.getGameRecord(ctx, gameURI)
		if err != nil {
			return fmt.Errorf("failed to get game record for status update: %w", err)
		}
		
		// Parse the game URI to check if we own the game record
		gameParts := strings.Split(gameURI, "/")
		if len(gameParts) >= 5 && gameParts[2] == c.did {
			// Update the game status to draw
			gameValue["status"] = "draw"
			gameValue["updatedAt"] = time.Now().Format(time.RFC3339)
			
			// Update the game record
			gameRkey := gameParts[4]
			updateGameReq := map[string]interface{}{
				"repo":       c.did,
				"collection": "app.atchess.game",
				"rkey":       gameRkey,
				"record":     gameValue,
				"swapCid":    gameCID,
			}
			
			updateGameReqBody, _ := json.Marshal(updateGameReq)
			updateGameResp, err := c.makeRequest("POST", c.pdsURL+"/xrpc/com.atproto.repo.putRecord", updateGameReqBody)
			if err != nil {
				return fmt.Errorf("failed to update game record: %w", err)
			}
			defer updateGameResp.Body.Close()
			
			if updateGameResp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(updateGameResp.Body)
				return fmt.Errorf("failed to update game record: HTTP %d - %s", updateGameResp.StatusCode, string(body))
			}
		}
	}
	
	return nil
}

// ResignGame creates a resignation record and updates the game status
func (c *Client) ResignGame(ctx context.Context, gameID string, reason string) error {
	// First, fetch the game record to get its CID and current state
	gameCID, gameValue, err := c.getGameRecord(ctx, gameID)
	if err != nil {
		return fmt.Errorf("failed to get game record: %w", err)
	}
	
	// Verify the game is active
	if status, ok := gameValue["status"].(string); ok && status != "active" {
		return fmt.Errorf("cannot resign from a game with status: %s", status)
	}
	
	// Determine who won based on who is resigning
	whiteDID, _ := gameValue["white"].(string)
	blackDID, _ := gameValue["black"].(string)
	
	var newStatus string
	if c.did == whiteDID {
		newStatus = "black_won"
	} else if c.did == blackDID {
		newStatus = "white_won"
	} else {
		return fmt.Errorf("player is not part of this game")
	}
	
	// Create resignation record
	resignationRecord := map[string]interface{}{
		"$type":           "app.atchess.resignation",
		"createdAt":       time.Now().Format(time.RFC3339),
		"game": map[string]interface{}{
			"uri": gameID,
			"cid": gameCID,
		},
		"resigningPlayer": c.did,
	}
	
	// Add optional reason
	if reason != "" {
		resignationRecord["reason"] = reason
	}
	
	// Create record in repository
	createReq := map[string]interface{}{
		"repo":       c.did,
		"collection": "app.atchess.resignation",
		"record":     resignationRecord,
	}
	
	reqBody, _ := json.Marshal(createReq)
	resp, err := c.makeRequest("POST", c.pdsURL+"/xrpc/com.atproto.repo.createRecord", reqBody)
	if err != nil {
		return fmt.Errorf("failed to create resignation record: %w", err)
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed to create resignation record: HTTP %d - %s", resp.StatusCode, string(body))
	}
	
	// Update the game status if we own the game record
	parts := strings.Split(gameID, "/")
	if len(parts) >= 5 && parts[2] == c.did {
		gameValue["status"] = newStatus
		gameValue["updatedAt"] = time.Now().Format(time.RFC3339)
		
		// Update the game record
		rkey := parts[4]
		updateReq := map[string]interface{}{
			"repo":       c.did,
			"collection": "app.atchess.game",
			"rkey":       rkey,
			"record":     gameValue,
			"swapCid":    gameCID,
		}
		
		updateReqBody, _ := json.Marshal(updateReq)
		updateResp, err := c.makeRequest("POST", c.pdsURL+"/xrpc/com.atproto.repo.putRecord", updateReqBody)
		if err != nil {
			return fmt.Errorf("failed to update game record: %w", err)
		}
		defer updateResp.Body.Close()
		
		if updateResp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(updateResp.Body)
			return fmt.Errorf("failed to update game record: HTTP %d - %s", updateResp.StatusCode, string(body))
		}
	}
	
	return nil
}

// GetDrawOffers retrieves pending draw offers for a game
func (c *Client) GetDrawOffers(ctx context.Context, gameID string) ([]*DrawOffer, error) {
	// List draw offer records
	url := fmt.Sprintf("%s/xrpc/com.atproto.repo.listRecords?repo=%s&collection=app.atchess.drawOffer&limit=100",
		c.pdsURL, c.did)
	resp, err := c.makeRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to list draw offers: %w", err)
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("failed to list draw offers: HTTP %d - %s", resp.StatusCode, string(body))
	}
	
	var listResp struct {
		Records []struct {
			URI   string `json:"uri"`
			CID   string `json:"cid"`
			Value struct {
				Type      string `json:"$type"`
				CreatedAt string `json:"createdAt"`
				Game struct {
					URI string `json:"uri"`
					CID string `json:"cid"`
				} `json:"game"`
				OfferedBy    string `json:"offeredBy"`
				MoveNumber   int    `json:"moveNumber"`
				Message      string `json:"message"`
				Status       string `json:"status"`
				RespondedAt  string `json:"respondedAt"`
				RespondedBy  string `json:"respondedBy"`
			} `json:"value"`
		} `json:"records"`
	}
	
	if err := json.NewDecoder(resp.Body).Decode(&listResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}
	
	// Filter for the specific game and pending status
	var offers []*DrawOffer
	for _, record := range listResp.Records {
		if record.Value.Game.URI == gameID && record.Value.Status == "pending" {
			offer := &DrawOffer{
				URI:         record.URI,
				CID:         record.CID,
				CreatedAt:   record.Value.CreatedAt,
				GameURI:     record.Value.Game.URI,
				GameCID:     record.Value.Game.CID,
				OfferedBy:   record.Value.OfferedBy,
				MoveNumber:  record.Value.MoveNumber,
				Message:     record.Value.Message,
				Status:      record.Value.Status,
				RespondedAt: record.Value.RespondedAt,
				RespondedBy: record.Value.RespondedBy,
			}
			offers = append(offers, offer)
		}
	}
	
	return offers, nil
}

// DrawOffer represents a draw offer record
type DrawOffer struct {
	URI         string
	CID         string
	CreatedAt   string
	GameURI     string
	GameCID     string
	OfferedBy   string
	MoveNumber  int
	Message     string
	Status      string
	RespondedAt string
	RespondedBy string
}

// TimeViolation represents a time violation claim record
type TimeViolation struct {
	URI               string
	CID               string
	CreatedAt         string
	GameURI           string
	GameCID           string
	ClaimingPlayer    string
	ViolatingPlayer   string
	LastMoveTimestamp string
	TimeControlType   string
	DaysPerMove       int
	TimeRemaining     int
}

// CheckTimeViolation checks if the current player has violated time control in a game
func (c *Client) CheckTimeViolation(ctx context.Context, gameID string) (bool, *TimeViolation, error) {
	// Get the game record to check status and players
	gameCID, gameValue, err := c.getGameRecord(ctx, gameID)
	if err != nil {
		return false, nil, fmt.Errorf("failed to get game record: %w", err)
	}
	
	// Check if game is still active
	if status, ok := gameValue["status"].(string); ok && status != "active" {
		return false, nil, nil // Game is not active, no time violation possible
	}
	
	// Get players
	whiteDID, _ := gameValue["white"].(string)
	blackDID, _ := gameValue["black"].(string)
	
	// Determine whose turn it is from FEN
	fen, _ := gameValue["fen"].(string)
	fenParts := strings.Split(fen, " ")
	if len(fenParts) < 2 {
		return false, nil, fmt.Errorf("invalid FEN format")
	}
	
	var currentPlayerDID string
	if fenParts[1] == "w" {
		currentPlayerDID = whiteDID
	} else {
		currentPlayerDID = blackDID
	}
	
	// Get the challenge reference to access time control settings
	var timeControlType string
	var daysPerMove int
	
	if challengeRef, ok := gameValue["challenge"].(map[string]interface{}); ok {
		challengeURI, _ := challengeRef["uri"].(string)
		if challengeURI != "" {
			// Get the challenge record to access time control
			challengeParts := strings.Split(challengeURI, "/")
			if len(challengeParts) >= 5 {
				challengeRepo := challengeParts[2]
				challengeRkey := challengeParts[4]
				
				url := fmt.Sprintf("%s/xrpc/com.atproto.repo.getRecord?repo=%s&collection=app.atchess.challenge&rkey=%s",
					c.pdsURL, challengeRepo, challengeRkey)
				resp, err := c.makeRequest("GET", url, nil)
				if err == nil && resp.StatusCode == http.StatusOK {
					defer resp.Body.Close()
					
					var challengeResp struct {
						Value struct {
							TimeControl map[string]interface{} `json:"timeControl"`
						} `json:"value"`
					}
					
					if err := json.NewDecoder(resp.Body).Decode(&challengeResp); err == nil {
						if tc := challengeResp.Value.TimeControl; tc != nil {
							if tcType, ok := tc["type"].(string); ok {
								timeControlType = tcType
							}
							if days, ok := tc["daysPerMove"].(float64); ok {
								daysPerMove = int(days)
							}
						}
					}
				}
			}
		}
	}
	
	// Default to correspondence with 3 days per move if not specified
	if timeControlType == "" {
		timeControlType = "correspondence"
		daysPerMove = 3
	}
	
	// For correspondence games, check the last move timestamp
	if timeControlType == "correspondence" {
		// Get the most recent move
		lastMove, err := c.getLastMove(ctx, gameID, currentPlayerDID)
		if err != nil {
			return false, nil, fmt.Errorf("failed to get last move: %w", err)
		}
		
		// If no moves yet, use game creation time
		var lastMoveTime time.Time
		if lastMove != nil {
			lastMoveTime, err = time.Parse(time.RFC3339, lastMove.CreatedAt)
			if err != nil {
				return false, nil, fmt.Errorf("failed to parse move timestamp: %w", err)
			}
		} else {
			// Use game creation time
			if createdAt, ok := gameValue["createdAt"].(string); ok {
				lastMoveTime, err = time.Parse(time.RFC3339, createdAt)
				if err != nil {
					return false, nil, fmt.Errorf("failed to parse game creation timestamp: %w", err)
				}
			} else {
				return false, nil, fmt.Errorf("game missing createdAt timestamp")
			}
		}
		
		// Check if time has expired
		timeLimit := time.Duration(daysPerMove) * 24 * time.Hour
		if time.Since(lastMoveTime) > timeLimit {
			// Time violation detected
			violation := &TimeViolation{
				GameURI:           gameID,
				GameCID:           gameCID,
				ClaimingPlayer:    c.did,
				ViolatingPlayer:   currentPlayerDID,
				LastMoveTimestamp: lastMoveTime.Format(time.RFC3339),
				TimeControlType:   timeControlType,
				DaysPerMove:       daysPerMove,
			}
			return true, violation, nil
		}
	}
	
	// TODO: Implement for other time control types (rapid, blitz, bullet)
	// These would require tracking time remaining per player
	
	return false, nil, nil
}

// getLastMove retrieves the most recent move in a game
func (c *Client) getLastMove(ctx context.Context, gameID string, excludePlayerDID string) (*struct {
	CreatedAt string
	Player    string
}, error) {
	// List moves for both players
	players := []string{}
	
	// Parse game URI to get players
	gameParts := strings.Split(gameID, "/")
	if len(gameParts) >= 5 {
		gameRepo := gameParts[2]
		players = append(players, gameRepo)
	}
	
	// Get game record to find the other player
	_, gameValue, err := c.getGameRecord(ctx, gameID)
	if err != nil {
		return nil, err
	}
	
	whiteDID, _ := gameValue["white"].(string)
	blackDID, _ := gameValue["black"].(string)
	
	// Add the other player if different from repo owner
	if whiteDID != players[0] {
		players = append(players, whiteDID)
	}
	if blackDID != players[0] && blackDID != whiteDID {
		players = append(players, blackDID)
	}
	
	var lastMove *struct {
		CreatedAt string
		Player    string
	}
	var lastMoveTime time.Time
	
	// Check moves from all players
	for _, playerDID := range players {
		url := fmt.Sprintf("%s/xrpc/com.atproto.repo.listRecords?repo=%s&collection=app.atchess.move&limit=100",
			c.pdsURL, playerDID)
		resp, err := c.makeRequest("GET", url, nil)
		if err != nil {
			continue // Skip if we can't access this player's moves
		}
		defer resp.Body.Close()
		
		if resp.StatusCode != http.StatusOK {
			continue
		}
		
		var listResp struct {
			Records []struct {
				Value struct {
					CreatedAt string `json:"createdAt"`
					Game      struct {
						URI string `json:"uri"`
					} `json:"game"`
					Player string `json:"player"`
				} `json:"value"`
			} `json:"records"`
		}
		
		if err := json.NewDecoder(resp.Body).Decode(&listResp); err != nil {
			continue
		}
		
		// Find the most recent move for this game
		for _, record := range listResp.Records {
			if record.Value.Game.URI == gameID && record.Value.Player != excludePlayerDID {
				moveTime, err := time.Parse(time.RFC3339, record.Value.CreatedAt)
				if err != nil {
					continue
				}
				
				if lastMove == nil || moveTime.After(lastMoveTime) {
					lastMoveTime = moveTime
					lastMove = &struct {
						CreatedAt string
						Player    string
					}{
						CreatedAt: record.Value.CreatedAt,
						Player:    record.Value.Player,
					}
				}
			}
		}
	}
	
	return lastMove, nil
}

// ClaimTimeVictory claims victory due to opponent's time violation
func (c *Client) ClaimTimeVictory(ctx context.Context, gameID string) error {
	// First check if there's actually a time violation
	hasViolation, violation, err := c.CheckTimeViolation(ctx, gameID)
	if err != nil {
		return fmt.Errorf("failed to check time violation: %w", err)
	}
	
	if !hasViolation {
		return fmt.Errorf("no time violation detected")
	}
	
	// Get the game record
	gameCID, gameValue, err := c.getGameRecord(ctx, gameID)
	if err != nil {
		return fmt.Errorf("failed to get game record: %w", err)
	}
	
	// Verify the claiming player is part of the game
	whiteDID, _ := gameValue["white"].(string)
	blackDID, _ := gameValue["black"].(string)
	
	if c.did != whiteDID && c.did != blackDID {
		return fmt.Errorf("you are not a player in this game")
	}
	
	// Create time violation record
	violationRecord := map[string]interface{}{
		"$type":           "app.atchess.timeViolation",
		"createdAt":       time.Now().Format(time.RFC3339),
		"game": map[string]interface{}{
			"uri": gameID,
			"cid": gameCID,
		},
		"claimingPlayer":    violation.ClaimingPlayer,
		"violatingPlayer":   violation.ViolatingPlayer,
		"lastMoveTimestamp": violation.LastMoveTimestamp,
		"timeControlType":   violation.TimeControlType,
	}
	
	if violation.DaysPerMove > 0 {
		violationRecord["daysPerMove"] = violation.DaysPerMove
	}
	if violation.TimeRemaining > 0 {
		violationRecord["timeRemaining"] = violation.TimeRemaining
	}
	
	// Create the violation record
	createReq := map[string]interface{}{
		"repo":       c.did,
		"collection": "app.atchess.timeViolation",
		"record":     violationRecord,
	}
	
	reqBody, _ := json.Marshal(createReq)
	resp, err := c.makeRequest("POST", c.pdsURL+"/xrpc/com.atproto.repo.createRecord", reqBody)
	if err != nil {
		return fmt.Errorf("failed to create time violation record: %w", err)
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed to create time violation record: HTTP %d - %s", resp.StatusCode, string(body))
	}
	
	// Update game status if we own the game record
	parts := strings.Split(gameID, "/")
	if len(parts) >= 5 && parts[2] == c.did {
		// Determine winner (the player who didn't violate time)
		var newStatus string
		if violation.ViolatingPlayer == whiteDID {
			newStatus = "black_won"
		} else {
			newStatus = "white_won"
		}
		
		gameValue["status"] = newStatus
		gameValue["updatedAt"] = time.Now().Format(time.RFC3339)
		
		// Update the game record
		rkey := parts[4]
		updateReq := map[string]interface{}{
			"repo":       c.did,
			"collection": "app.atchess.game",
			"rkey":       rkey,
			"record":     gameValue,
			"swapCid":    gameCID,
		}
		
		updateReqBody, _ := json.Marshal(updateReq)
		updateResp, err := c.makeRequest("POST", c.pdsURL+"/xrpc/com.atproto.repo.putRecord", updateReqBody)
		if err != nil {
			return fmt.Errorf("failed to update game record: %w", err)
		}
		defer updateResp.Body.Close()
		
		if updateResp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(updateResp.Body)
			return fmt.Errorf("failed to update game record: HTTP %d - %s", updateResp.StatusCode, string(body))
		}
	}
	
	return nil
}

// GetTimeRemaining calculates time remaining for the current player in a game
func (c *Client) GetTimeRemaining(ctx context.Context, gameID string) (time.Duration, error) {
	// Get the game record
	_, gameValue, err := c.getGameRecord(ctx, gameID)
	if err != nil {
		return 0, fmt.Errorf("failed to get game record: %w", err)
	}
	
	// Check if game is still active
	if status, ok := gameValue["status"].(string); ok && status != "active" {
		return 0, fmt.Errorf("game is not active")
	}
	
	// Get players
	whiteDID, _ := gameValue["white"].(string)
	blackDID, _ := gameValue["black"].(string)
	
	// Determine whose turn it is from FEN
	fen, _ := gameValue["fen"].(string)
	fenParts := strings.Split(fen, " ")
	if len(fenParts) < 2 {
		return 0, fmt.Errorf("invalid FEN format")
	}
	
	var currentPlayerDID string
	if fenParts[1] == "w" {
		currentPlayerDID = whiteDID
	} else {
		currentPlayerDID = blackDID
	}
	
	// Get time control settings from challenge
	var timeControlType string
	var daysPerMove int
	
	if challengeRef, ok := gameValue["challenge"].(map[string]interface{}); ok {
		challengeURI, _ := challengeRef["uri"].(string)
		if challengeURI != "" {
			challengeParts := strings.Split(challengeURI, "/")
			if len(challengeParts) >= 5 {
				challengeRepo := challengeParts[2]
				challengeRkey := challengeParts[4]
				
				url := fmt.Sprintf("%s/xrpc/com.atproto.repo.getRecord?repo=%s&collection=app.atchess.challenge&rkey=%s",
					c.pdsURL, challengeRepo, challengeRkey)
				resp, err := c.makeRequest("GET", url, nil)
				if err == nil && resp.StatusCode == http.StatusOK {
					defer resp.Body.Close()
					
					var challengeResp struct {
						Value struct {
							TimeControl map[string]interface{} `json:"timeControl"`
						} `json:"value"`
					}
					
					if err := json.NewDecoder(resp.Body).Decode(&challengeResp); err == nil {
						if tc := challengeResp.Value.TimeControl; tc != nil {
							if tcType, ok := tc["type"].(string); ok {
								timeControlType = tcType
							}
							if days, ok := tc["daysPerMove"].(float64); ok {
								daysPerMove = int(days)
							}
						}
					}
				}
			}
		}
	}
	
	// Default to correspondence with 3 days per move
	if timeControlType == "" {
		timeControlType = "correspondence"
		daysPerMove = 3
	}
	
	// For correspondence games, calculate time remaining
	if timeControlType == "correspondence" {
		// Get the most recent move
		lastMove, err := c.getLastMove(ctx, gameID, currentPlayerDID)
		if err != nil {
			return 0, fmt.Errorf("failed to get last move: %w", err)
		}
		
		var lastMoveTime time.Time
		if lastMove != nil {
			lastMoveTime, err = time.Parse(time.RFC3339, lastMove.CreatedAt)
			if err != nil {
				return 0, fmt.Errorf("failed to parse move timestamp: %w", err)
			}
		} else {
			// Use game creation time
			if createdAt, ok := gameValue["createdAt"].(string); ok {
				lastMoveTime, err = time.Parse(time.RFC3339, createdAt)
				if err != nil {
					return 0, fmt.Errorf("failed to parse game creation timestamp: %w", err)
				}
			} else {
				return 0, fmt.Errorf("game missing createdAt timestamp")
			}
		}
		
		// Calculate time remaining
		timeLimit := time.Duration(daysPerMove) * 24 * time.Hour
		elapsed := time.Since(lastMoveTime)
		remaining := timeLimit - elapsed
		
		if remaining < 0 {
			return 0, nil // Time has expired
		}
		
		return remaining, nil
	}
	
	// TODO: Implement for other time control types
	return 0, fmt.Errorf("time control type %s not yet implemented", timeControlType)
}