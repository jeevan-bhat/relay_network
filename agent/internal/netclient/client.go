// Package netclient provides a resilient, auto-reconnecting WebSocket client
// that connects the Windows Agent to the central Relay server.
package netclient

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"net/url"
	"os"
	"runtime"
	"sync"
	"time"

	"terminalagent/internal/config"
	"terminalagent/internal/filemgr"
	"terminalagent/internal/health"
	"terminalagent/internal/processmgr"
	"terminalagent/internal/protocol"
	"terminalagent/internal/store"

	"github.com/gorilla/websocket"
)

// Client manages the WebSocket connection, heartbeats, and queue synchronization with the Relay.
type Client struct {
	cfg              config.Config
	store            *store.Store
	log              *slog.Logger
	onCommandArrived func() // optional callback to wake execution worker

	conn      *websocket.Conn
	send      chan protocol.Envelope
	connected bool
	mu        sync.RWMutex
}

// New creates a new Relay WebSocket NetClient.
func New(st *store.Store, cfg config.Config, onCmd func(), log *slog.Logger) *Client {
	if log == nil {
		log = slog.Default()
	}
	return &Client{
		cfg:              cfg,
		store:            st,
		log:              log,
		onCommandArrived: onCmd,
		send:             make(chan protocol.Envelope, 64),
	}
}

// IsConnected returns whether the agent is currently connected and authenticated with the Relay.
func (c *Client) IsConnected() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.connected
}

