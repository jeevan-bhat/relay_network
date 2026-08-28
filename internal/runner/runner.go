// Package runner encapsulates the Relay server entrypoint for both root and cmd runners.
package runner

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"terminalrelay/internal/config"
	"terminalrelay/internal/hub"
	"terminalrelay/internal/store"
	"terminalrelay/internal/web"
)

// Run parses flags/environment variables and starts the Relay HTTP/WebSocket server.
func Run() {
	def := config.Default()

	port := flag.Int("port", def.Port, "HTTP/WebSocket listening port")
	dbPath := flag.String("db", def.DBPath, "path to SQLite database")
	token := flag.String("token", def.AuthToken, "secret authentication token (optional)")
	heartbeat := flag.Duration("heartbeat", def.HeartbeatInterval, "expected heartbeat interval")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg := def
	cfg.Port = *port
	cfg.DBPath = *dbPath
	cfg.AuthToken = *token
	cfg.HeartbeatInterval = *heartbeat

	// Support standard Cloud environment variables (Render, Fly.io, Railway, Docker)
	if envPort := os.Getenv("PORT"); envPort != "" {
		if p, err := strconv.Atoi(envPort); err == nil {
			cfg.Port = p
		}
	}
	if envDB := os.Getenv("DB_PATH"); envDB != "" {
		cfg.DBPath = envDB
	}
	if envToken := os.Getenv("AUTH_TOKEN"); envToken != "" {
		cfg.AuthToken = envToken
	}
	if envSupabaseURL := os.Getenv("SUPABASE_URL"); envSupabaseURL != "" {
		cfg.SupabaseURL = envSupabaseURL
	}
	if envSupabaseKey := os.Getenv("SUPABASE_KEY"); envSupabaseKey != "" {
		cfg.SupabaseKey = envSupabaseKey
	} else if envServiceKey := os.Getenv("SUPABASE_SERVICE_ROLE_KEY"); envServiceKey != "" {
		cfg.SupabaseKey = envServiceKey
	}

	// Ensure DB directory exists
	if dir := filepath.Dir(cfg.DBPath); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o755)
	}

	st, err := store.OpenWithConfig(cfg.DBPath, cfg.SupabaseURL, cfg.SupabaseKey)
	if err != nil {
		log.Error("failed to open relay database", "err", err, "path", cfg.DBPath)
		os.Exit(1)
	}
	defer st.Close()

	dbBackend := "Local SQLite (" + cfg.DBPath + ")"
	if st.IsSupabase() {
		dbBackend = "🟢 Supabase Cloud Active (" + cfg.SupabaseURL + ")"
	}

	h := hub.New(st, cfg, log)
	defer h.Close()

	mux := http.NewServeMux()
	webServer := web.New(h, st)
	webServer.RegisterRoutes(mux)

	addr := fmt.Sprintf(":%d", cfg.Port)
	srv := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	// Start server in background
	go func() {
		fmt.Printf(`
===========================================================
  🚀 TERMINAL RELAY SERVER STARTED
  ---------------------------------------------------------
  Web Dashboard: http://localhost:%d
  WebSocket URL: ws://localhost:%d/ws
  Storage Mode:  %s
===========================================================
`, cfg.Port, cfg.Port, dbBackend)

		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server error", "err", err)
			os.Exit(1)
		}
	}()

	// Wait for termination signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	<-sigChan

	log.Info("shutting down relay server...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_ = srv.Shutdown(ctx)
	log.Info("relay server stopped cleanly")
}
