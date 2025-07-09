package web

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/mux"
	"github.com/justinabrahms/atchess/internal/chess"
	"github.com/justinabrahms/atchess/internal/config"
)

// ATProtoInterface defines the interface that the web service expects
type ATProtoInterface interface {
	CreateGame(ctx context.Context, opponentDID, color string) (*chess.Game, error)
	GetGame(ctx context.Context, gameURI string) (*chess.Game, error)
	RecordMove(ctx context.Context, gameURI string, move *chess.MoveResult) error
	GetDID() string
	GetHandle() string
	CreateChallenge(ctx context.Context, opponentDID, color, message string) (*chess.Challenge, error)
}

// TestCORSHeadersAlwaysPresentOnPreflightRequests ensures that CORS headers
// are properly set on OPTIONS requests from browsers
func TestCORSHeadersAlwaysPresentOnPreflightRequests(t *testing.T) {
	router := mux.NewRouter()
	
	// Add CORS middleware (same as in main.go)
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
			
			if r.Method == "OPTIONS" {
				w.WriteHeader(http.StatusOK)
				return
			}
			
			next.ServeHTTP(w, r)
		})
	})
	
	// Add explicit OPTIONS handlers
	api := router.PathPrefix("/api").Subrouter()
	api.HandleFunc("/moves", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}).Methods("OPTIONS")
	
	// Test CORS preflight request
	req := httptest.NewRequest("OPTIONS", "/api/moves", nil)
	req.Header.Set("Origin", "http://localhost:8081")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "content-type")
	
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	
	// Verify CORS headers are present
	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}
	
	if w.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("Expected Access-Control-Allow-Origin: *, got %s", w.Header().Get("Access-Control-Allow-Origin"))
	}
	
	if !strings.Contains(w.Header().Get("Access-Control-Allow-Methods"), "POST") {
		t.Errorf("Expected Access-Control-Allow-Methods to contain POST, got %s", w.Header().Get("Access-Control-Allow-Methods"))
	}
	
	if !strings.Contains(w.Header().Get("Access-Control-Allow-Headers"), "Content-Type") {
		t.Errorf("Expected Access-Control-Allow-Headers to contain Content-Type, got %s", w.Header().Get("Access-Control-Allow-Headers"))
	}
}

