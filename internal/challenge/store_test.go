package challenge

import (
	"testing"
	"time"
)

func TestStoreAddAndForPlayer(t *testing.T) {
	s := NewStore()

	c := &PendingChallenge{
		ChallengeURI:  "at://did:plc:alice/app.atchess.challenge/abc",
		ChallengerDID: "did:plc:alice",
		ChallengedDID: "did:plc:bob",
		CreatedAt:     time.Now(),
		ExpiresAt:     time.Now().Add(24 * time.Hour),
	}

	if !s.Add(c) {
		t.Fatal("expected Add to return true for new challenge")
	}
	if s.Add(c) {
		t.Fatal("expected Add to return false for duplicate challenge")
	}

	got := s.ForPlayer("did:plc:bob")
	if len(got) != 1 {
		t.Fatalf("expected 1 challenge, got %d", len(got))
	}
	if got[0].ChallengeURI != c.ChallengeURI {
		t.Errorf("wrong URI: %s", got[0].ChallengeURI)
	}

	got = s.ForPlayer("did:plc:alice")
	if len(got) != 0 {
		t.Fatalf("expected 0 challenges for alice, got %d", len(got))
	}
}

func TestStoreExpiration(t *testing.T) {
	s := NewStore()

	expired := &PendingChallenge{
		ChallengeURI:  "at://did:plc:alice/app.atchess.challenge/old",
		ChallengerDID: "did:plc:alice",
		ChallengedDID: "did:plc:bob",
		CreatedAt:     time.Now().Add(-48 * time.Hour),
		ExpiresAt:     time.Now().Add(-1 * time.Hour),
	}
	valid := &PendingChallenge{
		ChallengeURI:  "at://did:plc:alice/app.atchess.challenge/new",
		ChallengerDID: "did:plc:alice",
		ChallengedDID: "did:plc:bob",
		CreatedAt:     time.Now(),
		ExpiresAt:     time.Now().Add(24 * time.Hour),
	}

	s.Add(expired)
	s.Add(valid)

	// ForPlayer should only return non-expired
	got := s.ForPlayer("did:plc:bob")
	if len(got) != 1 {
		t.Fatalf("expected 1 non-expired challenge, got %d", len(got))
	}

	// PruneExpired should clean up
	pruned := s.PruneExpired()
	if pruned != 1 {
		t.Fatalf("expected 1 pruned, got %d", pruned)
	}
}

func TestStoreRemove(t *testing.T) {
	s := NewStore()

	c := &PendingChallenge{
		ChallengeURI:  "at://did:plc:alice/app.atchess.challenge/abc",
		ChallengerDID: "did:plc:alice",
		ChallengedDID: "did:plc:bob",
		CreatedAt:     time.Now(),
		ExpiresAt:     time.Now().Add(24 * time.Hour),
	}
	s.Add(c)
	s.Remove(c.ChallengeURI)

	got := s.ForPlayer("did:plc:bob")
	if len(got) != 0 {
		t.Fatalf("expected 0 after remove, got %d", len(got))
	}

	// Re-adding after remove should work
	if !s.Add(c) {
		t.Fatal("expected Add to succeed after Remove")
	}
}

func TestFromChallengeRecord(t *testing.T) {
	record := map[string]interface{}{
		"challenged":       "did:plc:bob",
		"challenger":       "did:plc:alice",
		"challengerHandle": "alice.test",
		"color":            "white",
		"message":          "gg",
		"proposedGameId":   "game123",
		"createdAt":        "2026-01-01T00:00:00Z",
		"expiresAt":        "2026-01-02T00:00:00Z",
	}

	pc := FromChallengeRecord("did:plc:alice", "abc123", "bafycid", record)

	if pc.ChallengeURI != "at://did:plc:alice/app.atchess.challenge/abc123" {
		t.Errorf("unexpected ChallengeURI: %s", pc.ChallengeURI)
	}
	if pc.ChallengeCID != "bafycid" {
		t.Errorf("unexpected ChallengeCID: %s", pc.ChallengeCID)
	}
	if pc.ChallengerDID != "did:plc:alice" || pc.ChallengedDID != "did:plc:bob" {
		t.Errorf("unexpected DIDs: challenger=%s challenged=%s", pc.ChallengerDID, pc.ChallengedDID)
	}
	if pc.ChallengerHandle != "alice.test" {
		t.Errorf("unexpected ChallengerHandle: %s", pc.ChallengerHandle)
	}
	if pc.Color != "white" || pc.Message != "gg" || pc.ProposedGameID != "game123" {
		t.Errorf("unexpected fields: color=%s message=%s proposedGameID=%s", pc.Color, pc.Message, pc.ProposedGameID)
	}
	if pc.CreatedAt.Format(time.RFC3339) != "2026-01-01T00:00:00Z" {
		t.Errorf("unexpected CreatedAt: %v", pc.CreatedAt)
	}
	if pc.ExpiresAt.Format(time.RFC3339) != "2026-01-02T00:00:00Z" {
		t.Errorf("unexpected ExpiresAt: %v", pc.ExpiresAt)
	}
}

func TestFromChallengeRecord_DefaultsWhenTimestampsMissing(t *testing.T) {
	record := map[string]interface{}{
		"challenged": "did:plc:bob",
		"challenger": "did:plc:alice",
	}

	before := time.Now()
	pc := FromChallengeRecord("did:plc:alice", "abc123", "bafycid", record)
	after := time.Now()

	if pc.CreatedAt.Before(before) || pc.CreatedAt.After(after) {
		t.Errorf("expected CreatedAt to default to now, got %v (window %v..%v)", pc.CreatedAt, before, after)
	}
	if !pc.ExpiresAt.Equal(pc.CreatedAt.Add(24 * time.Hour)) {
		t.Errorf("expected ExpiresAt to default to CreatedAt+24h, got created=%v expires=%v", pc.CreatedAt, pc.ExpiresAt)
	}
}
