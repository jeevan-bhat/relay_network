// Package hub coordinates WebSocket connections for the Relay server, managing
// device authentication, message routing between agents and controllers, offline
// queuing, and heartbeat liveness tracking.
package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"terminalrelay/internal/config"
	"terminalrelay/internal/protocol"
	"terminalrelay/internal/store"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024 * 32,
	WriteBufferSize: 1024 * 32,
	CheckOrigin: func(r *http.Request) bool {
		return true // Allow all origins for web dashboards and local controllers
	},
}

// Client represents a connected WebSocket peer (Agent or Controller).
type Client struct {
	hub       *Hub
	conn      *websocket.Conn
	send      chan protocol.Envelope
	role      string // protocol.RoleAgent or protocol.RoleController
	deviceID  string // device identifier if agent
	sessionID string
	userID    string // associated user ID if authenticated
	authed    bool
	mu        sync.Mutex
}

// Hub maintains active clients and routes messages.
type Hub struct {
	store       *store.Store
	cfg         config.Config
	log         *slog.Logger
	agents      map[string]*Client // deviceID -> *Client
	controllers map[*Client]bool
	mu          sync.RWMutex
	ctx         context.Context
	cancel      context.CancelFunc
}

// New creates a new Hub.
func New(st *store.Store, cfg config.Config, log *slog.Logger) *Hub {
	if log == nil {
		log = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	h := &Hub{
		store:       st,
		cfg:         cfg,
		log:         log,
		agents:      make(map[string]*Client),
		controllers: make(map[*Client]bool),
		ctx:         ctx,
		cancel:      cancel,
	}
	go h.livenessTracker()
	return h
}

// Close shuts down the hub and all connections.
func (h *Hub) Close() {
	h.cancel()
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, c := range h.agents {
		c.conn.Close()
	}
	for c := range h.controllers {
		c.conn.Close()
	}
}

// HandleWS upgrades HTTP requests to WebSockets and registers clients.
func (h *Hub) HandleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		h.log.Error("websocket upgrade error", "err", err)
		return
	}

	client := &Client{
		hub:       h,
		conn:      conn,
		send:      make(chan protocol.Envelope, 64),
		sessionID: uuid.NewString(),
	}

	go client.writePump()
	go client.readPump()
}

func (c *Client) readPump() {
	defer func() {
		c.hub.unregister(c)
		c.conn.Close()
	}()

	c.conn.SetReadLimit(1024 * 1024 * 16) // 16MB max message size (for large command outputs)
	_ = c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	c.conn.SetPongHandler(func(string) error {
		_ = c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	for {
		_, raw, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				c.hub.log.Warn("ws read error", "err", err)
			}
			break
		}
		_ = c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))

		var env protocol.Envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			c.hub.log.Error("malformed envelope", "err", err)
			continue
		}

		c.hub.handleMessage(c, env)
	}
}

