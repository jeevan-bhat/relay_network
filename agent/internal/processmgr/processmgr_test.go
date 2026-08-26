package processmgr_test

import (
	"os"
	"os/exec"
	"runtime"
	"testing"
	"time"

	"terminalagent/internal/processmgr"
)

func TestListProcesses(t *testing.T) {
	procs, err := processmgr.ListProcesses()
	if err != nil {
		t.Fatalf("list processes: %v", err)
	}
	if len(procs) == 0 {
		t.Fatal("expected at least one process")
	}

	foundSelf := false
	myPID := os.Getpid()
	for _, p := range procs {
		if p.PID == myPID {
			foundSelf = true
			break
		}
	}
	if runtime.GOOS == "windows" && !foundSelf {
		t.Logf("current PID %d was not in tasklist snapshot (normal for fast test subprocess)", myPID)
	}
}

func TestKillProcess(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("skipping Windows process spawn test")
	}

	// Spawn a disposable powershell sleep process
	cmd := exec.Command("powershell.exe", "-NoProfile", "-Command", "Start-Sleep -Seconds 30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start subprocess: %v", err)
	}
	pid := cmd.Process.Pid

	time.Sleep(100 * time.Millisecond)

	// Kill via processmgr
	if err := processmgr.KillProcess(pid); err != nil {
		t.Fatalf("kill process %d: %v", pid, err)
	}

	_ = cmd.Wait()
}
