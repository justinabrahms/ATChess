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
	"github.com/justinabrahms/atchess/internal/config"
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
	
	// Setup routes
	router := mux.NewRouter()
	
	// Serve static files
	router.PathPrefix("/").Handler(http.FileServer(http.Dir("./web/static/")))
	
	// Create server
	srv := &http.Server{
		Addr:         fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port+1), // Web on port 8081
		Handler:      router,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}
	
	// Start server
	go func() {
		log.Info().Str("addr", srv.Addr).Msg("Starting web server")
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal().Err(err).Msg("Failed to start web server")
		}
	}()
	
	// Wait for interrupt signal
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Info().Msg("Shutting down web server...")
	
	// Graceful shutdown
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	
	if err := srv.Shutdown(ctx); err != nil {
		log.Fatal().Err(err).Msg("Web server forced to shutdown")
	}
	
	log.Info().Msg("Web server exited")
}

func showHelpMessage() {
	fmt.Println(`ATChess Web Server

DESCRIPTION:
    Interactive web interface for the ATChess decentralized chess platform.
    Serves a visual chessboard with drag-and-drop piece movement and real-time
    game state updates. Connects to the ATChess protocol service for game logic
    and AT Protocol storage.

USAGE:
    atchess-web [OPTIONS]

OPTIONS:
    -h, --help    Show this help message

CONFIGURATION:
    The web server is configured via config.yaml in the current directory.
    
    Example config.yaml:
        server:
          host: localhost
          port: 8080        # Protocol service port (web runs on port+1)
        
        atproto:
          pds_url: http://localhost:3000
          handle: "atchess.localhost"
          password: "atchess-service-password"
        
        development:
          debug: true
          log_level: debug

BEHAVIOR:
    - Web server runs on port 8081 (protocol service port + 1)
    - Serves static files from ./web/static/
    - Provides interactive chessboard interface
    - Connects to atchess-protocol service for game operations
    - Graceful shutdown on SIGINT/SIGTERM

EXAMPLES:
    # Start with default configuration
    atchess-web
    
    # Show help
    atchess-web --help

SEE ALSO:
    atchess-protocol(1), config.yaml(5)
    
    Documentation: docs/
    Repository: https://github.com/justinabrahms/atchess`)
}