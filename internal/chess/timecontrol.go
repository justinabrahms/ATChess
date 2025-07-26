package chess

import (
	"fmt"
	"time"
)

// TimeControl represents the time control settings for a game
type TimeControl struct {
	Type        string `json:"type"`        // "correspondence", "rapid", "blitz"
	DaysPerMove int    `json:"daysPerMove"` // For correspondence games
	Initial     int    `json:"initial"`     // Initial time in seconds (future use)
	Increment   int    `json:"increment"`   // Increment per move in seconds (future use)
}

// TimeViolation represents a time control violation
type TimeViolation struct {
	PlayerDID    string    `json:"playerDid"`
	GameID       string    `json:"gameId"`
	LastMoveAt   time.Time `json:"lastMoveAt"`
	DeadlineAt   time.Time `json:"deadlineAt"`
	ViolationType string   `json:"violationType"` // "timeout", "abandoned"
}

// TimeControlService manages time control enforcement
type TimeControlService struct {
	// In a real implementation, this would have a database connection
	// For now, we'll use in-memory tracking
	gameTimeControls map[string]TimeControl
	lastMoves        map[string]map[string]time.Time // gameID -> playerDID -> lastMoveTime
}

// NewTimeControlService creates a new time control service
func NewTimeControlService() *TimeControlService {
	return &TimeControlService{
		gameTimeControls: make(map[string]TimeControl),
		lastMoves:        make(map[string]map[string]time.Time),
	}
}

// SetGameTimeControl sets the time control for a game
func (s *TimeControlService) SetGameTimeControl(gameID string, tc TimeControl) {
	s.gameTimeControls[gameID] = tc
	if s.lastMoves[gameID] == nil {
		s.lastMoves[gameID] = make(map[string]time.Time)
	}
}

// RecordMove records when a player made a move
func (s *TimeControlService) RecordMove(gameID, playerDID string, moveTime time.Time) {
	if s.lastMoves[gameID] == nil {
		s.lastMoves[gameID] = make(map[string]time.Time)
	}
	s.lastMoves[gameID][playerDID] = moveTime
}

// CheckTimeViolation checks if a player has violated time control
func (s *TimeControlService) CheckTimeViolation(gameID, playerDID string, currentTime time.Time) (*TimeViolation, error) {
	tc, ok := s.gameTimeControls[gameID]
	if !ok {
		return nil, fmt.Errorf("no time control set for game %s", gameID)
	}

	// Only check correspondence games for now
	if tc.Type != "correspondence" || tc.DaysPerMove <= 0 {
		return nil, nil
	}

	// Get last move time for the player whose turn it is
	lastMoveTime, ok := s.lastMoves[gameID][playerDID]
	if !ok {
		// No moves recorded yet, use game start time
		// In a real implementation, we'd get this from the game record
		return nil, nil
	}

	// Calculate deadline
	deadline := lastMoveTime.Add(time.Duration(tc.DaysPerMove) * 24 * time.Hour)
	
	// Check if deadline has passed
	if currentTime.After(deadline) {
		return &TimeViolation{
			PlayerDID:     playerDID,
			GameID:        gameID,
			LastMoveAt:    lastMoveTime,
			DeadlineAt:    deadline,
			ViolationType: "timeout",
		}, nil
	}

	return nil, nil
}

// GetTimeRemaining returns the time remaining for a player to move
func (s *TimeControlService) GetTimeRemaining(gameID, playerDID string, currentTime time.Time) (time.Duration, error) {
	tc, ok := s.gameTimeControls[gameID]
	if !ok {
		return 0, fmt.Errorf("no time control set for game %s", gameID)
	}

	if tc.Type != "correspondence" || tc.DaysPerMove <= 0 {
		return 0, fmt.Errorf("time control not applicable for game type %s", tc.Type)
	}

	lastMoveTime, ok := s.lastMoves[gameID][playerDID]
	if !ok {
		// No moves yet, full time available
		return time.Duration(tc.DaysPerMove) * 24 * time.Hour, nil
	}

	deadline := lastMoveTime.Add(time.Duration(tc.DaysPerMove) * 24 * time.Hour)
	remaining := deadline.Sub(currentTime)
	
	if remaining < 0 {
		return 0, nil
	}
	
	return remaining, nil
}

// CheckAbandonment checks if a game has been abandoned (no moves from either player)
func (s *TimeControlService) CheckAbandonment(gameID string, currentTime time.Time) (*TimeViolation, error) {
	tc, ok := s.gameTimeControls[gameID]
	if !ok {
		return nil, fmt.Errorf("no time control set for game %s", gameID)
	}

	if tc.Type != "correspondence" || tc.DaysPerMove <= 0 {
		return nil, nil
	}

	// Check last move from any player
	var lastMoveTime time.Time
	var lastPlayer string
	
	for playerDID, moveTime := range s.lastMoves[gameID] {
		if moveTime.After(lastMoveTime) {
			lastMoveTime = moveTime
			lastPlayer = playerDID
		}
	}

	if lastMoveTime.IsZero() {
		// No moves recorded yet
		return nil, nil
	}

	// Check if it's been more than 3x the time control since last move
	abandonmentThreshold := time.Duration(tc.DaysPerMove*3) * 24 * time.Hour
	if currentTime.Sub(lastMoveTime) > abandonmentThreshold {
		return &TimeViolation{
			PlayerDID:     lastPlayer,
			GameID:        gameID,
			LastMoveAt:    lastMoveTime,
			DeadlineAt:    lastMoveTime.Add(abandonmentThreshold),
			ViolationType: "abandoned",
		}, nil
	}

	return nil, nil
}

// FormatTimeRemaining formats time remaining in a human-readable way
func FormatTimeRemaining(remaining time.Duration) string {
	if remaining <= 0 {
		return "Time expired"
	}

	days := int(remaining.Hours() / 24)
	hours := int(remaining.Hours()) % 24
	minutes := int(remaining.Minutes()) % 60

	if days > 0 {
		if hours > 0 {
			return fmt.Sprintf("%d days, %d hours", days, hours)
		}
		return fmt.Sprintf("%d days", days)
	}
	
	if hours > 0 {
		if minutes > 0 {
			return fmt.Sprintf("%d hours, %d minutes", hours, minutes)
		}
		return fmt.Sprintf("%d hours", hours)
	}
	
	return fmt.Sprintf("%d minutes", minutes)
}