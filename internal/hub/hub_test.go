package hub_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"terminalrelay/internal/config"
	"terminalrelay/internal/hub"
	"terminalrelay/internal/protocol"
	"terminalrelay/internal/store"

	"github.com/gorilla/websocket"
)

func setupTestServer(t *testing.T) (*hub.Hub, *store.Store, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "hub_test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	cfg := config.Default()
	cfg.AuthToken = "secret-token"
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := hub.New(st, cfg, log)
	t.Cleanup(func() { h.Close() })

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", h.HandleWS)
	srv := httptest.NewServer(mux)
	t.Cleanup(func() { srv.Close() })

	return h, st, srv
}

func connectWS(t *testing.T, srvURL string) *websocket.Conn {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(srvURL, "http") + "/ws"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial ws: %v", err)
	}
	return conn
}

func sendEnv(t *testing.T, conn *websocket.Conn, msgType, deviceID string, payload any) {
	t.Helper()
	env, err := protocol.NewEnvelope(msgType, deviceID, payload)
	if err != nil {
		t.Fatalf("new envelope: %v", err)
	}
	b, _ := json.Marshal(env)
	if err := conn.WriteMessage(websocket.TextMessage, b); err != nil {
		t.Fatalf("write msg: %v", err)
	}
}

func readEnv(t *testing.T, conn *websocket.Conn) protocol.Envelope {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	_, b, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read env: %v", err)
	}
	var env protocol.Envelope
	if err := json.Unmarshal(b, &env); err != nil {
		t.Fatalf("unmarshal env: %v", err)
	}
	return env
}

func TestAuthAgentAndHeartbeat(t *testing.T) {
	_, st, srv := setupTestServer(t)

	conn := connectWS(t, srv.URL)
	defer conn.Close()

	// 1. Send AUTH with wrong token
	sendEnv(t, conn, protocol.TypeAuth, "agent-1", protocol.AuthPayload{
		Role:     protocol.RoleAgent,
		DeviceID: "agent-1",
		Token:    "wrong-token",
		Hostname: "WIN-PC",
	})
	ack := readEnv(t, conn)
	if ack.Type != protocol.TypeAuthAck {
		t.Fatalf("type = %s, want AUTH_ACK", ack.Type)
	}
	var ackP protocol.AuthAckPayload
	_ = ack.DecodePayload(&ackP)
	if ackP.Success {
		t.Fatal("expected auth failure for wrong token")
	}

	// 2. Send AUTH with valid token
	sendEnv(t, conn, protocol.TypeAuth, "agent-1", protocol.AuthPayload{
		Role:     protocol.RoleAgent,
		DeviceID: "agent-1",
		Token:    "secret-token",
		Hostname: "WIN-PC",
		OS:       "windows",
	})
	ack2 := readEnv(t, conn)
	_ = ack2.DecodePayload(&ackP)
	if !ackP.Success {
		t.Fatalf("expected auth success, got %v", ackP.Error)
	}

	dev, _ := st.GetDevice("agent-1")
	if dev == nil || dev.Status != protocol.StatusOnline {
		t.Fatalf("dev status = %+v, want ONLINE", dev)
	}

	// 3. Send HEARTBEAT
	sendEnv(t, conn, protocol.TypeHeartbeat, "agent-1", protocol.HeartbeatPayload{
		DeviceID: "agent-1",
		Metrics: protocol.HealthMetrics{
			CPUPercent: 25.5,
			RAMPercent: 50.0,
		},
	})
	hbAck := readEnv(t, conn)
	if hbAck.Type != protocol.TypeHeartbeatAck {
		t.Fatalf("type = %s, want HEARTBEAT_ACK", hbAck.Type)
	}

	dev2, _ := st.GetDevice("agent-1")
	if dev2.Metrics.CPUPercent != 25.5 {
		t.Fatalf("stored metrics mismatch: %+v", dev2.Metrics)
	}
}

