// Command terminal-relay is the central signaling and offline-queuing server
// for the distributed remote-terminal system.
package main

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
	"syscall"
	"time"

	"terminalrelay/internal/config"
	"terminalrelay/internal/hub"
	"terminalrelay/internal/store"
	"terminalrelay/internal/web"
)

func main() {
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

	// Ensure DB directory exists
	if dir := filepath.Dir(cfg.DBPath); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o755)
	}

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		log.Error("failed to open relay database", "err", err, "path", cfg.DBPath)
		os.Exit(1)
	}
	defer st.Close()

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
  Database:      %s
===========================================================
`, cfg.Port, cfg.Port, cfg.DBPath)

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