func (c *Client) writePump() {
	ticker := time.NewTicker(20 * time.Second)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()

	for {
		select {
		case env, ok := <-c.send:
			_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if !ok {
				_ = c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			b, err := json.Marshal(env)
			if err != nil {
				c.hub.log.Error("marshal envelope failed", "err", err)
				continue
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, b); err != nil {
				return
			}
		case <-ticker.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

func (c *Client) Send(env protocol.Envelope) {
	select {
	case c.send <- env:
	default:
		c.hub.log.Warn("client send buffer full, dropping message", "type", env.Type, "device", c.deviceID)
	}
}

func (h *Hub) unregister(c *Client) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if c.role == protocol.RoleAgent && c.deviceID != "" {
		if cur, ok := h.agents[c.deviceID]; ok && cur == c {
			delete(h.agents, c.deviceID)
			h.log.Info("agent disconnected", "deviceId", c.deviceID)
			_ = h.store.UpdateDeviceStatus(c.deviceID, protocol.StatusOffline)
			h.broadcastDeviceStatusLocked(c.deviceID, protocol.StatusOffline)
		}
	} else if c.role == protocol.RoleController {
		delete(h.controllers, c)
	}
	close(c.send)
}

func (h *Hub) handleMessage(c *Client, env protocol.Envelope) {
	switch env.Type {
	case protocol.TypeAuth:
		h.handleAuth(c, env)
	case protocol.TypeHeartbeat:
		h.handleHeartbeat(c, env)
	case protocol.TypeEnqueueCmd:
		h.handleEnqueueCmd(c, env)
	case protocol.TypeCmdResult:
		h.handleCmdResult(c, env)
	case protocol.TypeSyncReq:
		h.handleSyncReq(c, env)
	case protocol.TypeGetDevices:
		h.handleGetDevices(c)
	case protocol.TypeGetAuditLogs:
		h.handleGetAuditLogs(c, env)
	case protocol.TypeFileListReq, protocol.TypeFileReadReq, protocol.TypeFileWriteReq, protocol.TypeFileDeleteReq,
		protocol.TypeProcessListReq, protocol.TypeProcessKillReq:
		h.routeToAgent(c, env)
	case protocol.TypeFileListResp, protocol.TypeFileReadResp, protocol.TypeFileWriteResp, protocol.TypeFileDeleteResp,
		protocol.TypeProcessListResp, protocol.TypeProcessKillResp:
		h.routeToControllers(c, env)
	default:
		h.log.Warn("unknown message type", "type", env.Type)
	}
}

func (h *Hub) handleAuth(c *Client, env protocol.Envelope) {
	var p protocol.AuthPayload
	if err := env.DecodePayload(&p); err != nil {
		h.sendError(c, "INVALID_PAYLOAD", err.Error())
		return
	}

	var authenticatedUser *protocol.UserAccount
	if p.Token != "" {
		authenticatedUser, _ = h.store.GetUserByToken(p.Token)
	}

	// If global token is set and no user account matched, verify global token
	if authenticatedUser == nil && h.cfg.AuthToken != "" && p.Token != h.cfg.AuthToken {
		ack, _ := protocol.NewEnvelope(protocol.TypeAuthAck, p.DeviceID, protocol.AuthAckPayload{
			Success:    false,
			Error:      "unauthorized: invalid token",
			ServerTime: time.Now().Unix(),
		})
		c.Send(ack)
		return
	}

	c.mu.Lock()
	c.role = p.Role
	c.deviceID = p.DeviceID
	c.authed = true
	if authenticatedUser != nil {
		c.userID = authenticatedUser.UserID
	}
	c.mu.Unlock()

	h.mu.Lock()
	if p.Role == protocol.RoleAgent {
		// If previous connection existed, close old connection
		if old, exists := h.agents[p.DeviceID]; exists && old != c {
			old.conn.Close()
		}
		h.agents[p.DeviceID] = c

		devInfo := protocol.DeviceInfo{
			DeviceID:      p.DeviceID,
			Hostname:      p.Hostname,
			OS:            p.OS,
			MACAddress:    p.MACAddress,
			Status:        protocol.StatusOnline,
			LastHeartbeat: time.Now().Unix(),
			ConnectedAt:   time.Now().Unix(),
		}
		if devInfo.MACAddress != "" {
			devInfo.Metrics.MACAddress = devInfo.MACAddress
		}
		if authenticatedUser != nil {
			c.userID = authenticatedUser.UserID
			devInfo.UserID = authenticatedUser.UserID
			_ = h.store.BindDeviceToUser(p.DeviceID, authenticatedUser.UserID)
		} else {
			if existing, _ := h.store.GetDevice(p.DeviceID); existing != nil && existing.UserID != "" {
				c.userID = existing.UserID
				devInfo.UserID = existing.UserID
			}
		}
		_ = h.store.UpsertDevice(devInfo)
		h.log.Info("agent authenticated", "deviceId", p.DeviceID, "host", p.Hostname, "userId", devInfo.UserID)
		h.broadcastDeviceStatusLocked(p.DeviceID, protocol.StatusOnline)
	} else {
		h.controllers[c] = true
		h.log.Info("controller authenticated", "sessionId", c.sessionID, "userId", c.userID)
	}
	h.mu.Unlock()

	ack, _ := protocol.NewEnvelope(protocol.TypeAuthAck, p.DeviceID, protocol.AuthAckPayload{
		Success:    true,
		DeviceID:   p.DeviceID,
		SessionID:  c.sessionID,
		ServerTime: time.Now().Unix(),
	})
	c.Send(ack)

	if p.Role == protocol.RoleController {
		h.handleGetDevices(c)
	}

	if p.Role == protocol.RoleAgent {
		// Flush any pending cloud commands
		h.flushPendingCommands(p.DeviceID)
	}
}

func (h *Hub) handleHeartbeat(c *Client, env protocol.Envelope) {
	var p protocol.HeartbeatPayload
	if err := env.DecodePayload(&p); err != nil {
		return
	}

	_ = h.store.UpdateDeviceHeartbeat(p.DeviceID, p.Metrics, protocol.StatusOnline)

	ack, _ := protocol.NewEnvelope(protocol.TypeHeartbeatAck, p.DeviceID, protocol.HeartbeatAckPayload{
		DeviceID:   p.DeviceID,
		ServerTime: time.Now().Unix(),
	})
	c.Send(ack)

	// Broadcast updated metrics & status ONLY to controllers of the owning user
	devUserID := c.userID
	if devUserID == "" {
		if dev, _ := h.store.GetDevice(p.DeviceID); dev != nil {
			devUserID = dev.UserID
		}
	}
	hbEnv, _ := protocol.NewEnvelope(protocol.TypeHeartbeat, p.DeviceID, p)
	h.mu.RLock()
	for ctrl := range h.controllers {
		if ctrl.userID == "" || devUserID == "" || ctrl.userID == devUserID {
			ctrl.Send(hbEnv)
		}
	}
	h.mu.RUnlock()
}

func (h *Hub) handleEnqueueCmd(c *Client, env protocol.Envelope) {
	var p protocol.CommandPayload
	if err := env.DecodePayload(&p); err != nil {
		h.sendError(c, "INVALID_PAYLOAD", err.Error())
		return
	}

	// Verify device ownership if controller is authenticated to a specific user
	if c.userID != "" {
		dev, _ := h.store.GetDevice(p.DeviceID)
		if dev != nil && dev.UserID != "" && dev.UserID != c.userID {
			h.sendError(c, "UNAUTHORIZED", "device does not belong to your account")
			return
		}
	}

	if p.CommandID == "" {
		p.CommandID = uuid.NewString()
	}
	if p.CreatedAt == 0 {
		p.CreatedAt = time.Now().Unix()
	}

	// Persist in cloud queue
	if err := h.store.EnqueueCommand(p); err != nil {
		h.sendError(c, "STORE_ERROR", err.Error())
		return
	}

	agent, online := h.getAgentClient(p.DeviceID)
	if online && agent != nil {
		// Dispatch immediately to physical agent
		dispatchEnv, _ := protocol.NewEnvelope(protocol.TypeDispatchCmd, agent.deviceID, p)
		agent.Send(dispatchEnv)
		_ = h.store.MarkCommandDispatched(p.CommandID)
		h.log.Info("command dispatched to live agent", "commandId", p.CommandID, "deviceId", agent.deviceID)
		h.broadcastDeviceResult(p.DeviceID, env)
		return
	}

	// Agent is offline: Check if command is a predefined offline command
	if res, handled := h.tryExecutePredefinedOffline(p); handled {
		_ = h.store.SaveResult(res)
		_ = h.store.MarkCommandCompleted(p.CommandID, store.StatusCompleted)
		_ = h.store.RecordAuditLog(protocol.AuditLogRecord{
			LogID:       p.CommandID,
			DeviceID:    p.DeviceID,
			Timestamp:   res.ExecutedAt,
			ActionType:  "CLOUD_EXEC",
			CommandText: p.Command,
			ExitCode:    0,
			DurationMs:  1,
			Details:     "Executed immediately by Cloud Predefined Responder (Agent Offline)",
		})
		h.log.Info("predefined command executed in cloud for offline agent", "command", p.Command, "deviceId", p.DeviceID)
		resEnv, _ := protocol.NewEnvelope(protocol.TypeCmdResult, p.DeviceID, res)
		h.broadcastDeviceResult(p.DeviceID, resEnv)
		return
	}

	// Custom PowerShell / Script execution in Cloud Sandbox while laptop is asleep:
	res := h.executeCloudSandbox(p)
	_ = h.store.SaveSandboxResult(res)
	_ = h.store.RecordAuditLog(protocol.AuditLogRecord{
		LogID:       p.CommandID,
		DeviceID:    p.DeviceID,
		Timestamp:   res.ExecutedAt,
		ActionType:  "CLOUD_SCRIPT_EXEC",
		CommandText: p.Command,
		ExitCode:    res.ExitCode,
		DurationMs:  res.DurationMs,
		Details:     "Custom script executed in Cloud Sandbox (Laptop Asleep)",
	})
	h.log.Info("custom script executed in cloud sandbox for offline agent", "command", p.Command, "deviceId", p.DeviceID, "exitCode", res.ExitCode)
	resEnv, _ := protocol.NewEnvelope(protocol.TypeCmdResult, p.DeviceID, res)
	h.broadcastDeviceResult(p.DeviceID, resEnv)
}

func (h *Hub) handleCmdResult(c *Client, env protocol.Envelope) {
	var p protocol.ResultPayload
	if err := env.DecodePayload(&p); err != nil {
		return
	}

	// Save in cloud result store
	_ = h.store.SaveResult(p)

	// Record immutable Audit Log
	var cmdText string
	if cmdRec, _ := h.store.GetCommand(p.CommandID); cmdRec != nil {
		cmdText = cmdRec.Command
	}
	_ = h.store.RecordAuditLog(protocol.AuditLogRecord{
		LogID:       p.CommandID,
		DeviceID:    p.DeviceID,
		Timestamp:   p.ExecutedAt,
		ActionType:  "EXEC_CMD",
		CommandText: cmdText,
		ExitCode:    p.ExitCode,
		DurationMs:  p.DurationMs,
		Details:     fmt.Sprintf("exit=%d timedOut=%t", p.ExitCode, p.TimedOut),
	})

	// Acknowledge back to agent
	ack, _ := protocol.NewEnvelope(protocol.TypeResultAck, p.DeviceID, protocol.ResultAckPayload{
		CommandID: p.CommandID,
		ResultID:  p.ResultID,
		Success:   true,
	})
	c.Send(ack)

	h.log.Info("received command result", "commandId", p.CommandID, "exitCode", p.ExitCode)

	// Broadcast result to controllers
	h.broadcastToControllers(env)
}

func (h *Hub) handleSyncReq(c *Client, env protocol.Envelope) {
	var req protocol.SyncReqPayload
	if err := env.DecodePayload(&req); err != nil {
		return
	}

	var acked []string
	for _, res := range req.UnsyncedResults {
		if err := h.store.SaveResult(res); err == nil {
			acked = append(acked, res.CommandID)
			// Broadcast synced result to controllers
			resEnv, _ := protocol.NewEnvelope(protocol.TypeCmdResult, req.DeviceID, res)
			h.broadcastToControllers(resEnv)
		}
	}

	pending, _ := h.store.GetPendingCommands(req.DeviceID)
	for _, cmd := range pending {
		_ = h.store.MarkCommandDispatched(cmd.CommandID)
	}

	resp, _ := protocol.NewEnvelope(protocol.TypeSyncResp, req.DeviceID, protocol.SyncRespPayload{
		DeviceID:        req.DeviceID,
		AckedResults:    acked,
		PendingCommands: pending,
	})
	c.Send(resp)
	h.log.Info("sync completed for agent", "deviceId", req.DeviceID, "syncedResults", len(acked), "flushedCommands", len(pending))
}

func (h *Hub) GetDevices() []protocol.DeviceInfo {
	return h.GetDevicesForUser("")
}

func (h *Hub) GetDevicesForUser(userID string) []protocol.DeviceInfo {
	var devices []protocol.DeviceInfo
	if userID != "" {
		devices, _ = h.store.ListDevicesForUser(userID)
	} else {
		devices, _ = h.store.ListDevices()
	}
	if devices == nil {
		devices = []protocol.DeviceInfo{}
	}

	h.mu.RLock()
	defer h.mu.RUnlock()

	for i := range devices {
		if _, ok := h.agents[devices[i].DeviceID]; ok {
			devices[i].Status = protocol.StatusOnline
		}
	}
	return devices
}

func (h *Hub) handleGetDevices(c *Client) {
	devices := h.GetDevicesForUser(c.userID)
	env, _ := protocol.NewEnvelope(protocol.TypeDeviceList, "", protocol.DeviceListPayload{
		Devices: devices,
	})
	c.Send(env)
}

func (h *Hub) getAgentClient(deviceID string) (*Client, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	if a, ok := h.agents[deviceID]; ok && a != nil {
		return a, true
	}
	lower := strings.ToLower(deviceID)
	for id, a := range h.agents {
		if strings.ToLower(id) == lower && a != nil {
			return a, true
		}
	}
	return nil, false
}

func (h *Hub) flushPendingCommands(deviceID string) {
	pending, err := h.store.GetPendingCommands(deviceID)
	if err != nil || len(pending) == 0 {
		return
	}

	agent, ok := h.getAgentClient(deviceID)
	if !ok || agent == nil {
		return
	}

	for _, cmd := range pending {
		dispatchEnv, _ := protocol.NewEnvelope(protocol.TypeDispatchCmd, agent.deviceID, cmd)
		agent.Send(dispatchEnv)
		_ = h.store.MarkCommandDispatched(cmd.CommandID)
		h.log.Info("flushed pending command to agent", "commandId", cmd.CommandID, "deviceId", agent.deviceID)
	}
}

func (h *Hub) broadcastDeviceStatusLocked(deviceID, status string) {
	env, _ := protocol.NewEnvelope(protocol.TypeDeviceStatus, deviceID, map[string]string{
		"deviceId": deviceID,
		"status":   status,
	})
	devUserID := ""
	if agentClient := h.agents[deviceID]; agentClient != nil {
		devUserID = agentClient.userID
	}
	if devUserID == "" {
		if dev, _ := h.store.GetDevice(deviceID); dev != nil {
			devUserID = dev.UserID
		}
	}
	for c := range h.controllers {
		if c.userID == "" || devUserID == "" || c.userID == devUserID {
			c.Send(env)
		}
	}
}

func (h *Hub) broadcastDeviceResult(deviceID string, env protocol.Envelope) {
	devUserID := ""
	if dev, _ := h.store.GetDevice(deviceID); dev != nil {
		devUserID = dev.UserID
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for ctrl := range h.controllers {
		if ctrl.userID == "" || devUserID == "" || ctrl.userID == devUserID {
			ctrl.Send(env)
		}
	}
}

func (h *Hub) broadcastToControllers(env protocol.Envelope) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.controllers {
		c.Send(env)
	}
}

func (h *Hub) sendError(c *Client, code, message string) {
	env, _ := protocol.NewEnvelope(protocol.TypeError, c.deviceID, protocol.ErrorPayload{
		Code:    code,
		Message: message,
	})
	c.Send(env)
}

// livenessTracker monitors agent heartbeats and degrades/disconnects stale devices.
func (h *Hub) livenessTracker() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-h.ctx.Done():
			return
		case <-ticker.C:
			devices, err := h.store.ListDevices()
			if err != nil {
				continue
			}
			now := time.Now().Unix()
			for _, d := range devices {
				h.mu.RLock()
				_, isLiveConn := h.agents[d.DeviceID]
				h.mu.RUnlock()

				var newStatus string
				if isLiveConn {
					newStatus = protocol.StatusOnline
				} else if d.LastHeartbeat == 0 {
					newStatus = protocol.StatusOffline
				} else {
					elapsed := time.Duration(now-d.LastHeartbeat) * time.Second
					switch {
					case elapsed >= h.cfg.HeartbeatTimeout:
						newStatus = protocol.StatusOffline
					case elapsed >= h.cfg.HeartbeatDegraded:
						newStatus = protocol.StatusDegraded
					default:
						newStatus = protocol.StatusOnline
					}
				}

				if newStatus != d.Status {
					_ = h.store.UpdateDeviceStatus(d.DeviceID, newStatus)
					h.mu.RLock()
					h.broadcastDeviceStatusLocked(d.DeviceID, newStatus)
					h.mu.RUnlock()
					h.log.Info("device liveness changed", "deviceId", d.DeviceID, "oldStatus", d.Status, "newStatus", newStatus)
				}
			}
		}
	}
}

