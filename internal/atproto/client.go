package atproto

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/justinabrahms/atchess/internal/auth"
	"github.com/justinabrahms/atchess/internal/chess"
)

// ErrIncompleteDerivation indicates that a game's derived status (see
// GetGame's doc comment) could not be fully verified because at least one
// player's repo could not be read during the terminal-event scan. This is
// deliberately treated as a hard error rather than folded silently into
// "no terminal event found" (which would derive as active): a truncated
// scan has only proven it did not FIND a terminal event, which is a
// different claim from proving one does not exist -- it cannot distinguish
// "no resignation exists" from "could not look". Since the derived status
// is a safety gate (MakeMoveHandler and friends use it to decide whether a
// write is even allowed, not just to render a label), every caller must
// treat this error as "unproven, do not authorize" and fail closed. A
// caller that only needs a best-effort display value may still inspect the
// partial *chess.Game returned alongside this error (see
// Game.DerivationIncomplete), but no such caller exists yet -- see
// atchess-1c9.51.
var ErrIncompleteDerivation = errors.New("game status derivation incomplete: one or more repos could not be read")

type Client struct {
	pdsURL      string
	accessJWT   string
	refreshJWT  string
	did         string
	handle      string
	httpClient  *http.Client
	dpopManager *auth.DPoPManager
	useDPoP     bool

	// plcDirectoryURL, when non-empty, overrides DefaultPLCDirectoryURL for
	// this client's DID/handle resolution (identity.go) -- see
	// SetPLCDirectoryURL. Empty means "use the default", so the zero value
	// *Client (as produced by any existing constructor) keeps working
	// unchanged.
	plcDirectoryURL string

	// auth, if set, supplies (and refreshes) the access token for this
	// client's requests -- see NewClientFromSession. When nil, the client
	// uses the static accessJWT captured at login (NewClient/
	// NewClientWithDPoP), matching this package's original behaviour.
	auth Authenticator
}

// Authenticator supplies the bearer token used to authenticate a Client's
// requests when it was built via NewClientFromSession (as opposed to a
// direct handle+password login), refreshing that token when necessary.
// Implementations must be safe for concurrent use -- see
// oauth.Session.EnsureFresh/ForceRefresh in internal/oauth, which
// internal/web's sessionAuthenticator is built on.
type Authenticator interface {
	// Token returns a currently-valid access token, refreshing it first if
	// it is at or near expiry.
	Token() (string, error)
	// ForceRefresh discards any local freshness assumption and refreshes
	// immediately. Called after the PDS itself rejects a request with 401,
	// in case the server invalidated the token for a reason this client
	// could not have predicted locally (e.g. revocation).
	ForceRefresh() (string, error)
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
	req, err := http.NewRequest("POST", xrpcURL(pdsURL, "com.atproto.server.createSession", nil), bytes.NewBuffer(reqBody))
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
		AccessJwt  string `json:"accessJwt"`
		RefreshJwt string `json:"refreshJwt"`
		Did        string `json:"did"`
		Handle     string `json:"handle"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&session); err != nil {
		return nil, fmt.Errorf("failed to decode session response: %w", err)
	}

	client := &Client{
		pdsURL:      pdsURL,
		accessJWT:   session.AccessJwt,
		refreshJWT:  session.RefreshJwt,
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

// GetAccessJWT returns the current AT Protocol access token (accessJwt).
// Exposed so callers (e.g. internal/web's LoginHandler) can persist it in a
// session for later use by NewClientFromSession.
func (c *Client) GetAccessJWT() string {
	return c.accessJWT
}

// GetRefreshJWT returns the AT Protocol refresh token (refreshJwt) obtained
// at login, if any.
func (c *Client) GetRefreshJWT() string {
	return c.refreshJWT
}

// NewClientFromSession builds a Client that authenticates as the identity
// described by did/handle via auth, rather than performing a fresh
// handle+password login. This is how internal/web's authenticated handlers
// act as the caller (AuthenticatedDID) instead of the protocol-service
// instance's own static configured identity (atchess-1c9.9). useDPoP
// controls whether the Authorization header is "DPoP <token>" or
// "Bearer <token>"; today only Bearer (app-password) sessions are wired
// end-to-end -- DPoP-bound OAuth sessions are atchess-1c9.12's job.
func NewClientFromSession(pdsURL, did, handle string, useDPoP bool, auth Authenticator) (*Client, error) {
	if auth == nil {
		return nil, fmt.Errorf("NewClientFromSession: an Authenticator is required")
	}
	accessJWT, err := auth.Token()
	if err != nil {
		return nil, fmt.Errorf("failed to obtain access token for session: %w", err)
	}
	return &Client{
		pdsURL:     pdsURL,
		accessJWT:  accessJWT,
		did:        did,
		handle:     handle,
		httpClient: &http.Client{Timeout: 30 * time.Second},
		useDPoP:    useDPoP,
		auth:       auth,
	}, nil
}

// RefreshSession exchanges a refresh token for a new access/refresh token
// pair via com.atproto.server.refreshSession. Used by internal/web to renew
// an app-password session's access token without requiring the user to
// re-enter their password.
func RefreshSession(pdsURL, refreshJWT string) (accessJWT, newRefreshJWT string, err error) {
	req, err := http.NewRequest("POST", xrpcURL(pdsURL, "com.atproto.server.refreshSession", nil), nil)
	if err != nil {
		return "", "", fmt.Errorf("failed to create refresh request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+refreshJWT)

	httpClient := &http.Client{Timeout: 30 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("refreshSession request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", "", fmt.Errorf("refreshSession returned HTTP %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		AccessJwt  string `json:"accessJwt"`
		RefreshJwt string `json:"refreshJwt"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", "", fmt.Errorf("failed to decode refreshSession response: %w", err)
	}

	return result.AccessJwt, result.RefreshJwt, nil
}

// ParseJWTExpiry extracts the "exp" claim (Unix seconds) from a JWT's
// payload without verifying its signature -- used only to decide when a
// locally-held access token is due for a proactive refresh; the PDS remains
// the sole authority on whether the token is actually still valid.
func ParseJWTExpiry(token string) (time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Exp == 0 {
		return time.Time{}, false
	}
	return time.Unix(claims.Exp, 0), true
}

