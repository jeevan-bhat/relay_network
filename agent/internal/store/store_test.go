package store_test

import (
	"path/filepath"
	"testing"

	"terminalagent/internal/store"
)

func tempStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestEnqueueClaimComplete(t *testing.T) {
	st := tempStore(t)

	id, err := st.Enqueue(store.Payload{Command: "Write-Output hi"})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if id == "" {
		t.Fatal("empty command id")
	}

	cmd, err := st.ClaimNext(3)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if cmd == nil {
		t.Fatal("expected a claimed command")
	}
	if cmd.CommandID != id {
		t.Fatalf("claimed id = %s, want %s", cmd.CommandID, id)
	}
	if cmd.Status != store.StatusExecuting {
		t.Fatalf("status = %s, want EXECUTING", cmd.Status)
	}
	if cmd.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1", cmd.Attempts)
	}

	if again, err := st.ClaimNext(3); err != nil || again != nil {
		t.Fatalf("expected empty queue, got cmd=%v err=%v", again, err)
	}

	if err := st.SaveResult(store.Result{CommandID: id, Stdout: "hi", ExitCode: 0}); err != nil {
		t.Fatalf("save result: %v", err)
	}
	if err := st.Complete(id, store.StatusCompleted); err != nil {
		t.Fatalf("complete: %v", err)
	}

	got, err := st.GetCommand(id)
	if err != nil || got == nil {
		t.Fatalf("get command: %v (got=%v)", err, got)
	}
	if got.Status != store.StatusCompleted {
		t.Fatalf("final status = %s, want COMPLETED", got.Status)
	}

	results, err := st.ListResults(id, 10)
	if err != nil {
		t.Fatalf("list results: %v", err)
	}
	if len(results) != 1 || results[0].Stdout != "hi" {
		t.Fatalf("results = %+v, want one row with stdout 'hi'", results)
	}
	if results[0].Synced {
		t.Fatal("result should be unsynced in Phase 1")
	}
}

func TestClaimEmpty(t *testing.T) {
	st := tempStore(t)
	cmd, err := st.ClaimNext(3)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if cmd != nil {
		t.Fatalf("expected nil on empty queue, got %+v", cmd)
	}
}

func TestRecoverStaleRequeues(t *testing.T) {
	st := tempStore(t)
	id, _ := st.Enqueue(store.Payload{Command: "x"})
	if _, err := st.ClaimNext(3); err != nil { // -> EXECUTING, Attempts=1
		t.Fatalf("claim: %v", err)
	}

	n, err := st.RecoverStale(3)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if n != 1 {
		t.Fatalf("recovered = %d, want 1", n)
	}

	got, _ := st.GetCommand(id)
	if got.Status != store.StatusPending {
		t.Fatalf("status = %s, want PENDING after recovery", got.Status)
	}
	// Still claimable, and the attempt counter is preserved (increments to 2).
	cmd, _ := st.ClaimNext(3)
	if cmd == nil || cmd.Attempts != 2 {
		t.Fatalf("re-claim = %+v, want attempts 2", cmd)
	}
}

func TestRecoverStaleFailsAtMaxAttempts(t *testing.T) {
	st := tempStore(t)
	id, _ := st.Enqueue(store.Payload{Command: "x"})

	// Drive the attempt counter up to the cap, leaving it EXECUTING.
	for i := 0; i < 3; i++ {
		cmd, err := st.ClaimNext(3)
		if err != nil {
			t.Fatalf("claim %d: %v", i, err)
		}
		if cmd == nil {
			t.Fatalf("claim %d: nothing to claim (attempts exhausted early)", i)
		}
		if i < 2 {
			if err := st.Requeue(id); err != nil {
				t.Fatalf("requeue: %v", err)
			}
		}
	}
	// Now EXECUTING with Attempts == 3 == max.
	if _, err := st.RecoverStale(3); err != nil {
		t.Fatalf("recover: %v", err)
	}
	got, _ := st.GetCommand(id)
	if got.Status != store.StatusFailed {
		t.Fatalf("status = %s, want FAILED (attempts exhausted)", got.Status)
	}
	if cmd, _ := st.ClaimNext(3); cmd != nil {
		t.Fatalf("exhausted command should not be claimable, got %+v", cmd)
	}
}
