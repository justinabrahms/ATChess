package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/justinabrahms/atchess/internal/chess"
	"github.com/justinabrahms/atchess/internal/config"
)

// MockATProtoClient for testing spectator endpoints
type MockATProtoClient struct {
	games map[string]*chess.Game
	moves map[string][]chess.Move
}

func (m *MockATProtoClient) GetGame(gameID string) (*chess.Game, error) {
	if game, ok := m.games[gameID]; ok {
		return game, nil
	}
	return nil, nil
}

func (m *MockATProtoClient) GetMoves(gameID string) ([]chess.Move, error) {
	if moves, ok := m.moves[gameID]; ok {
		return moves, nil
	}
	return []chess.Move{}, nil
}

func TestGetActiveGamesHandler(t *testing.T) {
	// Create test service
	cfg := &config.Config{}
	service := &Service{
		config: cfg,
	}
	
	// Create request
	req, err := http.NewRequest("GET", "/api/spectator/games", nil)
	if err != nil {
		t.Fatal(err)
	}
	
	// Record response
	rr := httptest.NewRecorder()
	handler := http.HandlerFunc(service.GetActiveGamesHandler)
	handler.ServeHTTP(rr, req)
	
	// Check status
	if status := rr.Code; status != http.StatusOK {
		t.Errorf("Expected status 200, got %v", status)
	}
	
	// Check response
	var response map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Errorf("Failed to parse response: %v", err)
	}
	
	if _, ok := response["games"]; !ok {
		t.Error("Response missing 'games' field")
	}
	if _, ok := response["total"]; !ok {
		t.Error("Response missing 'total' field")
	}
}

func TestGetSpectatorGameHandler(t *testing.T) {
	// Create mock client
	mockClient := &MockATProtoClient{
		games: map[string]*chess.Game{
			"test-game-1": {
				ID:        "test-game-1",
				White:     "did:plc:white",
				Black:     "did:plc:black",
				Status:    chess.GameStatusActive,
				FEN:       "rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR w KQkq - 0 1",
				CreatedAt: time.Now().Format(time.RFC3339),
			},
		},
		moves: map[string][]chess.Move{
			"test-game-1": {
				{From: "e2", To: "e4", SAN: "e4"},
				{From: "e7", To: "e5", SAN: "e5"},
			},
		},
	}
	
	// Create test service
	cfg := &config.Config{}
	service := &Service{
		config: cfg,
		client: mockClient,
	}
	
	// Create request
	req, err := http.NewRequest("GET", "/api/spectator/games/test-game-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	
	// Add route vars
	req = mux.SetURLVars(req, map[string]string{
		"id": "test-game-1",
	})
	
	// Record response
	rr := httptest.NewRecorder()
	handler := http.HandlerFunc(service.GetSpectatorGameHandler)
	handler.ServeHTTP(rr, req)
	
	// Check status
	if status := rr.Code; status != http.StatusOK {
		t.Errorf("Expected status 200, got %v", status)
	}
	
	// Check response
	var response map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Errorf("Failed to parse response: %v", err)
	}
	
	// Verify expected fields
	expectedFields := []string{"game", "moves", "moveCount", "materialCount", "materialBalance"}
	for _, field := range expectedFields {
		if _, ok := response[field]; !ok {
			t.Errorf("Response missing '%s' field", field)
		}
	}
	
	// Check move count
	if moveCount, ok := response["moveCount"].(float64); ok {
		if int(moveCount) != 2 {
			t.Errorf("Expected moveCount 2, got %v", moveCount)
		}
	}
}

func TestCheckAbandonmentHandler(t *testing.T) {
	// Create mock client with an old game
	oldTime := time.Now().Add(-4 * 24 * time.Hour) // 4 days ago
	mockClient := &MockATProtoClient{
		games: map[string]*chess.Game{
			"abandoned-game": {
				ID:        "abandoned-game",
				Status:    chess.GameStatusActive,
				CreatedAt: oldTime.Format(time.RFC3339),
			},
		},
		moves: map[string][]chess.Move{
			"abandoned-game": {
				{
					From:      "e2",
					To:        "e4",
					CreatedAt: oldTime.Format(time.RFC3339),
				},
			},
		},
	}
	
	// Create test service
	cfg := &config.Config{}
	service := &Service{
		config: cfg,
		client: mockClient,
	}
	
	// Create request
	req, err := http.NewRequest("GET", "/api/spectator/games/abandoned-game/abandonment", nil)
	if err != nil {
		t.Fatal(err)
	}
	
	// Add route vars
	req = mux.SetURLVars(req, map[string]string{
		"id": "abandoned-game",
	})
	
	// Record response
	rr := httptest.NewRecorder()
	handler := http.HandlerFunc(service.CheckAbandonmentHandler)
	handler.ServeHTTP(rr, req)
	
	// Check status
	if status := rr.Code; status != http.StatusOK {
		t.Errorf("Expected status 200, got %v", status)
	}
	
	// Check response
	var response map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Errorf("Failed to parse response: %v", err)
	}
	
	// Check abandonment status
	if abandoned, ok := response["abandoned"].(bool); ok {
		if !abandoned {
			t.Error("Expected game to be marked as abandoned after 4 days")
		}
	} else {
		t.Error("Response missing 'abandoned' field")
	}
	
	if canClaim, ok := response["canClaim"].(bool); ok {
		if !canClaim {
			t.Error("Expected canClaim to be true for abandoned game")
		}
	}
}

func TestUpdateSpectatorCountHandler(t *testing.T) {
	// Create hub
	hub := NewHub()
	go hub.Run()
	
	// Create test service
	cfg := &config.Config{}
	service := &Service{
		config: cfg,
	}
	
	// Create request to join as spectator
	reqBody := map[string]string{"action": "join"}
	body, _ := json.Marshal(reqBody)
	req, err := http.NewRequest("POST", "/api/spectator/games/test-game/count", bytes.NewBuffer(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	
	// Add route vars
	req = mux.SetURLVars(req, map[string]string{
		"id": "test-game",
	})
	
	// Record response
	rr := httptest.NewRecorder()
	handler := service.UpdateSpectatorCountHandler(hub)
	handler.ServeHTTP(rr, req)
	
	// Check status
	if status := rr.Code; status != http.StatusOK {
		t.Errorf("Expected status 200, got %v", status)
	}
	
	// Check response
	var response map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Errorf("Failed to parse response: %v", err)
	}
	
	if gameID, ok := response["gameId"].(string); ok {
		if gameID != "test-game" {
			t.Errorf("Expected gameId 'test-game', got %s", gameID)
		}
	}
	
	if _, ok := response["spectatorCount"]; !ok {
		t.Error("Response missing 'spectatorCount' field")
	}
}