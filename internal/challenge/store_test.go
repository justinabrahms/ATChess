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
