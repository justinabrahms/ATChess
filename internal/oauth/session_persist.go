package oauth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// session_persist.go — sessions that survive a restart.
//
// WHY. Sessions lived in a map and nowhere else, so every restart of the
// protocol service silently invalidated every logged-in user. Reported
// 2026-08-30 as "when I refresh, I'm logged out", during an afternoon in which
// the service was deployed five times.
//
// That matters far more for this project than it would for most: the entire
// point of the pipeline this repository is a workload for is that deploys
// happen unattended and often. A deploy that logs out every player is a defect
// that gets worse exactly as the thing works better.
//
// It was also invisible until recently. Before the UI cleared its stored
// session on a 401, a user whose session had evaporated stayed "logged in"
// looking at a page where nothing worked — which is how this survived: the
// symptom was the fix making an old bug legible, not a new bug.
//
// WHAT IS IN THE FILE. Refresh tokens and DPoP private keys. It is written
// 0600 and belongs on the same disk the service already treats as private
// state; there is no encryption at rest here, and anyone who can read this file
// can act as any logged-in user, exactly as they could by reading the process's
// memory.

// persistedSessions is the on-disk shape: session id -> session.
type persistedSessions map[string]*Session

// EnablePersistence points the store at a file, loading whatever is already
// there. Call once, before the store serves any request.
//
// A load failure is logged by the caller and otherwise ignored: starting with
// an empty store logs everyone out, which is bad, but refusing to start is
// worse, and a corrupt session file must never be able to keep the service down.
func (s *SessionStore) EnablePersistence(path string) error {
	if path == "" {
		return fmt.Errorf("session persistence needs a path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("creating session directory: %w", err)
	}

	s.mu.Lock()
	s.persistPath = path
	s.mu.Unlock()

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // first run
		}
		return fmt.Errorf("reading sessions from %s: %w", path, err)
	}
	var loaded persistedSessions
	if err := json.Unmarshal(data, &loaded); err != nil {
		return fmt.Errorf("parsing sessions in %s: %w", path, err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for id, sess := range loaded {
		if sess != nil {
			s.sessions[id] = sess
		}
	}
	return nil
}

// persistLocked writes the whole session map. Caller must hold s.mu.
//
// Whole-file rather than incremental because the set is small (one entry per
// logged-in person) and a partial write is the failure mode that would lose
// sessions rather than merely fail to save one. Written to a temporary file and
// renamed so a crash mid-write cannot leave a truncated file that then fails to
// parse at startup — which would log everyone out, the exact thing this exists
// to prevent.
func (s *SessionStore) persistLocked() {
	if s.persistPath == "" {
		return
	}
	data, err := json.Marshal(persistedSessions(s.sessions))
	if err != nil {
		return
	}
	dir := filepath.Dir(s.persistPath)
	tmp, err := os.CreateTemp(dir, ".sessions-*")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op once renamed

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return
	}
	// 0600 before the rename: the file holds refresh tokens and DPoP private
	// keys, and CreateTemp's 0600 is already right — this is belt and braces
	// against a future change to the temp-file call.
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return
	}
	if err := tmp.Close(); err != nil {
		return
	}
	_ = os.Rename(tmpName, s.persistPath)
}

// persistMu serializes writers that are not already under s.mu.
var persistMu sync.Mutex