// makeRequest creates and executes an HTTP request with proper
// authentication, transparently refreshing and retrying once on a 401 when
// the client was built via NewClientFromSession (c.auth != nil).
func (c *Client) makeRequest(method, url string, body []byte) (*http.Response, error) {
	resp, err := c.doRequest(method, url, body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode == http.StatusUnauthorized && c.auth != nil {
		resp.Body.Close()
		if _, rerr := c.auth.ForceRefresh(); rerr != nil {
			return nil, fmt.Errorf("request unauthorized (401) and token refresh failed: %w", rerr)
		}
		resp, err = c.doRequest(method, url, body)
		if err != nil {
			return nil, err
		}
	}

	return resp, nil
}

// doRequest performs a single attempt of an authenticated HTTP request.
func (c *Client) doRequest(method, url string, body []byte) (*http.Response, error) {
	req, err := http.NewRequest(method, url, bytes.NewBuffer(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	token := c.accessJWT
	if c.auth != nil {
		t, terr := c.auth.Token()
		if terr != nil {
			return nil, fmt.Errorf("failed to obtain access token: %w", terr)
		}
		token = t
	}

	// Set authorization header based on whether DPoP is enabled
	if c.useDPoP {
		req.Header.Set("Authorization", "DPoP "+token)
	} else {
		req.Header.Set("Authorization", "Bearer "+token)
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
	resp, err := c.makeRequest("POST", xrpcURL(c.pdsURL, "com.atproto.repo.createRecord", nil), reqBody)
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
	// Fetch the game record to get its CID and current value
	gameCID, gameValue, err := c.getGameRecord(ctx, gameURI)
	if err != nil {
		return fmt.Errorf("failed to get game record: %w", err)
	}

	// Parse the game URI to get repo and rkey
	parts := strings.Split(gameURI, "/")
	if len(parts) < 5 || !strings.HasPrefix(gameURI, "at://") {
		return fmt.Errorf("invalid game URI format: %s", gameURI)
	}

	repo := parts[2] // The DID
	rkey := parts[4] // The record key

	// Update game record FIRST (CAS-protected) to prevent race conditions.
	// Only the game owner can update the game record; if it belongs to the
	// opponent we still create the move record (the game record is a
	// denormalized cache that gets reconstructed from move records).
	if repo == c.did {
		gameValue["fen"] = move.FEN
		if move.Checkmate || move.Draw {
			if move.Checkmate {
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

		putReq := map[string]interface{}{
			"repo":       repo,
			"collection": "app.atchess.game",
			"rkey":       rkey,
			"record":     gameValue,
			"swapCid":    gameCID, // Optimistic concurrency control
		}

		putReqBody, _ := json.Marshal(putReq)
		putResp, err := c.makeRequest("POST", xrpcURL(c.pdsURL, "com.atproto.repo.putRecord", nil), putReqBody)
		if err != nil {
			return fmt.Errorf("failed to update game record: %w", err)
		}
		defer putResp.Body.Close()

		if putResp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(putResp.Body)
			return fmt.Errorf("failed to update game record (conflict — another move may have been played): HTTP %d, body: %s", putResp.StatusCode, string(body))
		}
	}

	// CAS succeeded (or game is in opponent's repo) — now create the move record.
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
	if move.Draw {
		moveRecord["draw"] = true
	}

	createReq := map[string]interface{}{
		"repo":       c.did,
		"collection": "app.atchess.move",
		"record":     moveRecord,
	}

	reqBody, _ := json.Marshal(createReq)
	resp, err := c.makeRequest("POST", xrpcURL(c.pdsURL, "com.atproto.repo.createRecord", nil), reqBody)
	if err != nil {
		return fmt.Errorf("failed to create move record: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to create move record: HTTP %d", resp.StatusCode)
	}

	return nil
}

// CreateChallenge writes an app.atchess.challenge record into the CALLER's
// OWN repository (c.did) naming opponentDID as the challenged party. It
// deliberately does NOT attempt to write anything into the challenged
// player's repository -- AT Protocol never permits writing into a repo that
// isn't your own with your own session credentials, so an attempt to do that
// (the retired CreateChallengeNotification, see atchess-1c9.11) always
// failed in federation and has been removed entirely rather than retried.
//
// Delivery to the challenged player is instead the responsibility of the
// discovery mechanism described in internal/challenge, internal/firehose,
// and internal/backfill (see docs/firehose-and-backfill.md for the full
// picture): the challenged player's own protocol-service instance
// discovers this record two ways -- (1) subscribing to the firehose of any
// PDS that might host a challenger (see cmd/protocol/main.go) and indexing
// app.atchess.challenge commits whose "challenged" field matches its own
// authenticated users, with cursor persistence across restarts
// (atchess-1c9.46) rather than a full-log replay on every boot; and (2) a
// login-time repo-read backfill (internal/backfill), run synchronously on
// every login, that queries known PDSes' repos directly for challenges
// issued while that player's session wasn't around -- see that package's
// doc comment for exactly what it can and cannot find. challengerHandle is
// embedded directly in the record so a subscriber can display it without a
// second DID resolution round trip.
func (c *Client) CreateChallenge(ctx context.Context, opponentDID, color, message string) (*chess.Challenge, error) {
	createdAt := time.Now()
	proposedGameID := generateGameID(c.did, opponentDID, createdAt)

	challengeRecord := map[string]interface{}{
		"$type":            "app.atchess.challenge",
		"createdAt":        createdAt.Format(time.RFC3339),
		"challenger":       c.did,
		"challengerHandle": c.handle,
		"challenged":       opponentDID,
		"status":           "pending",
		"color":            color,
		"proposedGameId":   proposedGameID,
		"message":          message,
		"expiresAt":        createdAt.Add(24 * time.Hour).Format(time.RFC3339),
	}

	createReq := map[string]interface{}{
		"repo":       c.did,
		"collection": "app.atchess.challenge",
		"record":     challengeRecord,
	}

	reqBody, _ := json.Marshal(createReq)
	resp, err := c.makeRequest("POST", xrpcURL(c.pdsURL, "com.atproto.repo.createRecord", nil), reqBody)
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

// RespondToChallenge records a decline of a pending challenge by writing an
// app.atchess.challengeResponse record into the CALLER's (the responding
// player's) OWN repository -- never into the challenger's, which AT
// Protocol does not permit. It references the original challenge by
// strongRef (challengeURI + challengeCID) rather than being co-located with
// it. Acceptance is not expressed through this method: an accepted
// challenge is instead represented by the app.atchess.game record the
// accepting player creates (see CreateGameFromChallenge), which already
// carries the same challenge strongRef.
//
// response is currently always "declined" -- the parameter exists (rather
// than a bool) so the lexicon's enum can grow without an API change.
func (c *Client) RespondToChallenge(ctx context.Context, challengeURI, challengeCID, response string) error {
	if response != "declined" {
		return fmt.Errorf("unsupported challenge response %q (only \"declined\" is currently supported)", response)
	}

	record := map[string]interface{}{
		"$type":     "app.atchess.challengeResponse",
		"createdAt": time.Now().Format(time.RFC3339),
		"challenge": map[string]interface{}{
			"uri": challengeURI,
			"cid": challengeCID,
		},
		"response": response,
	}

	createReq := map[string]interface{}{
		"repo":       c.did,
		"collection": "app.atchess.challengeResponse",
		"record":     record,
	}

	reqBody, _ := json.Marshal(createReq)
	resp, err := c.makeRequest("POST", xrpcURL(c.pdsURL, "com.atproto.repo.createRecord", nil), reqBody)
	if err != nil {
		return fmt.Errorf("failed to create challenge response record: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed to create challenge response record: HTTP %d - %s", resp.StatusCode, string(body))
	}

	return nil
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

	base, ownRepo, err := c.resolveReadEndpoint(ctx, repo)
	if err != nil {
		return "", nil, fmt.Errorf("failed to get game record: %w", err)
	}
	params := url.Values{"repo": {repo}, "collection": {"app.atchess.game"}, "rkey": {rkey}}
	resp, err := c.getXRPC(ctx, base, ownRepo, "com.atproto.repo.getRecord", params)
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

// getRecordByURI fetches an arbitrary record given its full at:// URI
// (parsing repo/collection/rkey out of it) and returns its CID and value.
// Unlike getGameRecord it is not specific to app.atchess.game -- it exists
// so a strongRef (uri+cid) embedded in one record can be verified to
// actually point at a real record before being trusted, rather than taking
// the embedding record's word for it. See getDrawAcceptOutcome, which uses
// this to confirm a "drawResponse: accepted" record references a
// drawOffer that genuinely exists in the other player's repo -- without
// this, any player could fabricate an acceptance out of thin air.
func (c *Client) getRecordByURI(ctx context.Context, atURI string) (string, map[string]interface{}, error) {
	parts := strings.Split(atURI, "/")
	if len(parts) < 5 || !strings.HasPrefix(atURI, "at://") {
		return "", nil, fmt.Errorf("invalid AT Protocol URI format: %s", atURI)
	}

	repo := parts[2]
	collection := parts[3]
	rkey := parts[4]

	base, ownRepo, err := c.resolveReadEndpoint(ctx, repo)
	if err != nil {
		return "", nil, fmt.Errorf("failed to resolve repo for %s: %w", atURI, err)
	}
	params := url.Values{"repo": {repo}, "collection": {collection}, "rkey": {rkey}}
	resp, err := c.getXRPC(ctx, base, ownRepo, "com.atproto.repo.getRecord", params)
	if err != nil {
		return "", nil, fmt.Errorf("failed to get record %s: %w", atURI, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", nil, fmt.Errorf("failed to get record %s: HTTP %d - %s", atURI, resp.StatusCode, string(body))
	}

	var getResp struct {
		CID   string                 `json:"cid"`
		Value map[string]interface{} `json:"value"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&getResp); err != nil {
		return "", nil, fmt.Errorf("failed to decode record %s: %w", atURI, err)
	}

	return getResp.CID, getResp.Value, nil
}

type moveRecord struct {
	FEN       string
	Checkmate bool
	Draw      bool
	CreatedAt time.Time
	// rkey is the move record's own AT-URI record key (a TID -- see
	// recordKey/moveIsAfter). CreatedAt (an RFC3339 timestamp, second
	// resolution) is not fine-grained enough to order two moves recorded
	// within the same second -- which cross-repo moves routinely are, once
	// atchess-1c9.10 makes it possible to read them at all -- so rkey
	// breaks ties.
	rkey string
}

// recordKey extracts the trailing record key (TID) from an at:// record
// URI, e.g. "at://did:plc:x/app.atchess.move/3mthkghep7k2k" -> "3mthkghep7k2k".
func recordKey(atURI string) string {
	idx := strings.LastIndex(atURI, "/")
	if idx < 0 {
		return atURI
	}
	return atURI[idx+1:]
}

// recordRepo extracts the repo DID from an at:// record URI, e.g.
// "at://did:plc:x/app.atchess.move/3mthkghep7k2k" -> "did:plc:x". Returns
// "" if atURI is not a well-formed at:// record URI.
func recordRepo(atURI string) string {
	parts := strings.Split(atURI, "/")
	if len(parts) < 5 || !strings.HasPrefix(atURI, "at://") {
		return ""
	}
	return parts[2]
}

// moveIsAfter reports whether the move record identified by (t, rkey)
// should be considered strictly more recent than (otherT, otherRkey). AT
// Protocol TIDs are, by design, lexicographically sortable by creation time
// (base32-sortable, monotonic per repo) -- see
// https://atproto.com/specs/tid -- so comparing rkey strings resolves a
// CreatedAt tie (same second) far more precisely than CreatedAt alone.
func moveIsAfter(t time.Time, rkey string, otherT time.Time, otherRkey string) bool {
	if t.After(otherT) {
		return true
	}
	if t.Before(otherT) {
		return false
	}
	return rkey > otherRkey
}

// StoredMove represents a move record retrieved from a player's PDS repository.
type StoredMove struct {
	From      string    `json:"from"`
	To        string    `json:"to"`
	SAN       string    `json:"san"`
	FEN       string    `json:"fen"`
	Player    string    `json:"player"`
	Check     bool      `json:"check"`
	Checkmate bool      `json:"checkmate"`
	Draw      bool      `json:"draw"`
	CreatedAt time.Time `json:"createdAt"`
}

// ListMovesForGame fetches all app.atchess.move records from this client's
// repository that belong to the given game URI.
func (c *Client) ListMovesForGame(ctx context.Context, gameURI string) ([]StoredMove, error) {
	params := url.Values{"repo": {c.did}, "collection": {"app.atchess.move"}, "limit": {"100"}}
	resp, err := c.getXRPC(ctx, c.pdsURL, true, "com.atproto.repo.listRecords", params)
	if err != nil {
		return nil, fmt.Errorf("failed to list move records: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to list move records: HTTP %d", resp.StatusCode)
	}

	var listResp struct {
		Records []struct {
			Value struct {
				Game struct {
					URI string `json:"uri"`
				} `json:"game"`
				From      string `json:"from"`
				To        string `json:"to"`
				SAN       string `json:"san"`
				FEN       string `json:"fen"`
				Player    string `json:"player"`
				Check     bool   `json:"check"`
				Checkmate bool   `json:"checkmate"`
				Draw      bool   `json:"draw"`
				CreatedAt string `json:"createdAt"`
			} `json:"value"`
		} `json:"records"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&listResp); err != nil {
		return nil, fmt.Errorf("failed to decode move records: %w", err)
	}

	var moves []StoredMove
	for _, record := range listResp.Records {
		if record.Value.Game.URI != gameURI {
			continue
		}
		t, err := time.Parse(time.RFC3339, record.Value.CreatedAt)
		if err != nil {
			continue
		}
		moves = append(moves, StoredMove{
			From:      record.Value.From,
			To:        record.Value.To,
			SAN:       record.Value.SAN,
			FEN:       record.Value.FEN,
			Player:    record.Value.Player,
			Check:     record.Value.Check,
			Checkmate: record.Value.Checkmate,
			Draw:      record.Value.Draw,
			CreatedAt: t,
		})
	}

	return moves, nil
}

// getLatestMoveForGame fetches moves from both players' repos and returns
// the latest move for the given game. This is the source of truth for game state.
func (c *Client) getLatestMoveForGame(ctx context.Context, gameURI string, whiteDID, blackDID string) (*moveRecord, error) {
	var latest *moveRecord
	var errs []error

	for _, playerDID := range []string{whiteDID, blackDID} {
		if playerDID == "" {
			continue
		}
		base, ownRepo, err := c.resolveReadEndpoint(ctx, playerDID)
		if err != nil {
			errs = append(errs, fmt.Errorf("resolve read endpoint for %s: %w: %v", playerDID, ErrIncompleteDerivation, err))
			continue
		}
		params := url.Values{"repo": {playerDID}, "collection": {"app.atchess.move"}, "limit": {"100"}}
		resp, err := c.getXRPC(ctx, base, ownRepo, "com.atproto.repo.listRecords", params)
		if err != nil {
			errs = append(errs, fmt.Errorf("list moves for %s: %w: %v", playerDID, ErrIncompleteDerivation, err))
			continue
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			errs = append(errs, fmt.Errorf("list moves for %s: HTTP %d: %w", playerDID, resp.StatusCode, ErrIncompleteDerivation))
			continue
		}

		var listResp struct {
			Records []struct {
				URI   string `json:"uri"`
				Value struct {
					Game struct {
						URI string `json:"uri"`
					} `json:"game"`
					FEN       string `json:"fen"`
					Checkmate bool   `json:"checkmate"`
					Draw      bool   `json:"draw"`
					CreatedAt string `json:"createdAt"`
				} `json:"value"`
			} `json:"records"`
		}

		if err := json.NewDecoder(resp.Body).Decode(&listResp); err != nil {
			errs = append(errs, fmt.Errorf("decode moves list for %s: %w: %v", playerDID, ErrIncompleteDerivation, err))
			continue
		}

		for _, record := range listResp.Records {
			if record.Value.Game.URI != gameURI {
				continue
			}
			t, err := time.Parse(time.RFC3339, record.Value.CreatedAt)
			if err != nil {
				continue
			}
			rkey := recordKey(record.URI)
			if latest == nil || moveIsAfter(t, rkey, latest.CreatedAt, latest.rkey) {
				latest = &moveRecord{
					FEN:       record.Value.FEN,
					Checkmate: record.Value.Checkmate,
					rkey:      rkey,
					Draw:      record.Value.Draw,
					CreatedAt: t,
				}
			}
		}
	}

	return latest, errors.Join(errs...)
}

// terminalEvent is one candidate final outcome for a game, sourced from a
// move (checkmate/draw), a resignation, a time-violation claim, or an
// accepted draw-offer response. GetGame merges candidates from every
// source -- each itself read across BOTH players' repos, since a terminal
// event is always written into the triggering player's own repo, which may
// not be the repo that owns the shared (and otherwise possibly stale)
// app.atchess.game record -- and applies whichever is most recent. at/rkey
// use the same (createdAt, TID) ordering as moveIsAfter for the same
// reason moveRecord does: createdAt alone is only second-resolution.
type terminalEvent struct {
	status chess.GameStatus
	at     time.Time
	rkey   string
}

// latestTerminalEvent returns whichever of the given candidate terminal
// events (any of which may be nil, meaning "that source found nothing") is
// most recent, or nil if none were found. In a well-behaved client at most
// one of these should ever be non-nil for a given game, but ties are
// broken deterministically rather than left to map/slice iteration order.
func latestTerminalEvent(events ...*terminalEvent) *terminalEvent {
	var latest *terminalEvent
	for _, e := range events {
		if e == nil {
			continue
		}
		if latest == nil || moveIsAfter(e.at, e.rkey, latest.at, latest.rkey) {
			latest = e
		}
	}
	return latest
}

// getResignationOutcome scans app.atchess.resignation records in BOTH
// players' repos for gameURI and returns the most recent one as a
// terminalEvent (the resigning player's opponent wins), or nil if none
// exists. ResignGame (this file) always writes the resignation record into
// the RESIGNING player's own repo, which is not necessarily the repo that
// owns the shared app.atchess.game record, so it must be discoverable
// regardless of who created that record -- the same reason
// getLatestMoveForGame reads both repos for moves.
//
// Authorship is checked against the repo the record was actually read
// from: a record found in playerDID's own repo may only assert that
// playerDID resigned. Without this, black could write a resignation
// record into black's OWN repo naming white as the resigningPlayer and
// unilaterally declare a black_won outcome -- nobody's repo-write
// permissions stop them writing arbitrary field values into their own
// records, so the repo boundary is the only thing that can be trusted,
// never a same-record field (atchess-1c9.48 review).
func (c *Client) getResignationOutcome(ctx context.Context, gameURI, whiteDID, blackDID string) (*terminalEvent, error) {
	var latest *terminalEvent
	var errs []error

	for _, playerDID := range []string{whiteDID, blackDID} {
		if playerDID == "" {
			continue
		}
		base, ownRepo, err := c.resolveReadEndpoint(ctx, playerDID)
		if err != nil {
			errs = append(errs, fmt.Errorf("resolve read endpoint for %s: %w: %v", playerDID, ErrIncompleteDerivation, err))
			continue
		}
		params := url.Values{"repo": {playerDID}, "collection": {"app.atchess.resignation"}, "limit": {"100"}}
		resp, err := c.getXRPC(ctx, base, ownRepo, "com.atproto.repo.listRecords", params)
		if err != nil {
			errs = append(errs, fmt.Errorf("list resignations for %s: %w: %v", playerDID, ErrIncompleteDerivation, err))
			continue
		}

		func() {
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				errs = append(errs, fmt.Errorf("list resignations for %s: HTTP %d: %w", playerDID, resp.StatusCode, ErrIncompleteDerivation))
				return
			}

			var listResp struct {
				Records []struct {
					URI   string `json:"uri"`
					Value struct {
						Game struct {
							URI string `json:"uri"`
						} `json:"game"`
						ResigningPlayer string `json:"resigningPlayer"`
						CreatedAt       string `json:"createdAt"`
					} `json:"value"`
				} `json:"records"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&listResp); err != nil {
				errs = append(errs, fmt.Errorf("decode resignations list for %s: %w: %v", playerDID, ErrIncompleteDerivation, err))
				return
			}

			for _, record := range listResp.Records {
				if record.Value.Game.URI != gameURI {
					continue
				}
				if record.Value.ResigningPlayer != playerDID {
					log.Warn().Str("gameURI", gameURI).Str("repo", playerDID).
						Str("claimedResigningPlayer", record.Value.ResigningPlayer).
						Str("recordURI", record.URI).
						Msg("ignoring forged resignation record: repo owner is not the player it names as resigning")
					continue
				}
				t, err := time.Parse(time.RFC3339, record.Value.CreatedAt)
				if err != nil {
					continue
				}
				status := chess.StatusWhiteWon
				if record.Value.ResigningPlayer == whiteDID {
					status = chess.StatusBlackWon
				}
				candidate := &terminalEvent{status: status, at: t, rkey: recordKey(record.URI)}
				if latest == nil || moveIsAfter(candidate.at, candidate.rkey, latest.at, latest.rkey) {
					latest = candidate
				}
			}
		}()
	}

	return latest, errors.Join(errs...)
}

// getTimeViolationOutcome scans app.atchess.timeViolation records in BOTH
// players' repos for gameURI and returns the most recent one as a
// terminalEvent (the violating player's opponent wins), or nil if none
// exists. See getResignationOutcome's doc comment for why both repos must
// be read.
//
// A timeViolation record is a one-sided claim -- "my opponent didn't move
// in time" -- and nothing stops the claiming player from writing one the
// instant after their own move, or with a fabricated violatingPlayer. So,
// unlike a resignation (self-report, trivially checked against the repo it
// came from) a timeViolation claim is only trusted here if it can be
// checked against something the claimer does NOT control:
//   - authorship: found in repo X, the record's claimingPlayer must be X
//     (this mirrors ClaimTimeVictory, which always writes into its own
//     caller's repo) and its violatingPlayer must be the OTHER player, not
//     X itself;
//   - timing: lastActivityAt (the real timestamp of the game's last move,
//     or its creation if there is none yet -- supplied by the caller,
//     which already has this from the move-record scan every GetGame call
//     does anyway, so this costs no extra round trip) plus the game's
//     actual daysPerMove time control must already have elapsed as of the
//     claim's own createdAt. A claim made before its own deadline had
//     passed is rejected outright.
//
// For any time control type other than "correspondence" this package has
// no sound way to verify elapsed time server-side (see CheckTimeViolation's
// own TODO -- rapid/blitz/bullet per-player clocks are not tracked at
// all), so such a claim can never be verified here; rather than silently
// trusting it, it is treated as advisory only and excluded from the
// authoritative merge (logged, not applied).
func (c *Client) getTimeViolationOutcome(ctx context.Context, gameURI, whiteDID, blackDID string, timeControlType string, daysPerMove int, lastActivityAt time.Time, lastActivityKnown bool) (*terminalEvent, error) {
	var latest *terminalEvent
	var errs []error

	for _, playerDID := range []string{whiteDID, blackDID} {
		if playerDID == "" {
			continue
		}
		base, ownRepo, err := c.resolveReadEndpoint(ctx, playerDID)
		if err != nil {
			errs = append(errs, fmt.Errorf("resolve read endpoint for %s: %w: %v", playerDID, ErrIncompleteDerivation, err))
			continue
		}
		params := url.Values{"repo": {playerDID}, "collection": {"app.atchess.timeViolation"}, "limit": {"100"}}
		resp, err := c.getXRPC(ctx, base, ownRepo, "com.atproto.repo.listRecords", params)
		if err != nil {
			errs = append(errs, fmt.Errorf("list timeViolations for %s: %w: %v", playerDID, ErrIncompleteDerivation, err))
			continue
		}

		func() {
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				errs = append(errs, fmt.Errorf("list timeViolations for %s: HTTP %d: %w", playerDID, resp.StatusCode, ErrIncompleteDerivation))
				return
			}

			var listResp struct {
				Records []struct {
					URI   string `json:"uri"`
					Value struct {
						Game struct {
							URI string `json:"uri"`
						} `json:"game"`
						ClaimingPlayer  string `json:"claimingPlayer"`
						ViolatingPlayer string `json:"violatingPlayer"`
						CreatedAt       string `json:"createdAt"`
					} `json:"value"`
				} `json:"records"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&listResp); err != nil {
				errs = append(errs, fmt.Errorf("decode timeViolations list for %s: %w: %v", playerDID, ErrIncompleteDerivation, err))
				return
			}

			for _, record := range listResp.Records {
				if record.Value.Game.URI != gameURI {
					continue
				}

				if record.Value.ClaimingPlayer != playerDID {
					log.Warn().Str("gameURI", gameURI).Str("repo", playerDID).
						Str("claimedClaimingPlayer", record.Value.ClaimingPlayer).
						Str("recordURI", record.URI).
						Msg("ignoring forged timeViolation record: repo owner is not the player it names as claiming")
					continue
				}
				if record.Value.ViolatingPlayer != whiteDID && record.Value.ViolatingPlayer != blackDID {
					continue
				}
				if record.Value.ViolatingPlayer == playerDID {
					log.Warn().Str("gameURI", gameURI).Str("repo", playerDID).Str("recordURI", record.URI).
						Msg("ignoring nonsensical timeViolation record: claimant named themselves as the violator")
					continue
				}

				t, err := time.Parse(time.RFC3339, record.Value.CreatedAt)
				if err != nil {
					continue
				}

				if timeControlType != "correspondence" {
					// Cannot be soundly verified -- advisory only, never
					// authoritative. See doc comment above.
					log.Warn().Str("gameURI", gameURI).Str("recordURI", record.URI).Str("timeControlType", timeControlType).
						Msg("timeViolation record for a non-correspondence time control cannot be verified server-side; treating as advisory and excluding it from derived game status")
					continue
				}
				if !lastActivityKnown {
					log.Warn().Str("gameURI", gameURI).Str("recordURI", record.URI).
						Msg("timeViolation record cannot be verified without a known last-activity timestamp; treating as advisory and excluding it from derived game status")
					continue
				}
				timeLimit := time.Duration(daysPerMove) * 24 * time.Hour
				if daysPerMove <= 0 || t.Sub(lastActivityAt) < timeLimit {
					log.Warn().Str("gameURI", gameURI).Str("recordURI", record.URI).
						Time("lastActivityAt", lastActivityAt).Time("claimedAt", t).Int("daysPerMove", daysPerMove).
						Msg("ignoring premature/forged timeViolation record: claimed before its own deadline had actually elapsed")
					continue
				}

				// Mirrors ClaimTimeVictory's own winner determination:
				// the violating player loses.
				status := chess.StatusWhiteWon
				if record.Value.ViolatingPlayer == whiteDID {
					status = chess.StatusBlackWon
				}
				candidate := &terminalEvent{status: status, at: t, rkey: recordKey(record.URI)}
				if latest == nil || moveIsAfter(candidate.at, candidate.rkey, latest.at, latest.rkey) {
					latest = candidate
				}
			}
		}()
	}

	return latest, errors.Join(errs...)
}

// getDrawAcceptOutcome scans app.atchess.drawResponse records in BOTH
// players' repos for gameURI and returns the most recent "accepted" one as
// a terminalEvent (status=draw), or nil if none exists. RespondToDrawOffer
// always writes the response into the RESPONDING player's own repo (never
// the offering player's -- AT Protocol forbids that cross-repo write), so
// it must be discoverable regardless of who created it or who owns the
// game record.
//
// An "accepted" drawResponse is otherwise a completely unilateral claim: a
// player could write one into their own repo with no corresponding offer
// at all and, absent a check, derive a draw out of thin air. So a
// candidate is only trusted here once its drawOffer strongRef is verified
// to actually resolve, in the OTHER player's repo, to a real, matching
// app.atchess.drawOffer for this same game -- "no offer, no draw"
// (atchess-1c9.48 review). This costs one extra getRecord round trip, but
// only for records that pass the cheap in-memory checks first (game URI,
// response == "accepted", and respondedBy authorship), so it is paid at
// most once per game, right when a draw is actually being accepted -- not
// on the hot per-move path.
func (c *Client) getDrawAcceptOutcome(ctx context.Context, gameURI, whiteDID, blackDID string) (*terminalEvent, error) {
	var latest *terminalEvent
	var errs []error

	for _, playerDID := range []string{whiteDID, blackDID} {
		if playerDID == "" {
			continue
		}
		base, ownRepo, err := c.resolveReadEndpoint(ctx, playerDID)
		if err != nil {
			errs = append(errs, fmt.Errorf("resolve read endpoint for %s: %w: %v", playerDID, ErrIncompleteDerivation, err))
			continue
		}
		params := url.Values{"repo": {playerDID}, "collection": {"app.atchess.drawResponse"}, "limit": {"100"}}
		resp, err := c.getXRPC(ctx, base, ownRepo, "com.atproto.repo.listRecords", params)
		if err != nil {
			errs = append(errs, fmt.Errorf("list drawResponses for %s: %w: %v", playerDID, ErrIncompleteDerivation, err))
			continue
		}

		func() {
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				errs = append(errs, fmt.Errorf("list drawResponses for %s: HTTP %d: %w", playerDID, resp.StatusCode, ErrIncompleteDerivation))
				return
			}

			var listResp struct {
				Records []struct {
					URI   string `json:"uri"`
					Value struct {
						Game struct {
							URI string `json:"uri"`
						} `json:"game"`
						DrawOffer struct {
							URI string `json:"uri"`
							CID string `json:"cid"`
						} `json:"drawOffer"`
						RespondedBy string `json:"respondedBy"`
						Response    string `json:"response"`
						CreatedAt   string `json:"createdAt"`
					} `json:"value"`
				} `json:"records"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&listResp); err != nil {
				errs = append(errs, fmt.Errorf("decode drawResponses list for %s: %w: %v", playerDID, ErrIncompleteDerivation, err))
				return
			}

			for _, record := range listResp.Records {
				if record.Value.Game.URI != gameURI || record.Value.Response != "accepted" {
					continue
				}
				if record.Value.RespondedBy != playerDID {
					log.Warn().Str("gameURI", gameURI).Str("repo", playerDID).
						Str("claimedRespondedBy", record.Value.RespondedBy).Str("recordURI", record.URI).
						Msg("ignoring forged drawResponse record: repo owner is not the player it names as responding")
					continue
				}

				offerURI := record.Value.DrawOffer.URI
				offerRepo := recordRepo(offerURI)
				if offerRepo == "" || offerRepo == playerDID {
					log.Warn().Str("gameURI", gameURI).Str("repo", playerDID).Str("recordURI", record.URI).Str("offerURI", offerURI).
						Msg("ignoring drawResponse record: its drawOffer strongRef does not point at the other player's repo")
					continue
				}
				if offerRepo != whiteDID && offerRepo != blackDID {
					continue
				}

				offerCID, offerValue, err := c.getRecordByURI(ctx, offerURI)
				if err != nil {
					log.Warn().Err(err).Str("gameURI", gameURI).Str("recordURI", record.URI).Str("offerURI", offerURI).
						Msg("ignoring drawResponse record: its referenced drawOffer could not be read (no offer, no draw)")
					continue
				}
				if offerCID != record.Value.DrawOffer.CID {
					log.Warn().Str("gameURI", gameURI).Str("recordURI", record.URI).Str("offerURI", offerURI).
						Str("wantCID", record.Value.DrawOffer.CID).Str("gotCID", offerCID).
						Msg("ignoring drawResponse record: its drawOffer strongRef CID does not match the current offer record")
					continue
				}
				offerGameRef, _ := offerValue["game"].(map[string]interface{})
				offerGameURI, _ := offerGameRef["uri"].(string)
				if offerGameURI != gameURI {
					log.Warn().Str("gameURI", gameURI).Str("recordURI", record.URI).Str("offerGameURI", offerGameURI).
						Msg("ignoring drawResponse record: its drawOffer belongs to a different game")
					continue
				}
				offeredBy, _ := offerValue["offeredBy"].(string)
				if offeredBy != offerRepo {
					log.Warn().Str("gameURI", gameURI).Str("recordURI", record.URI).Str("offerURI", offerURI).
						Msg("ignoring drawResponse record: its drawOffer's offeredBy does not match the repo it was found in")
					continue
				}

				t, err := time.Parse(time.RFC3339, record.Value.CreatedAt)
				if err != nil {
					continue
				}
				candidate := &terminalEvent{status: chess.StatusDraw, at: t, rkey: recordKey(record.URI)}
				if latest == nil || moveIsAfter(candidate.at, candidate.rkey, latest.at, latest.rkey) {
					latest = candidate
				}
			}
		}()
	}

	return latest, errors.Join(errs...)
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

	base, ownRepo, err := c.resolveReadEndpoint(ctx, repo)
	if err != nil {
		return nil, fmt.Errorf("failed to get game record: %w", err)
	}
	params := url.Values{"repo": {repo}, "collection": {"app.atchess.game"}, "rkey": {rkey}}
	resp, err := c.getXRPC(ctx, base, ownRepo, "com.atproto.repo.getRecord", params)
	if err != nil {
		return nil, fmt.Errorf("failed to get game record: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to get game record: HTTP %d", resp.StatusCode)
	}

	var getResp struct {
		Value struct {
			Type        string `json:"$type"`
			CreatedAt   string `json:"createdAt"`
			White       string `json:"white"`
			Black       string `json:"black"`
			Status      string `json:"status"`
			FEN         string `json:"fen"`
			PGN         string `json:"pgn"`
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

	game := &chess.Game{
		ID:          gameURI,
		White:       getResp.Value.White,
		Black:       getResp.Value.Black,
		Status:      chess.GameStatus(getResp.Value.Status),
		FEN:         getResp.Value.FEN,
		PGN:         getResp.Value.PGN,
		TimeControl: timeControl,
		CreatedAt:   getResp.Value.CreatedAt,
	}

	// Reconstruct current state from records across BOTH players' repos.
	// The game record is a denormalized cache that may be stale -- not just
	// for ordinary moves (e.g. when the opponent made the last move and
	// couldn't update this repo's record) but for every terminal event
	// (checkmate, resignation, time violation, accepted draw offer): each
	// is written by whichever player triggered it, into THEIR OWN repo,
	// which may not be the repo that owns this app.atchess.game record. So
	// status is derived by merging candidate terminal events from every
	// source, each read across both repos, and taking whichever is most
	// recent (see terminalEvent/latestTerminalEvent). FEN always comes from
	// the latest move, since none of the other event types change the
	// board position.
	//
	// This is real network cost -- up to 4 sources x 2 repos = 8 XRPC calls
	// on top of the game record fetch above, half of them typically to a
	// remote PDS. It is deliberately paid on every call (including from the
	// move-submission path -- MakeMoveHandler must reject a move into an
	// already-terminal game, see atchess-1c9.48) rather than trusted from
	// this repo's own possibly-stale/forgeable "status" cache field. To
	// keep the wall-clock cost sane despite the round-trip count being
	// unavoidable for correctness, the three sources that don't depend on
	// each other's results (moves, resignation, accepted draws) run
	// concurrently; getTimeViolationOutcome needs the real last-move
	// timestamp from the move scan to verify a claim (see its doc comment),
	// so it runs once that finishes rather than being a fourth independent
	// branch.
	var latestMove *moveRecord
	var moveErr error
	var resignationEvent, drawAcceptEvent *terminalEvent
	var resignationErr, drawAcceptErr error

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		latestMove, moveErr = c.getLatestMoveForGame(ctx, gameURI, game.White, game.Black)
	}()
	go func() {
		defer wg.Done()
		resignationEvent, resignationErr = c.getResignationOutcome(ctx, gameURI, game.White, game.Black)
	}()
	drawAcceptDone := make(chan struct{})
	go func() {
		defer close(drawAcceptDone)
		drawAcceptEvent, drawAcceptErr = c.getDrawAcceptOutcome(ctx, gameURI, game.White, game.Black)
	}()
	wg.Wait()

	var moveEvent *terminalEvent
	if moveErr == nil && latestMove != nil {
		game.FEN = latestMove.FEN
		if latestMove.Checkmate {
			fenParts := strings.Split(latestMove.FEN, " ")
			status := chess.StatusWhiteWon
			if len(fenParts) > 1 && fenParts[1] == "w" {
				status = chess.StatusBlackWon
			}
			moveEvent = &terminalEvent{status: status, at: latestMove.CreatedAt, rkey: latestMove.rkey}
		} else if latestMove.Draw {
			moveEvent = &terminalEvent{status: chess.StatusDraw, at: latestMove.CreatedAt, rkey: latestMove.rkey}
		}
	}

	// The real timestamp a timeViolation claim must be checked against:
	// the last actual move, or (if there have been none) the game's own
	// creation time.
	var lastActivityAt time.Time
	lastActivityKnown := false
	if latestMove != nil {
		lastActivityAt = latestMove.CreatedAt
		lastActivityKnown = true
	} else if t, err := time.Parse(time.RFC3339, getResp.Value.CreatedAt); err == nil {
		lastActivityAt = t
		lastActivityKnown = true
	}

	timeControlType := ""
	daysPerMove := 0
	if timeControl != nil {
		timeControlType = timeControl.Type
		daysPerMove = timeControl.DaysPerMove
	}
	timeViolationEvent, timeViolationErr := c.getTimeViolationOutcome(ctx, gameURI, game.White, game.Black, timeControlType, daysPerMove, lastActivityAt, lastActivityKnown)

	<-drawAcceptDone

	if final := latestTerminalEvent(moveEvent, resignationEvent, timeViolationEvent, drawAcceptEvent); final != nil {
		game.Status = final.status
	}

	// Fail closed (see ErrIncompleteDerivation's doc comment): if ANY of the
	// four scans above could not read every repo, Status (and possibly FEN)
	// is unproven, not just "possibly stale". The partial *chess.Game is
	// still returned -- a caller that has deliberately opted into a
	// degraded read-only view could use it -- but every caller in this
	// codebase currently treats a non-nil error here as authoritative and
	// must reject any write it was about to authorize (atchess-1c9.51).
	if derivationErr := errors.Join(moveErr, resignationErr, timeViolationErr, drawAcceptErr); derivationErr != nil {
		game.DerivationIncomplete = true
		return game, fmt.Errorf("%w: %v", ErrIncompleteDerivation, derivationErr)
	}

	return game, nil
}

func (c *Client) GetHandle() string {
	return c.handle
}

func (c *Client) GetPDSURL() string {
	return c.pdsURL
}

// SetPLCDirectoryURL overrides the PLC directory this client's DID and
// (as a last resort) handle resolution uses in place of
// DefaultPLCDirectoryURL -- see config.ATProtoConfig.PLCDirectoryURL. Needed
// so the local dual-PDS test harness (which runs its own hermetic did:plc
// server, since its accounts' DIDs do not exist on the public
// https://plc.directory) can be pointed at that server instead. Must be
// called before any resolution-dependent call (GetGame, ResolveHandle,
// etc.); it is not safe to call concurrently with those.
func (c *Client) SetPLCDirectoryURL(plcDirectoryURL string) {
	c.plcDirectoryURL = plcDirectoryURL
}

// resolver returns the identityResolver this client uses for DID->PDS and
// PLC-export handle resolution, shared (by plcDirectoryURL) across every
// *Client pointed at the same directory -- see getIdentityResolver for why
// that sharing matters given how short-lived *Client instances are in
// internal/web.
func (c *Client) resolver() *identityResolver {
	return getIdentityResolver(c.plcDirectoryURL)
}

// resolveReadEndpoint returns the base PDS URL to read repoDID's records
// from: this client's own PDS when repoDID is empty or c.did, otherwise the
// PDS resolved from repoDID's DID document. ownRepo tells getXRPC whether it
// is safe to attach this client's own bearer token to the request -- see
// getXRPC.
func (c *Client) resolveReadEndpoint(ctx context.Context, repoDID string) (base string, ownRepo bool, err error) {
	if repoDID == "" || repoDID == c.did {
		return c.pdsURL, true, nil
	}
	endpoint, err := c.resolver().resolvePDS(ctx, repoDID)
	if err != nil {
		return "", false, fmt.Errorf("resolving PDS for repo %s: %w", repoDID, err)
	}
	return endpoint, false, nil
}

// getXRPC issues an (optionally query-parameterised) GET against method on
// base. When ownRepo is true, base is this client's own authenticated PDS:
// the request goes through makeRequest, attaching the Bearer/DPoP
// Authorization header (with its 401-triggered refresh-and-retry). When
// ownRepo is false, base is a DID-resolved, possibly-foreign PDS: this
// client's access token is NEVER sent there -- it is only valid at
// c.pdsURL, and sending it to a third-party origin would leak it -- so the
// request is unauthenticated, matching these com.atproto.repo.* /
// com.atproto.identity.* endpoints being public reads.
func (c *Client) getXRPC(ctx context.Context, base string, ownRepo bool, method string, params url.Values) (*http.Response, error) {
	u := xrpcURL(base, method, params)
	if ownRepo {
		return c.makeRequest("GET", u, nil)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	return c.httpClient.Do(req)
}

// ResolveHandle resolves a handle to a DID. A handle can be hosted on any
// PDS, not necessarily this client's own (c.pdsURL) -- see atchess-1c9.10 --
// so this tries, in order:
//  1. this client's own PDS's com.atproto.identity.resolveHandle (fast path
//     for handles it hosts locally; also the only path that works against a
//     PDS with no real public DNS at all, e.g. a fully offline dev setup);
//  2. the standard AT Protocol DNS TXT record (_atproto.<handle>);
//  3. the standard AT Protocol HTTPS well-known endpoint
//     (https://<handle>/.well-known/atproto-did);
//  4. a bounded PLC-export scan (see resolveHandleViaPLCExport), as a last
//     resort for handles that are real accounts but not DNS-resolvable at
//     all -- notably the local dual-PDS test harness's ".test" handles
//     (RFC 2606: permanently reserved, never publicly resolvable).
//
// Returns an error naming the handle and every resolver tried, with each
// one's own failure reason, if all of them fail.
func (c *Client) ResolveHandle(ctx context.Context, handle string) (string, error) {
	// If it's already a DID, return it
	if strings.HasPrefix(handle, "did:") {
		return handle, nil
	}

	// Validate (and normalize to lowercase) BEFORE any resolution strategy
	// runs -- see normalizeAndValidateHandle's doc comment. This is the
	// single choke point every strategy below shares, so none of them ever
	// builds a URL/hostname/DNS query from an unvalidated handle
	// (atchess-1c9.69).
	validated, err := normalizeAndValidateHandle(handle)
	if err != nil {
		return "", err
	}
	handle = validated

	var attempts []string

	if did, err := c.resolveHandleSamePDS(ctx, handle); err == nil {
		return did, nil
	} else {
		attempts = append(attempts, fmt.Sprintf("same-PDS resolveHandle (%s): %v", c.pdsURL, err))
	}

	if did, err := resolveHandleViaDNS(ctx, handle); err == nil {
		return did, nil
	} else {
		attempts = append(attempts, fmt.Sprintf("DNS TXT: %v", err))
	}

	if did, err := resolveHandleViaWellKnown(ctx, c.httpClient, handle); err == nil {
		return did, nil
	} else {
		attempts = append(attempts, fmt.Sprintf("HTTPS well-known: %v", err))
	}

	if did, err := c.resolver().resolveHandleViaPLCExport(ctx, handle); err == nil {
		return did, nil
	} else {
		attempts = append(attempts, fmt.Sprintf("PLC export: %v", err))
	}

	return "", fmt.Errorf("failed to resolve handle %q via any resolver: %s", handle, strings.Join(attempts, "; "))
}

// resolveHandleSamePDS asks this client's own PDS to resolve handle via
// com.atproto.identity.resolveHandle. This only succeeds for handles that
// PDS itself hosts (or otherwise already knows how to resolve); a handle
// hosted elsewhere returns an XRPC InvalidRequest error, which the caller
// (ResolveHandle) treats as "try the next resolver", not a hard failure.
func (c *Client) resolveHandleSamePDS(ctx context.Context, handle string) (string, error) {
	params := url.Values{"handle": {handle}}
	resp, err := c.getXRPC(ctx, c.pdsURL, true, "com.atproto.identity.resolveHandle", params)
	if err != nil {
		return "", fmt.Errorf("failed to resolve handle: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		DID string `json:"did"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("failed to decode response: %w", err)
	}
	if result.DID == "" {
		return "", fmt.Errorf("response had no did")
	}

	return result.DID, nil
}

// currentGameStatus returns gameURI's authoritative, derived status (see
// GetGame's doc comment) -- never the raw, possibly-stale-or-forged
// "status" field cached on the app.atchess.game record itself. On error it
// returns ("", err); the error may wrap ErrIncompleteDerivation (one or
// more repos could not be read while scanning for terminal events). Every
// caller of this method uses the result to decide whether to AUTHORIZE a
// write (resign, offer/accept a draw, claim a time violation), so callers
// MUST fail closed on a non-nil error -- i.e. reject the write -- rather
// than treating "could not verify" the same as "verified active". Treating
// an unreadable repo as equivalent to an active game is exactly the bug
// atchess-1c9.51 fixed: an opponent-PDS outage must not silently reopen a
// game that has already ended. Do not restore the old "err == nil &&
// status != active" fail-open pattern here.
func (c *Client) currentGameStatus(ctx context.Context, gameURI string) (chess.GameStatus, error) {
	g, err := c.GetGame(ctx, gameURI)
	if err != nil {
		return "", err
	}
	return g.Status, nil
}

// OfferDraw creates a draw offer record for a game
func (c *Client) OfferDraw(ctx context.Context, gameID string, message string) (*DrawOffer, error) {
	// First, fetch the game record to get its CID
	gameCID, _, err := c.getGameRecord(ctx, gameID)
	if err != nil {
		return nil, fmt.Errorf("failed to get game record: %w", err)
	}

	// Verify the game is active. Uses the derived status (GetGame), not the
	// raw cached gameValue["status"] field, so this is consistent with
	// terminal events the game's own repo owner may not know about yet
	// (atchess-1c9.48 review). Fail closed if the status could not be
	// verified at all (atchess-1c9.51) -- an unreadable repo must not be
	// treated as "active".
	status, err := c.currentGameStatus(ctx, gameID)
	if err != nil {
		return nil, fmt.Errorf("cannot verify game is still active: %w", err)
	}
	if status != chess.StatusActive {
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
	resp, err := c.makeRequest("POST", xrpcURL(c.pdsURL, "com.atproto.repo.createRecord", nil), reqBody)
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
	base, ownRepo, err := c.resolveReadEndpoint(ctx, repo)
	if err != nil {
		return fmt.Errorf("failed to get draw offer record: %w", err)
	}
	params := url.Values{"repo": {repo}, "collection": {"app.atchess.drawOffer"}, "rkey": {rkey}}
	resp, err := c.getXRPC(ctx, base, ownRepo, "com.atproto.repo.getRecord", params)
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
	// Fallback game CID already embedded in the offer's own strongRef --
	// possibly stale by the time of this response (more moves may have
	// happened since), but good enough for a decline (see below).
	offerGameCID, _ := gameRef["cid"].(string)

	// Verify the game hasn't already reached a terminal state by some
	// OTHER event (e.g. the opponent resigned, or the game timed out)
	// since this offer was made. Unlike OfferDraw/ResignGame/
	// CheckTimeViolation, this check used to be missing entirely here --
	// RespondToDrawOffer only ever looked at the offer record's own
	// "status" field, never the derived game status -- so a draw could be
	// accepted into a game that had already ended, producing two
	// competing terminal events (atchess-1c9.56). Uses the derived status
	// (GetGame), not the raw cached game record status -- see OfferDraw's
	// comment (atchess-1c9.48 review). Fail closed if the status could
	// not be verified at all (atchess-1c9.51).
	if status, statusErr := c.currentGameStatus(ctx, gameURI); statusErr != nil {
		return fmt.Errorf("cannot verify game is still active: %w", statusErr)
	} else if status != chess.StatusActive {
		return fmt.Errorf("cannot respond to draw offer in a game with status: %s", status)
	}

	// Record the response. AT Protocol never permits writing into another
	// account's repository, and the draw offer record (drawOfferURI) lives
	// in the OFFERING player's repo -- which, for the ordinary case of the
	// OTHER player responding, is not c.did. Mutating it via putRecord (as
	// this used to do) always failed in real federation with HTTP 403
	// AccountNotFound, exactly like the retired cross-repo challenge
	// notification write (atchess-1c9.11). Instead, write an
	// app.atchess.drawResponse record into the CALLER's OWN repo,
	// referencing both the offer and the game by strongRef. GetGame
	// derives "draw" status by reading these across both players' repos
	// (getDrawAcceptOutcome), the same pattern already used for moves.
	response := "accepted"
	if !accept {
		response = "declined"
	}

	// An accept is a meaningful state transition, so fetch the freshest
	// possible game CID and fail loudly if that's not possible. A decline
	// is comparatively low-stakes -- it never updates the game record
	// (below) -- so it must not hard-fail just because the game record
	// happens to be momentarily unreadable; fall back to the CID already
	// recorded on the offer itself instead (atchess-1c9.48 review: this
	// getGameRecord call used to be accept-only, and unconditionally
	// requiring it here regressed declines to hard-fail on the same
	// condition an accept does).
	var gameCID string
	if accept {
		gameCID, _, err = c.getGameRecord(ctx, gameURI)
		if err != nil {
			return fmt.Errorf("failed to get game record for draw response: %w", err)
		}
	} else if fresh, _, err := c.getGameRecord(ctx, gameURI); err == nil {
		gameCID = fresh
	} else {
		gameCID = offerGameCID
	}

	drawResponseRecord := map[string]interface{}{
		"$type":     "app.atchess.drawResponse",
		"createdAt": time.Now().Format(time.RFC3339),
		"drawOffer": map[string]interface{}{
			"uri": getResp.URI,
			"cid": getResp.CID,
		},
		"game": map[string]interface{}{
			"uri": gameURI,
			"cid": gameCID,
		},
		"respondedBy": c.did,
		"response":    response,
	}

	createReq := map[string]interface{}{
		"repo":       c.did,
		"collection": "app.atchess.drawResponse",
		"record":     drawResponseRecord,
	}

	createReqBody, _ := json.Marshal(createReq)
	createResp, err := c.makeRequest("POST", xrpcURL(c.pdsURL, "com.atproto.repo.createRecord", nil), createReqBody)
	if err != nil {
		return fmt.Errorf("failed to create draw response record: %w", err)
	}
	defer createResp.Body.Close()

	if createResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(createResp.Body)
		return fmt.Errorf("failed to create draw response record: HTTP %d - %s", createResp.StatusCode, string(body))
	}

	// Best-effort cache refresh: if this caller happens to also own the
	// shared app.atchess.game record (repo == c.did), update its status
	// field too. This is never required for correctness -- GetGame always
	// derives the authoritative status from drawResponse records across
	// both repos regardless of who owns the game record -- but keeps the
	// cached record from looking obviously stale to any reader that
	// doesn't go through derivation.
	if accept {
		gameParts := strings.Split(gameURI, "/")
		if len(gameParts) >= 5 && gameParts[2] == c.did {
			gCID, gameValue, err := c.getGameRecord(ctx, gameURI)
			if err == nil {
				gameValue["status"] = "draw"
				gameValue["updatedAt"] = time.Now().Format(time.RFC3339)
				gameRkey := gameParts[4]
				updateGameReq := map[string]interface{}{
					"repo":       c.did,
					"collection": "app.atchess.game",
					"rkey":       gameRkey,
					"record":     gameValue,
					"swapCid":    gCID,
				}
				updateGameReqBody, _ := json.Marshal(updateGameReq)
				if updateGameResp, err := c.makeRequest("POST", xrpcURL(c.pdsURL, "com.atproto.repo.putRecord", nil), updateGameReqBody); err == nil {
					updateGameResp.Body.Close()
				}
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

	// Verify the game is active. Uses the derived status (GetGame), not the
	// raw cached gameValue["status"] field -- see OfferDraw's comment
	// (atchess-1c9.48 review). Fail closed if the status could not be
	// verified at all (atchess-1c9.51).
	if status, statusErr := c.currentGameStatus(ctx, gameID); statusErr != nil {
		return fmt.Errorf("cannot verify game is still active: %w", statusErr)
	} else if status != chess.StatusActive {
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
		"$type":     "app.atchess.resignation",
		"createdAt": time.Now().Format(time.RFC3339),
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
	resp, err := c.makeRequest("POST", xrpcURL(c.pdsURL, "com.atproto.repo.createRecord", nil), reqBody)
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
		updateResp, err := c.makeRequest("POST", xrpcURL(c.pdsURL, "com.atproto.repo.putRecord", nil), updateReqBody)
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
	params := url.Values{"repo": {c.did}, "collection": {"app.atchess.drawOffer"}, "limit": {"100"}}
	resp, err := c.getXRPC(ctx, c.pdsURL, true, "com.atproto.repo.listRecords", params)
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
				Game      struct {
					URI string `json:"uri"`
					CID string `json:"cid"`
				} `json:"game"`
				OfferedBy   string `json:"offeredBy"`
				MoveNumber  int    `json:"moveNumber"`
				Message     string `json:"message"`
				Status      string `json:"status"`
				RespondedAt string `json:"respondedAt"`
				RespondedBy string `json:"respondedBy"`
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

	// Check if game is still active. Uses the derived status (GetGame), not
	// the raw cached gameValue["status"] field -- see OfferDraw's comment
	// (atchess-1c9.48 review). Fail closed if the status could not be
	// verified at all (atchess-1c9.51): this result also gates
	// ClaimTimeVictory's write, so "could not verify" must not be treated
	// as "still active".
	status, statusErr := c.currentGameStatus(ctx, gameID)
	if statusErr != nil {
		return false, nil, fmt.Errorf("cannot verify game is still active: %w", statusErr)
	}
	if status != chess.StatusActive {
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

				base, ownRepo, resolveErr := c.resolveReadEndpoint(ctx, challengeRepo)
				var resp *http.Response
				var err error
				if resolveErr == nil {
					params := url.Values{"repo": {challengeRepo}, "collection": {"app.atchess.challenge"}, "rkey": {challengeRkey}}
					resp, err = c.getXRPC(ctx, base, ownRepo, "com.atproto.repo.getRecord", params)
				} else {
					err = resolveErr
				}
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
		base, ownRepo, err := c.resolveReadEndpoint(ctx, playerDID)
		if err != nil {
			continue // Skip if we can't resolve this player's PDS
		}
		params := url.Values{"repo": {playerDID}, "collection": {"app.atchess.move"}, "limit": {"100"}}
		resp, err := c.getXRPC(ctx, base, ownRepo, "com.atproto.repo.listRecords", params)
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
		"$type":     "app.atchess.timeViolation",
		"createdAt": time.Now().Format(time.RFC3339),
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
	resp, err := c.makeRequest("POST", xrpcURL(c.pdsURL, "com.atproto.repo.createRecord", nil), reqBody)
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
		updateResp, err := c.makeRequest("POST", xrpcURL(c.pdsURL, "com.atproto.repo.putRecord", nil), updateReqBody)
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

				base, ownRepo, resolveErr := c.resolveReadEndpoint(ctx, challengeRepo)
				var resp *http.Response
				var err error
				if resolveErr == nil {
					params := url.Values{"repo": {challengeRepo}, "collection": {"app.atchess.challenge"}, "rkey": {challengeRkey}}
					resp, err = c.getXRPC(ctx, base, ownRepo, "com.atproto.repo.getRecord", params)
				} else {
					err = resolveErr
				}
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