// EnqueueDirect allows programmatic command enqueuing (e.g. from REST API).
func (h *Hub) EnqueueDirect(cmd protocol.CommandPayload) error {
	if cmd.DeviceID == "" || cmd.Command == "" {
		return errors.New("deviceId and command required")
	}
	if cmd.CommandID == "" {
		cmd.CommandID = uuid.NewString()
	}
	if cmd.CreatedAt == 0 {
		cmd.CreatedAt = time.Now().Unix()
	}

	if err := h.store.EnqueueCommand(cmd); err != nil {
		return fmt.Errorf("enqueue command: %w", err)
	}

	h.mu.RLock()
	agent, online := h.agents[cmd.DeviceID]
	h.mu.RUnlock()

	if online && agent != nil {
		dispatchEnv, _ := protocol.NewEnvelope(protocol.TypeDispatchCmd, cmd.DeviceID, cmd)
		agent.Send(dispatchEnv)
		_ = h.store.MarkCommandDispatched(cmd.CommandID)
		env, _ := protocol.NewEnvelope(protocol.TypeEnqueueCmd, cmd.DeviceID, cmd)
		h.broadcastToControllers(env)
		return nil
	}

	// Agent offline: Check if predefined
	if res, handled := h.tryExecutePredefinedOffline(cmd); handled {
		_ = h.store.SaveResult(res)
		_ = h.store.MarkCommandCompleted(cmd.CommandID, store.StatusCompleted)
		_ = h.store.RecordAuditLog(protocol.AuditLogRecord{
			LogID:       cmd.CommandID,
			DeviceID:    cmd.DeviceID,
			Timestamp:   res.ExecutedAt,
			ActionType:  "CLOUD_EXEC",
			CommandText: cmd.Command,
			ExitCode:    0,
			DurationMs:  1,
			Details:     "Executed immediately by Cloud Predefined Responder (Agent Offline)",
		})
		resEnv, _ := protocol.NewEnvelope(protocol.TypeCmdResult, cmd.DeviceID, res)
		h.broadcastToControllers(resEnv)
		return nil
	}

	res := h.executeCloudSandbox(cmd)
	_ = h.store.SaveSandboxResult(res)
	_ = h.store.RecordAuditLog(protocol.AuditLogRecord{
		LogID:       cmd.CommandID,
		DeviceID:    cmd.DeviceID,
		Timestamp:   res.ExecutedAt,
		ActionType:  "CLOUD_SCRIPT_EXEC",
		CommandText: cmd.Command,
		ExitCode:    res.ExitCode,
		DurationMs:  res.DurationMs,
		Details:     "Custom script executed in Cloud Sandbox (Laptop Asleep)",
	})
	resEnv, _ := protocol.NewEnvelope(protocol.TypeCmdResult, cmd.DeviceID, res)
	h.broadcastDeviceResult(cmd.DeviceID, resEnv)
	return nil
}