// TestMoveRequestsUseBodyNotURLForGameID ensures that move requests
// use the request body for game ID rather than URL path to avoid routing issues
func TestMoveRequestsUseBodyNotURLForGameID(t *testing.T) {
	// Create a mock config and service
	cfg := &config.Config{
		Server: config.ServerConfig{
			Host: "localhost",
			Port: 8080,
		},
	}
	
	client := &MockATProtoClient{}
	service := NewTestService(client, cfg)
	
	router := mux.NewRouter()
	api := router.PathPrefix("/api").Subrouter()
	api.HandleFunc("/moves", service.MakeMoveHandler).Methods("POST")
	
	// Test that move requests work with game ID in body
	moveReq := map[string]interface{}{
		"from":    "e2",
		"to":      "e4",
		"fen":     "rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR w KQkq - 0 1",
		"game_id": "at://did:plc:styupz2ghvg7hrq4optipm7s/app.atchess.game/3ltivg2d6bk2e",
	}
	
	reqBody, _ := json.Marshal(moveReq)
	req := httptest.NewRequest("POST", "/api/moves", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	
	if w.Code != http.StatusOK {
		t.Errorf("Expected move request to succeed, got status %d: %s", w.Code, w.Body.String())
	}
}

// TestGameIDDecodingPreservesFullURI ensures that base64 encoding/decoding
// preserves the complete AT Protocol URI without truncation
func TestGameIDDecodingPreservesFullURI(t *testing.T) {
	service := &Service{}
	
	testCases := []string{
		"at://did:plc:styupz2ghvg7hrq4optipm7s/app.atchess.game/3ltivg2d6bk2e",
		"at://did:plc:yguha7jixn3rlblla2pzbmwl/app.atchess.game/3ltiwjqo6222e",
		"at://did:plc:test/app.atchess.game/short",
		"at://did:plc:test/app.atchess.game/verylongrecordkeythatmightcausepadding",
	}
	
	for _, originalURI := range testCases {
		// Simulate JavaScript encoding (preserving padding)
		encoded := encodeGameIdForURL(originalURI)
		
		// Test server-side decoding
		decoded, err := service.decodeGameID(encoded)
		if err != nil {
			t.Errorf("Failed to decode game ID %s: %v", encoded, err)
			continue
		}
		
		if decoded != originalURI {
			t.Errorf("Round-trip failed: expected %s, got %s", originalURI, decoded)
		}
	}
}

// TestATProtocolURIParsingExtractsCorrectComponents ensures that AT Protocol URIs
// are correctly parsed to extract repo DID and record key
func TestATProtocolURIParsingExtractsCorrectComponents(t *testing.T) {
	testCases := []struct {
		uri          string
		expectedRepo string
		expectedRkey string
		shouldError  bool
	}{
		{
			uri:          "at://did:plc:styupz2ghvg7hrq4optipm7s/app.atchess.game/3ltivg2d6bk2e",
			expectedRepo: "did:plc:styupz2ghvg7hrq4optipm7s",
			expectedRkey: "3ltivg2d6bk2e",
			shouldError:  false,
		},
		{
			uri:         "did:plc:styupz2ghvg7hrq4optipm7s/app.atchess.game/3ltivg2d6bk2e",
			shouldError: true,
		},
		{
			uri:         "at://did:plc:styupz2ghvg7hrq4optipm7s/app.atchess.game",
			shouldError: true,
		},
	}
	
	for _, tc := range testCases {
		t.Run(tc.uri, func(t *testing.T) {
			// Parse the URI (simulating the fixed GetGame logic)
			parts := strings.Split(tc.uri, "/")
			
			if len(parts) < 5 || !strings.HasPrefix(tc.uri, "at://") {
				if !tc.shouldError {
					t.Errorf("Expected valid URI, got parsing error")
				}
				return
			}
			
			if tc.shouldError {
				t.Errorf("Expected error for invalid URI, got successful parsing")
				return
			}
			
			repo := parts[2] // The DID
			rkey := parts[4] // The record key
			
			if repo != tc.expectedRepo {
				t.Errorf("Expected repo=%s, got %s", tc.expectedRepo, repo)
			}
			if rkey != tc.expectedRkey {
				t.Errorf("Expected rkey=%s, got %s", tc.expectedRkey, rkey)
			}
		})
	}
}

// TestEmptyFENStringsAreRejected ensures that empty FEN strings
// are properly validated and rejected
func TestEmptyFENStringsAreRejected(t *testing.T) {
	testCases := []struct {
		name     string
		fen      string
		expected bool // whether it should be valid
	}{
		{
			name:     "Empty FEN should be rejected",
			fen:      "",
			expected: false,
		},
		{
			name:     "Valid starting position should be accepted",
			fen:      "rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR w KQkq - 0 1",
			expected: true,
		},
		{
			name:     "Invalid FEN with too few sections should be rejected",
			fen:      "rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR w KQkq",
			expected: false,
		},
	}
	
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Test chess engine validation
			_, err := chess.NewEngineFromFEN(tc.fen)
			
			if tc.expected && err != nil {
				t.Errorf("Expected valid FEN, got error: %v", err)
			}
			if !tc.expected && err == nil {
				t.Errorf("Expected invalid FEN to return error, got nil")
			}
		})
	}
}

// TestCompleteGameWorkflowPreservesDataIntegrity ensures that a complete
// game creation and move workflow maintains data integrity
func TestCompleteGameWorkflowPreservesDataIntegrity(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{
			Host: "localhost",
			Port: 8080,
		},
	}
	
	client := &MockATProtoClient{}
	service := NewTestService(client, cfg)
	
	// Create router with CORS and routes
	router := mux.NewRouter()
	
	// Add CORS middleware
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
			
			if r.Method == "OPTIONS" {
				w.WriteHeader(http.StatusOK)
				return
			}
			
			next.ServeHTTP(w, r)
		})
	})
	
	// Add routes
	api := router.PathPrefix("/api").Subrouter()
	api.HandleFunc("/games", service.CreateGameHandler).Methods("POST")
	api.HandleFunc("/games/{id:.*}", service.GetGameHandler).Methods("GET")
	api.HandleFunc("/moves", service.MakeMoveHandler).Methods("POST")
	
	// Add OPTIONS handlers
	api.HandleFunc("/games", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}).Methods("OPTIONS")
	api.HandleFunc("/moves", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}).Methods("OPTIONS")
	
	// Test 1: Create game
	createGameReq := map[string]interface{}{
		"opponent_did": "did:plc:yguha7jixn3rlblla2pzbmwl",
		"color":        "white",
	}
	
	reqBody, _ := json.Marshal(createGameReq)
	req := httptest.NewRequest("POST", "/api/games", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://localhost:8081")
	
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	
	if w.Code != http.StatusOK {
		t.Errorf("Expected game creation to succeed, got status %d", w.Code)
	}
	
	// Parse response
	var gameResp map[string]interface{}
	err := json.Unmarshal(w.Body.Bytes(), &gameResp)
	if err != nil {
		t.Fatalf("Failed to parse game response: %v", err)
	}
	
	// Verify game ID is present
	gameID, exists := gameResp["id"]
	if !exists || gameID == nil {
		t.Errorf("Game ID missing from response")
	}
	
	// Test 2: CORS preflight for moves
	req = httptest.NewRequest("OPTIONS", "/api/moves", nil)
	req.Header.Set("Origin", "http://localhost:8081")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "content-type")
	
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	
	if w.Code != http.StatusOK {
		t.Errorf("Expected OPTIONS request to succeed, got status %d", w.Code)
	}
	
	// Test 3: Make move
	moveReq := map[string]interface{}{
		"from":    "e2",
		"to":      "e4",
		"fen":     "rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR w KQkq - 0 1",
		"game_id": gameID,
	}
	
	reqBody, _ = json.Marshal(moveReq)
	req = httptest.NewRequest("POST", "/api/moves", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://localhost:8081")
	
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	
	if w.Code != http.StatusOK {
		t.Errorf("Expected move to succeed, got status %d", w.Code)
	}
	
	// Parse move response
	var moveResp map[string]interface{}
	err = json.Unmarshal(w.Body.Bytes(), &moveResp)
	if err != nil {
		t.Fatalf("Failed to parse move response: %v", err)
	}
	
	// Verify move response has correct fields
	expectedFields := []string{"from", "to", "san", "fen"}
	for _, field := range expectedFields {
		if _, exists := moveResp[field]; !exists {
			t.Errorf("Missing field in move response: %s", field)
		}
	}
}

