package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
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
	challengeStore := challenge.NewStore()

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
		urls := splitFirehoseURLs(cfg.Firehose.URL)
		if len(urls) == 0 {
			log.Warn().Msg("firehose enabled but no URL(s) configured; challenge delivery will not work")
		}
		for _, u := range urls {
			u := u
			firehoseClient := firehose.NewClient(
				firehose.CreateChessEventHandler(processor),
				firehose.WithURL(u),
				// WithCursor(0) forces THIS process's first connection to
				// every watched PDS to replay from the very beginning of
				// its commit log, not just the live tip -- this is
				// atchess-1c9.11's backfill-on-login: a challenge issued
				// while this process was not running is still discovered
				// once it starts and (re)subscribes, rather than only ever
				// seeing challenges created after that moment. Every
				// subsequent reconnect resumes from whatever sequence was
				// actually last processed (see firehose.Client.LastSequence),
				// not cursor 0 again, once at least one message has been
				// processed.
				firehose.WithCursor(0),
			)

			go func() {
				log.Info().Str("url", u).Msg("Starting firehose client (backfill-from-beginning + live subscription)")
				if err := firehoseClient.Start(); err != nil {
					log.Error().Err(err).Str("url", u).Msg("Firehose client error")
				}
			}()
		}

		// Track the current user's games
		processor.TrackPlayer(client.GetDID())
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
	authed.HandleFunc("/moves", service.MakeMoveHandler).Methods("POST")
	authed.HandleFunc("/challenges", service.CreateChallengeHandler).Methods("POST")
	authed.HandleFunc("/challenge-notifications", service.GetChallengeNotificationsHandler).Methods("GET")
	authed.HandleFunc("/challenge-notifications/{key}", service.DeclineChallengeHandler).Methods("DELETE")
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

	// Graceful shutdown
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		log.Fatal().Err(err).Msg("Server forced to shutdown")
	}

	log.Info().Msg("Server exited")
}

// splitFirehoseURLs parses cfg.Firehose.URL (see FirehoseConfig.URL's doc
// comment) as a comma-separated list of com.atproto.sync.subscribeRepos
// websocket URLs, trimming whitespace and dropping empty entries.
func splitFirehoseURLs(raw string) []string {
	var urls []string
	for _, part := range strings.Split(raw, ",") {
		u := strings.TrimSpace(part)
		if u != "" {
			urls = append(urls, u)
		}
	}
	return urls
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
