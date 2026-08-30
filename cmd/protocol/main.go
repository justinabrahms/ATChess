package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gorilla/mux"
	"github.com/justinabrahms/atchess/internal/atproto"
	"github.com/justinabrahms/atchess/internal/challenge"
	"github.com/justinabrahms/atchess/internal/config"
	"github.com/justinabrahms/atchess/internal/firehose"
	"github.com/justinabrahms/atchess/internal/web"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

func main() {
	// Parse command line flags
	var showHelp bool
	flag.BoolVar(&showHelp, "help", false, "Show help information")
	flag.BoolVar(&showHelp, "h", false, "Show help information")
	flag.Parse()

	if showHelp {
		showHelpMessage()
		return
	}

	// Setup logging
	log.Logger = zerolog.New(os.Stdout).With().Timestamp().Logger()

	// Load config
	cfg, err := config.Load()
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to load config")
	}

	// Create AT Protocol client
	client, err := atproto.NewClientWithDPoP(
		cfg.ATProto.PDSURL,
		cfg.ATProto.Handle,
		cfg.ATProto.Password,
		cfg.ATProto.UseDPoP,
	)
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to create AT Protocol client")
	}
	client.SetPLCDirectoryURL(cfg.ATProto.PLCDirectoryURL)

	// Create WebSocket hub
	hub := web.NewHub()
	go hub.Run()

	// Create challenge store for cross-federation challenge discovery
	// (atchess-1c9.50): a durable, SQLite-backed index keyed by challenged
	// DID, so a challenge issued while this process was down -- even past
	// a relay's retention window -- is still discoverable once the
	// firehose subscription or the login backfill catches up. See
	// internal/challenge.Store's doc comment and
	// docs/firehose-and-backfill.md.
	// Sessions are mirrored to disk BEFORE anything can create one, so a
	// restart does not silently log every user out. Reported 2026-08-30 as
	// "when I refresh, I'm logged out" during an afternoon of five deploys —
	// and it matters more here than most places, because the whole point of
	// the pipeline this repo feeds is that deploys are frequent and unattended.
	//
	// A failure to load is logged and tolerated: starting empty logs people
	// out, which is bad, but refusing to start is worse, and a corrupt session
	// file must never be able to hold the service down.
	if p := cfg.Session.StorePath; p != "" {
		if err := web.EnableSessionPersistence(p); err != nil {
			log.Warn().Err(err).Str("path", p).
				Msg("could not load persisted sessions; users will need to sign in again")
		} else {
			log.Info().Str("path", p).Msg("session persistence enabled")
		}
	}

	challengeStore, err := challenge.NewStore(cfg.Challenge.DBPath)
	if err != nil {
		log.Fatal().Err(err).Str("dbPath", cfg.Challenge.DBPath).Msg("Failed to open challenge store")
	}
	defer func() {
		if err := challengeStore.Close(); err != nil {
			log.Error().Err(err).Msg("Failed to close challenge store cleanly")
		}
	}()

	// Periodically prune expired OPEN challenges from the durable index
	// (atchess-1c9.47 part 1): challenge.Store.PruneExpired previously had
	// zero production callers, so rows -- now on disk rather than just in
	// RAM, since atchess-1c9.50 made this store SQLite-backed -- would
	// accumulate forever on a small (~2GB/1vCPU) production droplet.
	// declined/removed tombstones are deliberately NEVER deleted by this
	// loop; see PruneExpired's doc comment for why pruning a tombstone
	// too early would let a firehose replay resurrect a challenge the
	// user already dismissed.
	stopChallengePrune := make(chan struct{})
	challengePruneDone := make(chan struct{})
	go pruneChallengesPeriodically(challengeStore, cfg.Challenge.PruneInterval, stopChallengePrune, challengePruneDone)

	// Create service
	service := web.NewService(client, cfg, challengeStore)

	// Initialize OAuth if base URL is configured
	if cfg.Server.BaseURL != "" {
		if err := web.InitializeOAuth(cfg.Server.BaseURL); err != nil {
			log.Error().Err(err).Msg("Failed to initialize OAuth, falling back to password auth")
		} else {
			// Pass OAuth client to service for dynamic metadata
			service.SetOAuthClient(web.GetOAuthClient())
		}
	}

	// Create firehose processor with shared challenge store
	processor := firehose.NewEventProcessor(hub, challengeStore)

	// firehoseClients/firehoseClientURLs and cursorStore are declared here
	// (rather than scoped to the `if cfg.Firehose.Enabled` block below) so
	// the graceful-shutdown code further down can flush final cursor
	// positions and stop the clients; they stay nil/empty when firehose is
	// disabled.
	var (
		firehoseClients    []*firehose.Client
		firehoseClientURLs []string
		cursorStore        *firehose.CursorStore
	)

	// Start firehose client(s) (optional -- can be disabled in config).
	// Challenge delivery (atchess-1c9.11) depends on this: a challenge
	// record only ever lives in its CHALLENGER's own repo (AT Protocol
	// forbids writing it anywhere else), so the challenged player's own
	// instance can only discover it by watching the firehose of whatever
	// PDS(es) might host a challenger. cfg.Firehose.URL may name more than
	// one PDS's com.atproto.sync.subscribeRepos endpoint, comma-separated
	// (see FirehoseConfig.URL's doc comment) -- this harness/deployment
	// topology has no relay/Jetstream aggregator in front of it, so every
	// watched PDS needs its own subscription. Each one gets its own
	// firehose.Client (sequence numbers, and therefore cursors, are
	// per-source), all reporting into the same shared processor/challengeStore.
	if cfg.Firehose.Enabled {
		urls := config.SplitFirehoseURLs(cfg.Firehose.URL)
		if len(urls) == 0 {
			log.Warn().Msg("firehose enabled but no URL(s) configured; challenge delivery will not work")
		}

		// Cursor persistence (atchess-1c9.46): resume each watched host
		// from wherever this process last got to, instead of either
		// replaying that host's ENTIRE retained commit log on every boot
		// (WithCursor(0), unconditionally, on every process start -- the
		// defect this fixes: against a production-scale host such as the
		// shipped default wss://bsky.social, that is an unbounded replay
		// of a huge commit log on every single restart) or silently
		// discarding history by always starting at the live tip. A failure
		// to initialize the cursor store (e.g. an unwritable state dir) is
		// logged and degrades to "no persistence this run" rather than
		// failing startup -- firehose subscription and login backfill both
		// still work, just without cross-restart cursor resumption.
		var err error
		cursorStore, err = firehose.NewCursorStore(cfg.Firehose.StateDir, log.Logger)
		if err != nil {
			log.Error().Err(err).Str("stateDir", cfg.Firehose.StateDir).Msg("failed to initialize firehose cursor store; continuing without cursor persistence for this run")
			cursorStore = nil
		}

		for _, u := range urls {
			u := u

			// Transport selection (atchess-1c9.49): guessed per-URL from
			// its shape (a Jetstream "/subscribe" path vs. the original
			// com.atproto.sync.subscribeRepos XRPC path), unless
			// cfg.Firehose.Transport forces one explicitly for every
			// configured URL. This means a mixed comma-separated URL list
			// (some subscribeRepos hosts, some Jetstream instances) just
			// works: each firehose.Client independently detects its own
			// URL's transport, with no shared/global transport state
			// between them -- unless the config override is set, in which
			// case it applies uniformly to every URL in the list.
			transport := firehose.DetectTransport(u)
			if cfg.Firehose.Transport != "" {
				if t, ok := firehose.ParseTransportOverride(cfg.Firehose.Transport); ok {
					transport = t
				} else {
					log.Warn().Str("value", cfg.Firehose.Transport).Str("url", u).Msg("unrecognized firehose.transport override; falling back to automatic per-URL transport detection")
				}
			}

			// WithLogger (atchess-1c9.46 review fix): without this, Client
			// falls back to zerolog.Nop() (see firehose.NewClient) and
			// every #info/OutdatedCursor/FutureCursor/error-frame log this
			// bead added -- the only diagnostic signal for atchess-1c9.16's
			// runbook and atchess-1c9.14's live-network validation -- is
			// silently discarded. Tagged with the host's URL so multi-host
			// deployments (this harness watches more than one PDS) can
			// tell which client emitted a given line.
			opts := []firehose.Option{
				firehose.WithURL(u),
				firehose.WithTransport(transport),
				firehose.WithLogger(log.Logger.With().Str("firehoseURL", u).Str("firehoseTransport", string(transport)).Logger()),
			}

			// BOUNDED INITIAL BACKFILL (atchess-1c9.46): when there is no
			// persisted cursor for this host (first run against it, or a
			// cursor that was cleared -- see CursorStore.Store and
			// firehose.Client's FutureCursor handling), this process does
			// NOT request cursor 0. Requesting 0 means "replay this host's
			// entire retained commit log from the very beginning", which
			// for a production-scale host is unbounded and exactly the
			// defect this bead fixes; it is not "backfill", it is "replay
			// everything, every boot". Instead, the client starts at the
			// live tip (no WithCursor option at all -- see
			// firehose.Client's lastSequence field doc comment for why -1
			// means exactly that), and the HISTORICAL side of "don't miss
			// a challenge issued while offline" is handled by a much more
			// targeted mechanism: the login-time repo-read backfill
			// (internal/backfill, invoked from internal/web's
			// LoginHandler/OAuthCallbackHandler), which reads directly and
			// specifically for the logging-in user rather than replaying
			// every record for every user on the host.
			if cursorStore != nil {
				if stored, ok := cursorStore.Get(u, transport); ok {
					opts = append(opts, firehose.WithCursor(stored))
					log.Info().Str("url", u).Int64("cursor", stored).Msg("resuming firehose subscription from persisted cursor")
				} else {
					log.Info().Str("url", u).Msg("no persisted cursor for this host; starting at the live tip (no historical replay) -- history is instead covered by the login-time repo-read backfill (internal/backfill)")
				}
			} else {
				log.Info().Str("url", u).Msg("cursor persistence unavailable this run; starting at the live tip (no historical replay)")
			}

			firehoseClient := firehose.NewClient(
				firehose.CreateChessEventHandler(processor),
				opts...,
			)
			firehoseClients = append(firehoseClients, firehoseClient)
			firehoseClientURLs = append(firehoseClientURLs, u)

			go func() {
				log.Info().Str("url", u).Msg("Starting firehose client")
				if err := firehoseClient.Start(); err != nil {
					log.Error().Err(err).Str("url", u).Msg("Firehose client error")
				}
			}()
		}

		// Track the current user's games
		processor.TrackPlayer(client.GetDID())
	}

	// Periodically persist each watched host's last-processed sequence
	// number (atchess-1c9.46), rather than on every single processed
	// frame: a write (fsync-adjacent rename, see CursorStore.saveLocked)
	// per firehose message would be needless I/O pressure against a
	// busy/production-scale host, and it is not necessary for correctness
	// -- a restart that resumes from a cursor up to
	// firehoseCursorPersistInterval (plus the final flush performed during
	// graceful shutdown below) old will, at worst, reprocess a handful of
	// already-seen events, which is safe: challenge.Store.Add dedups by
	// challenge URI, so reprocessing is a no-op there, and every other
	// event type this processor handles is itself naturally idempotent
	// (see internal/firehose.EventProcessor).
	var stopCursorPersist chan struct{}
	if cursorStore != nil && len(firehoseClients) > 0 {
		stopCursorPersist = make(chan struct{})
		go persistFirehoseCursorsPeriodically(cursorStore, firehoseClients, firehoseClientURLs, stopCursorPersist)
	}

	// Setup routes
	router := mux.NewRouter()

	// Add CORS middleware with origin allowlist
	router.Use(service.GetOriginChecker().CORSMiddleware)

	// Root level health endpoint for load balancers and monitoring
	router.HandleFunc("/health", service.HealthHandler).Methods("GET")

	// OAuth client metadata endpoint (must be before static file handler)
	router.HandleFunc("/client-metadata.json", service.ClientMetadataHandler).Methods("GET")

	// API routes
	api := router.PathPrefix("/api").Subrouter()

	// Public endpoints (no auth required)
	api.HandleFunc("/health", service.HealthHandler).Methods("GET")
	api.HandleFunc("/auth/login", service.LoginHandler).Methods("POST")
	api.HandleFunc("/auth/oauth/login", service.OAuthLoginHandler).Methods("POST")
	api.HandleFunc("/callback", service.OAuthCallbackHandler).Methods("GET")
	api.HandleFunc("/auth/session", service.GetSessionHandler).Methods("GET")
	api.HandleFunc("/auth/logout", service.LogoutHandler).Methods("POST")
	api.HandleFunc("/games/{id:.*}", service.GetGameHandler).Methods("GET")

	// Spectator endpoints (public, read-only)
	api.HandleFunc("/spectator/games", service.GetActiveGamesHandler).Methods("GET")
	api.HandleFunc("/spectator/games/{id:.*}", service.GetSpectatorGameHandler).Methods("GET")
	api.HandleFunc("/spectator/games/{id:.*}/count", service.UpdateSpectatorCountHandler(hub)).Methods("POST")
	api.HandleFunc("/spectator/games/{id:.*}/abandonment", service.CheckAbandonmentHandler).Methods("GET")

	// Time control read endpoints (public)
	api.HandleFunc("/games/{id:.*}/time-violation", service.CheckTimeViolationHandler).Methods("GET")
	api.HandleFunc("/games/{id:.*}/time-remaining", service.GetTimeRemainingHandler).Methods("GET")

	// WebSocket endpoint for real-time updates
	api.HandleFunc("/ws", service.WebSocketHandler(hub))

	// Authenticated endpoints (require valid session)
	authed := api.PathPrefix("").Subrouter()
	authed.Use(web.AuthMiddleware)
	authed.HandleFunc("/auth/current", service.GetCurrentUserHandler).Methods("GET")
	authed.HandleFunc("/games", service.CreateGameHandler).Methods("POST")
	// GET /games answers "what am I playing?". It did not exist until
	// 2026-08-30, which is why the page's game list was a hardcoded
	// "No active games" and a player who closed the tab lost their game.
	authed.HandleFunc("/games", service.ListGamesHandler).Methods("GET")
	authed.HandleFunc("/moves", service.MakeMoveHandler).Methods("POST")
	authed.HandleFunc("/challenges", service.CreateChallengeHandler).Methods("POST")
	// GET /challenges lists the challenges you SENT. The page only ever
	// showed incoming ones, so issuing a challenge produced no visible
	// evidence anywhere (2026-08-30).
	authed.HandleFunc("/challenges", service.ListOutgoingChallengesHandler).Methods("GET")
	authed.HandleFunc("/challenge-notifications", service.GetChallengeNotificationsHandler).Methods("GET")
	authed.HandleFunc("/challenge-notifications/{key}", service.DeclineChallengeHandler).Methods("DELETE")
	authed.HandleFunc("/challenge-notifications/{key}/accept", service.AcceptChallengeHandler).Methods("POST")
	authed.HandleFunc("/draw-offers", service.OfferDrawHandler).Methods("POST")
	authed.HandleFunc("/draw-offers/respond", service.RespondToDrawHandler).Methods("POST")
	authed.HandleFunc("/resign", service.ResignGameHandler).Methods("POST")
	authed.HandleFunc("/spectator/games/{id:.*}/claim-abandonment", service.ClaimAbandonedGameHandler).Methods("POST")
	authed.HandleFunc("/games/{id:.*}/claim-time", service.ClaimTimeVictoryHandler).Methods("POST")

	// Explicit OPTIONS handlers for CORS preflight requests
	api.HandleFunc("/auth/login", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}).Methods("OPTIONS")
	api.HandleFunc("/auth/current", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}).Methods("OPTIONS")
	api.HandleFunc("/auth/oauth/login", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}).Methods("OPTIONS")
	api.HandleFunc("/auth/session", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}).Methods("OPTIONS")
	api.HandleFunc("/auth/logout", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}).Methods("OPTIONS")
	api.HandleFunc("/games", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}).Methods("OPTIONS")
	api.HandleFunc("/games/{id:.*}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}).Methods("OPTIONS")
	api.HandleFunc("/moves", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}).Methods("OPTIONS")
	api.HandleFunc("/challenges", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}).Methods("OPTIONS")
	api.HandleFunc("/challenge-notifications", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}).Methods("OPTIONS")
	api.HandleFunc("/challenge-notifications/{key}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}).Methods("OPTIONS")
	api.HandleFunc("/challenge-notifications/{key}/accept", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}).Methods("OPTIONS")
	api.HandleFunc("/draw-offers", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}).Methods("OPTIONS")
	api.HandleFunc("/draw-offers/respond", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}).Methods("OPTIONS")
	api.HandleFunc("/resign", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}).Methods("OPTIONS")
	api.HandleFunc("/spectator/games", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}).Methods("OPTIONS")
	api.HandleFunc("/spectator/games/{id:.*}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}).Methods("OPTIONS")
	api.HandleFunc("/spectator/games/{id:.*}/count", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}).Methods("OPTIONS")
	api.HandleFunc("/spectator/games/{id:.*}/abandonment", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}).Methods("OPTIONS")
	api.HandleFunc("/spectator/games/{id:.*}/claim-abandonment", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}).Methods("OPTIONS")
	api.HandleFunc("/games/{id:.*}/time-violation", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}).Methods("OPTIONS")
	api.HandleFunc("/games/{id:.*}/claim-time", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}).Methods("OPTIONS")
	api.HandleFunc("/games/{id:.*}/time-remaining", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}).Methods("OPTIONS")

	// Serve static files
	staticDir := os.Getenv("ATCHESS_STATIC_DIR")
	if staticDir == "" {
		staticDir = "./web/static/"
	}
	router.PathPrefix("/").Handler(http.FileServer(http.Dir(staticDir)))

	// Create server
	srv := &http.Server{
		Addr:         fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port),
		Handler:      router,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Start server
	go func() {
		log.Info().Str("addr", srv.Addr).Msg("Starting server")
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal().Err(err).Msg("Failed to start server")
		}
	}()

	// Wait for interrupt signal
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Info().Msg("Shutting down server...")

	// Stop the periodic challenge-prune loop and wait (bounded) for it to
	// actually exit, so a leaked goroutine past this point is something a
	// test can catch rather than something merely assumed away by process
	// exit shortly afterward.
	close(stopChallengePrune)
	select {
	case <-challengePruneDone:
	case <-time.After(5 * time.Second):
		log.Warn().Msg("timed out waiting for challenge prune loop to stop")
	}

	// Stop the periodic cursor-persistence loop and do one final flush so
	// the on-disk cursor is as fresh as possible at shutdown (bounding the
	// replay-on-restart window to whatever happened in the last instant
	// before this flush, rather than up to firehoseCursorPersistInterval
	// stale).
	if stopCursorPersist != nil {
		close(stopCursorPersist)
	}
	if cursorStore != nil {
		flushFirehoseCursors(cursorStore, firehoseClients, firehoseClientURLs)
	}
	for _, c := range firehoseClients {
		if err := c.Stop(); err != nil {
			log.Warn().Err(err).Msg("error stopping firehose client during shutdown")
		}
	}

	// Graceful shutdown
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		log.Fatal().Err(err).Msg("Server forced to shutdown")
	}

	log.Info().Msg("Server exited")
}

