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

func TestGetPieceValues(t *testing.T) {
	engine := NewEngine()
	values := engine.GetPieceValues()
	
	expectedValues := map[string]int{
		"pawn":   1,
		"knight": 3,
		"bishop": 3,
		"rook":   5,
		"queen":  9,
		"king":   0,
	}
	
	for piece, expectedValue := range expectedValues {
		if value, ok := values[piece]; !ok || value != expectedValue {
			t.Errorf("Expected %s value %d, got %d", piece, expectedValue, value)
		}
	}
}

func TestGetMaterialCount(t *testing.T) {
	tests := []struct {
		name          string
		fen           string
		expectedWhite int
		expectedBlack int
	}{
		{
			name:          "Starting position",
			fen:           "rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR w KQkq - 0 1",
			expectedWhite: 39, // 8 pawns (8) + 2 knights (6) + 2 bishops (6) + 2 rooks (10) + 1 queen (9) = 39
			expectedBlack: 39,
		},
		{
			name:          "Queen vs Rook endgame",
			fen:           "8/8/8/8/8/8/4Q3/4K2k b - - 0 1",
			expectedWhite: 9,  // Queen only
			expectedBlack: 0,  // King only
		},
		{
			name:          "Minor piece endgame",
			fen:           "8/8/8/8/8/8/N3B3/K6k w - - 0 1",
			expectedWhite: 6,  // Knight (3) + Bishop (3)
			expectedBlack: 0,  // King only
		},
		{
			name:          "Complex position",
			fen:           "rnbq1rk1/pppp1ppp/5n2/2b1p3/2B1P3/5N2/PPPP1PPP/RNBQK2R w KQ - 4 5",
			expectedWhite: 39, // 8 pawns (8) + 2 knights (6) + 2 bishops (6) + 2 rooks (10) + 1 queen (9) = 39
			expectedBlack: 39, // 8 pawns (8) + 2 knights (6) + 2 bishops (6) + 2 rooks (10) + 1 queen (9) = 39
		},
	}
	
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			engine, err := NewEngineFromFEN(test.fen)
			if err != nil {
				t.Fatalf("Failed to create engine from FEN: %v", err)
			}
			
			count := engine.GetMaterialCount()
			if count.White != test.expectedWhite {
				t.Errorf("Expected white material %d, got %d", test.expectedWhite, count.White)
			}
			if count.Black != test.expectedBlack {
				t.Errorf("Expected black material %d, got %d", test.expectedBlack, count.Black)
			}
		})
	}
}

func TestGetMaterialBalance(t *testing.T) {
	tests := []struct {
		name            string
		fen             string
		expectedBalance int
	}{
		{
			name:            "Starting position",
			fen:             "rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR w KQkq - 0 1",
			expectedBalance: 0, // Equal material
		},
		{
			name:            "White up a queen",
			fen:             "rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR w KQkq - 0 1",
			expectedBalance: 0, // Still equal in starting position
		},
		{
			name:            "White has queen advantage",
			fen:             "8/8/8/8/8/8/4Q3/4K2k b - - 0 1",
			expectedBalance: 9, // White has queen (+9), black has nothing
		},
		{
			name:            "Black has rook advantage",
			fen:             "8/8/8/8/8/8/4r3/4K2k b - - 0 1",
			expectedBalance: -5, // Black has rook (+5), white has nothing
		},
		{
			name:            "Complex unbalanced position",
			fen:             "rnb1k2r/pppp1ppp/5n2/2b1p3/2B1P3/5N2/PPPP1PPP/RNBQK2R w KQkq - 4 5",
			expectedBalance: 9, // White has queen, black doesn't
		},
	}
	
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			engine, err := NewEngineFromFEN(test.fen)
			if err != nil {
				t.Fatalf("Failed to create engine from FEN: %v", err)
			}
			
			balance := engine.GetMaterialBalance()
			if balance != test.expectedBalance {
				t.Errorf("Expected material balance %d, got %d", test.expectedBalance, balance)
			}
		})
	}
}

func TestMaterialCountAfterMoves(t *testing.T) {
	engine := NewEngine()
	
	// Initial material should be equal
	initialCount := engine.GetMaterialCount()
	if initialCount.White != 39 || initialCount.Black != 39 {
		t.Errorf("Expected initial material 39-39, got %d-%d", initialCount.White, initialCount.Black)
	}
	
	// Make a few moves
	moves := []struct {
		from string
		to   string
	}{
		{"e2", "e4"},
		{"d7", "d5"},
		{"e4", "d5"}, // White captures black pawn
	}
	
	for _, move := range moves {
		_, err := engine.MakeMove(move.from, move.to, chess.NoPieceType)
		if err != nil {
			t.Fatalf("Failed to make move %s-%s: %v", move.from, move.to, err)
		}
	}
	
	// After capturing a pawn, white should be up by 1
	finalCount := engine.GetMaterialCount()
	finalBalance := engine.GetMaterialBalance()
	
	if finalCount.White != 39 {
		t.Errorf("Expected white material 39, got %d", finalCount.White)
	}
	if finalCount.Black != 38 {
		t.Errorf("Expected black material 38 (lost a pawn), got %d", finalCount.Black)
	}
	if finalBalance != 1 {
		t.Errorf("Expected material balance +1 (white up a pawn), got %d", finalBalance)
	}
}