func TestLiveCommandDispatchAndResult(t *testing.T) {
	_, st, srv := setupTestServer(t)

	// Connect Agent
	agentConn := connectWS(t, srv.URL)
	defer agentConn.Close()
	sendEnv(t, agentConn, protocol.TypeAuth, "agent-live", protocol.AuthPayload{
		Role:     protocol.RoleAgent,
		DeviceID: "agent-live",
		Token:    "secret-token",
	})
	_ = readEnv(t, agentConn) // AUTH_ACK

	// Connect Controller
	ctrlConn := connectWS(t, srv.URL)
	defer ctrlConn.Close()
	sendEnv(t, ctrlConn, protocol.TypeAuth, "", protocol.AuthPayload{
		Role:  protocol.RoleController,
		Token: "secret-token",
	})
	_ = readEnv(t, ctrlConn) // AUTH_ACK

	// Controller enqueues command
	sendEnv(t, ctrlConn, protocol.TypeEnqueueCmd, "agent-live", protocol.CommandPayload{
		CommandID: "cmd-live-1",
		DeviceID:  "agent-live",
		Command:   "Get-Date",
	})

	// Agent should immediately receive DISPATCH_CMD
	dispatched := readEnv(t, agentConn)
	if dispatched.Type != protocol.TypeDispatchCmd {
		t.Fatalf("expected DISPATCH_CMD on agent, got %s", dispatched.Type)
	}
	var cmdP protocol.CommandPayload
	_ = dispatched.DecodePayload(&cmdP)
	if cmdP.CommandID != "cmd-live-1" || cmdP.Command != "Get-Date" {
		t.Fatalf("dispatched payload mismatch: %+v", cmdP)
	}

	// Controller should receive ENQUEUE_CMD notification
	ctrlNotif := readEnv(t, ctrlConn)
	if ctrlNotif.Type != protocol.TypeEnqueueCmd {
		t.Fatalf("expected ENQUEUE_CMD broadcast to controller, got %s", ctrlNotif.Type)
	}

	// Agent finishes and sends CMD_RESULT
	sendEnv(t, agentConn, protocol.TypeCmdResult, "agent-live", protocol.ResultPayload{
		CommandID:  "cmd-live-1",
		DeviceID:   "agent-live",
		Stdout:     "Wed Aug 26 2026\r\n",
		ExitCode:   0,
		ExecutedAt: time.Now().Unix(),
	})

	// Agent receives RESULT_ACK
	resAck := readEnv(t, agentConn)
	if resAck.Type != protocol.TypeResultAck {
		t.Fatalf("expected RESULT_ACK, got %s", resAck.Type)
	}

	// Controller receives CMD_RESULT broadcast
	ctrlRes := readEnv(t, ctrlConn)
	if ctrlRes.Type != protocol.TypeCmdResult {
		t.Fatalf("expected CMD_RESULT broadcast to controller, got %s", ctrlRes.Type)
	}

	// Verify database record
	savedRes, _ := st.GetResult("cmd-live-1")
	if savedRes == nil || savedRes.Stdout != "Wed Aug 26 2026\r\n" {
		t.Fatalf("result in store mismatch: %+v", savedRes)
	}
}

