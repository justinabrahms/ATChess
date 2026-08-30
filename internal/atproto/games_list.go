package atproto

import (
	"context"
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/justinabrahms/atchess/internal/chess"
)

// games_list.go — finding the games a player is in.
//
// WHY THIS EXISTS. Until 2026-08-30 there was no way to ask "what games am I
// playing?". The UI's game list was a literal stub that rendered "No active
// games" unconditionally, the spectator listing returned an empty slice with a
// TODO, and no endpoint existed behind either. A player who closed the tab lost
// their game: it was reachable only by holding its URI.
//
// WHY IT IS A SCAN. A game record lives in exactly one repo — the acceptor's.
// It cannot be copied into the opponent's, because AT Protocol does not permit
// writing to someone else's repository; this project already learned that when
// cross-repo challenge notifications were removed for being impossible rather
// than merely broken. The README's claim that games are "stored in both
// players' repositories for redundancy" was never true and cannot be.
//
// So the only way to find a game you did not create is to look in the repo of
// someone you have played. The set of those people is small and knowable: it is
// exactly the counterparties of your challenges, in both directions. That is
// what CandidateRepos assembles and what GamesForPlayer scans.

// gameIndexEntryValue is the app.atchess.gameIndex record body, used both to
// read a memoized pointer and to write one.
type gameIndexEntryValue struct {
	Type      string `json:"$type"`
	CreatedAt string `json:"createdAt"`
	Game      struct {
		URI string `json:"uri"`
		CID string `json:"cid"`
	} `json:"game"`
	Players struct {
		White struct {
			DID    string `json:"did"`
			Handle string `json:"handle"`
		} `json:"white"`
		Black struct {
			DID    string `json:"did"`
			Handle string `json:"handle"`
		} `json:"black"`
	} `json:"players"`
	Status     string `json:"status"`
	Visibility string `json:"visibility"`
}

// gameRecordValue is the subset of app.atchess.game this file reads. It is
// deliberately partial: listing needs participants, status and position, and
// decoding more would couple the listing to fields it does not use.
type gameRecordValue struct {
	White struct {
		DID string `json:"did"`
	} `json:"white"`
	Black struct {
		DID string `json:"did"`
	} `json:"black"`
	Status    string `json:"status"`
	FEN       string `json:"fen"`
	PGN       string `json:"pgn"`
	CreatedAt string `json:"createdAt"`
}

// participantDIDs pulls both players out of a game record, tolerating the two
// shapes seen in the wild: nested objects ({"white":{"did":...}}) and the flat
// form some older records use ({"white":"did:plc:..."}).
func participantDIDs(raw json.RawMessage) (white, black string) {
	// Decoded FIELD BY FIELD, deliberately. An earlier version unmarshalled the
	// whole record into a nested-shape struct and fell back to a flat-shape one,
	// which cannot work: encoding/json aborts the ENTIRE unmarshal on the first
	// type mismatch, so a record with one field of each shape produced neither.
	// Both fallbacks failed identically and the game silently never appeared in
	// anyone's list. Caught by TestParticipantDIDsHandlesBothRecordShapes.
	var fields struct {
		White json.RawMessage `json:"white"`
		Black json.RawMessage `json:"black"`
	}
	if json.Unmarshal(raw, &fields) != nil {
		return "", ""
	}
	return didFromField(fields.White), didFromField(fields.Black)
}

// didFromField reads a player DID written either as a bare string or as an
// object with a "did" key. Both shapes exist in real repositories.
func didFromField(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var o struct {
		DID string `json:"did"`
	}
	if json.Unmarshal(raw, &o) == nil {
		return o.DID
	}
	return ""
}

// ListGamesInRepo returns every game in repoDID's repository in which
// participantDID plays, newest first.
//
// A repo that cannot be read is an error, never an empty result. "I could not
// look" and "there is nothing there" are different answers, and collapsing them
// is how a listing quietly loses a game.
func (c *Client) ListGamesInRepo(ctx context.Context, repoDID, participantDID string) ([]*chess.Game, error) {
	base, ownRepo, err := c.resolveReadEndpoint(ctx, repoDID)
	if err != nil {
		return nil, fmt.Errorf("resolve read endpoint for %s: %w", repoDID, err)
	}
	records, err := c.listAllRecords(ctx, base, ownRepo, repoDID, "app.atchess.game")
	if err != nil {
		return nil, fmt.Errorf("list games for %s: %w", repoDID, err)
	}

	var games []*chess.Game
	for _, rec := range records {
		white, black := participantDIDs(rec.Value)
		if white != participantDID && black != participantDID {
			continue
		}
		var v gameRecordValue
		_ = json.Unmarshal(rec.Value, &v)
		games = append(games, &chess.Game{
			ID:        rec.URI,
			White:     white,
			Black:     black,
			Status:    chess.GameStatus(v.Status),
			FEN:       v.FEN,
			PGN:       v.PGN,
			CreatedAt: v.CreatedAt,
		})
	}
	sort.Slice(games, func(i, j int) bool { return games[i].CreatedAt > games[j].CreatedAt })
	return games, nil
}

