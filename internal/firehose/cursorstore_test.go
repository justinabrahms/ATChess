package firehose

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"
)

const testHost = "wss://example.test/xrpc/com.atproto.sync.subscribeRepos"

func TestCursorStore_NoPriorFile_FirstRun(t *testing.T) {
	dir := t.TempDir()

	store, err := NewCursorStore(dir, zerolog.Nop())
	if err != nil {
		t.Fatalf("NewCursorStore on a fresh (empty) dir returned an error: %v", err)
	}

	if seq, ok := store.Get(testHost); ok {
		t.Fatalf("expected no stored cursor on first run, got seq=%d ok=%v", seq, ok)
	}
}

func TestCursorStore_StoreAndReload_Resumes(t *testing.T) {
	dir := t.TempDir()

	store, err := NewCursorStore(dir, zerolog.Nop())
	if err != nil {
		t.Fatalf("NewCursorStore failed: %v", err)
	}

	if err := store.Store(testHost, 12345); err != nil {
		t.Fatalf("Store failed: %v", err)
	}

	// Simulate a process restart: construct a brand new CursorStore against
	// the same state dir and confirm it resumes from the persisted value.
	reloaded, err := NewCursorStore(dir, zerolog.Nop())
	if err != nil {
		t.Fatalf("NewCursorStore (reload) failed: %v", err)
	}

	seq, ok := reloaded.Get(testHost)
	if !ok {
		t.Fatalf("expected a stored cursor to survive reload, found none")
	}
	if seq != 12345 {
		t.Errorf("reloaded cursor = %d, want 12345", seq)
	}
}

func TestCursorStore_CorruptFile_DoesNotCrashAndStartsEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, cursorStoreFileName)

	if err := os.WriteFile(path, []byte("{not valid json"), 0o644); err != nil {
		t.Fatalf("failed to seed corrupt cursor file: %v", err)
	}

	store, err := NewCursorStore(dir, zerolog.Nop())
	if err != nil {
		t.Fatalf("NewCursorStore returned an error for a corrupt file; want graceful recovery, got: %v", err)
	}

	if seq, ok := store.Get(testHost); ok {
		t.Fatalf("expected a corrupt file to be treated as no-stored-cursor, got seq=%d ok=%v", seq, ok)
	}

	// The corrupt file must be preserved (not silently deleted/overwritten)
	// for post-mortem inspection.
	corruptPath := path + ".corrupt"
	if _, err := os.Stat(corruptPath); err != nil {
		t.Errorf("expected the original corrupt file to be preserved at %s, but it is missing: %v", corruptPath, err)
	}

	// The store must still be usable (and able to write a fresh, valid
	// file) after recovering from corruption.
	if err := store.Store(testHost, 99); err != nil {
		t.Fatalf("Store after corrupt-file recovery failed: %v", err)
	}
	reloaded, err := NewCursorStore(dir, zerolog.Nop())
	if err != nil {
		t.Fatalf("NewCursorStore after writing a fresh file failed: %v", err)
	}
	if seq, ok := reloaded.Get(testHost); !ok || seq != 99 {
		t.Errorf("expected the freshly written cursor to reload correctly, got seq=%d ok=%v", seq, ok)
	}
}

func TestCursorStore_NegativeSequence_ClearsStoredCursor(t *testing.T) {
	dir := t.TempDir()

	store, err := NewCursorStore(dir, zerolog.Nop())
	if err != nil {
		t.Fatalf("NewCursorStore failed: %v", err)
	}

	if err := store.Store(testHost, 500); err != nil {
		t.Fatalf("Store failed: %v", err)
	}
	if seq, ok := store.Get(testHost); !ok || seq != 500 {
		t.Fatalf("precondition failed: expected stored cursor 500, got seq=%d ok=%v", seq, ok)
	}

	// This is the path a host's rejection of our stored cursor as
	// too-old/invalid takes: the firehose client resets its in-memory
	// LastSequence to -1, and the periodic persistence loop in
	// cmd/protocol/main.go stores that -1, which must clear the entry
	// rather than writing a nonsensical negative sequence to disk.
	if err := store.Store(testHost, -1); err != nil {
		t.Fatalf("Store(-1) failed: %v", err)
	}
	if seq, ok := store.Get(testHost); ok {
		t.Errorf("expected storing a negative sequence to clear the cursor, got seq=%d ok=%v", seq, ok)
	}

	reloaded, err := NewCursorStore(dir, zerolog.Nop())
	if err != nil {
		t.Fatalf("NewCursorStore (reload) failed: %v", err)
	}
	if seq, ok := reloaded.Get(testHost); ok {
		t.Errorf("expected the cleared cursor to stay cleared across reload, got seq=%d ok=%v", seq, ok)
	}
}

func TestCursorStore_MultipleHosts_TrackedIndependently(t *testing.T) {
	dir := t.TempDir()
	store, err := NewCursorStore(dir, zerolog.Nop())
	if err != nil {
		t.Fatalf("NewCursorStore failed: %v", err)
	}

	hostA := "wss://a.test/xrpc/com.atproto.sync.subscribeRepos"
	hostB := "wss://b.test/xrpc/com.atproto.sync.subscribeRepos"

	if err := store.Store(hostA, 10); err != nil {
		t.Fatalf("Store(hostA) failed: %v", err)
	}
	if err := store.Store(hostB, 20); err != nil {
		t.Fatalf("Store(hostB) failed: %v", err)
	}

	if seq, ok := store.Get(hostA); !ok || seq != 10 {
		t.Errorf("hostA cursor = %d (ok=%v), want 10", seq, ok)
	}
	if seq, ok := store.Get(hostB); !ok || seq != 20 {
		t.Errorf("hostB cursor = %d (ok=%v), want 20", seq, ok)
	}
}
