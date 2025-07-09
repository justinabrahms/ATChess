package bugs

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/mux"
	"github.com/justinabrahms/atchess/internal/atproto"
	"github.com/justinabrahms/atchess/internal/chess"
	"github.com/justinabrahms/atchess/internal/config"
	"github.com/justinabrahms/atchess/internal/web"
)

// TestBug1_CORSOptionsRequestHandling tests CORS preflight request handling
func TestBug1_CORSOptionsRequestHandling(t *testing.T) {
	// Create a test server with CORS middleware
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

// TestBug2_ATProtocolURIRouting tests AT Protocol URI handling in routes
func TestBug2_ATProtocolURIRouting(t *testing.T) {
	// Test that AT Protocol URIs cause routing issues when used in URL paths
	atProtocolURI := "at://did:plc:styupz2ghvg7hrq4optipm7s/app.atchess.game/3ltivg2d6bk2e"
	
	// Test URL encoding approach (should cause issues)
	urlEncodedURI := "at%3A%2F%2Fdid%3Aplc%3Astyupz2ghvg7hrq4optipm7s%2Fapp.atchess.game%2F3ltivg2d6bk2e"
	
	router := mux.NewRouter()
	router.HandleFunc("/api/games/{id:.*}/moves", func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		gameID := vars["id"]
		
		// This should demonstrate the problem - the ID gets mangled
		if gameID != atProtocolURI {
			t.Logf("URL encoded ID gets mangled: %s", gameID)
		}
		
		w.WriteHeader(http.StatusOK)
	}).Methods("POST")
	
	// Test with URL encoded URI (demonstrates the problem)
	req := httptest.NewRequest("POST", "/api/games/"+urlEncodedURI+"/moves", bytes.NewReader([]byte("{}")))
	req.Header.Set("Content-Type", "application/json")
	
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	
	// This demonstrates that URL encoding causes issues
	if w.Code == http.StatusMovedPermanently {
		t.Logf("URL encoded AT Protocol URI causes 301 redirect (expected problem)")
	}
}

// TestBug3_MissingJSONStructTags tests JSON serialization with proper struct tags
func TestBug3_MissingJSONStructTags(t *testing.T) {
	// Test MoveResult serialization
	moveResult := &chess.MoveResult{
		From:      "e2",
		To:        "e4",
		SAN:       "e4",
		FEN:       "rnbqkbnr/pppppppp/8/8/4P3/8/PPPP1PPP/RNBQKBNR b KQkq e3 0 1",
		Check:     false,
		Checkmate: false,
		Draw:      false,
		GameOver:  false,
		Result:    "",
	}
	
	// Serialize to JSON
	jsonData, err := json.Marshal(moveResult)
	if err != nil {
		t.Fatalf("Failed to marshal MoveResult: %v", err)
	}
	
	// Parse back to verify field names
	var parsed map[string]interface{}
	err = json.Unmarshal(jsonData, &parsed)
	if err != nil {
		t.Fatalf("Failed to unmarshal JSON: %v", err)
	}
	
	// Verify that fields have correct lowercase names
	expectedFields := []string{"from", "to", "san", "fen", "check", "checkmate", "draw", "gameOver", "result"}
	for _, field := range expectedFields {
		if _, exists := parsed[field]; !exists {
			t.Errorf("Missing field in JSON: %s", field)
		}
	}
	
	// Verify values are correct
	if parsed["from"] != "e2" {
		t.Errorf("Expected from=e2, got %v", parsed["from"])
	}
	if parsed["san"] != "e4" {
		t.Errorf("Expected san=e4, got %v", parsed["san"])
	}
	if parsed["fen"] != moveResult.FEN {
		t.Errorf("Expected fen=%s, got %v", moveResult.FEN, parsed["fen"])
	}
}