func TestOfflineQueuingAndFlushOnReconnect(t *testing.T) {
	_, st, srv := setupTestServer(t)

	// Controller connects while Agent is completely OFFLINE
	ctrlConn := connectWS(t, srv.URL)
	defer ctrlConn.Close()
	sendEnv(t, ctrlConn, protocol.TypeAuth, "", protocol.AuthPayload{
		Role:  protocol.RoleController,
		Token: "secret-token",
	})
	_ = readEnv(t, ctrlConn) // AUTH_ACK

	// Enqueue 2 physical commands for offline agent
	sendEnv(t, ctrlConn, protocol.TypeEnqueueCmd, "agent-offline", protocol.CommandPayload{
		CommandID: "cmd-off-1",
		DeviceID:  "agent-offline",
		Command:   "Get-Process -Name svchost",
	})
	_ = readEnv(t, ctrlConn) // ENQUEUE_CMD broadcast

	sendEnv(t, ctrlConn, protocol.TypeEnqueueCmd, "agent-offline", protocol.CommandPayload{
		CommandID: "cmd-off-2",
		DeviceID:  "agent-offline",
		Command:   "Restart-Service -Name Spooler",
	})
	_ = readEnv(t, ctrlConn) // ENQUEUE_CMD broadcast

	// Verify stored in cloud SQLite
	pending, _ := st.GetPendingCommands("agent-offline")
	if len(pending) != 2 {
		t.Fatalf("expected 2 pending commands in cloud queue, got %d", len(pending))
	}

	// Agent connects now!
	agentConn := connectWS(t, srv.URL)
	defer agentConn.Close()
	sendEnv(t, agentConn, protocol.TypeAuth, "agent-offline", protocol.AuthPayload{
		Role:     protocol.RoleAgent,
		DeviceID: "agent-offline",
		Token:    "secret-token",
	})
	_ = readEnv(t, agentConn) // AUTH_ACK

	// Agent should immediately receive the 2 queued commands in FIFO order
	d1 := readEnv(t, agentConn)
	if d1.Type != protocol.TypeDispatchCmd {
		t.Fatalf("expected flush DISPATCH_CMD 1, got %s", d1.Type)
	}
	var p1 protocol.CommandPayload
	_ = d1.DecodePayload(&p1)
	if p1.CommandID != "cmd-off-1" {
		t.Fatalf("expected cmd-off-1 first, got %s", p1.CommandID)
	}

	d2 := readEnv(t, agentConn)
	if d2.Type != protocol.TypeDispatchCmd {
		t.Fatalf("expected flush DISPATCH_CMD 2, got %s", d2.Type)
	}
	var p2 protocol.CommandPayload
	_ = d2.DecodePayload(&p2)
	if p2.CommandID != "cmd-off-2" {
		t.Fatalf("expected cmd-off-2 second, got %s", p2.CommandID)
	}
}

