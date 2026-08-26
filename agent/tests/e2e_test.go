package tests_test

import (
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"terminalagent/internal/config"
	"terminalagent/internal/executor"
	"terminalagent/internal/netclient"
	"terminalagent/internal/protocol"
	"terminalagent/internal/store"
	"terminalagent/internal/worker"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}

type mockRelay struct {
	srv                  *httptest.Server
	wsURL                string
	agentConn            *websocket.Conn
	ctrlConn             *websocket.Conn
	mu                   sync.Mutex
	dispatchedCmds       chan protocol.CommandPayload
	receivedResults      chan protocol.ResultPayload
	syncRequests         chan protocol.SyncReqPayload
	heartbeats           chan protocol.HeartbeatPayload
	fileListResponses    chan protocol.FileListRespPayload
	fileReadResponses    chan protocol.FileReadRespPayload
	fileWriteResponses   chan protocol.FileWriteRespPayload
	processListResponses chan protocol.ProcessListRespPayload
}

func newMockRelay(t *testing.T) *mockRelay {
	t.Helper()
	mr := &mockRelay{
		dispatchedCmds:       make(chan protocol.CommandPayload, 10),
		receivedResults:      make(chan protocol.ResultPayload, 10),
		syncRequests:         make(chan protocol.SyncReqPayload, 10),
		heartbeats:           make(chan protocol.HeartbeatPayload, 10),
		fileListResponses:    make(chan protocol.FileListRespPayload, 10),
		fileReadResponses:    make(chan protocol.FileReadRespPayload, 10),
		fileWriteResponses:   make(chan protocol.FileWriteRespPayload, 10),
		processListResponses: make(chan protocol.ProcessListRespPayload, 10),
	}

	mr.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		for {
			var env protocol.Envelope
			if err := conn.ReadJSON(&env); err != nil {
				return
			}

			mr.mu.Lock()
			switch env.Type {
			case protocol.TypeAuth:
				var p protocol.AuthPayload
				_ = env.DecodePayload(&p)
				if p.Role == protocol.RoleAgent {
					mr.agentConn = conn
				} else {
					mr.ctrlConn = conn
				}
				ack, _ := protocol.NewEnvelope(protocol.TypeAuthAck, p.DeviceID, protocol.AuthAckPayload{
					Success: true,
				})
				_ = conn.WriteJSON(ack)

			case protocol.TypeHeartbeat:
				var p protocol.HeartbeatPayload
				_ = env.DecodePayload(&p)
				mr.heartbeats <- p
				ack, _ := protocol.NewEnvelope(protocol.TypeHeartbeatAck, p.DeviceID, protocol.HeartbeatAckPayload{
					DeviceID: p.DeviceID,
				})
				_ = conn.WriteJSON(ack)

			case protocol.TypeCmdResult:
				var p protocol.ResultPayload
				_ = env.DecodePayload(&p)
				mr.receivedResults <- p
				ack, _ := protocol.NewEnvelope(protocol.TypeResultAck, p.DeviceID, protocol.ResultAckPayload{
					CommandID: p.CommandID,
					ResultID:  p.ResultID,
					Success:   true,
				})
				_ = conn.WriteJSON(ack)

			case protocol.TypeSyncReq:
				var p protocol.SyncReqPayload
				_ = env.DecodePayload(&p)
				mr.syncRequests <- p
				var acked []string
				for _, r := range p.UnsyncedResults {
					acked = append(acked, r.CommandID)
					mr.receivedResults <- r
				}
				resp, _ := protocol.NewEnvelope(protocol.TypeSyncResp, p.DeviceID, protocol.SyncRespPayload{
					DeviceID:     p.DeviceID,
					AckedResults: acked,
				})
				_ = conn.WriteJSON(resp)

			case protocol.TypeFileListResp:
				var p protocol.FileListRespPayload
				_ = env.DecodePayload(&p)
				mr.fileListResponses <- p

			case protocol.TypeFileReadResp:
				var p protocol.FileReadRespPayload
				_ = env.DecodePayload(&p)
				mr.fileReadResponses <- p

			case protocol.TypeFileWriteResp:
				var p protocol.FileWriteRespPayload
				_ = env.DecodePayload(&p)
				mr.fileWriteResponses <- p

			case protocol.TypeProcessListResp:
				var p protocol.ProcessListRespPayload
				_ = env.DecodePayload(&p)
				mr.processListResponses <- p
			}
			mr.mu.Unlock()
		}
	}))

	mr.wsURL = "ws" + strings.TrimPrefix(mr.srv.URL, "http")
	t.Cleanup(func() { mr.srv.Close() })
	return mr
}

