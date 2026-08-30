package challenge

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/rs/zerolog/log"
	_ "modernc.org/sqlite"
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

// Challenge record status values, persisted in the "status" column.
//
// statusOpen is the only status ForPlayer ever returns: a challenge that
// has never been responded to (or removed) and is still discoverable by
// the challenged player.
//
// statusDeclined is set by Remove (the challenged player explicitly
// declined -- see internal/web.Service.DeclineChallengeHandler). It is
// deliberately DISTINCT from a row simply not existing: replaying the
// same "create" firehose event, or re-running the login backfill, after a
// decline MUST NOT resurrect the challenge as open again (atchess-1c9.47's
// regression, which this status column exists specifically to prevent --
// see Add's ON CONFLICT DO NOTHING, which never overwrites an existing
// row's status). A declined challenge's row is kept, not deleted, so this
// distinction survives a restart.
//
// statusRemoved is set when the challenge record itself was deleted from
// the challenger's repo (a firehose "delete" op on app.atchess.challenge --
// see internal/firehose.EventProcessor.processChallengeEvent). Modeled
// separately from statusDeclined because the two are different real-world
// events (the challenger withdrew it vs. the challenged player declined
// it), even though both currently have the same observable effect on
// ForPlayer (excluded).
//
// statusAccepted is set by MarkAccepted once a challenge has been
// accepted (atchess-1c9.29: internal/web.Service.AcceptChallengeHandler,
// after atproto.Client.AcceptChallenge succeeds) -- the same treatment as
// statusDeclined/statusRemoved: a tombstone that survives a restart and
// can never be resurrected by a later out-of-order replay of the original
// "create" (Add's ON CONFLICT DO NOTHING).
const (
	statusOpen     = "open"
	statusDeclined = "declined"
	statusRemoved  = "removed"
	statusAccepted = "accepted"
)

// Store is a durable, SQLite-backed index of pending challenges, keyed by
// challenged DID -- it is the AppView this package implements for
// atchess-1c9.50: a challenge record only ever lives in its CHALLENGER's
// own AT Protocol repo (AT Protocol never permits writing into a repo that
// isn't your own), and there is no query anywhere in the AT Protocol
// network that answers "which repos contain a challenge addressed to me"
// directly. Store is what makes that answerable in constant time,
// independent of how long this process was down: it is populated by (1)
// internal/firehose.EventProcessor as app.atchess.challenge commits arrive
// live, subscribed directly against every watched PDS, with cursor-based
// resumption persisted across restarts (see internal/firehose.CursorStore
// and cmd/protocol/main.go, atchess-1c9.46), and (2) internal/backfill's
// login-time repo-read backfill, run synchronously on every login (see
// internal/web.LoginHandler/OAuthCallbackHandler), so challenges issued
// while this process -- or this player's session -- was not around are
// still discovered, even past a relay's ~72h retention window (the gap
// atchess-1c9.50 exists to close). See docs/firehose-and-backfill.md for
// what remains undiscoverable even with this index in place.
//
// Unlike the earlier in-memory cache this type replaces (atchess-1c9.11 /
// .46 / .47), losing the process is NOT equivalent to losing the data: the
// backing SQLite file at the configured path (see internal/config's
// challenge.db_path, following the firehose.state_dir pattern) survives a
// restart, and Add's dedup-by-URI plus the status column together make a
// declined challenge stay declined across that restart too (see the
// status constants above).
//
// Every exported method returns an error. A durable store can fail (disk
// full, permissions, corruption, a locked file); atchess-1c9.51 already
// established the rule this package follows: a storage failure must
// surface as an error to the caller, never silently degrade into "no
// challenges for you" -- which is indistinguishable from a real empty
// result and would hide a genuine challenge from the player it is
// addressed to.
type Store struct {
	db *sql.DB
}

