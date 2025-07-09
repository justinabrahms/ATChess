package chess

import (
	"encoding/json"
	"testing"
)

// TestMoveResultJSONSerializationAlwaysIncludesRequiredFields ensures that
// MoveResult structs always serialize to JSON with the expected field names
func TestMoveResultJSONSerializationAlwaysIncludesRequiredFields(t *testing.T) {
	moveResult := &MoveResult{
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

// TestGameJSONSerializationAlwaysIncludesRequiredFields ensures that
// Game structs always serialize to JSON with the expected field names
func TestGameJSONSerializationAlwaysIncludesRequiredFields(t *testing.T) {
	game := &Game{
		ID:        "at://did:plc:styupz2ghvg7hrq4optipm7s/app.atchess.game/3ltivg2d6bk2e",
		White:     "did:plc:styupz2ghvg7hrq4optipm7s",
		Black:     "did:plc:yguha7jixn3rlblla2pzbmwl",
		Status:    StatusActive,
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

// TestFENValidationRejectsInvalidInput ensures that the chess engine
// properly validates FEN strings and rejects invalid input
func TestFENValidationRejectsInvalidInput(t *testing.T) {
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
		{
			name:     "Valid mid-game position should be accepted",
			fen:      "rnbqkbnr/pppppppp/8/8/4P3/8/PPPP1PPP/RNBQKBNR b KQkq e3 0 1",
			expected: true,
		},
		{
			name:     "Invalid board configuration should be rejected",
			fen:      "invalid/board/config/here w KQkq - 0 1",
			expected: false,
		},
	}
	
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Test chess engine validation
			_, err := NewEngineFromFEN(tc.fen)
			
			if tc.expected && err != nil {
				t.Errorf("Expected valid FEN, got error: %v", err)
			}
			if !tc.expected && err == nil {
				t.Errorf("Expected invalid FEN to return error, got nil")
			}
		})
	}
}

// TestMoveValidationEnforcesChessRules ensures that the chess engine
// properly validates moves according to chess rules
func TestMoveValidationEnforcesChessRules(t *testing.T) {
	// Test with starting position
	engine, err := NewEngineFromFEN("rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR w KQkq - 0 1")
	if err != nil {
		t.Fatalf("Failed to create engine: %v", err)
	}
	
	testCases := []struct {
		name     string
		from     string
		to       string
		expected bool // whether move should be valid
	}{
		{
			name:     "Valid pawn move should be accepted",
			from:     "e2",
			to:       "e4",
			expected: true,
		},
		{
			name:     "Invalid pawn move should be rejected",
			from:     "e2",
			to:       "e5",
			expected: false,
		},
		{
			name:     "Valid knight move should be accepted",
			from:     "g1",
			to:       "f3",
			expected: true,
		},
		{
			name:     "Invalid knight move should be rejected",
			from:     "g1",
			to:       "e2",
			expected: false,
		},
		{
			name:     "Move to occupied square by same color should be rejected",
			from:     "e2",
			to:       "d1",
			expected: false,
		},
	}
	
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := engine.MakeMove(tc.from, tc.to, "")
			
			if tc.expected && err != nil {
				t.Errorf("Expected valid move, got error: %v", err)
			}
			if !tc.expected && err == nil {
				t.Errorf("Expected invalid move to return error, got nil")
			}
		})
	}
}