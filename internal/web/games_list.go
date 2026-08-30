package web

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/justinabrahms/atchess/internal/atproto"
	"github.com/justinabrahms/atchess/internal/challenge"
	"github.com/justinabrahms/atchess/internal/chess"
)

// games_list.go — GET /api/games, the answer to "what am I playing?".
//
// WHAT WAS HERE BEFORE. Nothing. The route did not exist, and the page's
// loadActiveGames() was:
//
//	// TODO: Implement endpoint to fetch user's active games
//	// For now, show empty state
//	document.getElementById('gamesList').innerHTML = '...No active games...';
//
// So a player who closed the tab lost the game. Reported 2026-08-30 by someone
// who had issued a challenge, had it accepted, made a move, and then found an
// empty list on his own account with no way back to a game that plainly
// existed.
//
// WHY IT SCANS. A game record lives in exactly one repository — whoever
// accepted the challenge — and AT Protocol does not permit writing into anybody
// else's. So the opponent's copy cannot exist, and finding a game you did not
// create means looking where it actually is.
//
// The set of places to look is small and derivable, and it comes from three
// sources, each of which covers a case the others miss:
//
//	own repo          games you accepted yourself
//	game index        anyone you have already been found to play (memoized)
//	your challenges   the person you challenged, before any index exists
//	incoming store    the person who challenged you, before you accepted
//
// The last two are what make the FIRST listing work, and dropping either
// silently loses one direction of play.
//
// AND IT MEMOIZES. After a scan finds a game, an app.atchess.gameIndex entry is
// written into the caller's own repo pointing at it. That write IS legal —
// it is your repo — and it means existing games are backfilled simply by being
// looked at once, with no migration, and subsequent listings go straight to the
// repos that matter.

// gameListEntry is one row of the listing. It carries the opponent explicitly
// so the page does not have to work out which colour the viewer is.
type gameListEntry struct {
	*chess.Game
	Opponent  string `json:"opponent"`
	YourColor string `json:"yourColor"`
	YourTurn  bool   `json:"yourTurn"`
}

// ListGamesHandler serves GET /api/games for the authenticated player.
func (s *Service) ListGamesHandler(w http.ResponseWriter, r *http.Request) {
	did := AuthenticatedDID(r)
	if did == "" {
		http.Error(w, "Authentication required", http.StatusUnauthorized)
		return
	}
	client, ok := s.requireClient(w, r)
	if !ok {
		return
	}
	ctx := r.Context()

	// --- where to look ----------------------------------------------------
	repos := []string{did}
	add := func(dids []string) {
		for _, d := range dids {
			if d != "" {
				repos = append(repos, d)
			}
		}
	}
	if idx, err := client.ListGameIndexRepos(ctx, did); err == nil {
		add(idx)
	} else {
		log.Debug().Err(err).Msg("games list: no usable game index yet")
	}
	if cps, err := client.ListChallengeCounterparties(ctx, did); err == nil {
		add(cps)
	} else {
		log.Warn().Err(err).Msg("games list: could not read own challenges")
	}
	// Incoming challenges: whoever challenged US may hold the game if we
	// accepted it. The local store is the index the firehose maintains.
	if s.challengeStore != nil {
		if pending, err := s.challengeStore.ForPlayer(did); err == nil {
			for _, p := range pending {
				add([]string{p.ChallengerDID})
			}
		}
	}

	seenRepo := map[string]bool{}
	// --- scan -------------------------------------------------------------
	var (
		games    []*chess.Game
		partial  bool
		seenGame = map[string]bool{}
	)
	for _, repo := range repos {
		if seenRepo[repo] {
			continue
		}
		seenRepo[repo] = true

		found, err := client.ListGamesInRepo(ctx, repo, did)
		if err != nil {
			// One unreadable repo must not empty the whole list, but the caller
			// has to be told the answer is incomplete. Silently returning a
			// short list is how a player concludes their game is gone.
			partial = true
			log.Warn().Err(err).Str("repo", repo).Msg("games list: a repo could not be read")
			continue
		}
		for _, g := range found {
			if seenGame[g.ID] {
				continue
			}
			seenGame[g.ID] = true
			games = append(games, g)
		}
		s.memoizeFoundGames(ctx, client, repo, did, found)
		s.indexFoundGames(found)
	}

	sort.Slice(games, func(i, j int) bool { return games[i].CreatedAt > games[j].CreatedAt })

	entries := make([]gameListEntry, 0, len(games))
	for _, g := range games {
		e := gameListEntry{Game: g, YourColor: "white", Opponent: g.Black}
		if g.Black == did {
			e.YourColor, e.Opponent = "black", g.White
		}
		e.YourTurn = turnOf(g.FEN) == e.YourColor && g.Status == chess.StatusActive
		entries = append(entries, e)
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"games": entries,
		"total": len(entries),
		// Named "incomplete" rather than omitted when false: a client that
		// wants to say "some of your games could not be loaded" needs to be
		// able to ask, and a missing key reads as false to anyone not looking.
		"incomplete": partial,
	})
}

