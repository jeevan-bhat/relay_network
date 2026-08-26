package executor_test

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	"terminalagent/internal/executor"
)

func requireWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "windows" {
		t.Skip("powershell.exe is only available on Windows")
	}
}

func TestRunEcho(t *testing.T) {
	requireWindows(t)
	res := executor.New("Bypass").Run(context.Background(), "Write-Output 'hello-world'", 30*time.Second)
	if res.ExitCode != 0 {
		t.Fatalf("exit = %d, stderr = %q", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "hello-world") {
		t.Fatalf("stdout = %q, want it to contain hello-world", res.Stdout)
	}
	if res.TimedOut {
		t.Fatal("unexpected timeout")
	}
}

func TestRunExitCode(t *testing.T) {
	requireWindows(t)
	res := executor.New("Bypass").Run(context.Background(), "exit 3", 30*time.Second)
	if res.ExitCode != 3 {
		t.Fatalf("exit = %d, want 3 (stderr=%q)", res.ExitCode, res.Stderr)
	}
}

func TestRunTimeout(t *testing.T) {
	requireWindows(t)
	start := time.Now()
	res := executor.New("Bypass").Run(context.Background(), "Start-Sleep -Seconds 30", 2*time.Second)
	if !res.TimedOut {
		t.Fatalf("expected TimedOut, got exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if elapsed := time.Since(start); elapsed > 15*time.Second {
		t.Fatalf("timeout took %s, expected the process to be killed near 2s", elapsed)
	}
}

func TestRunStderr(t *testing.T) {
	requireWindows(t)
	// Writing to the error stream should be captured without failing the process.
	res := executor.New("Bypass").Run(context.Background(),
		"[Console]::Error.WriteLine('oops')", 30*time.Second)
	if !strings.Contains(res.Stderr, "oops") {
		t.Fatalf("stderr = %q, want it to contain oops", res.Stderr)
	}
}