// firehoseCursorPersistInterval bounds how often each watched host's
// last-processed sequence number is written to disk -- see the comment
// where persistFirehoseCursorsPeriodically is started in main() for why a
// bounded interval (rather than every processed frame) was chosen.
const firehoseCursorPersistInterval = 5 * time.Second

// persistFirehoseCursorsPeriodically writes each client's current
// LastSequence to store, keyed by its corresponding URL in urls (same
// index), every firehoseCursorPersistInterval until stop is closed.
func persistFirehoseCursorsPeriodically(store *firehose.CursorStore, clients []*firehose.Client, urls []string, stop <-chan struct{}) {
	ticker := time.NewTicker(firehoseCursorPersistInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			flushFirehoseCursors(store, clients, urls)
		case <-stop:
			return
		}
	}
}

// flushFirehoseCursors writes each client's current LastSequence to store
// immediately (used both by the periodic loop and by the final shutdown
// flush).
func flushFirehoseCursors(store *firehose.CursorStore, clients []*firehose.Client, urls []string) {
	for i, c := range clients {
		seq := c.LastSequence()
		if err := store.Store(urls[i], c.Transport(), seq); err != nil {
			log.Error().Err(err).Str("url", urls[i]).Msg("failed to persist firehose cursor")
		}
	}
}