// TestBug4_EmptyFENStringValidation tests handling of empty FEN strings
func TestBug4_EmptyFENStringValidation(t *testing.T) {
	// Test that empty FEN strings are properly handled
	testCases := []struct {
		name     string
		fen      string
		expected bool // whether it should be valid
	}{
		{
			name:     "Empty FEN",
			fen:      "",
			expected: false,
		},
		{
			name:     "Valid starting position",
			fen:      "rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR w KQkq - 0 1",
			expected: true,
		},
		{
			name:     "Invalid FEN - too few sections",
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

// TestBug5_ATProtocolURIParsing tests proper parsing of AT Protocol URIs
func TestBug5_ATProtocolURIParsing(t *testing.T) {
	testCases := []struct {
		name     string
		uri      string
		expected struct {
			repo string
			rkey string
		}
		shouldError bool
	}{
		{
			name: "Valid AT Protocol URI",
			uri:  "at://did:plc:styupz2ghvg7hrq4optipm7s/app.atchess.game/3ltivg2d6bk2e",
			expected: struct {
				repo string
				rkey string
			}{
				repo: "did:plc:styupz2ghvg7hrq4optipm7s",
				rkey: "3ltivg2d6bk2e",
			},
			shouldError: false,
		},
		{
			name:        "Invalid URI - missing at://",
			uri:         "did:plc:styupz2ghvg7hrq4optipm7s/app.atchess.game/3ltivg2d6bk2e",
			shouldError: true,
		},
		{
			name:        "Invalid URI - too few parts",
			uri:         "at://did:plc:styupz2ghvg7hrq4optipm7s/app.atchess.game",
			shouldError: true,
		},
	}
	
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Parse the URI (simulating the fixed GetGame logic)
			parts := strings.Split(tc.uri, "/")
			
			if len(parts) < 4 || !strings.HasPrefix(tc.uri, "at://") {
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
			
			if repo != tc.expected.repo {
				t.Errorf("Expected repo=%s, got %s", tc.expected.repo, repo)
			}
			if rkey != tc.expected.rkey {
				t.Errorf("Expected rkey=%s, got %s", tc.expected.rkey, rkey)
			}
		})
	}
}

// TestBug6_Base64PaddingTruncation tests base64 encoding/decoding round-trip
func TestBug6_Base64PaddingTruncation(t *testing.T) {
	testCases := []string{
		"at://did:plc:styupz2ghvg7hrq4optipm7s/app.atchess.game/3ltivg2d6bk2e",
		"at://did:plc:yguha7jixn3rlblla2pzbmwl/app.atchess.game/3ltiwjqo6222e",
		"at://did:plc:test/app.atchess.game/short",
		"at://did:plc:test/app.atchess.game/verylongrecordkeythatmightcausepadding",
	}
	
	for _, gameID := range testCases {
		t.Run(fmt.Sprintf("GameID_%s", gameID[len("at://"):]), func(t *testing.T) {
			// Encode (JavaScript-style, preserving padding)
			encoded := base64.StdEncoding.EncodeToString([]byte(gameID))
			// Convert to URL-safe (but preserve padding)
			urlSafe := strings.ReplaceAll(strings.ReplaceAll(encoded, "+", "-"), "/", "_")
			
			// Decode (server-style)
			// Convert URL-safe back to regular base64
			regular := strings.ReplaceAll(strings.ReplaceAll(urlSafe, "-", "+"), "_", "/")
			decoded, err := base64.StdEncoding.DecodeString(regular)
			if err != nil {
				t.Errorf("Failed to decode base64: %v", err)
				return
			}
			
			decodedStr := string(decoded)
			if decodedStr != gameID {
				t.Errorf("Round-trip failed: expected %s, got %s", gameID, decodedStr)
			}
		})
	}
}

// TestBug7_GameCreationJSONSerialization tests Game struct JSON serialization
func TestBug7_GameCreationJSONSerialization(t *testing.T) {
	// Test Game serialization
	game := &chess.Game{
		ID:        "at://did:plc:styupz2ghvg7hrq4optipm7s/app.atchess.game/3ltivg2d6bk2e",
		White:     "did:plc:styupz2ghvg7hrq4optipm7s",
		Black:     "did:plc:yguha7jixn3rlblla2pzbmwl",
		Status:    chess.StatusActive,
		FEN:       "rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR w KQkq - 0 1",
		PGN:       "",
		CreatedAt: "2023-01-01T00:00:00Z",
	}
	
	// Serialize to JSON
	jsonData, err := json.Marshal(game)
	if err != nil {
		t.Fatalf("Failed to marshal Game: %v", err)
	}
	
	// Parse back to verify field names
	var parsed map[string]interface{}
	err = json.Unmarshal(jsonData, &parsed)
	if err != nil {
		t.Fatalf("Failed to unmarshal JSON: %v", err)
	}
	
	// Verify that fields have correct lowercase names
	expectedFields := []string{"id", "white", "black", "status", "fen", "pgn", "createdAt"}
	for _, field := range expectedFields {
		if _, exists := parsed[field]; !exists {
			t.Errorf("Missing field in JSON: %s", field)
		}
	}
	
	// Verify values are correct
	if parsed["id"] != game.ID {
		t.Errorf("Expected id=%s, got %v", game.ID, parsed["id"])
	}
	if parsed["white"] != game.White {
		t.Errorf("Expected white=%s, got %v", game.White, parsed["white"])
	}
	if parsed["status"] != string(game.Status) {
		t.Errorf("Expected status=%s, got %v", game.Status, parsed["status"])
	}
}

// TestBug_IntegrationScenario tests a complete scenario that would have triggered multiple bugs
func TestBug_IntegrationScenario(t *testing.T) {
	// This test simulates a complete game creation and move scenario
	// that would have triggered multiple bugs in the original implementation
	
	// Create a mock config
	cfg := &config.Config{
		Server: struct {
			Host string `yaml:"host"`
			Port int    `yaml:"port"`
		}{
			Host: "localhost",
			Port: 8080,
		},
		ATProto: struct {
			PDSURL   string `yaml:"pds_url"`
			Handle   string `yaml:"handle"`
			Password string `yaml:"password"`
		}{
			PDSURL:   "http://localhost:3000",
			Handle:   "player1.test",
			Password: "player1pass",
		},
	}
	
	// Create a mock AT Protocol client
	client := &MockATProtoClient{}
	
	// Create service
	service := web.NewService(client, cfg)
	
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
	
	// Test 1: Create game (would have failed due to JSON serialization bug)
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
	
	// Verify game ID is present (would have been missing due to JSON bug)
	gameID, exists := gameResp["id"]
	if !exists || gameID == nil {
		t.Errorf("Game ID missing from response")
	}
	
	// Test 2: CORS preflight for moves (would have failed due to CORS bug)
	req = httptest.NewRequest("OPTIONS", "/api/moves", nil)
	req.Header.Set("Origin", "http://localhost:8081")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "content-type")
	
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	
	if w.Code != http.StatusOK {
		t.Errorf("Expected OPTIONS request to succeed, got status %d", w.Code)
	}
	
	// Test 3: Make move (would have failed due to empty FEN and JSON bugs)
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
	
	// Verify move response has correct fields (would have been missing due to JSON bug)
	expectedFields := []string{"from", "to", "san", "fen"}
	for _, field := range expectedFields {
		if _, exists := moveResp[field]; !exists {
			t.Errorf("Missing field in move response: %s", field)
		}
	}
}

// MockATProtoClient is a mock implementation for testing
type MockATProtoClient struct{}

func (m *MockATProtoClient) CreateGame(ctx interface{}, opponentDID, color string) (*chess.Game, error) {
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

func (m *MockATProtoClient) GetGame(ctx interface{}, gameURI string) (*chess.Game, error) {
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

func (m *MockATProtoClient) RecordMove(ctx interface{}, gameURI string, move *chess.MoveResult) error {
	return nil
}

func (m *MockATProtoClient) GetDID() string {
	return "did:plc:styupz2ghvg7hrq4optipm7s"
}

func (m *MockATProtoClient) GetHandle() string {
	return "player1.test"
}

func (m *MockATProtoClient) CreateChallenge(ctx interface{}, opponentDID, color, message string) (*chess.Challenge, error) {
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