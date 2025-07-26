package chess

import (
	"testing"
)

func TestGetMaterialCount(t *testing.T) {
	tests := []struct {
		name            string
		fen             string
		expectedWhite   int
		expectedBlack   int
		expectedBalance int
	}{
		{
			name:            "Starting position",
			fen:             "rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR w KQkq - 0 1",
			expectedWhite:   39, // 8 pawns (8) + 2 knights (6) + 2 bishops (6) + 2 rooks (10) + 1 queen (9)
			expectedBlack:   39,
			expectedBalance: 0,
		},
		{
			name:            "White up a pawn",
			fen:             "rnbqkbnr/ppp1pppp/8/8/8/8/PPPPPPPP/RNBQKBNR w KQkq - 0 1",
			expectedWhite:   39,
			expectedBlack:   38,
			expectedBalance: 1,
		},
		{
			name:            "Black up a knight",
			fen:             "rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/R1BQKBNR w KQkq - 0 1",
			expectedWhite:   36,
			expectedBlack:   39,
			expectedBalance: -3,
		},
		{
			name:            "Endgame - King and pawn vs King",
			fen:             "8/8/8/8/4P3/8/8/4K2k w - - 0 1",
			expectedWhite:   1,
			expectedBlack:   0,
			expectedBalance: 1,
		},
		{
			name:            "Queen endgame",
			fen:             "8/8/8/8/8/8/4Q3/4K2k w - - 0 1",
			expectedWhite:   9,
			expectedBlack:   0,
			expectedBalance: 9,
		},
		{
			name:            "Rook and pawn endgame",
			fen:             "8/8/8/8/8/p7/P7/R3K2k w - - 0 1",
			expectedWhite:   6, // Rook (5) + Pawn (1)
			expectedBlack:   1, // Pawn (1)
			expectedBalance: 5,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			engine := NewEngine()
			if err := engine.LoadFEN(tt.fen); err != nil {
				t.Fatalf("Failed to load FEN: %v", err)
			}

			// Test GetMaterialCount
			count := engine.GetMaterialCount()
			if count.White != tt.expectedWhite {
				t.Errorf("White material: expected %d, got %d", tt.expectedWhite, count.White)
			}
			if count.Black != tt.expectedBlack {
				t.Errorf("Black material: expected %d, got %d", tt.expectedBlack, count.Black)
			}

			// Test GetMaterialBalance
			balance := engine.GetMaterialBalance()
			if balance != tt.expectedBalance {
				t.Errorf("Material balance: expected %d, got %d", tt.expectedBalance, balance)
			}
		})
	}
}

func TestGetPieceValues(t *testing.T) {
	engine := NewEngine()
	values := engine.GetPieceValues()

	expected := map[string]int{
		"pawn":   1,
		"knight": 3,
		"bishop": 3,
		"rook":   5,
		"queen":  9,
		"king":   0,
	}

	for piece, expectedValue := range expected {
		if value, ok := values[piece]; !ok {
			t.Errorf("Missing piece value for %s", piece)
		} else if value != expectedValue {
			t.Errorf("Piece %s: expected value %d, got %d", piece, expectedValue, value)
		}
	}
}

func TestMaterialCountAfterCaptures(t *testing.T) {
	engine := NewEngine()
	
	// Start from initial position
	if err := engine.LoadFEN("rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR w KQkq - 0 1"); err != nil {
		t.Fatalf("Failed to load starting FEN: %v", err)
	}

	// Verify starting material
	count := engine.GetMaterialCount()
	if count.White != 39 || count.Black != 39 {
		t.Errorf("Starting material incorrect: White=%d, Black=%d", count.White, count.Black)
	}

	// Play some moves leading to a capture
	moves := []struct {
		from string
		to   string
	}{
		{"e2", "e4"},
		{"d7", "d5"},
		{"e4", "d5"}, // White pawn captures black pawn
	}

	for _, move := range moves {
		result := engine.MakeMove(move.from, move.to)
		if !result.Valid {
			t.Fatalf("Move %s-%s failed: %s", move.from, move.to, result.Error)
		}
	}

	// After capture, white should have all pieces, black should be down a pawn
	count = engine.GetMaterialCount()
	if count.White != 39 {
		t.Errorf("White material after capture: expected 39, got %d", count.White)
	}
	if count.Black != 38 {
		t.Errorf("Black material after capture: expected 38, got %d", count.Black)
	}

	balance := engine.GetMaterialBalance()
	if balance != 1 {
		t.Errorf("Material balance after pawn capture: expected 1, got %d", balance)
	}
}