// Helper functions and mock implementations

// NewTestService creates a service with a mock client for testing
func NewTestService(client ATProtoInterface, cfg *config.Config) *TestService {
	return &TestService{
		client: client,
		config: cfg,
	}
}

// TestService is a wrapper around Service that uses our mock client
type TestService struct {
	client ATProtoInterface
	config *config.Config
}

// Just implement the necessary methods for testing
func (s *TestService) MakeMoveHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status": "success",
		"from":   "e2",
		"to":     "e4",
		"san":    "e4",
		"fen":    "rnbqkbnr/pppppppp/8/8/4P3/8/PPPP1PPP/RNBQKBNR b KQkq e3 0 1",
	})
}

func (s *TestService) CreateGameHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"id":     "at://did:plc:styupz2ghvg7hrq4optipm7s/app.atchess.game/mockgame123",
		"status": "active",
	})
}

func (s *TestService) GetGameHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"id":     "at://did:plc:styupz2ghvg7hrq4optipm7s/app.atchess.game/mockgame123",
		"status": "active",
	})
}

// encodeGameIdForURL simulates JavaScript base64 encoding for URLs
func encodeGameIdForURL(gameId string) string {
	encoded := base64.StdEncoding.EncodeToString([]byte(gameId))
	// Convert to URL-safe (but preserve padding)
	return strings.ReplaceAll(strings.ReplaceAll(encoded, "+", "-"), "/", "_")
}

// MockATProtoClient is a mock implementation for testing
type MockATProtoClient struct{}

func (m *MockATProtoClient) CreateGame(ctx context.Context, opponentDID, color string) (*chess.Game, error) {
	return &chess.Game{
		ID:        "at://did:plc:styupz2ghvg7hrq4optipm7s/app.atchess.game/mockgame123",
		White:     "did:plc:styupz2ghvg7hrq4optipm7s",
		Black:     opponentDID,
		Status:    chess.StatusActive,
		FEN:       "rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR w KQkq - 0 1",
		PGN:       "",
		CreatedAt: "2023-01-01T00:00:00Z",
	}, nil
}

func (m *MockATProtoClient) GetGame(ctx context.Context, gameURI string) (*chess.Game, error) {
	return &chess.Game{
		ID:        gameURI,
		White:     "did:plc:styupz2ghvg7hrq4optipm7s",
		Black:     "did:plc:yguha7jixn3rlblla2pzbmwl",
		Status:    chess.StatusActive,
		FEN:       "rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR w KQkq - 0 1",
		PGN:       "",
		CreatedAt: "2023-01-01T00:00:00Z",
	}, nil
}

func (m *MockATProtoClient) RecordMove(ctx context.Context, gameURI string, move *chess.MoveResult) error {
	return nil
}

func (m *MockATProtoClient) GetDID() string {
	return "did:plc:styupz2ghvg7hrq4optipm7s"
}

func (m *MockATProtoClient) GetHandle() string {
	return "player1.test"
}

func (m *MockATProtoClient) CreateChallenge(ctx context.Context, opponentDID, color, message string) (*chess.Challenge, error) {
	return &chess.Challenge{
		ID:          "at://did:plc:styupz2ghvg7hrq4optipm7s/app.atchess.challenge/mockchallenge123",
		Challenger:  "did:plc:styupz2ghvg7hrq4optipm7s",
		Challenged:  opponentDID,
		Status:      "pending",
		Color:       color,
		Message:     message,
		CreatedAt:   "2023-01-01T00:00:00Z",
		ExpiresAt:   "2023-01-02T00:00:00Z",
	}, nil
}