// NewStore opens (creating if necessary) a SQLite database at dbPath and
// ensures its schema exists. dbPath's parent directory is created if
// missing (mirroring internal/firehose.NewCursorStore's handling of
// state_dir). The returned Store owns the *sql.DB; call Close when done
// with it (notably on graceful shutdown, so the final WAL checkpoint is
// flushed to the main database file).
//
// modernc.org/sqlite is used deliberately over mattn/go-sqlite3: it is a
// pure-Go, cgo-free driver, and this project builds with CGO_ENABLED=0
// (see Makefile and .github/workflows/deploy-abrahms.yml) -- a cgo-based
// driver would break both the build and the deploy.
func NewStore(dbPath string) (*Store, error) {
	if dir := filepath.Dir(dbPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("creating challenge store directory %s: %w", dir, err)
		}
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("opening challenge store at %s: %w", dbPath, err)
	}

	// SQLite allows only one writer at a time regardless of connection
	// pooling; serializing every access through a single connection avoids
	// SQLITE_BUSY errors under concurrent goroutines (the firehose
	// processor, the login backfill, and HTTP handlers can all reach this
	// Store at once) without needing a retry loop of our own. Chess
	// challenges are rare events (see the package doc comment on the
	// target host's sizing), so serializing all access has no meaningful
	// throughput cost.
	db.SetMaxOpenConns(1)

	if _, err := db.Exec("PRAGMA journal_mode=WAL;"); err != nil {
		db.Close()
		return nil, fmt.Errorf("setting WAL journal mode on challenge store at %s: %w", dbPath, err)
	}
	if _, err := db.Exec("PRAGMA busy_timeout=5000;"); err != nil {
		db.Close()
		return nil, fmt.Errorf("setting busy_timeout on challenge store at %s: %w", dbPath, err)
	}
	if _, err := db.Exec("PRAGMA foreign_keys=ON;"); err != nil {
		db.Close()
		return nil, fmt.Errorf("enabling foreign_keys on challenge store at %s: %w", dbPath, err)
	}

	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrating challenge store at %s: %w", dbPath, err)
	}

	return &Store{db: db}, nil
}

func migrate(db *sql.DB) error {
	const schema = `
CREATE TABLE IF NOT EXISTS challenges (
	uri               TEXT PRIMARY KEY,
	cid               TEXT NOT NULL DEFAULT '',
	challenger_did    TEXT NOT NULL,
	challenger_handle TEXT NOT NULL DEFAULT '',
	challenged_did    TEXT NOT NULL,
	color             TEXT NOT NULL DEFAULT '',
	message           TEXT NOT NULL DEFAULT '',
	proposed_game_id  TEXT NOT NULL DEFAULT '',
	created_at        TEXT NOT NULL,
	expires_at        TEXT NOT NULL,
	status            TEXT NOT NULL DEFAULT 'open'
);
CREATE INDEX IF NOT EXISTS idx_challenges_challenged_did ON challenges(challenged_did);

-- games: the service's own index of games it has LEARNED ABOUT.
--
-- A game record lives in exactly one player's repository and AT Protocol has no
-- way to enumerate "all games everywhere", so a spectator listing has to be
-- built from observation. Before this table, GetActiveGamesHandler returned a
-- hardcoded empty slice with a TODO -- it was not that the query was slow, it
-- was that no query existed.
--
-- Rows are written whenever this service sees a game: on accept, on a move, and
-- whenever a player's game listing scans a repo. That makes the index eventually
-- complete for anything anyone here has touched, which is the honest scope of
-- what a single instance can know.
CREATE TABLE IF NOT EXISTS games (
	uri        TEXT PRIMARY KEY,
	white_did  TEXT NOT NULL,
	black_did  TEXT NOT NULL,
	status     TEXT NOT NULL DEFAULT '',
	fen        TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT '',
	updated_at TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_games_status ON games(status);
CREATE INDEX IF NOT EXISTS idx_games_players ON games(white_did, black_did);
`
	if _, err := db.Exec(schema); err != nil {
		return err
	}
	return nil
}

