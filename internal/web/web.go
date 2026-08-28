// Package web provides the embedded single-page Web Dashboard and REST API for the Relay server.
package web

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"strconv"
	"strings"

	"terminalrelay/internal/hub"
	"terminalrelay/internal/protocol"
	"terminalrelay/internal/store"
)

//go:embed static/*
var staticFS embed.FS

// Server attaches the web dashboard and REST API routes to an http.ServeMux.
type Server struct {
	hub   *hub.Hub
	store *store.Store
}

// New creates a new Web Server.
func New(h *hub.Hub, st *store.Store) *Server {
	return &Server{hub: h, store: st}
}

// RegisterRoutes sets up all dashboard and REST API endpoints.
func (s *Server) RegisterRoutes(mux *http.ServeMux) {
	// WebSocket endpoint
	mux.HandleFunc("/ws", s.hub.HandleWS)

	// Health check endpoints for cloud load balancers (Render, Fly.io, Kubernetes)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	// REST API
	mux.HandleFunc("/api/devices", s.handleDevices)
	mux.HandleFunc("/api/commands", s.handleCommands)
	mux.HandleFunc("/api/results", s.handleResults)
	mux.HandleFunc("/api/audit", s.handleAuditLogs)
	mux.HandleFunc("/api/wol", s.handleWakeOnLAN)

	// Static UI assets
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "Dashboard asset error: "+err.Error(), http.StatusInternalServerError)
		})
		return
	}
	fileServer := http.FileServer(http.FS(sub))
	mux.Handle("/", fileServer)
}

func (s *Server) handleDevices(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	devices := s.hub.GetDevices()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(devices)
}

type enqueueReq struct {
	DeviceID   string `json:"deviceId"`
	Command    string `json:"command"`
	TimeoutSec int    `json:"timeoutSec"`
}

func (s *Server) handleCommands(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		var req enqueueReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
		if req.DeviceID == "" || req.Command == "" {
			http.Error(w, "deviceId and command are required", http.StatusBadRequest)
			return
		}

		cmd := protocol.CommandPayload{
			DeviceID:   req.DeviceID,
			Command:    req.Command,
			TimeoutSec: req.TimeoutSec,
		}
		if err := s.hub.EnqueueDirect(cmd); err != nil {
			http.Error(w, "Failed to enqueue: "+err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success":   true,
			"commandId": cmd.CommandID,
			"deviceId":  cmd.DeviceID,
		})

	case http.MethodGet:
		deviceID := r.URL.Query().Get("deviceId")
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit <= 0 {
			limit = 50
		}
		cmds, err := s.store.ListCommands(deviceID, limit)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(cmds)

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleResults(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	commandID := strings.TrimSpace(r.URL.Query().Get("commandId"))
	if commandID != "" {
		res, err := s.store.GetResult(commandID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if res == nil {
			http.Error(w, "Result not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(res)
		return
	}

	deviceID := r.URL.Query().Get("deviceId")
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 50
	}
	results, err := s.store.ListResults(deviceID, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(results)
}

func (s *Server) handleAuditLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	deviceID := r.URL.Query().Get("deviceId")
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 100
	}
	logs, err := s.store.ListAuditLogs(deviceID, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(logs)
}

type wolRequest struct {
	MACAddress  string `json:"macAddress"`
	BroadcastIP string `json:"broadcastIp"`
}

func (s *Server) handleWakeOnLAN(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req wolRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	if req.MACAddress == "" {
		http.Error(w, "macAddress is required (e.g. AA:BB:CC:DD:EE:FF)", http.StatusBadRequest)
		return
	}
	if req.BroadcastIP == "" {
		req.BroadcastIP = "255.255.255.255"
	}

	if err := sendMagicPacket(req.MACAddress, req.BroadcastIP); err != nil {
		http.Error(w, "Failed to send WoL packet: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"success":    true,
		"macAddress": req.MACAddress,
		"broadcast":  req.BroadcastIP + ":9",
		"message":    "Magic packet sent successfully",
	})
}

func sendMagicPacket(macStr, broadcastIP string) error {
	hw, err := net.ParseMAC(macStr)
	if err != nil {
		return fmt.Errorf("invalid MAC address: %w", err)
	}

	// Build 102-byte Magic Packet: 6 bytes 0xFF followed by 16 repetitions of the 6-byte MAC
	packet := make([]byte, 6+16*len(hw))
	for i := 0; i < 6; i++ {
		packet[i] = 0xFF
	}
	for i := 0; i < 16; i++ {
		copy(packet[6+i*len(hw):], hw)
	}

	addr, err := net.ResolveUDPAddr("udp", broadcastIP+":9")
	if err != nil {
		return fmt.Errorf("resolve UDP addr: %w", err)
	}

	conn, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		return fmt.Errorf("dial UDP: %w", err)
	}
	defer conn.Close()

	_, err = conn.Write(packet)
	return err
}


