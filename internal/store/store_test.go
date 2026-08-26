package store_test

import (
	"path/filepath"
	"testing"
	"time"

	"terminalrelay/internal/protocol"
	"terminalrelay/internal/store"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "relay_test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestDeviceUpsertAndList(t *testing.T) {
	st := newTestStore(t)

	dev := protocol.DeviceInfo{
		DeviceID:      "test-agent-1",
		Hostname:      "WIN-SRV-01",
		OS:            "windows/amd64",
		Status:        protocol.StatusOnline,
		LastHeartbeat: time.Now().Unix(),
		ConnectedAt:   time.Now().Unix(),
		Metrics: protocol.HealthMetrics{
			CPUPercent: 12.5,
			RAMPercent: 45.0,
			UptimeSec:  3600,
		},
	}

	if err := st.UpsertDevice(dev); err != nil {
		t.Fatalf("upsert device: %v", err)
	}

	got, err := st.GetDevice("test-agent-1")
	if err != nil {
		t.Fatalf("get device: %v", err)
	}
	if got == nil || got.DeviceID != "test-agent-1" || got.Hostname != "WIN-SRV-01" {
		t.Fatalf("unexpected device: %+v", got)
	}
	if got.Metrics.CPUPercent != 12.5 {
		t.Fatalf("cpu = %f, want 12.5", got.Metrics.CPUPercent)
	}

	// Update heartbeat
	if err := st.UpdateDeviceHeartbeat("test-agent-1", protocol.HealthMetrics{CPUPercent: 20.0}, protocol.StatusOnline); err != nil {
		t.Fatalf("update heartbeat: %v", err)
	}
	got2, _ := st.GetDevice("test-agent-1")
	if got2.Metrics.CPUPercent != 20.0 {
		t.Fatalf("updated cpu = %f, want 20.0", got2.Metrics.CPUPercent)
	}

	devices, err := st.ListDevices()
	if err != nil || len(devices) != 1 {
		t.Fatalf("list devices: %v, count=%d", err, len(devices))
	}
}

func TestCloudCommandQueueAndDispatch(t *testing.T) {
	st := newTestStore(t)

	cmd1 := protocol.CommandPayload{
		CommandID:  "cmd-1",
		DeviceID:   "dev-1",
		Command:    "Get-Process",
		TimeoutSec: 10,
		CreatedAt:  time.Now().Unix() - 10,
	}
	cmd2 := protocol.CommandPayload{
		CommandID:  "cmd-2",
		DeviceID:   "dev-1",
		Command:    "Get-Date",
		TimeoutSec: 5,
		CreatedAt:  time.Now().Unix(),
	}

	if err := st.EnqueueCommand(cmd1); err != nil {
		t.Fatalf("enqueue 1: %v", err)
	}
	if err := st.EnqueueCommand(cmd2); err != nil {
		t.Fatalf("enqueue 2: %v", err)
	}

	pending, err := st.GetPendingCommands("dev-1")
	if err != nil {
		t.Fatalf("get pending: %v", err)
	}
	if len(pending) != 2 {
		t.Fatalf("pending count = %d, want 2", len(pending))
	}
	if pending[0].CommandID != "cmd-1" || pending[1].CommandID != "cmd-2" {
		t.Fatalf("pending FIFO order incorrect: %+v", pending)
	}

	// Mark dispatched
	if err := st.MarkCommandDispatched("cmd-1"); err != nil {
		t.Fatalf("mark dispatched: %v", err)
	}
	pendingAfter, _ := st.GetPendingCommands("dev-1")
	if len(pendingAfter) != 1 || pendingAfter[0].CommandID != "cmd-2" {
		t.Fatalf("pending after dispatch: %+v", pendingAfter)
	}

	rec, _ := st.GetCommand("cmd-1")
	if rec.Status != store.StatusDispatched {
		t.Fatalf("cmd-1 status = %s, want DISPATCHED", rec.Status)
	}
}

func TestSaveAndListResults(t *testing.T) {
	st := newTestStore(t)

	_ = st.EnqueueCommand(protocol.CommandPayload{
		CommandID: "cmd-res-1",
		DeviceID:  "dev-1",
		Command:   "Write-Output 'hi'",
	})

	res := protocol.ResultPayload{
		CommandID:  "cmd-res-1",
		DeviceID:   "dev-1",
		Stdout:     "hi\r\n",
		Stderr:     "",
		ExitCode:   0,
		ExecutedAt: time.Now().Unix(),
	}

	if err := st.SaveResult(res); err != nil {
		t.Fatalf("save result: %v", err)
	}

	gotRes, err := st.GetResult("cmd-res-1")
	if err != nil || gotRes == nil {
		t.Fatalf("get result: %v, got=%+v", err, gotRes)
	}
	if gotRes.Stdout != "hi\r\n" || gotRes.ExitCode != 0 {
		t.Fatalf("result content mismatch: %+v", gotRes)
	}

	// Verify command status completed
	cmdRec, _ := st.GetCommand("cmd-res-1")
	if cmdRec.Status != store.StatusCompleted {
		t.Fatalf("command status = %s, want COMPLETED", cmdRec.Status)
	}

	list, err := st.ListResults("dev-1", 10)
	if err != nil || len(list) != 1 {
		t.Fatalf("list results: %v, len=%d", err, len(list))
	}
}
