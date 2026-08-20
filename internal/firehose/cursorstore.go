package firehose

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/rs/zerolog"
)

// cursorStoreFileName is the single file (within a caller-supplied state
// directory) CursorStore reads and writes. One file, not one-per-host,
// keeps "no prior cursor at all" (first run) and "corrupt state" trivially
// distinguishable from "some hosts recorded, others not yet".
const cursorStoreFileName = "cursors.json"

// CursorStore persists the last-processed firehose sequence number per
// watched host (keyed by the exact subscribeRepos URL passed to
// firehose.WithURL / FIREHOSE_URL) to a small JSON file under stateDir, so
// a process restart can resume from where it left off instead of either
// replaying a host's entire retained commit log (cursor 0 -- the defect
// atchess-1c9.46 exists to fix) or silently discarding history by always
// starting at the live tip on every boot.
//
// This project has no database (see CLAUDE.md); a small file is the
// natural fit for what is, worst case, "replay a handful of
// already-processed (and idempotently-deduped -- see
// challenge.Store.Add) events" if the file is lost or stale.
//
// CursorStore is safe for concurrent use.
//
// atchess-1c9.49: every stored cursor is tagged with the Transport it was
// recorded under (cursorEntry.Transport). subscribeRepos cursors are
// host-local sequence numbers; Jetstream cursors are unix microseconds
// (time_us) -- two completely different scales and meanings (see the
// Transport type's doc comment). Get refuses to hand back an entry whose
// recorded transport does not match the transport being asked for,
// falling back to "no cursor" (exactly like a first run) rather than let a
// value recorded under one transport be silently replayed as the other's
// cursor -- which would request either "the beginning of time" or "some
// point far in the future", never what was intended.
type CursorStore struct {
	path   string
	logger zerolog.Logger

	mu      sync.Mutex
	cursors map[string]cursorEntry
}

// cursorEntry is one host's persisted cursor plus the Transport it was
// recorded under. json field names are deliberately short/stable since
// this is the on-disk format (cursors.json).
type cursorEntry struct {
	Transport Transport `json:"transport"`
	Seq       int64     `json:"seq"`
}

// NewCursorStore creates stateDir if it does not already exist and loads
// any previously persisted cursors from stateDir/cursors.json.
//
// Edge cases handled explicitly (atchess-1c9.46's acceptance criteria):
//   - No file yet (first run ever, or a fresh stateDir): NOT an error;
//     returns an empty CursorStore, so every host starts with no stored
//     cursor.
//   - A file that exists but fails to parse as valid JSON (corrupt/
//     truncated, e.g. from a crash mid-write, or hand-edited): NOT fatal.
//     Logged as a warning, the corrupt file is preserved on disk under a
//     `.corrupt` suffix for post-mortem inspection (rather than silently
//     overwritten on the very next Store call, which would destroy the
//     evidence), and the store starts empty -- equivalent to "no prior
//     cursor" for every host, matching the first-run case rather than
//     crashing the process over a damaged cache file.
func NewCursorStore(stateDir string, logger zerolog.Logger) (*CursorStore, error) {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating firehose cursor state dir %s: %w", stateDir, err)
	}

	s := &CursorStore{
		path:    filepath.Join(stateDir, cursorStoreFileName),
		logger:  logger,
		cursors: make(map[string]cursorEntry),
	}

	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			// First run: no cursor file yet. Not an error.
			return s, nil
		}
		return nil, fmt.Errorf("reading firehose cursor state file %s: %w", s.path, err)
	}

	// NOTE (atchess-1c9.49): the on-disk format changed from
	// map[string]int64 to map[string]cursorEntry (transport-tagged) when
	// the Jetstream transport was added. A pre-existing cursors.json
	// written by the older, untagged format will fail this Unmarshal (a
	// bare JSON number where a {"transport","seq"} object is expected) and
	// fall into the same "corrupt/unparseable" handling below: preserved
	// as .corrupt, store starts empty. That is a one-time, graceful
	// degradation on upgrade -- equivalent to a first run, NOT a crash --
	// consistent with this function's existing corrupt-file handling, and
	// is why that handling was designed to be non-fatal in the first
	// place. Losing a persisted cursor only means resubscribing from the
	// live tip; it never causes duplicate/incorrect processing (see this
	// type's doc comment and challenge.Store.Add's dedup).
	var loaded map[string]cursorEntry
	if err := json.Unmarshal(data, &loaded); err != nil {
		corruptPath := s.path + ".corrupt"
		logger.Warn().
			Err(err).
			Str("path", s.path).
			Str("preservedAs", corruptPath).
			Msg("firehose cursor state file is corrupt/unparseable; starting with no stored cursors (equivalent to first run) rather than crashing")
		// Best-effort preservation of the bad file for debugging; failure
		// to preserve it is logged but must not block startup.
		if renameErr := os.Rename(s.path, corruptPath); renameErr != nil {
			logger.Warn().Err(renameErr).Str("path", s.path).Msg("failed to preserve corrupt firehose cursor state file")
		}
		return s, nil
	}

	s.cursors = loaded
	return s, nil
}