// Run starts the reconnection and sync loop until ctx is cancelled.
func (c *Client) Run(ctx context.Context) {
	if c.cfg.RelayURL == "" {
		c.log.Info("relay url not configured, running in standalone offline mode")
		<-ctx.Done()
		return
	}

	backoff := 1 * time.Second
	maxBackoff := 30 * time.Second

	for {
		if ctx.Err() != nil {
			return
		}

		c.log.Info("connecting to relay...", "url", c.cfg.RelayURL, "deviceId", c.cfg.DeviceID)
		err := c.connectAndServe(ctx)
		if err != nil && !errors.Is(err, context.Canceled) {
			c.log.Warn("relay connection closed", "err", err)
		}

		c.setConnected(false)

		// Calculate exponential backoff with jitter
		jitter := time.Duration(rand.Intn(500)) * time.Millisecond
		sleepDur := backoff + jitter
		c.log.Info("retrying relay connection", "after", sleepDur)

		select {
		case <-ctx.Done():
			return
		case <-time.After(sleepDur):
		}

		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

func (c *Client) connectAndServe(ctx context.Context) error {
	u, err := url.Parse(c.cfg.RelayURL)
	if err != nil {
		return fmt.Errorf("invalid relay url: %w", err)
	}

	conn, _, err := websocket.DefaultDialer.DialContext(ctx, u.String(), nil)
	if err != nil {
		return fmt.Errorf("dial relay: %w", err)
	}
	defer conn.Close()

	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()

	// 1. Perform Authentication Handshake
	hostname, _ := os.Hostname()
	authEnv, _ := protocol.NewEnvelope(protocol.TypeAuth, c.cfg.DeviceID, protocol.AuthPayload{
		Role:       protocol.RoleAgent,
		DeviceID:   c.cfg.DeviceID,
		Token:      c.cfg.AuthToken,
		Hostname:   hostname,
		OS:         runtime.GOOS + "/" + runtime.GOARCH,
		Version:    "1.0.0",
		MACAddress: health.GetPrimaryMACAddress(),
	})
	if err := conn.WriteJSON(authEnv); err != nil {
		return fmt.Errorf("send auth: %w", err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	var ackEnv protocol.Envelope
	if err := conn.ReadJSON(&ackEnv); err != nil {
		return fmt.Errorf("read auth ack: %w", err)
	}

	var ack protocol.AuthAckPayload
	if err := ackEnv.DecodePayload(&ack); err != nil || !ack.Success {
		return fmt.Errorf("auth rejected by relay: %v", ack.Error)
	}

	c.log.Info("authenticated with relay successfully", "deviceId", c.cfg.DeviceID)
	c.setConnected(true)

	// Sub-context for this active connection
	connCtx, connCancel := context.WithCancel(ctx)
	defer connCancel()

	// Launch Pumps
	go c.writePump(connCtx, conn)
	go c.heartbeatLoop(connCtx)

	// Send immediate initial heartbeat
	metrics := health.Collect()
	initialHb, _ := protocol.NewEnvelope(protocol.TypeHeartbeat, c.cfg.DeviceID, protocol.HeartbeatPayload{
		DeviceID: c.cfg.DeviceID,
		Metrics:  metrics,
	})
	c.Send(initialHb)

	// 2. Perform Startup Offline Queue Synchronization
	c.performSync()

	// 3. Read Loop
	return c.readLoop(connCtx, conn)
}

func (c *Client) readLoop(ctx context.Context, conn *websocket.Conn) error {
	for {
		_ = conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		var env protocol.Envelope
		if err := conn.ReadJSON(&env); err != nil {
			return err
		}

		c.handleIncoming(env)
	}
}

func (c *Client) writePump(ctx context.Context, conn *websocket.Conn) {
	for {
		select {
		case <-ctx.Done():
			return
		case env, ok := <-c.send:
			if !ok {
				return
			}
			_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := conn.WriteJSON(env); err != nil {
				c.log.Warn("failed to write ws message", "type", env.Type, "err", err)
				return
			}
		}
	}
}

func (c *Client) handleIncoming(env protocol.Envelope) {
	switch env.Type {
	case protocol.TypeDispatchCmd:
		var cmd protocol.CommandPayload
		if err := env.DecodePayload(&cmd); err != nil {
			return
		}
		c.log.Info("received command from relay", "commandId", cmd.CommandID, "cmd", cmd.Command)
		_ = c.store.EnqueueWithID(cmd.CommandID, store.Payload{
			Command:    cmd.Command,
			TimeoutSec: cmd.TimeoutSec,
		})
		if c.onCommandArrived != nil {
			c.onCommandArrived()
		}

	case protocol.TypeSyncResp:
		var resp protocol.SyncRespPayload
		if err := env.DecodePayload(&resp); err != nil {
			return
		}
		for _, ackID := range resp.AckedResults {
			_ = c.store.MarkResultSynced(ackID)
		}
		for _, cmd := range resp.PendingCommands {
			_ = c.store.EnqueueWithID(cmd.CommandID, store.Payload{
				Command:    cmd.Command,
				TimeoutSec: cmd.TimeoutSec,
			})
		}
		c.log.Info("sync response processed", "ackedResults", len(resp.AckedResults), "pendingCommands", len(resp.PendingCommands))
		if len(resp.PendingCommands) > 0 && c.onCommandArrived != nil {
			c.onCommandArrived()
		}

	case protocol.TypeResultAck:
		var ack protocol.ResultAckPayload
		if err := env.DecodePayload(&ack); err == nil && ack.Success {
			_ = c.store.MarkResultSynced(ack.CommandID)
		}

	case protocol.TypeFileListReq:
		var req protocol.FileListReqPayload
		_ = env.DecodePayload(&req)
		files, err := filemgr.ListDirectory(req.Path)
		resp := protocol.FileListRespPayload{
			DeviceID: c.cfg.DeviceID,
			Path:     req.Path,
			Files:    files,
		}
		if err != nil {
			resp.Error = err.Error()
		}
		respEnv, _ := protocol.NewEnvelope(protocol.TypeFileListResp, c.cfg.DeviceID, resp)
		respEnv.ID = env.ID // Preserve request ID for correlation
		c.Send(respEnv)

	case protocol.TypeFileReadReq:
		var req protocol.FileReadReqPayload
		_ = env.DecodePayload(&req)
		content, size, err := filemgr.ReadFile(req.Path)
		resp := protocol.FileReadRespPayload{
			DeviceID:      c.cfg.DeviceID,
			Path:          req.Path,
			ContentBase64: content,
			SizeBytes:     size,
		}
		if err != nil {
			resp.Error = err.Error()
		}
		respEnv, _ := protocol.NewEnvelope(protocol.TypeFileReadResp, c.cfg.DeviceID, resp)
		respEnv.ID = env.ID
		c.Send(respEnv)

	case protocol.TypeFileWriteReq:
		var req protocol.FileWriteReqPayload
		_ = env.DecodePayload(&req)
		err := filemgr.WriteFile(req.Path, req.ContentBase64, req.Overwrite)
		resp := protocol.FileWriteRespPayload{
			DeviceID: c.cfg.DeviceID,
			Path:     req.Path,
			Success:  err == nil,
		}
		if err != nil {
			resp.Error = err.Error()
		}
		respEnv, _ := protocol.NewEnvelope(protocol.TypeFileWriteResp, c.cfg.DeviceID, resp)
		respEnv.ID = env.ID
		c.Send(respEnv)

	case protocol.TypeFileDeleteReq:
		var req protocol.FileDeleteReqPayload
		_ = env.DecodePayload(&req)
		err := filemgr.DeleteFile(req.Path)
		resp := protocol.FileDeleteRespPayload{
			DeviceID: c.cfg.DeviceID,
			Path:     req.Path,
			Success:  err == nil,
		}
		if err != nil {
			resp.Error = err.Error()
		}
		respEnv, _ := protocol.NewEnvelope(protocol.TypeFileDeleteResp, c.cfg.DeviceID, resp)
		respEnv.ID = env.ID
		c.Send(respEnv)

	case protocol.TypeProcessListReq:
		procs, err := processmgr.ListProcesses()
		resp := protocol.ProcessListRespPayload{
			DeviceID:  c.cfg.DeviceID,
			Processes: procs,
		}
		if err != nil {
			resp.Error = err.Error()
		}
		respEnv, _ := protocol.NewEnvelope(protocol.TypeProcessListResp, c.cfg.DeviceID, resp)
		respEnv.ID = env.ID
		c.Send(respEnv)

	case protocol.TypeProcessKillReq:
		var req protocol.ProcessKillReqPayload
		_ = env.DecodePayload(&req)
		err := processmgr.KillProcess(req.PID)
		resp := protocol.ProcessKillRespPayload{
			DeviceID: c.cfg.DeviceID,
			PID:      req.PID,
			Success:  err == nil,
		}
		if err != nil {
			resp.Error = err.Error()
		}
		respEnv, _ := protocol.NewEnvelope(protocol.TypeProcessKillResp, c.cfg.DeviceID, resp)
		respEnv.ID = env.ID
		c.Send(respEnv)

	case protocol.TypeHeartbeatAck:
		// Heartbeat confirmed by relay
	}
}

func (c *Client) heartbeatLoop(ctx context.Context) {
	ticker := time.NewTicker(c.cfg.HeartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !c.IsConnected() {
				return
			}
			metrics := health.Collect()
			env, _ := protocol.NewEnvelope(protocol.TypeHeartbeat, c.cfg.DeviceID, protocol.HeartbeatPayload{
				DeviceID: c.cfg.DeviceID,
				Metrics:  metrics,
			})
			c.Send(env)
		}
	}
}

func (c *Client) performSync() {
	unsynced, err := c.store.ListUnsyncedResults(50)
	if err != nil {
		c.log.Error("failed to list unsynced results", "err", err)
		return
	}

	var results []protocol.ResultPayload
	for _, r := range unsynced {
		results = append(results, protocol.ResultPayload{
			ResultID:   r.ResultID,
			CommandID:  r.CommandID,
			DeviceID:   c.cfg.DeviceID,
			Stdout:     r.Stdout,
			Stderr:     r.Stderr,
			ExitCode:   r.ExitCode,
			ExecutedAt: r.ExecutedAt,
		})
	}

	syncEnv, _ := protocol.NewEnvelope(protocol.TypeSyncReq, c.cfg.DeviceID, protocol.SyncReqPayload{
		DeviceID:        c.cfg.DeviceID,
		UnsyncedResults: results,
	})
	c.Send(syncEnv)
}

// Send enqueues an envelope to be sent over the active WebSocket connection.
func (c *Client) Send(env protocol.Envelope) {
	select {
	case c.send <- env:
	default:
		c.log.Warn("netclient send buffer full, dropping message", "type", env.Type)
	}
}

// ReportResult sends an execution result to the relay immediately if connected.
func (c *Client) ReportResult(res store.Result) {
	env, _ := protocol.NewEnvelope(protocol.TypeCmdResult, c.cfg.DeviceID, protocol.ResultPayload{
		ResultID:   res.ResultID,
		CommandID:  res.CommandID,
		DeviceID:   c.cfg.DeviceID,
		Stdout:     res.Stdout,
		Stderr:     res.Stderr,
		ExitCode:   res.ExitCode,
		ExecutedAt: res.ExecutedAt,
	})
	c.Send(env)
}

func (c *Client) setConnected(v bool) {
	c.mu.Lock()
	c.connected = v
	c.mu.Unlock()
}
