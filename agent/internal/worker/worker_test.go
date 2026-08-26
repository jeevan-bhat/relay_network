package worker_test

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"terminalagent/internal/config"
	"terminalagent/internal/executor"
	"terminalagent/internal/store"
	"terminalagent/internal/worker"
)

func newWorker(t *testing.T) (*worker.Worker, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return worker.New(st, executor.New("Bypass"), config.Default(), log), st
}

func TestRunOnceEmpty(t *testing.T) {
	w, _ := newWorker(t)
	did, err := w.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("run once: %v", err)
	}
	if did {
		t.Fatal("did work on an empty queue")
	}
}

func TestRunOnceExecutes(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("requires powershell.exe")
	}
	w, st := newWorker(t)
	id, err := st.Enqueue(store.Payload{Command: "Write-Output 'from-worker'"})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	did, err := w.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("run once: %v", err)
	}
	if !did {
		t.Fatal("expected work")
	}

	got, _ := st.GetCommand(id)
	if got == nil || got.Status != store.StatusCompleted {
		t.Fatalf("command status = %v, want COMPLETED", got)
	}
	results, _ := st.ListResults(id, 10)
	if len(results) != 1 || !strings.Contains(results[0].Stdout, "from-worker") {
		t.Fatalf("results = %+v, want stdout containing from-worker", results)
	}
}

func TestRunOnceRetriesThenFails(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("requires powershell.exe")
	}
	w, st := newWorker(t) // MaxAttempts = 3 (default)
	id, _ := st.Enqueue(store.Payload{Command: "exit 1"})

	for i := 0; i < 3; i++ {
		if _, err := w.RunOnce(context.Background()); err != nil {
			t.Fatalf("run once %d: %v", i, err)
		}
	}
	got, _ := st.GetCommand(id)
	if got.Status != store.StatusFailed {
		t.Fatalf("status = %s, want FAILED after exhausting attempts", got.Status)
	}
	results, _ := st.ListResults(id, 10)
	if len(results) != 3 {
		t.Fatalf("results = %d, want 3 (one per attempt)", len(results))
	}
}