// memoizeFoundGames records, in the caller's OWN repo, a pointer to each game
// found in someone else's. Best effort by design: an index is a cache, and a
// player whose index write fails must still see their games.
func (s *Service) memoizeFoundGames(ctx context.Context, client *atproto.Client, repo, did string, found []*chess.Game) {
	if repo == did || len(found) == 0 {
		return // our own games need no pointer
	}
	cids, err := client.GameCIDs(ctx, repo)
	if err != nil {
		log.Debug().Err(err).Str("repo", repo).Msg("games list: no CIDs, skipping memoization")
		return
	}
	for _, g := range found {
		cid := cids[g.ID]
		if cid == "" {
			continue
		}
		// Handles are decoration on the index record, resolved best
		// effort: an empty one is a cosmetic gap, a failed write is a
		// game the player cannot find.
		if err := client.MemoizeGameIndex(ctx, g, cid,
			client.HandleForDID(ctx, g.White), client.HandleForDID(ctx, g.Black)); err != nil {
			log.Debug().Err(err).Str("game", g.ID).Msg("games list: could not memoize game index")
		}
	}
}

// turnOf reports whose move it is from a FEN's active-colour field.
func turnOf(fen string) string {
	// "rnbq... w KQkq - 0 1" -> the field after the board is the side to move.
	for i := 0; i < len(fen); i++ {
		if fen[i] == ' ' {
			if i+1 < len(fen) && fen[i+1] == 'b' {
				return "black"
			}
			return "white"
		}
	}
	return "white"
}

// ListOutgoingChallengesHandler serves GET /api/challenges — the challenges the
// caller has SENT.
//
// The page previously listed only incoming challenges, so a player who issued
// one saw nothing at all and had no evidence it existed. Worse, the challenge
// record in their own repo reads "pending" forever no matter what happens,
// because the acceptor cannot write into someone else's repository to update
// it. So "was my challenge accepted?" is only answerable by looking for the
// game — which is what GameURI below does.
func (s *Service) ListOutgoingChallengesHandler(w http.ResponseWriter, r *http.Request) {
	did := AuthenticatedDID(r)
	if did == "" {
		http.Error(w, "Authentication required", http.StatusUnauthorized)
		return
	}
	client, ok := s.requireClient(w, r)
	if !ok {
		return
	}
	ctx := r.Context()

	challenges, err := client.ListOutgoingChallenges(ctx, did)
	if err != nil {
		log.Error().Err(err).Msg("could not list outgoing challenges")
		http.Error(w, "Could not read your challenges", statusForError(err))
		return
	}

	// Resolve "accepted" by finding THIS challenge's game, since the challenge
	// record cannot be updated by the person who accepted it.
	//
	// Matched on proposedGameId, which is the rkey the game takes when the
	// challenge is accepted. An earlier version took the counterparty's newest
	// game for every challenge with that counterparty, so all four challenges
	// against the same opponent pointed at one game and clicking any of them
	// opened the wrong one — visible immediately once anyone played the same
	// person twice, which is the normal case.
	byRepo := map[string][]*chess.Game{}
	for _, ch := range challenges {
		if _, done := byRepo[ch.ChallengedDID]; done {
			continue
		}
		games, gerr := client.ListGamesInRepo(ctx, ch.ChallengedDID, did)
		if gerr != nil {
			log.Debug().Err(gerr).Str("repo", ch.ChallengedDID).Msg("challenges: opponent repo unreadable")
			games = nil
		}
		byRepo[ch.ChallengedDID] = games
	}
	for _, ch := range challenges {
		if ch.ProposedGameID == "" {
			continue
		}
		for _, g := range byRepo[ch.ChallengedDID] {
			if rkeyOfATURI(g.ID) == ch.ProposedGameID {
				ch.GameURI = g.ID
				ch.Status = "accepted"
				break
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"challenges": challenges,
		"total":      len(challenges),
	})
}

// indexFoundGames records what a listing saw into the service's own game index,
// which is what makes the spectator listing possible at all: AT Protocol offers
// no way to enumerate every game in the network, so the index is built from
// whatever this service happens to observe.
//
// Best effort throughout. A failed index write must never affect the player who
// triggered it — they asked for their games, not to maintain a cache.
func (s *Service) indexFoundGames(found []*chess.Game) {
	if s.challengeStore == nil {
		return
	}
	for _, g := range found {
		if err := s.challengeStore.RecordGame(challenge.IndexedGame{
			URI:       g.ID,
			WhiteDID:  g.White,
			BlackDID:  g.Black,
			Status:    string(g.Status),
			FEN:       g.FEN,
			CreatedAt: g.CreatedAt,
			UpdatedAt: time.Now().UTC().Format(time.RFC3339),
		}); err != nil {
			log.Debug().Err(err).Str("game", g.ID).Msg("games list: could not index game")
		}
	}
}

// rkeyOfATURI returns the record key (final segment) of an at:// URI.
func rkeyOfATURI(uri string) string {
	if i := strings.LastIndex(uri, "/"); i >= 0 && i+1 < len(uri) {
		return uri[i+1:]
	}
	return ""
}
