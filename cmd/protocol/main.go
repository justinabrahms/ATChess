package main

import (
	"context"
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
	
	// API routes
	api := router.PathPrefix("/api").Subrouter()
	api.HandleFunc("/health", service.HealthHandler).Methods("GET")
	api.HandleFunc("/games", service.CreateGameHandler).Methods("POST")
	api.HandleFunc("/games/{id}/moves", service.MakeMoveHandler).Methods("POST")
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