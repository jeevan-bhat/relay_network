// Package config provides configuration parameters and defaults for the Relay server.
package config

import (
	"os"
	"path/filepath"
	"time"
)

// Config holds runtime settings for the Relay server.
type Config struct {
	Port              int
	DBPath            string
	AuthToken         string
	SupabaseURL       string
	SupabaseKey       string
	HeartbeatInterval time.Duration
	HeartbeatDegraded time.Duration // Duration without heartbeat before status becomes DEGRADED
	HeartbeatTimeout  time.Duration // Duration without heartbeat before status becomes OFFLINE
}

// Default returns sensible production-ready defaults.
func Default() Config {
	supabaseKey := os.Getenv("SUPABASE_KEY")
	if supabaseKey == "" {
		supabaseKey = os.Getenv("SUPABASE_SERVICE_ROLE_KEY")
	}
	if supabaseKey == "" {
		supabaseKey = os.Getenv("SUPABASE_ANON_KEY")
	}

	return Config{
		Port:              8080,
		DBPath:            filepath.Join("data", "relay.db"),
		AuthToken:         os.Getenv("AUTH_TOKEN"),
		SupabaseURL:       os.Getenv("SUPABASE_URL"),
		SupabaseKey:       supabaseKey,
		HeartbeatInterval: 15 * time.Second,
		HeartbeatDegraded: 20 * time.Second,
		HeartbeatTimeout:  45 * time.Second,
	}
}
