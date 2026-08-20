package challenge

import (
	"fmt"
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

// Store is a per-process, in-memory CACHE of pending challenges, keyed by
// challenged DID -- it is deliberately not the source of truth (atchess-1c9.11).
// The source of truth is always each app.atchess.challenge record in its
// author's (the challenger's) own AT Protocol repo; a challenge can never be
// written into the challenged player's repo (AT Protocol does not permit
// writing into a repo that isn't your own). Store exists purely so
// GET /api/challenge-notifications does not have to re-derive "which
// challenges are addressed to me" from scratch on every request: it is
// populated by (1) internal/firehose.EventProcessor as
// app.atchess.challenge commits arrive live, subscribed directly against
// every watched PDS, with cursor-based resumption persisted across
// restarts (see internal/firehose.CursorStore and cmd/protocol/main.go,
// atchess-1c9.46), and (2) internal/backfill's login-time repo-read
// backfill, run synchronously on every login (see
// internal/web.LoginHandler/OAuthCallbackHandler), so challenges issued
// while this process -- or this player's session -- was not around are
// still discovered without depending on a full-log firehose replay. See
// docs/firehose-and-backfill.md for the full picture, including what the
// login backfill can and cannot find. Because this Store is only a cache,
// losing it (e.g. a process restart) is not data loss: the live
// subscription and the next login's backfill both rebuild it from the same
// repo records.
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

// FromChallengeRecord builds a *PendingChallenge from an app.atchess.challenge
// record's decoded fields (as delivered by internal/firehose, whether live or
// during a backfill resubscribe), plus the repo DID, record key, and CID the
// commit that carried it identified. It is the single place that maps the
// wire record shape to PendingChallenge, shared by the live and backfill
// paths so they cannot drift out of sync with each other.
//
// A missing/unparsable createdAt defaults to now; a missing/unparsable
// expiresAt defaults to 24h after createdAt (matching CreateChallenge's own
// default expiry, internal/atproto/client.go).
func FromChallengeRecord(repoDID, rkey, cid string, record map[string]interface{}) *PendingChallenge {
	challengedDID, _ := record["challenged"].(string)
	challengerDID, _ := record["challenger"].(string)
	challengerHandle, _ := record["challengerHandle"].(string)
	color, _ := record["color"].(string)
	message, _ := record["message"].(string)
	proposedGameID, _ := record["proposedGameId"].(string)

	createdAt := time.Now()
	if ts, ok := record["createdAt"].(string); ok {
		if parsed, err := time.Parse(time.RFC3339, ts); err == nil {
			createdAt = parsed
		}
	}
	expiresAt := createdAt.Add(24 * time.Hour)
	if ts, ok := record["expiresAt"].(string); ok {
		if parsed, err := time.Parse(time.RFC3339, ts); err == nil {
			expiresAt = parsed
		}
	}

	return &PendingChallenge{
		ChallengeURI:     fmt.Sprintf("at://%s/app.atchess.challenge/%s", repoDID, rkey),
		ChallengeCID:     cid,
		ChallengerDID:    challengerDID,
		ChallengerHandle: challengerHandle,
		ChallengedDID:    challengedDID,
		Color:            color,
		Message:          message,
		ProposedGameID:   proposedGameID,
		CreatedAt:        createdAt,
		ExpiresAt:        expiresAt,
	}
}