// Close closes the underlying database, flushing any pending WAL
// checkpoint to the main database file.
func (s *Store) Close() error {
	return s.db.Close()
}

// timeFormat is the on-disk timestamp encoding: RFC3339Nano, always in
// UTC. Fixed-width-per-field and always UTC so the lexical ordering SQLite
// would use for these strings (were it ever needed, e.g. in a future
// ORDER BY) matches chronological ordering -- not currently relied upon
// (expiry filtering happens in Go, see ForPlayer), but kept consistent on
// principle rather than mixing offsets.
const timeFormat = time.RFC3339Nano

// Add idempotently indexes a challenge for discovery by the challenged
// player. Returns added=true only if this call actually inserted a NEW
// row; a challenge URI already present -- regardless of its current
// status (open, declined, or removed) -- is left completely untouched
// and added is false. This is what makes replaying an already-seen
// firehose event, or re-running the login backfill, safe to do
// repeatedly: it can never duplicate a row, and it can never resurrect a
// challenge that was previously declined or removed (see the status
// constants' doc comments, and TestStore_DeclineSurvivesReplayAndRestart).
func (s *Store) Add(c *PendingChallenge) (bool, error) {
	res, err := s.db.Exec(`
		INSERT INTO challenges
			(uri, cid, challenger_did, challenger_handle, challenged_did, color, message, proposed_game_id, created_at, expires_at, status)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(uri) DO NOTHING
	`,
		c.ChallengeURI, c.ChallengeCID, c.ChallengerDID, c.ChallengerHandle, c.ChallengedDID,
		c.Color, c.Message, c.ProposedGameID,
		c.CreatedAt.UTC().Format(timeFormat), c.ExpiresAt.UTC().Format(timeFormat),
		statusOpen,
	)
	if err != nil {
		return false, fmt.Errorf("indexing challenge %s: %w", c.ChallengeURI, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("checking insert result for challenge %s: %w", c.ChallengeURI, err)
	}
	return n > 0, nil
}

// ForPlayer returns all currently-open, non-expired pending challenges
// addressed to challengedDID -- and ONLY to challengedDID; the WHERE
// clause below is the entire enforcement of the security property that a
// user must never see a challenge addressed to someone else (see
// internal/web.Service.GetChallengeNotificationsHandler, which passes the
// AUTHENTICATED caller's own DID here, and
// TestStore_ForPlayer_DoesNotLeakOtherPlayersChallenges).
//
// A query failure returns a non-nil error and a nil slice -- NEVER a nil
// error with an empty slice -- so a caller can never mistake "the store
// is broken" for "you have no challenges" (atchess-1c9.51's rule; see the
// Store doc comment).
func (s *Store) ForPlayer(challengedDID string) ([]*PendingChallenge, error) {
	rows, err := s.db.Query(`
		SELECT uri, cid, challenger_did, challenger_handle, challenged_did, color, message, proposed_game_id, created_at, expires_at
		FROM challenges
		WHERE challenged_did = ? AND status = ?
	`, challengedDID, statusOpen)
	if err != nil {
		return nil, fmt.Errorf("querying challenges for %s: %w", challengedDID, err)
	}
	defer rows.Close()

	now := time.Now()
	var result []*PendingChallenge
	for rows.Next() {
		var c PendingChallenge
		var createdAtStr, expiresAtStr string
		if err := rows.Scan(
			&c.ChallengeURI, &c.ChallengeCID, &c.ChallengerDID, &c.ChallengerHandle, &c.ChallengedDID,
			&c.Color, &c.Message, &c.ProposedGameID, &createdAtStr, &expiresAtStr,
		); err != nil {
			return nil, fmt.Errorf("reading challenge row for %s: %w", challengedDID, err)
		}
		c.CreatedAt, err = time.Parse(timeFormat, createdAtStr)
		if err != nil {
			return nil, fmt.Errorf("parsing created_at for challenge %s: %w", c.ChallengeURI, err)
		}
		c.ExpiresAt, err = time.Parse(timeFormat, expiresAtStr)
		if err != nil {
			return nil, fmt.Errorf("parsing expires_at for challenge %s: %w", c.ChallengeURI, err)
		}
		if c.ExpiresAt.After(now) {
			result = append(result, &c)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating challenges for %s: %w", challengedDID, err)
	}
	return result, nil
}

// Remove marks a challenge as declined by the challenged player (see
// internal/web.Service.DeclineChallengeHandler). The row is NOT deleted --
// its status is updated to statusDeclined so it stops appearing in
// ForPlayer while remaining durably distinguishable, across a restart,
// from a challenge that was simply never indexed (see Add's doc comment
// for why that distinction matters). A URI with no matching row is not an
// error (mirrors the prior in-memory Store's no-op-if-unknown behavior).
func (s *Store) Remove(challengeURI string) error {
	if _, err := s.db.Exec(`UPDATE challenges SET status = ? WHERE uri = ?`, statusDeclined, challengeURI); err != nil {
		return fmt.Errorf("declining challenge %s: %w", challengeURI, err)
	}
	return nil
}

// MarkAccepted marks a challenge as accepted (see
// internal/web.Service.AcceptChallengeHandler, which calls this after
// atproto.Client.AcceptChallenge succeeds). Like Remove, the row is NOT
// deleted -- its status is updated to statusAccepted so it stops
// appearing in ForPlayer while remaining durably distinguishable, across a
// restart, from a challenge that was simply never indexed, declined, or
// removed (see the status constants' doc comments). A URI with no
// matching row is not an error (mirrors Remove's same no-op-if-unknown
// behavior): the local index is a latency optimization
// (CreateChallengeHandler's doc comment), not the source of truth, so its
// absence here must never block or fail the accept it is merely trying to
// reflect.
func (s *Store) MarkAccepted(challengeURI string) error {
	if _, err := s.db.Exec(`UPDATE challenges SET status = ? WHERE uri = ?`, statusAccepted, challengeURI); err != nil {
		return fmt.Errorf("marking challenge %s accepted: %w", challengeURI, err)
	}
	return nil
}

// MarkRemoved marks a challenge as removed because its underlying
// app.atchess.challenge record was deleted from the challenger's repo
// (a firehose "delete" op -- see
// internal/firehose.EventProcessor.processChallengeEvent). Like Remove,
// this updates status rather than deleting the row, so the tombstone
// survives a restart and an out-of-order replay of an earlier "create" for
// the same URI can never resurrect it as open (Add's ON CONFLICT DO
// NOTHING leaves an existing row, of any status, untouched). A URI with no
// matching row is not an error: the delete may be observed before any
// create ever was (e.g. this instance started after the record was
// created but is still watching when it gets deleted), in which case
// there is nothing to mark -- but the row is inserted directly as
// statusRemoved so a LATER out-of-order "create" replay for the same URI
// still cannot resurrect it (Add's ON CONFLICT DO NOTHING again).
func (s *Store) MarkRemoved(challengeURI, challengerDID, challengedDID string) error {
	res, err := s.db.Exec(`UPDATE challenges SET status = ? WHERE uri = ?`, statusRemoved, challengeURI)
	if err != nil {
		return fmt.Errorf("marking challenge %s removed: %w", challengeURI, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("checking update result for challenge %s: %w", challengeURI, err)
	}
	if n > 0 {
		return nil
	}

	// No existing row: insert a tombstone directly, so an out-of-order
	// "create" for this same URI arriving later cannot resurrect it (Add's
	// ON CONFLICT DO NOTHING leaves this row alone). now() is used for
	// created_at/expires_at since neither is meaningful for a tombstone
	// with no observed record and ForPlayer already filters status !=
	// statusOpen before it would ever look at expiry.
	now := time.Now().UTC().Format(timeFormat)
	if _, err := s.db.Exec(`
		INSERT INTO challenges
			(uri, cid, challenger_did, challenger_handle, challenged_did, color, message, proposed_game_id, created_at, expires_at, status)
		VALUES (?, '', ?, '', ?, '', '', '', ?, ?, ?)
		ON CONFLICT(uri) DO NOTHING
	`, challengeURI, challengerDID, challengedDID, now, now, statusRemoved); err != nil {
		return fmt.Errorf("tombstoning challenge %s: %w", challengeURI, err)
	}
	return nil
}

// PruneExpired permanently deletes every OPEN challenge row (statusOpen
// only) whose expires_at is in the past, and returns how many rows were
// deleted. Called periodically by cmd/protocol/main.go's
// pruneChallengesPeriodically loop so the table stays bounded over the
// life of a long-running deployment: before that loop existed,
// PruneExpired had zero production callers at all (atchess-1c9.47 part
// 1), and once atchess-1c9.50 moved this store from an in-memory cache to
// a SQLite file, "nobody calls this" stopped meaning "a restart clears
// it" and started meaning "rows accumulate on disk forever".
//
// TOMBSTONE RETENTION POLICY -- read this before "simplifying" the query
// below to `DELETE FROM challenges WHERE expires_at <= ?` with no status
// filter. declined/removed rows (see the status constants' doc comments
// above) are NEVER deleted by this method, no matter how long ago their
// expires_at passed -- they are retained indefinitely. This is
// deliberate:
//
//   - A tombstone's entire reason to exist is to survive a later
//     out-of-order or replayed firehose "create" event, or a re-run login
//     backfill, for the SAME uri: Add's ON CONFLICT(uri) DO NOTHING
//     leaves any existing row -- of any status -- untouched, so a create
//     seen again after a decline/removal is a no-op. If PruneExpired ever
//     deleted that tombstone row, the next replay would go through Add's
//     INSERT path fresh and reopen the challenge as statusOpen -- this is
//     exactly the atchess-1c9.47 resurrection regression that
//     atchess-1c9.50 fixed (see TestStore_DeclineSurvivesReplayAndRestart
//     and TestStore_PruneExpired_RetainsTombstones in store_test.go).
//   - expires_at is specifically NOT a safe cutoff for a tombstone's own
//     lifetime, which is why "prune tombstones on the same schedule,
//     just later" was rejected in favor of "never, here": MarkRemoved's
//     no-prior-row path (a delete observed before any create was ever
//     seen) inserts its tombstone with expires_at set to the time of
//     tombstoning itself, not any real challenge expiry -- an
//     expires_at-based prune of tombstones would therefore be eligible to
//     delete that row on the very next run, almost immediately after it
//     was created, reopening the identical resurrection window this
//     policy exists to close.
//   - Chess challenges -- and declines/removals of them -- are rare
//     events (see the Store doc comment's sizing note); retaining only
//     this tombstoned subset of rows forever is a small, deliberate,
//     documented tradeoff against ever pruning one even slightly too
//     early. If tombstone volume ever becomes material, the correct fix
//     is a dedicated tombstoned_at column with its own (much longer)
//     grace period -- never reusing expires_at for it -- filed and done
//     separately rather than folded into this method.
func (s *Store) PruneExpired() (int, error) {
	now := time.Now().UTC().Format(timeFormat)
	res, err := s.db.Exec(`DELETE FROM challenges WHERE status = ? AND expires_at <= ?`, statusOpen, now)
	if err != nil {
		return 0, fmt.Errorf("pruning expired challenges: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("checking prune result: %w", err)
	}
	return int(n), nil
}

// BuildChallengeURI constructs the at:// URI of an app.atchess.challenge
// record from its repo and record key. The repo argument here is always
// the repo the record actually lives in (event.Repo on the firehose path,
// repoDID on the backfill path) -- NOT trusted from any field inside the
// record. FromChallengeRecord, below, is what enforces that a record's
// self-reported "challenger" field must agree with this same repo before
// the record is indexed at all; a mismatched record is refused, not
// merely mislabeled (atchess-1c9.106). Exported so internal/firehose can
// reconstruct the URI for a "delete" op, which carries no record body to
// derive it from any other way (see FromChallengeRecord, the single other
// place this same URI shape is built, for the create/update path).
func BuildChallengeURI(repoDID, rkey string) string {
	return fmt.Sprintf("at://%s/app.atchess.challenge/%s", repoDID, rkey)
}

// challengeClockSkewTolerance is how far ahead of our local clock a
// record's self-reported createdAt is allowed to be before FromChallengeRecord
// stops trusting it as the anchor for the expiresAt ceiling (see below).
// Records arrive from other people's PDSes, so a LEGITIMATE createdAt can
// sit a little ahead of our own clock due to ordinary NTP-level clock
// skew; a few minutes comfortably covers that without giving an attacker
// meaningful room to extend a forged/spam challenge's lifetime.
const challengeClockSkewTolerance = 5 * time.Minute

// FromChallengeRecord builds a *PendingChallenge from an app.atchess.challenge
// record's decoded fields (as delivered by internal/firehose, whether live or
// during a backfill resubscribe), plus the repo DID, record key, and CID the
// commit that carried it identified. It is the single place that maps the
// wire record shape to PendingChallenge, shared by the live and backfill
// paths so they cannot drift out of sync with each other.
//
// AT Protocol only permits a repo owner to write into their own repo, so
// repoDID (the repo that actually hosted this record) is the record's
// authorship proof -- its self-reported "challenger" field is not. If the
// two disagree, this is a forged challenge: someone wrote a record into
// THEIR OWN repo naming an uninvolved third party as challenger, hoping
// the challenged party's accept mints a public game crediting that third
// party as a player they never agreed to be (atchess-1c9.106). Such a
// record is refused outright -- not indexed, not broadcast -- and this
// function returns nil; callers MUST check for nil before using the
// result.
//
// A missing/unparsable createdAt defaults to now; a missing/unparsable
// expiresAt defaults to 24h after createdAt (matching CreateChallenge's own
// default expiry, internal/atproto/client.go).
//
// expiresAt is bounded to at most 24h after createdAt (atchess-1c9.107).
// Crucially, that ceiling is anchored to a BOUNDED createdAt, not the
// record's raw self-reported one: a record's createdAt is itself
// attacker-controlled (it comes from the same untrusted record as
// expiresAt), so a naive "expiresAt <= createdAt+24h" clamp does nothing
// if createdAt is ALSO claimed to be in the year 3000 -- the ceiling just
// moves out with it, and the challenge is still never pruned or filtered
// out. To close that, createdAt is itself clamped to "now" before it is
// used as the ceiling's anchor, whenever it claims to be more than
// challengeClockSkewTolerance ahead of our local clock. That tolerance
// (a few minutes) exists because these records come from other people's
// PDSes: a LEGITIMATE createdAt can sit slightly ahead of our local clock
// due to ordinary clock skew, and clamping hard to "now" would shorten
// that challenge's window by the skew amount for no attacker-related
// reason. Only a createdAt beyond that tolerance -- i.e. one that cannot
// plausibly be explained by clock skew -- is treated as untrusted and
// anchored to "now" instead. The stored CreatedAt field itself is left as
// the record's original (unbounded) claim; only the expiresAt ceiling
// computation uses the bounded anchor.
//
// An expiresAt in the past, or before createdAt, is left as-is;
// PruneExpired/ForPlayer already treat an expired row as inert, so no
// separate floor is needed.
func FromChallengeRecord(repoDID, rkey, cid string, record map[string]interface{}) *PendingChallenge {
	challengedDID, _ := record["challenged"].(string)
	challengerDID, _ := record["challenger"].(string)
	challengerHandle, _ := record["challengerHandle"].(string)
	color, _ := record["color"].(string)
	message, _ := record["message"].(string)
	proposedGameID, _ := record["proposedGameId"].(string)

	if challengerDID != repoDID {
		log.Warn().Str("repo", repoDID).Str("rkey", rkey).
			Str("claimedChallenger", challengerDID).
			Msg("refusing forged challenge: repo hosting the record is not the challenger it names")
		return nil
	}

	createdAt := time.Now()
	if ts, ok := record["createdAt"].(string); ok {
		if parsed, err := time.Parse(time.RFC3339, ts); err == nil {
			createdAt = parsed
		}
	}

	// The 24h ceiling below matches what CreateChallenge itself writes
	// (internal/atproto/client.go: expiresAt = createdAt.Add(24*time.Hour)),
	// so no legitimate challenge from our own client is ever clamped. The
	// ceiling's anchor (anchorCreatedAt) is createdAt itself EXCEPT when
	// createdAt claims to be more than challengeClockSkewTolerance ahead
	// of our local clock -- at that point createdAt is no longer
	// plausible as ordinary clock skew from a remote PDS and is treated
	// as untrusted, so the anchor falls back to "now" instead. Without
	// this, an attacker could claim a far-future createdAt AND a
	// far-future expiresAt together, and the ceiling would simply move
	// out to match: the record would still never be pruned or filtered
	// out (atchess-1c9.107).
	now := time.Now()
	anchorCreatedAt := createdAt
	if anchorCreatedAt.After(now.Add(challengeClockSkewTolerance)) {
		anchorCreatedAt = now
	}
	maxExpiresAt := anchorCreatedAt.Add(24 * time.Hour)
	expiresAt := createdAt.Add(24 * time.Hour)
	if expiresAt.After(maxExpiresAt) {
		expiresAt = maxExpiresAt
	}
	if ts, ok := record["expiresAt"].(string); ok {
		if parsed, err := time.Parse(time.RFC3339, ts); err == nil {
			expiresAt = parsed
			if expiresAt.After(maxExpiresAt) {
				expiresAt = maxExpiresAt
			}
		}
	}

	return &PendingChallenge{
		ChallengeURI:     BuildChallengeURI(repoDID, rkey),
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

// IndexedGame is one row of the service's own game index.
type IndexedGame struct {
	URI       string
	WhiteDID  string
	BlackDID  string
	Status    string
	FEN       string
	CreatedAt string
	UpdatedAt string
}

// RecordGame upserts what this service currently knows about a game.
//
// Called from every path that observes one — accept, move, and a player's game
// listing — so the index fills in from ordinary use rather than needing a
// crawler. An upsert rather than an insert because the same game is seen many
// times and the newest observation is the one worth keeping.
//
// Errors are returned but every caller treats them as non-fatal: this is an
// index, and failing a player's move because a cache write failed would be a
// far worse bug than a missing spectator row.
func (s *Store) RecordGame(g IndexedGame) error {
	if g.URI == "" {
		return fmt.Errorf("refusing to index a game with no URI")
	}
	_, err := s.db.Exec(`
INSERT INTO games (uri, white_did, black_did, status, fen, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(uri) DO UPDATE SET
	status     = excluded.status,
	fen        = excluded.fen,
	updated_at = excluded.updated_at
`, g.URI, g.WhiteDID, g.BlackDID, g.Status, g.FEN, g.CreatedAt, g.UpdatedAt)
	return err
}

// ActiveGames returns games this service has seen that are still in progress,
// most recently updated first.
func (s *Store) ActiveGames(limit int) ([]IndexedGame, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.Query(`
SELECT uri, white_did, black_did, status, fen, created_at, updated_at
FROM games WHERE status = 'active'
ORDER BY updated_at DESC, created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []IndexedGame
	for rows.Next() {
		var g IndexedGame
		if err := rows.Scan(&g.URI, &g.WhiteDID, &g.BlackDID, &g.Status, &g.FEN, &g.CreatedAt, &g.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}
