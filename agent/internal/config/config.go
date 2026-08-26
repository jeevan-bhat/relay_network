// Package config holds the agent's runtime configuration and defaults.
package config

import (
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
}

// Default returns the baseline configuration. DBPath honors the
// TERMINAL_AGENT_DB environment variable, else falls back to
// %ProgramData%\TerminalAgent\queue.db (shared, writable by the service).
func Default() Config {
	return Config{
		DBPath:       DefaultDBPath(),
		Timeout:      60 * time.Second,
		PollInterval: 1 * time.Second,
		MaxAttempts:  3,
		ExecPolicy:   "Bypass",
	}
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
