// Package config holds the agent's runtime configuration and defaults.
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// Config controls the agent's execution and queueing behavior.
type Config struct {
	// DBPath is the location of the local SQLite queue database.
	DBPath string
	// Timeout is the hard per-command execution timeout.
	Timeout time.Duration
	// PollInterval is how often the worker checks the queue when idle.
	PollInterval time.Duration
	// MaxAttempts caps how many times a single command is retried before FAILED.
	MaxAttempts int
	// ExecPolicy is the PowerShell -ExecutionPolicy value.
	ExecPolicy string
	// RelayURL is the WebSocket URL to the central Relay server (e.g. ws://localhost:8080/ws).
	RelayURL string
	// DeviceID identifies this agent machine. Defaults to OS hostname.
	DeviceID string
	// AuthToken is the shared secret for authenticating to the Relay.
	AuthToken string
	// HeartbeatInterval is the duration between telemetry heartbeats.
	HeartbeatInterval time.Duration
}

// Default returns the baseline configuration. DBPath honors the
// TERMINAL_AGENT_DB environment variable, else falls back to
// %ProgramData%\TerminalAgent\queue.db (shared, writable by the service).
func Default() Config {
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "win-agent"
	}
	cfg := Config{
		DBPath:            DefaultDBPath(),
		Timeout:           60 * time.Second,
		PollInterval:      1 * time.Second,
		MaxAttempts:       3,
		ExecPolicy:        "Bypass",
		RelayURL:          os.Getenv("TERMINAL_RELAY_URL"),
		DeviceID:          hostname,
		AuthToken:         os.Getenv("TERMINAL_AUTH_TOKEN"),
		HeartbeatInterval: 15 * time.Second,
	}

	// Read config.json if present in data directory
	cfgPath := filepath.Join(cfg.DataDir(), "config.json")
	if data, err := os.ReadFile(cfgPath); err == nil {
		type jsonConfig struct {
			RelayURL          string `json:"relay_url"`
			DeviceID          string `json:"device_id"`
			AuthToken         string `json:"auth_token"`
			DBPath            string `json:"db_path"`
			HeartbeatInterval string `json:"heartbeat_interval"`
		}
		var jc jsonConfig
		if err := json.Unmarshal(data, &jc); err == nil {
			if jc.RelayURL != "" {
				cfg.RelayURL = jc.RelayURL
			}
			if jc.DeviceID != "" {
				cfg.DeviceID = jc.DeviceID
			}
			if jc.AuthToken != "" {
				cfg.AuthToken = jc.AuthToken
			}
			if jc.DBPath != "" {
				cfg.DBPath = jc.DBPath
			}
			if jc.HeartbeatInterval != "" {
				if d, err := time.ParseDuration(jc.HeartbeatInterval); err == nil {
					cfg.HeartbeatInterval = d
				}
			}
		}
	}

	if envDev := os.Getenv("TERMINAL_DEVICE_ID"); envDev != "" {
		cfg.DeviceID = envDev
	}

	return cfg
}

// DefaultDBPath resolves the queue database location.
func DefaultDBPath() string {
	if v := os.Getenv("TERMINAL_AGENT_DB"); v != "" {
		return v
	}
	base := os.Getenv("ProgramData")
	if base == "" {
		base = "."
	}
	return filepath.Join(base, "TerminalAgent", "queue.db")
}

// DataDir returns the directory containing the queue database (also used for logs).
func (c Config) DataDir() string {
	return filepath.Dir(c.DBPath)
}
