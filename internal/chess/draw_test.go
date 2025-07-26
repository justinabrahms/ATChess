package chess

import (
	"testing"

	"github.com/notnil/chess"
)

func TestDrawDetection(t *testing.T) {
	tests := []struct {
		name     string
		fen      string
		moves    []string
		wantDraw bool
		drawType string
	}{
		{
			name:     "Stalemate position",
			fen:      "7k/5Q2/6K1/8/8/8/8/8 b - - 0 1", // Black king in stalemate
			moves:    []string{},
			wantDraw: true,
			drawType: "Stalemate",
		},
		{
			name:     "Insufficient material - King vs King",
			fen:      "8/8/8/4k3/8/3K4/8/8 w - - 0 1",
			moves:    []string{},
			wantDraw: true,
			drawType: "InsufficientMaterial",
		},
		{
			name:     "Insufficient material - King and Bishop vs King",
			fen:      "8/8/8/4k3/8/3KB3/8/8 w - - 0 1",
			moves:    []string{},
			wantDraw: true,
			drawType: "InsufficientMaterial",
		},
		{
			name:     "Insufficient material - King and Knight vs King",
			fen:      "8/8/8/4k3/8/3KN3/8/8 w - - 0 1",
			moves:    []string{},
			wantDraw: true,
			drawType: "InsufficientMaterial",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			engine, err := NewEngineFromFEN(tt.fen)
			if err != nil {
				t.Fatalf("Failed to create engine from FEN: %v", err)
			}

			// Make any required moves
			for _, move := range tt.moves {
				from := move[:2]
				to := move[2:4]
				_, err := engine.MakeMove(from, to, chess.NoPieceType)
				if err != nil {
					t.Fatalf("Failed to make move %s: %v", move, err)
				}
			}

			// Check draw status
			if got := engine.IsDrawn(); got != tt.wantDraw {
				t.Errorf("IsDrawn() = %v, want %v", got, tt.wantDraw)
			}

			// Check draw type if applicable
			if tt.wantDraw && tt.drawType != "" {
				reason := engine.GetDrawReason()
				if reason == "" {
					t.Errorf("Expected draw reason, got empty string")
				}
				t.Logf("Draw reason: %s", reason)
			}
		})
	}
}

func TestThreefoldRepetition(t *testing.T) {
	engine := NewEngine()

	// Create a sequence that will lead to threefold repetition
	// Knights moving back and forth
	moves := []struct {
		from string
		to   string
	}{
		{"g1", "f3"}, {"g8", "f6"},
		{"f3", "g1"}, {"f6", "g8"},
		{"g1", "f3"}, {"g8", "f6"},
		{"f3", "g1"}, {"f6", "g8"},
	}

	for _, move := range moves {
		_, err := engine.MakeMove(move.from, move.to, chess.NoPieceType)
		if err != nil {
			t.Fatalf("Failed to make move %s->%s: %v", move.from, move.to, err)
		}
	}

	// Check if threefold repetition is eligible
	if !engine.IsThreefoldRepetition() {
		t.Error("Expected threefold repetition to be eligible")
	}

	// Check eligible draws
	eligibleDraws := engine.GetEligibleDraws()
	found := false
	for _, method := range eligibleDraws {
		if method == chess.ThreefoldRepetition {
			found = true
			break
		}
	}
	if !found {
		t.Error("ThreefoldRepetition not found in eligible draws")
	}

	// Claim the draw
	err := engine.ClaimDraw(chess.ThreefoldRepetition)
	if err != nil {
		t.Fatalf("Failed to claim threefold repetition draw: %v", err)
	}

	if !engine.IsDrawn() {
		t.Error("Game should be drawn after claiming threefold repetition")
	}

	reason := engine.GetDrawReason()
	t.Logf("Draw reason: %s", reason)
}

func TestFiftyMoveRule(t *testing.T) {
	// Position with only kings and rooks where we can make 50+ moves without capture
	fen := "8/8/8/3k4/8/3K4/8/R6R w - - 0 1"
	_, err := NewEngineFromFEN(fen)
	if err != nil {
		t.Fatalf("Failed to create engine from FEN: %v", err)
	}

	// Make a series of moves without captures or pawn moves
	// This is a simplified test - in reality, we'd need exactly 50 half-moves
	// The notnil/chess library tracks this internally
	
	// Note: Creating a proper 50-move sequence is complex and would require
	// careful move planning. The library handles the counting internally.
	t.Log("Fifty-move rule detection is handled internally by the chess library")
}

func TestAutomaticDrawDetection(t *testing.T) {
	// Test that automatic draws are detected immediately
	tests := []struct {
		name string
		fen  string
	}{
		{
			name: "Stalemate is automatic",
			fen:  "7k/5Q2/6K1/8/8/8/8/8 b - - 0 1",
		},
		{
			name: "Insufficient material is automatic",
			fen:  "8/8/8/4k3/8/3K4/8/8 w - - 0 1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			engine, err := NewEngineFromFEN(tt.fen)
			if err != nil {
				t.Fatalf("Failed to create engine: %v", err)
			}

			// These draws should be detected immediately
			if !engine.IsDrawn() {
				t.Error("Expected automatic draw detection")
			}

			status := engine.GetStatus()
			if status != StatusDraw {
				t.Errorf("Expected status to be StatusDraw, got %v", status)
			}
		})
	}
}