// Get returns the stored cursor for host, for a connection speaking
// transport, and whether one was found/usable. A missing entry (ok ==
// false) means "no cursor known for this host" -- the caller should NOT
// default that to 0 (a full-log replay); see cmd/protocol/main.go's
// bounded-initial-backfill decision.
//
// If an entry exists for host but was recorded under a DIFFERENT
// transport, Get also returns ok == false (logging a Warn explaining why)
// rather than handing back a value whose meaning does not match what the
// caller asked for -- see this type's doc comment. This is the structural
// guard atchess-1c9.49 requires: a subscribeRepos sequence number must
// never be replayed as a Jetstream time_us cursor, or vice versa.
func (s *CursorStore) Get(host string, transport Transport) (seq int64, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry, found := s.cursors[host]
	if !found {
		return 0, false
	}
	if entry.Transport != transport {
		s.logger.Warn().
			Str("host", host).
			Str("storedTransport", string(entry.Transport)).
			Str("requestedTransport", string(transport)).
			Int64("storedCursor", entry.Seq).
			Msg("firehose cursor store: stored cursor's transport does not match this connection's transport; refusing to reuse it and falling back to no-cursor (live tip) rather than risk conflating a subscribeRepos sequence number with a Jetstream time_us cursor")
		return 0, false
	}
	return entry.Seq, true
}

// Store records seq as the last-processed cursor for host under transport,
// and persists the full cursor map to disk immediately.
//
// seq < 0 (see firehose.Client.LastSequence's doc comment: -1 means "no
// cursor established/known") clears any previously stored entry for host
// instead of writing a negative number -- this is how a host that rejects
// our stored cursor as invalid (see Client's #info/error-frame handling)
// propagates that rejection to disk: the next periodic Store call after
// the client resets its in-memory cursor to -1 removes the stale value
// here too, so a subsequent restart does not immediately retry the same
// rejected cursor. This also means storing seq < 0 clears the entry
// regardless of what transport was previously recorded for host.
func (s *CursorStore) Store(host string, transport Transport, seq int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if seq < 0 {
		delete(s.cursors, host)
	} else {
		s.cursors[host] = cursorEntry{Transport: transport, Seq: seq}
	}

	return s.saveLocked()
}

// saveLocked writes s.cursors to s.path. Callers must hold s.mu.
//
// Writes via a temp file + rename in the same directory so a crash
// mid-write can never leave cursors.json truncated/corrupt (rename is
// atomic on the same filesystem) -- this is the write-side half of the
// corrupt-file handling in NewCursorStore; a store that could corrupt its
// own file on a bad shutdown would make load-time corruption handling a
// routine occurrence rather than a genuine edge case.
func (s *CursorStore) saveLocked() error {
	data, err := json.Marshal(s.cursors)
	if err != nil {
		return fmt.Errorf("marshaling firehose cursor state: %w", err)
	}

	tmpPath := s.path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		return fmt.Errorf("writing firehose cursor state temp file %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		return fmt.Errorf("renaming firehose cursor state temp file into place: %w", err)
	}
	return nil
}