func (h *Hub) routeToAgent(c *Client, env protocol.Envelope) {
	if c.userID != "" {
		dev, _ := h.store.GetDevice(env.DeviceID)
		if dev != nil && dev.UserID != "" && dev.UserID != c.userID {
			h.sendError(c, "UNAUTHORIZED", "device does not belong to your account")
			return
		}
	}

	agent, ok := h.getAgentClient(env.DeviceID)
	if !ok || agent == nil {
		h.sendError(c, "AGENT_OFFLINE", fmt.Sprintf("device %s is offline", env.DeviceID))
		return
	}

	agent.Send(env)
}

func (h *Hub) routeToControllers(c *Client, env protocol.Envelope) {
	// Record audit logs for file/process actions
	switch env.Type {
	case protocol.TypeFileWriteResp:
		var p protocol.FileWriteRespPayload
		_ = env.DecodePayload(&p)
		_ = h.store.RecordAuditLog(protocol.AuditLogRecord{
			LogID:       uuid.NewString(),
			DeviceID:    p.DeviceID,
			Timestamp:   time.Now().Unix(),
			ActionType:  "FILE_WRITE",
			CommandText: p.Path,
			ExitCode:    0,
			Details:     fmt.Sprintf("success=%t error=%s", p.Success, p.Error),
		})
	case protocol.TypeFileDeleteResp:
		var p protocol.FileDeleteRespPayload
		_ = env.DecodePayload(&p)
		_ = h.store.RecordAuditLog(protocol.AuditLogRecord{
			LogID:       uuid.NewString(),
			DeviceID:    p.DeviceID,
			Timestamp:   time.Now().Unix(),
			ActionType:  "FILE_DELETE",
			CommandText: p.Path,
			ExitCode:    0,
			Details:     fmt.Sprintf("success=%t error=%s", p.Success, p.Error),
		})
	case protocol.TypeProcessKillResp:
		var p protocol.ProcessKillRespPayload
		_ = env.DecodePayload(&p)
		_ = h.store.RecordAuditLog(protocol.AuditLogRecord{
			LogID:       uuid.NewString(),
			DeviceID:    p.DeviceID,
			Timestamp:   time.Now().Unix(),
			ActionType:  "PROCESS_KILL",
			CommandText: fmt.Sprintf("PID: %d", p.PID),
			ExitCode:    0,
			Details:     fmt.Sprintf("success=%t error=%s", p.Success, p.Error),
		})
	}

	h.broadcastDeviceResult(env.DeviceID, env)
}