// ListGameIndexRepos returns the repositories named by this player's own
// app.atchess.gameIndex entries — the memoized answer to "whose repos hold
// games I am in", written by MemoizeGameIndex after a scan finds one.
func (c *Client) ListGameIndexRepos(ctx context.Context, repoDID string) ([]string, error) {
	base, ownRepo, err := c.resolveReadEndpoint(ctx, repoDID)
	if err != nil {
		return nil, fmt.Errorf("resolve read endpoint for %s: %w", repoDID, err)
	}
	records, err := c.listAllRecords(ctx, base, ownRepo, repoDID, "app.atchess.gameIndex")
	if err != nil {
		return nil, fmt.Errorf("list game index for %s: %w", repoDID, err)
	}
	seen := map[string]bool{}
	var out []string
	for _, rec := range records {
		var v gameIndexEntryValue
		if json.Unmarshal(rec.Value, &v) != nil || v.Game.URI == "" {
			continue
		}
		if owner := repoOfATURI(v.Game.URI); owner != "" && !seen[owner] {
			seen[owner] = true
			out = append(out, owner)
		}
	}
	return out, nil
}

// repoOfATURI extracts the repository DID from an at:// URI.
func repoOfATURI(uri string) string {
	rest, ok := strings.CutPrefix(uri, "at://")
	if !ok {
		return ""
	}
	did, _, _ := strings.Cut(rest, "/")
	return did
}

// MemoizeGameIndex writes an app.atchess.gameIndex entry into the CALLER'S OWN
// repository pointing at a game that lives elsewhere, so the next listing does
// not have to rediscover it by scanning.
//
// This is the half of the design that makes scanning tolerable. The index entry
// is legal to write because it goes in your own repo — unlike the game record
// itself, which only the acceptor can create. It is written opportunistically
// after a scan succeeds, which means existing games are backfilled by being
// looked at once, with no migration.
//
// Failure is not fatal and is reported for logging only: an index is an
// optimisation, and a player whose index write fails must still see their game.
func (c *Client) MemoizeGameIndex(ctx context.Context, g *chess.Game, gameCID string, whiteHandle, blackHandle string) error {
	if g == nil || g.ID == "" || gameCID == "" {
		return fmt.Errorf("refusing to write a game index entry without a game URI and CID")
	}
	var v gameIndexEntryValue
	v.Type = "app.atchess.gameIndex"
	v.CreatedAt = nowRFC3339()
	v.Game.URI = g.ID
	v.Game.CID = gameCID
	v.Players.White.DID = g.White
	v.Players.White.Handle = whiteHandle
	v.Players.Black.DID = g.Black
	v.Players.Black.Handle = blackHandle
	v.Status = string(g.Status)
	// Games on this service are public: they are records in public
	// repositories and readable by anyone who knows the URI. Saying "public"
	// here is a description, not a decision.
	v.Visibility = "public"

	// The rkey is derived from the game URI so the entry is idempotent: writing
	// it twice updates one record rather than accumulating duplicates every
	// time a listing runs.
	rkey := deriveIndexRkey(g.ID)
	return c.putOwnRecord(ctx, "app.atchess.gameIndex", rkey, v)
}

// nowRFC3339 is the timestamp format every record in this package uses.
func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }

// deriveIndexRkey turns a game URI into a stable record key, so writing the
// index entry for the same game twice updates one record instead of growing a
// duplicate on every listing. A hash rather than the URI itself because rkeys
// have a restricted character set and a length limit that at:// URIs exceed.
func deriveIndexRkey(gameURI string) string {
	sum := sha256.Sum256([]byte(gameURI))
	enc := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:8])
	return strings.ToLower(enc)
}

