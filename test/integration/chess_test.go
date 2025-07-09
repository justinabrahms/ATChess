//go:build integration
// +build integration

package integration

import (
	"testing"

	"github.com/justinabrahms/atchess/internal/chess"
	notnil "github.com/notnil/chess"
)

func TestChessEngineIntegration(t *testing.T) {
	engine := chess.NewEngine()
	
	// Test initial position
	if engine.GetStatus() != chess.StatusActive {
		t.Errorf("Expected active status, got %s", engine.GetStatus())
	}
	
	if engine.GetActiveColor() != "white" {
		t.Errorf("Expected white to start, got %s", engine.GetActiveColor())
	}
	
	// Test a basic game sequence
	moves := [][]string{
		{"e2", "e4"},
		{"e7", "e5"},
		{"g1", "f3"},
		{"b8", "c6"},
	}
	
	for i, move := range moves {
		result, err := engine.MakeMove(move[0], move[1], notnil.NoPieceType)
		if err != nil {
			t.Fatalf("Move %d failed: %v", i+1, err)
		}
		
		if result.From != move[0] {
			t.Errorf("Expected from %s, got %s", move[0], result.From)
		}
		
		if result.To != move[1] {
			t.Errorf("Expected to %s, got %s", move[1], result.To)
		}
		
		if result.GameOver {
			t.Errorf("Game should not be over after move %d", i+1)
		}
	}
	
	// Verify the game state
	if engine.GetStatus() != chess.StatusActive {
		t.Errorf("Expected game to still be active")
	}
	
	// Test that PGN contains moves
	pgn := engine.GetPGN()
	if len(pgn) == 0 {
		t.Error("Expected PGN to contain moves")
	}
	
	// Test FEN has changed from initial position
	initialFEN := "rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR w KQkq - 0 1"
	if engine.GetFEN() == initialFEN {
		t.Error("Expected FEN to have changed after moves")
	}
}