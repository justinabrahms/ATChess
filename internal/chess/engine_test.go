package chess

import (
	"testing"

	"github.com/notnil/chess"
)

func TestNewEngine(t *testing.T) {
	engine := NewEngine()
	if engine == nil {
		t.Fatal("Expected non-nil engine")
	}
	
	expectedFEN := "rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR w KQkq - 0 1"
	if engine.GetFEN() != expectedFEN {
		t.Errorf("Expected FEN %s, got %s", expectedFEN, engine.GetFEN())
	}
	
	if engine.GetStatus() != StatusActive {
		t.Errorf("Expected status %s, got %s", StatusActive, engine.GetStatus())
	}
	
	if engine.GetActiveColor() != "white" {
		t.Errorf("Expected active color white, got %s", engine.GetActiveColor())
	}
}

func TestMakeMove(t *testing.T) {
	engine := NewEngine()
	
	// Test valid move
	result, err := engine.MakeMove("e2", "e4", chess.NoPieceType)
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}
	
	if result.From != "e2" {
		t.Errorf("Expected from e2, got %s", result.From)
	}
	
	if result.To != "e4" {
		t.Errorf("Expected to e4, got %s", result.To)
	}
	
	if result.SAN != "e4" {
		t.Errorf("Expected SAN e4, got %s", result.SAN)
	}
	
	if result.Check {
		t.Error("Expected no check")
	}
	
	if result.Checkmate {
		t.Error("Expected no checkmate")
	}
	
	// Test invalid move
	_, err = engine.MakeMove("e2", "e4", chess.NoPieceType)
	if err == nil {
		t.Error("Expected error for invalid move")
	}
}

func TestMakeMoveInvalidSquare(t *testing.T) {
	engine := NewEngine()
	
	// Test invalid square notation
	_, err := engine.MakeMove("z9", "e4", chess.NoPieceType)
	if err == nil {
		t.Error("Expected error for invalid square")
	}
	
	_, err = engine.MakeMove("e2", "z9", chess.NoPieceType)
	if err == nil {
		t.Error("Expected error for invalid square")
	}
}

func TestNewEngineFromFEN(t *testing.T) {
	validFEN := "rnbqkbnr/pppppppp/8/8/4P3/8/PPPP1PPP/RNBQKBNR b KQkq e3 0 1"
	engine, err := NewEngineFromFEN(validFEN)
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}
	
	if engine.GetFEN() != validFEN {
		t.Errorf("Expected FEN %s, got %s", validFEN, engine.GetFEN())
	}
	
	if engine.GetActiveColor() != "black" {
		t.Errorf("Expected active color black, got %s", engine.GetActiveColor())
	}
}

func TestNewEngineFromInvalidFEN(t *testing.T) {
	invalidFEN := "invalid-fen"
	_, err := NewEngineFromFEN(invalidFEN)
	if err == nil {
		t.Error("Expected error for invalid FEN")
	}
}

func TestParsePromotion(t *testing.T) {
	tests := []struct {
		input    string
		expected chess.PieceType
	}{
		{"q", chess.Queen},
		{"r", chess.Rook},
		{"b", chess.Bishop},
		{"n", chess.Knight},
		{"x", chess.NoPieceType},
		{"", chess.NoPieceType},
	}
	
	for _, test := range tests {
		result := ParsePromotion(test.input)
		if result != test.expected {
			t.Errorf("ParsePromotion(%s) = %v, expected %v", test.input, result, test.expected)
		}
	}
}

func TestValidateFEN(t *testing.T) {
	engine := NewEngine()
	
	// Valid FEN
	validFEN := "rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR w KQkq - 0 1"
	if err := engine.ValidateFEN(validFEN); err != nil {
		t.Errorf("Expected no error for valid FEN, got %v", err)
	}
	
	// Invalid FEN
	invalidFEN := "invalid-fen"
	if err := engine.ValidateFEN(invalidFEN); err == nil {
		t.Error("Expected error for invalid FEN")
	}
}