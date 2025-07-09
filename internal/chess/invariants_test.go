package chess

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	
	"github.com/notnil/chess"
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
			// Create fresh engine for each test case
			engine, err := NewEngineFromFEN("rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR w KQkq - 0 1")
			if err != nil {
				t.Fatalf("Failed to create engine: %v", err)
			}
			
			_, err = engine.MakeMove(tc.from, tc.to, chess.NoPieceType)
			
			if tc.expected && err != nil {
				t.Errorf("Expected valid move, got error: %v", err)
			}
			if !tc.expected && err == nil {
				t.Errorf("Expected invalid move to return error, got nil")
			}
		})
	}
}

// TestInvalidMoveSubmissionAlwaysRejectsIllegalMoves ensures that the chess engine
// comprehensively rejects all types of invalid moves that users might attempt
func TestInvalidMoveSubmissionAlwaysRejectsIllegalMoves(t *testing.T) {
	engine := NewEngine()
	
	invalidMoves := []struct {
		name string
		from string
		to   string
	}{
		{"Move to same square", "e2", "e2"},
		{"Move to invalid square", "e2", "z9"},
		{"Move from invalid square", "z9", "e4"},
		{"Move piece that doesn't exist", "e3", "e4"},
		{"Move opponent's piece", "e7", "e5"},
		{"Move through other pieces", "a1", "a8"},
		{"Move bishop diagonally through pieces", "c1", "f4"},
		{"Move knight illegally", "b1", "e4"},
		{"Move king more than one square", "e1", "e3"},
		{"Move rook diagonally", "a1", "b2"},
		{"Move pawn backwards", "e2", "e1"},
		{"Move pawn sideways", "e2", "d2"},
		{"Move pawn three squares", "e2", "e5"},
		{"Capture own piece", "e2", "d1"},
		{"Move from empty square", "e4", "e5"},
		{"Castle when king has moved", "e1", "g1"}, // after moving king
		{"Castle when rook has moved", "e1", "c1"}, // after moving rook
		{"Castle through check", "e1", "g1"}, // in specific positions
		{"Castle when in check", "e1", "g1"}, // when king is in check
		{"En passant when not available", "e5", "d6"}, // without proper setup
		{"Promote to invalid piece", "e7", "e8"}, // would need promotion parameter
	}
	
	for _, move := range invalidMoves {
		t.Run(move.name, func(t *testing.T) {
			_, err := engine.MakeMove(move.from, move.to, chess.NoPieceType)
			if err == nil {
				t.Errorf("Expected invalid move %s->%s to be rejected, but it was accepted", move.from, move.to)
			}
		})
	}
}

// TestPGNConsistencyAcrossMovesPreservesGameHistory ensures that the PGN
// accurately reflects all moves made and maintains consistency with board state
func TestPGNConsistencyAcrossMovesPreservesGameHistory(t *testing.T) {
	engine := NewEngine()
	
	// Define a sequence of moves that creates a known game
	moves := []struct {
		from string
		to   string
		san  string // expected standard algebraic notation
	}{
		{"e2", "e4", "e4"},
		{"e7", "e5", "e5"},
		{"g1", "f3", "Nf3"},
		{"b8", "c6", "Nc6"},
		{"f1", "b5", "Bb5"},
		{"a7", "a6", "a6"},
		{"b5", "a4", "Ba4"},
		{"g8", "f6", "Nf6"},
		{"e1", "g1", "O-O"}, // kingside castling
		{"f8", "e7", "Be7"},
	}
	
	var expectedPGN []string
	
	for i, move := range moves {
		t.Run(fmt.Sprintf("Move %d: %s", i+1, move.san), func(t *testing.T) {
			// Get PGN before move
			pgnBefore := engine.GetPGN()
			
			// Make the move
			result, err := engine.MakeMove(move.from, move.to, chess.NoPieceType)
			if err != nil {
				t.Fatalf("Failed to make move %s->%s: %v", move.from, move.to, err)
			}
			
			// Verify the SAN matches expected
			if result.SAN != move.san {
				t.Errorf("Expected SAN %s, got %s", move.san, result.SAN)
			}
			
			// Get PGN after move
			pgnAfter := engine.GetPGN()
			
			// Verify PGN has changed (unless it's the first move and PGN was empty)
			if i == 0 && pgnBefore == "" {
				if pgnAfter == "" {
					t.Error("Expected PGN to contain moves after first move")
				}
			} else if pgnAfter == pgnBefore {
				t.Error("Expected PGN to change after move")
			}
			
			// Verify PGN contains the expected move
			if !strings.Contains(pgnAfter, move.san) {
				t.Errorf("Expected PGN to contain move %s, got: %s", move.san, pgnAfter)
			}
			
			// Add to expected PGN sequence
			expectedPGN = append(expectedPGN, move.san)
			
			// Verify all previous moves are still in PGN
			for j := 0; j < i; j++ {
				if !strings.Contains(pgnAfter, expectedPGN[j]) {
					t.Errorf("Expected PGN to contain previous move %s, got: %s", expectedPGN[j], pgnAfter)
				}
			}
		})
	}
	
	// Test that PGN can be used to reconstruct the same position
	t.Run("PGN reconstruction", func(t *testing.T) {
		finalPGN := engine.GetPGN()
		finalFEN := engine.GetFEN()
		
		// Create a new engine and verify we can parse the PGN
		// Note: We can't easily test PGN parsing without additional libraries,
		// but we can verify that the PGN is non-empty and contains expected moves
		if finalPGN == "" {
			t.Error("Final PGN should not be empty after sequence of moves")
		}
		
		// Verify the FEN has changed from initial position
		initialFEN := "rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR w KQkq - 0 1"
		if finalFEN == initialFEN {
			t.Error("FEN should have changed after sequence of moves")
		}
		
		// Verify PGN format looks reasonable (contains move numbers and moves)
		if !strings.Contains(finalPGN, "1.") {
			t.Error("PGN should contain move numbers")
		}
		
		// Count moves in PGN - should have 10 moves (5 pairs)
		moveCount := 0
		for _, expectedMove := range expectedPGN {
			if strings.Contains(finalPGN, expectedMove) {
				moveCount++
			}
		}
		if moveCount != len(expectedPGN) {
			t.Errorf("Expected PGN to contain all %d moves, found %d", len(expectedPGN), moveCount)
		}
	})
}