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
	client, err := atproto.NewClient(
		cfg.ATProto.PDSURL,
		cfg.ATProto.Handle,
		cfg.ATProto.Password,
	)
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to create AT Protocol client")
	}
	
	// Create service
	service := web.NewService(client, cfg)
	
	// Setup routes
	router := mux.NewRouter()
	
	// Add CORS middleware
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
			
			if r.Method == "OPTIONS" {
				w.WriteHeader(http.StatusOK)
				return
			}
			
			next.ServeHTTP(w, r)
		})
	})
	
	// API routes
	api := router.PathPrefix("/api").Subrouter()
	api.HandleFunc("/health", service.HealthHandler).Methods("GET")
	api.HandleFunc("/games", service.CreateGameHandler).Methods("POST")
	api.HandleFunc("/games/{id:.*}/moves", service.MakeMoveHandler).Methods("POST")
	api.HandleFunc("/challenges", service.CreateChallengeHandler).Methods("POST")
	
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
    POST /api/games/{id}/moves    - Submit a move to a game
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