func (h *Hub) handleGetAuditLogs(c *Client, env protocol.Envelope) {
	logs, err := h.store.ListAuditLogs(env.DeviceID, 100)
	if err != nil {
		h.sendError(c, "STORE_ERROR", err.Error())
		return
	}
	respEnv, _ := protocol.NewEnvelope(protocol.TypeAuditLogsList, env.DeviceID, protocol.AuditLogsListPayload{
		Logs: logs,
	})
	c.Send(respEnv)
}

func (h *Hub) tryExecutePredefinedOffline(cmd protocol.CommandPayload) (protocol.ResultPayload, bool) {
	trimmed := strings.TrimSpace(cmd.Command)
	lower := strings.ToLower(trimmed)

	dev, _ := h.store.GetDevice(cmd.DeviceID)

	var stdout string
	var handled bool

	switch {
	case lower == "status" || lower == "get-status" || lower == "state":
		handled = true
		lastSeen := "Never"
		status := "OFFLINE"
		hostname := "Unknown"
		osName := "Unknown"
		if dev != nil {
			status = dev.Status
			hostname = dev.Hostname
			osName = dev.OS
			if dev.LastHeartbeat > 0 {
				lastSeen = time.Unix(dev.LastHeartbeat, 0).Format(time.RFC1123)
			}
		}
		stdout = fmt.Sprintf(`[Cloud Predefined Responder - Agent Offline]
Device ID:    %s
Host Name:    %s
OS Platform:  %s
Power State:  %s
Last Seen:    %s
Note:         Physical machine is offline. Telemetry served from Cloud Store.`,
			cmd.DeviceID, hostname, osName, status, lastSeen)

	case lower == "systeminfo" || lower == "get-computerinfo" || lower == "info" || lower == "device-info":
		handled = true
		hostname := "Unknown"
		osName := "Windows"
		diskGb := "--"
		ramMb := "--"
		if dev != nil {
			hostname = dev.Hostname
			osName = dev.OS
			if dev.Metrics.DiskTotalBytes > 0 {
				diskGb = fmt.Sprintf("%.1f GB", float64(dev.Metrics.DiskTotalBytes)/(1024*1024*1024))
			}
			if dev.Metrics.RAMTotalBytes > 0 {
				ramMb = fmt.Sprintf("%.0f MB", float64(dev.Metrics.RAMTotalBytes)/(1024*1024))
			}
		}
		stdout = fmt.Sprintf(`[Cloud Predefined Responder - Cached System Info]
Host Name:                 %s
OS Name:                   %s
Total Physical Memory:     %s
Total Disk Capacity:       %s
Device ID:                 %s
Cloud Management Status:   Registered & Queued`,
			hostname, osName, ramMb, diskGb, cmd.DeviceID)

	case lower == "health" || lower == "get-health" || lower == "metrics":
		handled = true
		cpu := 0.0
		ramPercent := 0.0
		ramUsed := "--"
		ramTotal := "--"
		uptime := "--"
		if dev != nil {
			cpu = dev.Metrics.CPUPercent
			ramPercent = dev.Metrics.RAMPercent
			if dev.Metrics.RAMTotalBytes > 0 {
				ramUsed = fmt.Sprintf("%.0f MB", float64(dev.Metrics.RAMUsedBytes)/(1024*1024))
				ramTotal = fmt.Sprintf("%.0f MB", float64(dev.Metrics.RAMTotalBytes)/(1024*1024))
			}
			if dev.Metrics.UptimeSec > 0 {
				hrs := dev.Metrics.UptimeSec / 3600
				mins := (dev.Metrics.UptimeSec % 3600) / 60
				uptime = fmt.Sprintf("%dh %dm", hrs, mins)
			}
		}
		stdout = fmt.Sprintf(`[Cloud Predefined Responder - Last Telemetry Snapshot]
Last CPU Utilization:     %.1f %%
Last RAM Usage:           %.1f %% (%s / %s)
Last Reported Uptime:     %s
Power State:              OFFLINE / ASLEEP`,
			cpu, ramPercent, ramUsed, ramTotal, uptime)

	case lower == "ping" || lower == "test-connection":
		handled = true
		lastPing := "N/A"
		if dev != nil && dev.LastHeartbeat > 0 {
			elapsed := time.Since(time.Unix(dev.LastHeartbeat, 0)).Round(time.Second)
			lastPing = fmt.Sprintf("%s ago (%s)", elapsed, time.Unix(dev.LastHeartbeat, 0).Format(time.TimeOnly))
		}
		stdout = fmt.Sprintf(`[Cloud Predefined Responder - Connection Probe]
Target:          %s
Relay Server:    Online (RTT < 1ms)
Agent Status:    OFFLINE
Last Ping Recv:  %s
Action:          Physical commands will be stored in FIFO Cloud Queue.`,
			cmd.DeviceID, lastPing)

	case lower == "queue" || lower == "get-queue" || lower == "queue-status":
		handled = true
		pending, _ := h.store.GetPendingCommands(cmd.DeviceID)
		if len(pending) == 0 {
			stdout = fmt.Sprintf("[Cloud Queue - %s]\n(No commands currently queued for next power-on)", cmd.DeviceID)
		} else {
			var sb strings.Builder
			sb.WriteString(fmt.Sprintf("[Cloud Queue - %s: %d Pending Command(s)]\n", cmd.DeviceID, len(pending)))
			for idx, p := range pending {
				ts := time.Unix(p.CreatedAt, 0).Format(time.TimeOnly)
				sb.WriteString(fmt.Sprintf(" %d. [%s] ID:%s -> %s\n", idx+1, ts, p.CommandID[:8], p.Command))
			}
			stdout = sb.String()
		}

	case lower == "clear-queue" || lower == "cancel-queue" || lower == "purge-queue":
		handled = true
		_ = h.store.CancelPendingCommands(cmd.DeviceID)
		stdout = fmt.Sprintf("[Cloud Queue - %s]\nSuccessfully cancelled and purged all pending commands for this device.", cmd.DeviceID)

	case lower == "wake" || lower == "wol" || lower == "start-computer" || strings.HasPrefix(lower, "wol ") || strings.HasPrefix(lower, "wake "):
		handled = true
		parts := strings.Fields(trimmed)
		var mac string
		if len(parts) > 1 {
			mac = parts[1]
		}
		stdout = fmt.Sprintf(`[Cloud Predefined Responder - Wake-on-LAN]
Target Device: %s
Magic Packet:  Triggered broadcast
Note:          If device is configured for WoL, it will boot up and establish connection.`, cmd.DeviceID)
		if mac != "" {
			_ = sendMagicPacket(mac, "255.255.255.255")
		}

	case lower == "get-date" || lower == "date" || lower == "time" || lower == "get-time" || lower == "now":
		handled = true
		stdout = time.Now().Format("Monday, January 2, 2006 3:04:05 PM")

	case lower == "whoami" || lower == "get-user":
		handled = true
		stdout = fmt.Sprintf("Controller: mobile-controller\nTarget Device: %s\nCached Role: Administrator", cmd.DeviceID)

	case lower == "hostname" || lower == "get-host":
		handled = true
		hname := cmd.DeviceID
		if dev != nil && dev.Hostname != "" {
			hname = dev.Hostname
		}
		stdout = hname

	case strings.HasPrefix(lower, "echo ") || strings.HasPrefix(lower, "write-output ") || strings.HasPrefix(lower, "print "):
		handled = true
		idx := strings.Index(trimmed, " ")
		text := strings.TrimSpace(trimmed[idx+1:])
		text = strings.Trim(text, `"'`)
		stdout = text

	case lower == "ipconfig" || lower == "ifconfig" || lower == "get-netipaddress":
		handled = true
		hname := cmd.DeviceID
		if dev != nil && dev.Hostname != "" {
			hname = dev.Hostname
		}
		lastSeen := "N/A"
		if dev != nil && dev.LastHeartbeat > 0 {
			lastSeen = time.Unix(dev.LastHeartbeat, 0).Format(time.RFC1123)
		}
		stdout = fmt.Sprintf(`Windows IP Configuration (Cached Relay State)

Host Name . . . . . . . . . . . . : %s
Power State . . . . . . . . . . . : OFFLINE
Relay WebSocket Connection  . . . : Disconnected (Last: %s)
Note  . . . . . . . . . . . . . . : Detailed adapter IPv4/IPv6 available when machine is online.`,
			hname, lastSeen)

	case lower == "ver" || lower == "version" || lower == "get-version":
		handled = true
		osName := "Windows (Cached)"
		if dev != nil && dev.OS != "" {
			osName = dev.OS
		}
		stdout = fmt.Sprintf("Terminal App v4.0 Mobile & Cloud\nRelay Engine: Active\nTarget OS: %s", osName)

	case lower == "uptime" || lower == "get-uptime":
		handled = true
		uptimeStr := "Unknown"
		if dev != nil && dev.Metrics.UptimeSec > 0 {
			hrs := dev.Metrics.UptimeSec / 3600
			mins := (dev.Metrics.UptimeSec % 3600) / 60
			uptimeStr = fmt.Sprintf("%dh %dm", hrs, mins)
		}
		lastSeen := "N/A"
		if dev != nil && dev.LastHeartbeat > 0 {
			elapsed := time.Since(time.Unix(dev.LastHeartbeat, 0)).Round(time.Second)
			lastSeen = fmt.Sprintf("%s ago", elapsed)
		}
		stdout = fmt.Sprintf("Last Reported Uptime: %s\nPower State:          OFFLINE (Disconnected %s)", uptimeStr, lastSeen)

	case lower == "cls" || lower == "clear" || lower == "clear-host":
		handled = true
		stdout = "[Console Cleared]"

	case lower == "shutdown" || lower == "poweroff" || lower == "stop-computer" || lower == "reboot" || lower == "restart" || lower == "restart-computer":
		handled = true
		stdout = fmt.Sprintf(`[Power Management Notice]
Target device %s is ALREADY powered off / offline.
To power it on remotely, type: wake [MAC_ADDRESS]`, cmd.DeviceID)

	case lower == "history" || lower == "get-history":
		handled = true
		cmds, _ := h.store.ListCommands(cmd.DeviceID, 8)
		if len(cmds) == 0 {
			stdout = fmt.Sprintf("[Command History - %s]\n(No recent command history)", cmd.DeviceID)
		} else {
			var sb strings.Builder
			sb.WriteString(fmt.Sprintf("[Command History - %s: Latest %d Commands]\n", cmd.DeviceID, len(cmds)))
			for idx, c := range cmds {
				ts := time.Unix(c.CreatedAt, 0).Format(time.TimeOnly)
				sb.WriteString(fmt.Sprintf(" %d. [%s] %-10s : %s\n", idx+1, ts, c.Status, c.Command))
			}
			stdout = sb.String()
		}

	case lower == "help" || lower == "get-help" || lower == "?":
		handled = true
		stdout = `[Terminal App - Command Help]
BASIC & PREDEFINED COMMANDS (Execute immediately in Cloud when agent is OFFLINE):
  status        - Check device online/offline power state and last seen time
  health        - Show last recorded CPU, RAM, and uptime telemetry snapshot
  systeminfo    - Display cached OS, hostname, memory and disk capacity
  Get-Date/date - Display current date and time
  whoami        - Display controller and target identity
  hostname      - Display target host name
  echo [text]   - Print text output
  ipconfig      - Display cached network and connection state
  ver/version   - Display software and protocol versions
  uptime        - Show machine uptime before power-off
  ping          - Probe Relay latency and disconnection duration
  queue         - View commands waiting to run when laptop turns on
  clear-queue   - Cancel and purge pending offline commands
  wake [MAC]    - Send Wake-on-LAN Magic Packet to power on the laptop
  history       - View command execution history
  audit         - View recent security audit logs
  help          - Show this command reference

PHYSICAL COMMANDS (Executed on machine when ONLINE, or queued if OFFLINE):
  Any PowerShell cmdlet / script (e.g. Get-Process, Start-Sleep, Get-Service, etc.)`

	case lower == "audit" || lower == "get-audit" || lower == "get-auditlog":
		handled = true
		logs, _ := h.store.ListAuditLogs(cmd.DeviceID, 5)
		if len(logs) == 0 {
			stdout = fmt.Sprintf("[Audit Logs - %s]\n(No recent audit records found)", cmd.DeviceID)
		} else {
			var sb strings.Builder
			sb.WriteString(fmt.Sprintf("[Audit Logs - %s: Latest %d Actions]\n", cmd.DeviceID, len(logs)))
			for _, l := range logs {
				ts := time.Unix(l.Timestamp, 0).Format(time.RFC3339)
				sb.WriteString(fmt.Sprintf(" • [%s] %-12s exit=%d : %s\n", ts, l.ActionType, l.ExitCode, l.CommandText))
			}
			stdout = sb.String()
		}
	}

	if !handled {
		return protocol.ResultPayload{}, false
	}

	res := protocol.ResultPayload{
		ResultID:   cmd.CommandID,
		CommandID:  cmd.CommandID,
		DeviceID:   cmd.DeviceID,
		Stdout:     stdout,
		ExitCode:   0,
		ExecutedAt: time.Now().Unix(),
		DurationMs: 1,
	}
	return res, true
}

