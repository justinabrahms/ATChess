package firehose

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/justinabrahms/atchess/internal/challenge"
	"github.com/justinabrahms/atchess/internal/web"
)

func newTestChallengeStore(t *testing.T) *challenge.Store {
	t.Helper()
	s, err := challenge.NewStore(filepath.Join(t.TempDir(), "challenges.db"))
	if err != nil {
		t.Fatalf("challenge.NewStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func newTestProcessor(t *testing.T) (*EventProcessor, *challenge.Store) {
	t.Helper()
	hub := web.NewHub()
	go hub.Run()
	store := newTestChallengeStore(t)
	return NewEventProcessor(hub, store), store
}

func challengeCreateEvent(repo, rkey string) Event {
	return Event{
		Type:      EventTypeChallenge,
		Repo:      repo,
		Path:      "app.atchess.challenge/" + rkey,
		CID:       "cid-" + rkey,
		Timestamp: time.Now(),
		Action:    "create",
		Record: map[string]interface{}{
			"challenger": repo,
			"challenged": "did:plc:bob",
			"color":      "white",
			"createdAt":  time.Now().UTC().Format(time.RFC3339),
			"expiresAt":  time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339),
		},
	}
}

// TestProcessChallengeEvent_IndexesChallenge is the basic ingest path: a
// "create" op is indexed and discoverable via ForPlayer.
func TestProcessChallengeEvent_IndexesChallenge(t *testing.T) {
	p, store := newTestProcessor(t)

	event := challengeCreateEvent("did:plc:alice", "abc123")
	if err := p.ProcessEvent(context.Background(), event); err != nil {
		t.Fatalf("ProcessEvent: %v", err)
	}

	got, err := store.ForPlayer("did:plc:bob")
	if err != nil {
		t.Fatalf("ForPlayer: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 indexed challenge, got %d", len(got))
	}
	wantURI := "at://did:plc:alice/app.atchess.challenge/abc123"
	if got[0].ChallengeURI != wantURI {
		t.Errorf("ChallengeURI = %q, want %q", got[0].ChallengeURI, wantURI)
	}
}

// TestProcessChallengeEvent_ForgedChallenger_NotIndexedNotBroadcast pins
// atchess-1c9.106's fix (a) as observed through the firehose path: a
// record whose "challenger" field disagrees with event.Repo (the repo it
// actually came from -- e.g. Mallory writing a challenge into HER OWN
// repo naming Carol as "challenger") must be neither indexed into the
// durable Store nor broadcast to the challenged player.
//
// The Hub here is deliberately never started (no `go hub.Run()`):
// Hub.BroadcastToPlayer sends on an unbuffered channel that only Run's
// goroutine ever drains, so if processChallengeEvent's early refusal
// didn't ALSO skip the broadcast call below it, ProcessEvent would block
// forever on that send and this test would time out -- a structural
// proof of "not broadcast", not just an absence-of-row check.
func TestProcessChallengeEvent_ForgedChallenger_NotIndexedNotBroadcast(t *testing.T) {
	hub := web.NewHub()
	store := newTestChallengeStore(t)
	p := NewEventProcessor(hub, store)

	event := Event{
		Type:      EventTypeChallenge,
		Repo:      "did:plc:mallory",
		Path:      "app.atchess.challenge/forged1",
		CID:       "cid-forged1",
		Timestamp: time.Now(),
		Action:    "create",
		Record: map[string]interface{}{
			"challenger":     "did:plc:carol",
			"challenged":     "did:plc:bob",
			"color":          "white",
			"proposedGameId": "forgedgame",
			"createdAt":      time.Now().UTC().Format(time.RFC3339),
			"expiresAt":      time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339),
		},
	}

	done := make(chan error, 1)
	go func() { done <- p.ProcessEvent(context.Background(), event) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ProcessEvent: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ProcessEvent did not return -- it appears to have blocked broadcasting a forged challenge that should have been refused before ever reaching Hub.BroadcastToPlayer")
	}

	got, err := store.ForPlayer("did:plc:bob")
	if err != nil {
		t.Fatalf("ForPlayer: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected the forged challenge to NOT be indexed, got %d entries: %#v", len(got), got)
	}
}

