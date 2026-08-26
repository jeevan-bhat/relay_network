// Package config provides configuration parameters and defaults for the Relay server.
package config

import (
	"path/filepath"
	"time"
)

// Config holds runtime settings for the Relay server.
type Config struct {
	Port              int
	DBPath            string
	AuthToken         string
	HeartbeatInterval time.Duration
	HeartbeatDegraded time.Duration // Duration without heartbeat before status becomes DEGRADED
	HeartbeatTimeout  time.Duration // Duration without heartbeat before status becomes OFFLINE
}

// Default returns sensible production-ready defaults.
func Default() Config {
	return Config{
		Port:              8080,
		DBPath:            filepath.Join("data", "relay.db"),
		AuthToken:         "", // if set, clients must match this token
		HeartbeatInterval: 15 * time.Second,
		HeartbeatDegraded: 20 * time.Second,
		HeartbeatTimeout:  45 * time.Second,
	}
}