func TestPredefinedOfflineCommands(t *testing.T) {
	_, st, srv := setupTestServer(t)

	// Seed registered device in store
	_ = st.UpsertDevice(protocol.DeviceInfo{
		DeviceID: "win-offline-pc",
		Hostname: "DESKTOP-TEST",
		OS:       "Windows 11 Pro",
		Status:   protocol.StatusOffline,
		Metrics: protocol.HealthMetrics{
			CPUPercent:     14.5,
			RAMPercent:     48.2,
			RAMTotalBytes:  16 * 1024 * 1024 * 1024,
			RAMUsedBytes:   8 * 1024 * 1024 * 1024,
			DiskTotalBytes: 512 * 1024 * 1024 * 1024,
			UptimeSec:      3600,
		},
	})

	// Connect controller
	ctrlConn := connectWS(t, srv.URL)
	defer ctrlConn.Close()
	sendEnv(t, ctrlConn, protocol.TypeAuth, "ctrl-1", protocol.AuthPayload{
		Role:  protocol.RoleController,
		Token: "secret-token",
	})
	_ = readEnv(t, ctrlConn) // AUTH_ACK

	// 1. Send predefined command "status" when agent is offline
	sendEnv(t, ctrlConn, protocol.TypeEnqueueCmd, "win-offline-pc", protocol.CommandPayload{
		CommandID: "cmd-predef-status",
		DeviceID:  "win-offline-pc",
		Command:   "status",
	})

	// Controller should receive immediate CMD_RESULT without being stuck in PENDING!
	resEnv := readEnv(t, ctrlConn)
	if resEnv.Type != protocol.TypeCmdResult {
		t.Fatalf("expected immediate CMD_RESULT for predefined status, got %s", resEnv.Type)
	}
	var res protocol.ResultPayload
	_ = resEnv.DecodePayload(&res)
	if res.ExitCode != 0 {
		t.Fatalf("exitCode = %d, want 0", res.ExitCode)
	}
	if !strings.Contains(res.Stdout, "Power State:  OFFLINE") {
		t.Fatalf("unexpected stdout: %s", res.Stdout)
	}

	// 2. Send predefined command "health"
	sendEnv(t, ctrlConn, protocol.TypeEnqueueCmd, "win-offline-pc", protocol.CommandPayload{
		CommandID: "cmd-predef-health",
		DeviceID:  "win-offline-pc",
		Command:   "health",
	})
	resEnv2 := readEnv(t, ctrlConn)
	if resEnv2.Type != protocol.TypeCmdResult {
		t.Fatalf("expected immediate CMD_RESULT for health, got %s", resEnv2.Type)
	}

	// 3. Send basic command "Get-Date"
	sendEnv(t, ctrlConn, protocol.TypeEnqueueCmd, "win-offline-pc", protocol.CommandPayload{
		CommandID: "cmd-basic-date",
		DeviceID:  "win-offline-pc",
		Command:   "Get-Date",
	})
	dateEnv := readEnv(t, ctrlConn)
	if dateEnv.Type != protocol.TypeCmdResult {
		t.Fatalf("expected immediate CMD_RESULT for Get-Date, got %s", dateEnv.Type)
	}

	// 4. Send basic command "whoami"
	sendEnv(t, ctrlConn, protocol.TypeEnqueueCmd, "win-offline-pc", protocol.CommandPayload{
		CommandID: "cmd-basic-whoami",
		DeviceID:  "win-offline-pc",
		Command:   "whoami",
	})
	whoEnv := readEnv(t, ctrlConn)
	if whoEnv.Type != protocol.TypeCmdResult {
		t.Fatalf("expected immediate CMD_RESULT for whoami, got %s", whoEnv.Type)
	}

	// 5. Send basic command "echo Hello Mobile"
	sendEnv(t, ctrlConn, protocol.TypeEnqueueCmd, "win-offline-pc", protocol.CommandPayload{
		CommandID: "cmd-basic-echo",
		DeviceID:  "win-offline-pc",
		Command:   "echo Hello Mobile",
	})
	echoEnv := readEnv(t, ctrlConn)
	if echoEnv.Type != protocol.TypeCmdResult {
		t.Fatalf("expected immediate CMD_RESULT for echo, got %s", echoEnv.Type)
	}
	var echoRes protocol.ResultPayload
	_ = echoEnv.DecodePayload(&echoRes)
	if echoRes.Stdout != "Hello Mobile" {
		t.Fatalf("echo stdout = %q, want %q", echoRes.Stdout, "Hello Mobile")
	}

	// 6. Send custom PowerShell script "$a=7; $b=3; Write-Output \"MathResult: $($a * $b)\""
	sendEnv(t, ctrlConn, protocol.TypeEnqueueCmd, "win-offline-pc", protocol.CommandPayload{
		CommandID: "cmd-custom-script",
		DeviceID:  "win-offline-pc",
		Command:   "$a=7; $b=3; Write-Output \"MathResult: $($a * $b)\"",
	})

	// Controller receives immediate CMD_RESULT from Cloud Sandbox
	scriptEnv := readEnv(t, ctrlConn)
	if scriptEnv.Type != protocol.TypeCmdResult {
		t.Fatalf("expected immediate CMD_RESULT from Cloud Sandbox, got %s", scriptEnv.Type)
	}
	var scriptRes protocol.ResultPayload
	_ = scriptEnv.DecodePayload(&scriptRes)
	if scriptRes.ExitCode != 0 {
		t.Fatalf("script execution exitCode = %d, want 0", scriptRes.ExitCode)
	}
	if !strings.Contains(scriptRes.Stdout, "MathResult: 21") {
		t.Fatalf("expected script output MathResult: 21, got: %s", scriptRes.Stdout)
	}
}

