package chess

import (
	"testing"
	"time"
)

func TestTimeControlService(t *testing.T) {
	service := NewTimeControlService()

	// Set up a correspondence game with 3 days per move
	gameID := "test-game-1"
	tc := TimeControl{
		Type:        "correspondence",
		DaysPerMove: 3,
	}
	service.SetGameTimeControl(gameID, tc)

	// Test initial state - no moves yet
	playerDID := "did:plc:player1"
	currentTime := time.Now()

	// Should have no violation initially
	violation, err := service.CheckTimeViolation(gameID, playerDID, currentTime)
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}
	if violation != nil {
		t.Error("Expected no violation initially")
	}

	// Record a move
	moveTime := currentTime.Add(-2 * 24 * time.Hour) // 2 days ago
	service.RecordMove(gameID, playerDID, moveTime)

	// Check time remaining - should have 1 day left
	remaining, err := service.GetTimeRemaining(gameID, playerDID, currentTime)
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}
	expectedRemaining := 24 * time.Hour
	if remaining < expectedRemaining-time.Minute || remaining > expectedRemaining+time.Minute {
		t.Errorf("Expected ~24 hours remaining, got %v", remaining)
	}

	// Check for violation - should still be none
	violation, err = service.CheckTimeViolation(gameID, playerDID, currentTime)
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}
	if violation != nil {
		t.Error("Expected no violation after 2 days")
	}

	// Check 4 days after move - should have violation
	futureTime := moveTime.Add(4 * 24 * time.Hour)
	violation, err = service.CheckTimeViolation(gameID, playerDID, futureTime)
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}
	if violation == nil {
		t.Error("Expected violation after 4 days")
	}
	if violation != nil {
		if violation.PlayerDID != playerDID {
			t.Errorf("Expected violation for player %s, got %s", playerDID, violation.PlayerDID)
		}
		if violation.ViolationType != "timeout" {
			t.Errorf("Expected timeout violation, got %s", violation.ViolationType)
		}
	}
}

func TestCheckAbandonment(t *testing.T) {
	service := NewTimeControlService()

	gameID := "abandoned-game"
	tc := TimeControl{
		Type:        "correspondence",
		DaysPerMove: 1, // 1 day per move
	}
	service.SetGameTimeControl(gameID, tc)

	// Record moves from both players
	player1 := "did:plc:player1"
	player2 := "did:plc:player2"
	
	baseTime := time.Now().Add(-10 * 24 * time.Hour) // 10 days ago
	service.RecordMove(gameID, player1, baseTime)
	service.RecordMove(gameID, player2, baseTime.Add(6*time.Hour)) // 6 hours later

	// Check abandonment after 2 days - should be none
	checkTime := baseTime.Add(2 * 24 * time.Hour)
	violation, err := service.CheckAbandonment(gameID, checkTime)
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}
	if violation != nil {
		t.Error("Expected no abandonment after 2 days")
	}

	// Check abandonment after 4 days (> 3x time control) - should be abandoned
	checkTime = baseTime.Add(4 * 24 * time.Hour)
	violation, err = service.CheckAbandonment(gameID, checkTime)
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}
	if violation == nil {
		t.Error("Expected abandonment after 4 days with 1-day time control")
	}
	if violation != nil && violation.ViolationType != "abandoned" {
		t.Errorf("Expected abandoned violation type, got %s", violation.ViolationType)
	}
}

func TestFormatTimeRemaining(t *testing.T) {
	tests := []struct {
		duration time.Duration
		expected string
	}{
		{0, "Time expired"},
		{-1 * time.Hour, "Time expired"},
		{30 * time.Minute, "30 minutes"},
		{1 * time.Hour, "1 hours"},
		{2*time.Hour + 30*time.Minute, "2 hours, 30 minutes"},
		{24 * time.Hour, "1 days"},
		{25 * time.Hour, "1 days, 1 hours"},
		{3*24*time.Hour + 6*time.Hour, "3 days, 6 hours"},
	}

	for _, tt := range tests {
		result := FormatTimeRemaining(tt.duration)
		if result != tt.expected {
			t.Errorf("FormatTimeRemaining(%v): expected %q, got %q", tt.duration, tt.expected, result)
		}
	}
}

func TestTimeControlWithNoTimeLimit(t *testing.T) {
	service := NewTimeControlService()

	// Set up a game with no time control
	gameID := "no-time-game"
	tc := TimeControl{
		Type:        "casual",
		DaysPerMove: 0, // No time limit
	}
	service.SetGameTimeControl(gameID, tc)

	playerDID := "did:plc:player1"
	currentTime := time.Now()

	// Should have no violation
	violation, err := service.CheckTimeViolation(gameID, playerDID, currentTime)
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}
	if violation != nil {
		t.Error("Expected no violation for game without time control")
	}

	// Should not be able to get time remaining
	_, err = service.GetTimeRemaining(gameID, playerDID, currentTime)
	if err == nil {
		t.Error("Expected error when getting time remaining for non-time-controlled game")
	}
}