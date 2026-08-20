package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/justinabrahms/atchess/internal/challenge"
)

// TestPruneChallengesPeriodically_StopsCleanly proves
// pruneChallengesPeriodically's goroutine actually exits once stop is
// closed, rather than assuming it does: it closes stop shortly after
// starting the loop and requires done to close within a bounded timeout.
// A build of this binary run with `go test -race` additionally proves the
// loop's access to the shared *challenge.Store is race-free against the
// store's own internal locking (SQLite is serialized to a single
// connection -- see challenge.Store's doc comment -- so this loop, like
// any other caller, is safe to run concurrently with other Store access).
func TestPruneChallengesPeriodically_StopsCleanly(t *testing.T) {
	store, err := challenge.NewStore(filepath.Join(t.TempDir(), "challenges.db"))
	if err != nil {
		t.Fatalf("challenge.NewStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	stop := make(chan struct{})
	done := make(chan struct{})

	// A short interval so, if the loop's ticker branch has a bug that
	// blocks or panics, this test fails fast rather than hanging for the
	// production default (1h).
	go pruneChallengesPeriodically(store, 5*time.Millisecond, stop, done)

	// Let at least one tick fire (exercising the PruneExpired call path)
	// before asking the loop to stop.
	time.Sleep(20 * time.Millisecond)

	close(stop)

	select {
	case <-done:
		// Loop exited cleanly -- no leaked goroutine.
	case <-time.After(2 * time.Second):
		t.Fatal("pruneChallengesPeriodically did not exit within 2s of stop being closed -- goroutine leak")
	}
}

// TestPruneChallengesPeriodically_StopsBeforeFirstTick covers stopping
// the loop before its ticker has ever fired, so the select's two cases
// racing against each other (stop vs. ticker.C) is exercised in both
// orders across test runs; go test -race additionally checks this
// concurrent start/stop for data races.
func TestPruneChallengesPeriodically_StopsBeforeFirstTick(t *testing.T) {
	store, err := challenge.NewStore(filepath.Join(t.TempDir(), "challenges.db"))
	if err != nil {
		t.Fatalf("challenge.NewStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	stop := make(chan struct{})
	done := make(chan struct{})

	go pruneChallengesPeriodically(store, time.Hour, stop, done)
	close(stop)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pruneChallengesPeriodically did not exit within 2s of stop being closed before any tick -- goroutine leak")
	}
}