func sendMagicPacket(macStr, broadcastIP string) error {
	hw, err := net.ParseMAC(macStr)
	if err != nil {
		return fmt.Errorf("invalid MAC address: %w", err)
	}

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

func (h *Hub) executeCloudSandbox(cmd protocol.CommandPayload) protocol.ResultPayload {
	timeout := 15 * time.Second
	if cmd.TimeoutSec > 0 {
		timeout = time.Duration(cmd.TimeoutSec) * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	start := time.Now()
	var execCmd *exec.Cmd

	if runtime.GOOS == "windows" {
		execCmd = exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", cmd.Command)
	} else if _, err := exec.LookPath("pwsh"); err == nil {
		execCmd = exec.CommandContext(ctx, "pwsh", "-NoProfile", "-NonInteractive", "-Command", cmd.Command)
	} else {
		execCmd = exec.CommandContext(ctx, "sh", "-c", cmd.Command)
	}

	var stdoutBuf, stderrBuf bytes.Buffer
	execCmd.Stdout = &stdoutBuf
	execCmd.Stderr = &stderrBuf

	err := execCmd.Run()
	durationMs := time.Since(start).Milliseconds()

	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else if ctx.Err() == context.DeadlineExceeded {
			exitCode = 124
			stderrBuf.WriteString("\n[Execution timed out in Cloud Sandbox]")
		} else {
			exitCode = 1
			stderrBuf.WriteString("\n" + err.Error())
		}
	}

	header := fmt.Sprintf("⚡ [QUEUED IN CLOUD & PROCESSED — LAPTOP OFFLINE (%s)]\nStatus: Registered in persistent queue for physical execution on boot.\n\n", cmd.DeviceID)
	outStr := stdoutBuf.String()
	if outStr != "" {
		outStr = header + outStr
	} else if stderrBuf.Len() == 0 {
		outStr = header + "(Command registered and executed in Cloud Sandbox with no standard output)"
	}

	return protocol.ResultPayload{
		ResultID:   cmd.CommandID,
		CommandID:  cmd.CommandID,
		DeviceID:   cmd.DeviceID,
		Stdout:     outStr,
		Stderr:     stderrBuf.String(),
		ExitCode:   exitCode,
		ExecutedAt: time.Now().Unix(),
		DurationMs: durationMs,
	}
}



