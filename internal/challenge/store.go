package challenge

import (
	"sync"
	"time"
)

// PendingChallenge represents a challenge indexed for discovery by the challenged player.
type PendingChallenge struct {
	ChallengeURI     string    `json:"challengeUri"`
	ChallengeCID     string    `json:"challengeCid"`
	ChallengerDID    string    `json:"challengerDid"`
	ChallengerHandle string    `json:"challengerHandle"`
	ChallengedDID    string    `json:"challengedDid"`
	Color            string    `json:"color"`
	Message          string    `json:"message,omitempty"`
	ProposedGameID   string    `json:"proposedGameId,omitempty"`
	CreatedAt        time.Time `json:"createdAt"`
	ExpiresAt        time.Time `json:"expiresAt"`
}

// Store is an in-memory index of pending challenges, keyed by challenged DID.
// This solves the fundamental AT Protocol limitation where you cannot write to
// another user's repository. Instead of trying to create notification records
// in the challenged player's repo (which always fails in federation), we
// maintain a local index that both the API and firehose can populate.
type Store struct {
	mu sync.RWMutex
	// challenges indexed by challenged DID
	byChallenged map[string][]*PendingChallenge
	// dedup by challenge URI
	seen map[string]bool
}

// NewStore creates a new challenge store.
func NewStore() *Store {
	return &Store{
		byChallenged: make(map[string][]*PendingChallenge),
		seen:         make(map[string]bool),
	}
}

// Add indexes a challenge for discovery by the challenged player.
// Returns false if the challenge URI was already indexed.
func (s *Store) Add(c *PendingChallenge) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.seen[c.ChallengeURI] {
		return false
	}
	s.seen[c.ChallengeURI] = true
	s.byChallenged[c.ChallengedDID] = append(s.byChallenged[c.ChallengedDID], c)
	return true
}

// ForPlayer returns all non-expired pending challenges for the given DID.
func (s *Store) ForPlayer(challengedDID string) []*PendingChallenge {
	s.mu.RLock()
	defer s.mu.RUnlock()

	now := time.Now()
	var result []*PendingChallenge
	for _, c := range s.byChallenged[challengedDID] {
		if c.ExpiresAt.After(now) {
			result = append(result, c)
		}
	}
	return result
}

// Remove deletes a challenge by URI. Used when a challenge is accepted or declined.
func (s *Store) Remove(challengeURI string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.seen, challengeURI)
	for did, challenges := range s.byChallenged {
		for i, c := range challenges {
			if c.ChallengeURI == challengeURI {
				s.byChallenged[did] = append(challenges[:i], challenges[i+1:]...)
				return
			}
		}
	}
}

// PruneExpired removes all expired challenges from the store.
func (s *Store) PruneExpired() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	pruned := 0
	for did, challenges := range s.byChallenged {
		kept := challenges[:0]
		for _, c := range challenges {
			if c.ExpiresAt.After(now) {
				kept = append(kept, c)
			} else {
				delete(s.seen, c.ChallengeURI)
				pruned++
			}
		}
		if len(kept) == 0 {
			delete(s.byChallenged, did)
		} else {
			s.byChallenged[did] = kept
		}
	}
	return pruned
}