func (mr *mockRelay) sendToAgent(env protocol.Envelope) error {
	mr.mu.Lock()
	defer mr.mu.Unlock()
	return mr.agentConn.WriteJSON(env)
}

func requireWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "windows" {
		t.Skip("skipping test requiring powershell.exe on non-Windows")
	}
}

func TestE2ELiveExecution(t *testing.T) {
	requireWindows(t)
	mr := newMockRelay(t)

	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "agent_live.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	cfg := config.Default()
	cfg.DBPath = filepath.Join(dir, "agent_live.db")
	cfg.RelayURL = mr.wsURL
	cfg.DeviceID = "win-agent-e2e"
	cfg.HeartbeatInterval = 100 * time.Millisecond

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	w := worker.New(st, executor.New("Bypass"), cfg, log)

	nc := netclient.New(st, cfg, func() {
		w.Wake()
	}, log)

	w.SetOnResult(func(res store.Result) {
		nc.ReportResult(res)
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go nc.Run(ctx)
	go w.Run(ctx)

	// Wait for agent connection and heartbeat
	select {
	case hb := <-mr.heartbeats:
		if hb.DeviceID != "win-agent-e2e" {
			t.Fatalf("heartbeat deviceId mismatch: %+v", hb)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for agent heartbeat")
	}

	// Dispatch command from relay
	cmd := protocol.CommandPayload{
		CommandID: "live-cmd-123",
		DeviceID:  "win-agent-e2e",
		Command:   "Write-Output 'E2E_POWERSHELL_SUCCESS'",
	}
	env, _ := protocol.NewEnvelope(protocol.TypeDispatchCmd, cmd.DeviceID, cmd)
	if err := mr.sendToAgent(env); err != nil {
		t.Fatalf("dispatch command: %v", err)
	}

	// Wait for command execution result
	select {
	case res := <-mr.receivedResults:
		if res.CommandID != "live-cmd-123" {
			t.Fatalf("commandId mismatch: %+v", res)
		}
		if !strings.Contains(res.Stdout, "E2E_POWERSHELL_SUCCESS") {
			t.Fatalf("stdout mismatch: %q", res.Stdout)
		}
		if res.ExitCode != 0 {
			t.Fatalf("exitCode = %d, want 0", res.ExitCode)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for command result")
	}

	// Verify local SQLite store marked command COMPLETED
	cmdRec, _ := st.GetCommand("live-cmd-123")
	if cmdRec == nil || cmdRec.Status != store.StatusCompleted {
		t.Fatalf("local command record = %+v, want COMPLETED", cmdRec)
	}
}

func TestE2EOfflineQueueSync(t *testing.T) {
	requireWindows(t)
	mr := newMockRelay(t)

	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "agent_offline_sync.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	// 1. Enqueue and execute command while completely OFFLINE (no netclient running)
	cmdID, _ := st.Enqueue(store.Payload{Command: "Write-Output 'OFFLINE_BEFORE_CONNECT'"})
	w := worker.New(st, executor.New("Bypass"), config.Default(), nil)
	did, err := w.RunOnce(context.Background())
	if err != nil || !did {
		t.Fatalf("run once offline: did=%t, err=%v", did, err)
	}

	// Verify result is stored with Synced = false
	unsynced, _ := st.ListUnsyncedResults(10)
	if len(unsynced) != 1 || unsynced[0].CommandID != cmdID || unsynced[0].Synced {
		t.Fatalf("unexpected unsynced list: %+v", unsynced)
	}

	// 2. Start NetClient now (simulating network reconnect)
	cfg := config.Default()
	cfg.DBPath = filepath.Join(dir, "agent_offline_sync.db")
	cfg.RelayURL = mr.wsURL
	cfg.DeviceID = "win-offline-sync"

	nc := netclient.New(st, cfg, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go nc.Run(ctx)

	// Verify Relay received the offline result via SYNC_REQ
	select {
	case res := <-mr.receivedResults:
		if res.CommandID != cmdID {
			t.Fatalf("commandId mismatch in synced result: %+v", res)
		}
		if !strings.Contains(res.Stdout, "OFFLINE_BEFORE_CONNECT") {
			t.Fatalf("stdout mismatch in synced result: %q", res.Stdout)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for offline result sync")
	}

	// Verify local SQLite store updated Synced = 1
	time.Sleep(200 * time.Millisecond)
	unsyncedAfter, _ := st.ListUnsyncedResults(10)
	if len(unsyncedAfter) != 0 {
		t.Fatalf("expected 0 unsynced results after reconnect, got %d", len(unsyncedAfter))
	}
}

func TestE2ERemoteFileAndProcessOps(t *testing.T) {
	mr := newMockRelay(t)

	dir := t.TempDir()
	st, _ := store.Open(filepath.Join(dir, "agent_files.db"))
	defer st.Close()

	cfg := config.Default()
	cfg.DBPath = filepath.Join(dir, "agent_files.db")
	cfg.RelayURL = mr.wsURL
	cfg.DeviceID = "win-file-ops"
	cfg.HeartbeatInterval = 100 * time.Millisecond

	nc := netclient.New(st, cfg, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go nc.Run(ctx)

	// Wait for connection
	select {
	case <-mr.heartbeats:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for agent")
	}

	// 1. Test Remote File Write via WebSocket
	targetFile := filepath.Join(dir, "mobile_upload.txt")
	testData := "Uploaded from mobile controller via WebSocket!"
	encData := base64.StdEncoding.EncodeToString([]byte(testData))

	writeEnv, _ := protocol.NewEnvelope(protocol.TypeFileWriteReq, "win-file-ops", protocol.FileWriteReqPayload{
		DeviceID:      "win-file-ops",
		Path:          targetFile,
		ContentBase64: encData,
		Overwrite:     true,
	})
	_ = mr.sendToAgent(writeEnv)

	select {
	case writeResp := <-mr.fileWriteResponses:
		if !writeResp.Success {
			t.Fatalf("remote file write failed: %s", writeResp.Error)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for file write response")
	}

	// 2. Test Remote File Read via WebSocket
	readEnv, _ := protocol.NewEnvelope(protocol.TypeFileReadReq, "win-file-ops", protocol.FileReadReqPayload{
		DeviceID: "win-file-ops",
		Path:     targetFile,
	})
	_ = mr.sendToAgent(readEnv)

	select {
	case readResp := <-mr.fileReadResponses:
		if readResp.Error != "" {
			t.Fatalf("remote file read failed: %s", readResp.Error)
		}
		raw, _ := base64.StdEncoding.DecodeString(readResp.ContentBase64)
		if string(raw) != testData {
			t.Fatalf("remote file content mismatch: %s", string(raw))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for file read response")
	}

	// 3. Test Remote Process Listing via WebSocket
	procEnv, _ := protocol.NewEnvelope(protocol.TypeProcessListReq, "win-file-ops", protocol.ProcessListReqPayload{
		DeviceID: "win-file-ops",
	})
	_ = mr.sendToAgent(procEnv)

	select {
	case procResp := <-mr.processListResponses:
		if procResp.Error != "" {
			t.Fatalf("process list error: %s", procResp.Error)
		}
		if len(procResp.Processes) == 0 {
			t.Fatal("expected at least one process returned")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for process list response")
	}
}
