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
	
	// Create WebSocket hub
	hub := web.NewHub()
	go hub.Run()
	
	// Create service
	service := web.NewService(client, cfg)
	
	// Initialize OAuth if base URL is configured
	if cfg.Server.BaseURL != "" {
		if err := web.InitializeOAuth(cfg.Server.BaseURL); err != nil {
			log.Error().Err(err).Msg("Failed to initialize OAuth, falling back to password auth")
		}
	}
	
	// Create firehose processor
	processor := firehose.NewEventProcessor(hub)
	
	// Start firehose client (optional - can be disabled in config)
	if cfg.Firehose.Enabled {
		firehoseClient := firehose.NewClient(
			firehose.CreateChessEventHandler(processor),
			firehose.WithURL(cfg.Firehose.URL),
		)
		
		go func() {
			log.Info().Str("url", cfg.Firehose.URL).Msg("Starting firehose client")
			if err := firehoseClient.Start(); err != nil {
				log.Error().Err(err).Msg("Firehose client error")
			}
		}()
		
		// Track the current user's games
		processor.TrackPlayer(client.GetDID())
	}
	
	// Setup routes
	router := mux.NewRouter()
	
	// Add CORS middleware
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Session-ID")
			
			if r.Method == "OPTIONS" {
				w.WriteHeader(http.StatusOK)
				return
			}
			
			next.ServeHTTP(w, r)
		})
	})
	
	// Root level health endpoint for load balancers and monitoring
	router.HandleFunc("/health", service.HealthHandler).Methods("GET")
	
	// OAuth callback must be registered before the catch-all static handler
	router.HandleFunc("/callback", service.OAuthCallbackHandler).Methods("GET")
	
	// API routes
	api := router.PathPrefix("/api").Subrouter()
	api.HandleFunc("/health", service.HealthHandler).Methods("GET")
	api.HandleFunc("/auth/login", service.LoginHandler).Methods("POST")
	api.HandleFunc("/auth/current", service.GetCurrentUserHandler).Methods("GET")
	api.HandleFunc("/auth/oauth/login", service.OAuthLoginHandler).Methods("POST")
	api.HandleFunc("/auth/session", service.GetSessionHandler).Methods("GET")
	api.HandleFunc("/auth/logout", service.LogoutHandler).Methods("POST")
	api.HandleFunc("/games", service.CreateGameHandler).Methods("POST")
	api.HandleFunc("/games/{id:.*}", service.GetGameHandler).Methods("GET")
	api.HandleFunc("/moves", service.MakeMoveHandler).Methods("POST")
	api.HandleFunc("/challenges", service.CreateChallengeHandler).Methods("POST")
	api.HandleFunc("/challenge-notifications", service.GetChallengeNotificationsHandler).Methods("GET")
	api.HandleFunc("/challenge-notifications/{key}", service.DeleteChallengeNotificationHandler).Methods("DELETE")
	api.HandleFunc("/draw-offers", service.OfferDrawHandler).Methods("POST")
	api.HandleFunc("/draw-offers/respond", service.RespondToDrawHandler).Methods("POST")
	api.HandleFunc("/resign", service.ResignGameHandler).Methods("POST")
	
	// Spectator endpoints
	api.HandleFunc("/spectator/games", service.GetActiveGamesHandler).Methods("GET")
	api.HandleFunc("/spectator/games/{id:.*}", service.GetSpectatorGameHandler).Methods("GET")
	api.HandleFunc("/spectator/games/{id:.*}/count", service.UpdateSpectatorCountHandler(hub)).Methods("POST")
	api.HandleFunc("/spectator/games/{id:.*}/abandonment", service.CheckAbandonmentHandler).Methods("GET")
	api.HandleFunc("/spectator/games/{id:.*}/claim-abandonment", service.ClaimAbandonedGameHandler).Methods("POST")
	
	// Time control endpoints
	api.HandleFunc("/games/{id:.*}/time-violation", service.CheckTimeViolationHandler).Methods("GET")
	api.HandleFunc("/games/{id:.*}/claim-time", service.ClaimTimeVictoryHandler).Methods("POST")
	api.HandleFunc("/games/{id:.*}/time-remaining", service.GetTimeRemainingHandler).Methods("GET")
	
	// WebSocket endpoint for real-time updates
	api.HandleFunc("/ws", service.WebSocketHandler(hub))
	
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
	router.PathPrefix("/").Handler(http.FileServer(http.Dir("./web/static/")))
	
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