// TestProcessChallengeEvent_IdempotentReplay proves replaying the exact
// same firehose event (e.g. after a cursor reset, or an at-least-once
// redelivery) does not produce a duplicate entry -- required for
// cmd/protocol/main.go's periodic (not per-message) cursor persistence to
// be safe (see that file's comment on challenge.Store.Add's dedup).
func TestProcessChallengeEvent_IdempotentReplay(t *testing.T) {
	p, store := newTestProcessor(t)

	event := challengeCreateEvent("did:plc:alice", "abc123")
	for i := 0; i < 3; i++ {
		if err := p.ProcessEvent(context.Background(), event); err != nil {
			t.Fatalf("ProcessEvent (replay %d): %v", i, err)
		}
	}

	got, err := store.ForPlayer("did:plc:bob")
	if err != nil {
		t.Fatalf("ForPlayer: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 indexed challenge after 3 replays, got %d", len(got))
	}
}

// TestProcessChallengeEvent_DeleteMarksRemoved covers the delete/tombstone
// case: a "delete" op for a challenge already indexed must remove it from
// ForPlayer's results, and a later out-of-order replay of the original
// "create" must not resurrect it.
func TestProcessChallengeEvent_DeleteMarksRemoved(t *testing.T) {
	p, store := newTestProcessor(t)

	createEvent := challengeCreateEvent("did:plc:alice", "abc123")
	if err := p.ProcessEvent(context.Background(), createEvent); err != nil {
		t.Fatalf("ProcessEvent (create): %v", err)
	}

	deleteEvent := Event{
		Type:      EventTypeChallenge,
		Repo:      "did:plc:alice",
		Path:      "app.atchess.challenge/abc123",
		Timestamp: time.Now(),
		Action:    "delete",
		Record:    nil,
	}
	if err := p.ProcessEvent(context.Background(), deleteEvent); err != nil {
		t.Fatalf("ProcessEvent (delete): %v", err)
	}

	got, err := store.ForPlayer("did:plc:bob")
	if err != nil {
		t.Fatalf("ForPlayer: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected 0 challenges after delete op, got %d", len(got))
	}

	// Out-of-order replay of the original create must not resurrect it.
	if err := p.ProcessEvent(context.Background(), createEvent); err != nil {
		t.Fatalf("ProcessEvent (create replay after delete): %v", err)
	}
	got, err = store.ForPlayer("did:plc:bob")
	if err != nil {
		t.Fatalf("ForPlayer after replay: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected removed challenge to stay removed after a replayed create, got %d", len(got))
	}
}

// TestProcessChallengeEvent_DeleteBeforeCreate covers observing a delete
// for a challenge this process never saw created (e.g. it started
// watching partway through the challenge's lifetime): it must not error,
// and a later create for the same URI must still not resurrect it.
func TestProcessChallengeEvent_DeleteBeforeCreate(t *testing.T) {
	p, store := newTestProcessor(t)

	deleteEvent := Event{
		Type:      EventTypeChallenge,
		Repo:      "did:plc:alice",
		Path:      "app.atchess.challenge/never-created",
		Timestamp: time.Now(),
		Action:    "delete",
	}
	if err := p.ProcessEvent(context.Background(), deleteEvent); err != nil {
		t.Fatalf("ProcessEvent (delete before create): %v", err)
	}

	createEvent := challengeCreateEvent("did:plc:alice", "never-created")
	if err := p.ProcessEvent(context.Background(), createEvent); err != nil {
		t.Fatalf("ProcessEvent (late create): %v", err)
	}

	got, err := store.ForPlayer("did:plc:bob")
	if err != nil {
		t.Fatalf("ForPlayer: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected the late create for an already-deleted URI to stay excluded, got %d", len(got))
	}
}
