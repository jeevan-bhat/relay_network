// Package executor runs shell commands as PowerShell subprocesses with a hard
// timeout and captured stdout/stderr.
package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"time"
)

// Result is the outcome of a single command execution.
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int  // process exit code; -1 if it never ran or timed out
	TimedOut bool // true if killed by the hard timeout
	Err      error
}

// PowerShell executes commands via powershell.exe.
type PowerShell struct {
	// ExecPolicy is passed to -ExecutionPolicy (default "Bypass").
	ExecPolicy string
}

// New returns a PowerShell executor.
func New(execPolicy string) *PowerShell {
	if execPolicy == "" {
		execPolicy = "Bypass"
	}
	return &PowerShell{ExecPolicy: execPolicy}
}

// Run executes command with a hard timeout. Stdout and stderr are captured
// concurrently: os/exec spawns copy goroutines for io.Writer sinks, so large
// output cannot deadlock. The parent ctx is honored for cancellation, but the
// worker passes a detached context so shutdown does not kill in-flight work.
func (p *PowerShell) Run(ctx context.Context, command string, timeout time.Duration) Result {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, "powershell.exe",
		"-NoProfile", "-NonInteractive", "-ExecutionPolicy", p.ExecPolicy, "-Command", command)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()

	res := Result{Stdout: stdout.String(), Stderr: stderr.String()}

	switch {
	case errors.Is(cctx.Err(), context.DeadlineExceeded):
		res.TimedOut = true
		res.ExitCode = -1
		res.Err = fmt.Errorf("command timed out after %s", timeout)
		if res.Stderr == "" {
			res.Stderr = res.Err.Error()
		}
	case runErr != nil:
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			res.ExitCode = ee.ExitCode()
		} else {
			// Failed to start (e.g. powershell.exe not found) or was cancelled.
			res.ExitCode = -1
			res.Err = runErr
			if res.Stderr == "" {
				res.Stderr = runErr.Error()
			}
		}
	default:
		res.ExitCode = 0
	}
	return res
}
