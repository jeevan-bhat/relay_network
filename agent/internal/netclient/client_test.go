package netclient_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"terminalagent/internal/config"
	"terminalagent/internal/netclient"
	"terminalagent/internal/protocol"
	"terminalagent/internal/store"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}

func TestNetClientConnectAuthAndDispatch(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "agent_net_test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	// Create test server simulating relay
	authReceived := make(chan protocol.AuthPayload, 1)
	syncReceived := make(chan protocol.SyncReqPayload, 1)
	var serverConn *websocket.Conn
	var writeMu sync.Mutex

	safeWrite := func(c *websocket.Conn, v any) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return c.WriteJSON(v)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		writeMu.Lock()
		serverConn = c
		writeMu.Unlock()
		defer c.Close()

		for {
			var env protocol.Envelope
			if err := c.ReadJSON(&env); err != nil {
				return
			}
			switch env.Type {
			case protocol.TypeAuth:
				var p protocol.AuthPayload
				_ = env.DecodePayload(&p)
				authReceived <- p
				ack, _ := protocol.NewEnvelope(protocol.TypeAuthAck, p.DeviceID, protocol.AuthAckPayload{Success: true})
				_ = safeWrite(c, ack)
			case protocol.TypeSyncReq:
				var p protocol.SyncReqPayload
				_ = env.DecodePayload(&p)
				syncReceived <- p
				resp, _ := protocol.NewEnvelope(protocol.TypeSyncResp, p.DeviceID, protocol.SyncRespPayload{
					DeviceID: p.DeviceID,
				})
				_ = safeWrite(c, resp)
			}
		}
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

	cfg := config.Default()
	cfg.RelayURL = wsURL
	cfg.DeviceID = "test-agent-node"
	cfg.AuthToken = "test-secret"
	cfg.HeartbeatInterval = 100 * time.Millisecond

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cmdArrived := make(chan struct{}, 1)
	client := netclient.New(st, cfg, func() {
		cmdArrived <- struct{}{}
	}, log)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go client.Run(ctx)

	// Verify Auth received by server
	select {
	case auth := <-authReceived:
		if auth.DeviceID != "test-agent-node" || auth.Token != "test-secret" {
			t.Fatalf("auth mismatch: %+v", auth)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for AUTH")
	}

	// Verify Sync request received
	select {
	case syncReq := <-syncReceived:
		if syncReq.DeviceID != "test-agent-node" {
			t.Fatalf("syncReq deviceId mismatch: %+v", syncReq)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for SYNC_REQ")
	}

	// Now simulate Relay dispatching a command to Agent
	cmdEnv, _ := protocol.NewEnvelope(protocol.TypeDispatchCmd, "test-agent-node", protocol.CommandPayload{
		CommandID: "dispatch-101",
		DeviceID:  "test-agent-node",
		Command:   "Get-Date",
	})
	_ = safeWrite(serverConn, cmdEnv)

	select {
	case <-cmdArrived:
		// Worker notified!
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for onCommandArrived")
	}

	// Check local SQLite store has the command
	cmdRec, err := st.GetCommand("dispatch-101")
	if err != nil || cmdRec == nil {
		t.Fatalf("command not in local store: %v, rec=%+v", err, cmdRec)
	}
	p, _ := cmdRec.Decode()
	if p.Command != "Get-Date" {
		t.Fatalf("payload command mismatch: %s", p.Command)
	}
}