// putOwnRecord writes a record into the authenticated user's own repository,
// creating or replacing whatever is at rkey.
//
// No swapRecord: this is an idempotent memo whose content is derived entirely
// from the game it points at, so a concurrent write of the same entry is the
// same entry. Compare-and-swap exists to stop two writers disagreeing, and
// here they cannot.
func (c *Client) putOwnRecord(ctx context.Context, collection, rkey string, value any) error {
	body, err := json.Marshal(map[string]any{
		"repo":       c.did,
		"collection": collection,
		"rkey":       rkey,
		"record":     value,
	})
	if err != nil {
		return fmt.Errorf("marshal %s record: %w", collection, err)
	}
	resp, err := c.makeRequest("POST", xrpcURL(c.pdsURL, "com.atproto.repo.putRecord", nil), body)
	if err != nil {
		return fmt.Errorf("put %s record: %w", collection, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("put %s record: HTTP %d", collection, resp.StatusCode)
	}
	return nil
}

// ListChallengeCounterparties returns the DIDs this player has challenged,
// read from their own app.atchess.challenge records.
//
// This is what makes a first-ever listing work. Before any game index exists,
// the only record of "I have played this person" that lives in YOUR repo is the
// challenge you sent them — the game itself is in theirs. Without this, a
// challenger who never accepted anything has no way to find the game their
// opponent created.
func (c *Client) ListChallengeCounterparties(ctx context.Context, repoDID string) ([]string, error) {
	base, ownRepo, err := c.resolveReadEndpoint(ctx, repoDID)
	if err != nil {
		return nil, fmt.Errorf("resolve read endpoint for %s: %w", repoDID, err)
	}
	records, err := c.listAllRecords(ctx, base, ownRepo, repoDID, "app.atchess.challenge")
	if err != nil {
		return nil, fmt.Errorf("list challenges for %s: %w", repoDID, err)
	}
	seen := map[string]bool{}
	var out []string
	for _, rec := range records {
		var v struct {
			Challenged    string `json:"challenged"`
			ChallengedAlt struct {
				DID string `json:"did"`
			} `json:"challengedDid"`
		}
		if json.Unmarshal(rec.Value, &v) != nil {
			continue
		}
		did := v.Challenged
		if did == "" {
			did = v.ChallengedAlt.DID
		}
		if did != "" && !seen[did] {
			seen[did] = true
			out = append(out, did)
		}
	}
	return out, nil
}

// GameCIDs returns the CID of each game record in repoDID's repo, keyed by URI.
// MemoizeGameIndex needs a CID for its strongRef, and a scan already has it.
func (c *Client) GameCIDs(ctx context.Context, repoDID string) (map[string]string, error) {
	base, ownRepo, err := c.resolveReadEndpoint(ctx, repoDID)
	if err != nil {
		return nil, err
	}
	records, err := c.listAllRecords(ctx, base, ownRepo, repoDID, "app.atchess.game")
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(records))
	for _, rec := range records {
		out[rec.URI] = rec.CID
	}
	return out, nil
}

// handleCache memoizes DID->handle for the life of the process. Handles change
// rarely and a wrong one in an index entry is cosmetic, so a long-lived cache
// is the right trade against a network round trip per opponent per listing.
var handleCache sync.Map // did -> handle

// HandleForDID resolves a DID to its current handle via
// com.atproto.repo.describeRepo, which is core AT Protocol and answers on any
// PDS — unlike app.bsky.* profile lookups, which only exist on Bluesky's.
//
// Returns "" rather than an error when the handle cannot be determined. Every
// caller here is decorating a record, not making a decision, and failing a
// game-index write because a handle lookup timed out would trade something
// that matters for something that does not.
func (c *Client) HandleForDID(ctx context.Context, did string) string {
	if did == "" {
		return ""
	}
	if h, ok := handleCache.Load(did); ok {
		return h.(string)
	}
	base, _, err := c.resolveReadEndpoint(ctx, did)
	if err != nil {
		return ""
	}
	resp, err := c.makeRequest("GET",
		xrpcURL(base, "com.atproto.repo.describeRepo", url.Values{"repo": []string{did}}), nil)
	if err != nil {
		return ""
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var out struct {
		Handle string `json:"handle"`
	}
	if json.NewDecoder(resp.Body).Decode(&out) != nil || out.Handle == "" {
		return ""
	}
	handleCache.Store(did, out.Handle)
	return out.Handle
}

// OutgoingChallenge is a challenge this player sent, read back from their own
// repository.
type OutgoingChallenge struct {
	URI           string `json:"uri"`
	ChallengedDID string `json:"challengedDid"`
	Color         string `json:"color"`
	Message       string `json:"message,omitempty"`
	Status        string `json:"status"`
	CreatedAt     string `json:"createdAt"`
	// GameURI is set when a game has been found for this challenge, which is
	// the only reliable way a challenger learns their challenge was accepted:
	// the acceptor cannot write into the challenger's repo to update it, so the
	// challenge record's own "status" stays "pending" forever regardless of
	// what actually happened.
	GameURI string `json:"gameUri,omitempty"`
}

// ListOutgoingChallenges returns the challenges repoDID has issued, newest
// first.
//
// The UI previously showed only INCOMING challenges, so a player who issued one
// saw an empty list and no evidence their challenge existed at all — reported
// 2026-08-30.
func (c *Client) ListOutgoingChallenges(ctx context.Context, repoDID string) ([]*OutgoingChallenge, error) {
	base, ownRepo, err := c.resolveReadEndpoint(ctx, repoDID)
	if err != nil {
		return nil, fmt.Errorf("resolve read endpoint for %s: %w", repoDID, err)
	}
	records, err := c.listAllRecords(ctx, base, ownRepo, repoDID, "app.atchess.challenge")
	if err != nil {
		return nil, fmt.Errorf("list challenges for %s: %w", repoDID, err)
	}
	var out []*OutgoingChallenge
	for _, rec := range records {
		var v struct {
			Challenged string `json:"challenged"`
			Color      string `json:"color"`
			Message    string `json:"message"`
			Status     string `json:"status"`
			CreatedAt  string `json:"createdAt"`
		}
		if json.Unmarshal(rec.Value, &v) != nil || v.Challenged == "" {
			continue
		}
		out = append(out, &OutgoingChallenge{
			URI:           rec.URI,
			ChallengedDID: v.Challenged,
			Color:         v.Color,
			Message:       v.Message,
			Status:        v.Status,
			CreatedAt:     v.CreatedAt,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	return out, nil
}