// pruneChallengesPeriodically calls store.PruneExpired every interval
// until stop is closed, at which point it returns and closes done (so a
// caller -- main()'s shutdown path, or a test -- can observe that the
// loop actually exited rather than assuming it did). Mirrors
// persistFirehoseCursorsPeriodically's stop-channel shutdown pattern
// above, so this loop is bound to the process lifetime the exact same
// way that one is: it cannot outlive shutdown (atchess-1c9.47 part 1 --
// PruneExpired previously had no caller, periodic or otherwise, at all).
//
// A PruneExpired error is logged and the loop continues to its next
// tick; a single failed prune (e.g. a transient disk issue) must not
// take down the whole service, and there is always another tick to try
// again.
func pruneChallengesPeriodically(store *challenge.Store, interval time.Duration, stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			n, err := store.PruneExpired()
			if err != nil {
				log.Error().Err(err).Msg("failed to prune expired challenges")
				continue
			}
			if n > 0 {
				log.Info().Int("pruned", n).Msg("pruned expired challenges")
			}
		case <-stop:
			return
		}
	}
}

func showHelpMessage() {
	fmt.Println(`ATChess Protocol Service

DESCRIPTION:
    AT Protocol service for the ATChess decentralized chess platform.
    Handles chess game logic, move validation, and AT Protocol interactions.
    Provides REST API endpoints for game operations and stores game data
    in personal AT Protocol repositories.

USAGE:
    atchess-protocol [OPTIONS]

OPTIONS:
    -h, --help    Show this help message

CONFIGURATION:
    The protocol service is configured via config.yaml in the current directory.
    
    Example config.yaml:
        server:
          host: localhost
          port: 8080        # Protocol service port
        
        atproto:
          pds_url: http://localhost:3000
          handle: "atchess.localhost"
          password: "atchess-service-password"
        
        development:
          debug: true
          log_level: debug

API ENDPOINTS:
    GET  /api/health              - Service health check
    POST /api/games               - Create a new chess game
    GET  /api/games/{id}          - Get game state by ID
    POST /api/moves               - Submit a move to a game (game_id in body)
    POST /api/challenges          - Create a game challenge

BEHAVIOR:
    - Validates chess moves using notnil/chess engine
    - Stores game data in AT Protocol repositories
    - Handles game state management with FEN/PGN notation
    - Provides REST API for chess operations
    - Graceful shutdown on SIGINT/SIGTERM

EXAMPLES:
    # Start with default configuration
    atchess-protocol
    
    # Show help
    atchess-protocol --help
    
    # Create a game via API
    curl -X POST http://localhost:8080/api/games \
      -H "Content-Type: application/json" \
      -d '{"opponent_did": "did:plc:...", "color": "white"}'

SEE ALSO:
    atchess-web(1), config.yaml(5)
    
    Documentation: docs/
    Repository: https://github.com/justinabrahms/atchess`)
}
