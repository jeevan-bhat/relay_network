// Package worker ties the queue and executor together: it claims pending
// commands, executes them, persists results, and applies the retry policy.
package worker

import (
	"context"
	"log/slog"
	"time"

	"terminalagent/internal/config"
	"terminalagent/internal/executor"
	"terminalagent/internal/store"
)

// Worker drains the command queue.
type Worker struct {
	store    *store.Store
	exec     *executor.PowerShell
	cfg      config.Config
	log      *slog.Logger
	onResult func(store.Result)
	wake     chan struct{}
}

// New builds a Worker.
func New(st *store.Store, ex *executor.PowerShell, cfg config.Config, log *slog.Logger) *Worker {
	if log == nil {
		log = slog.Default()
	}
	return &Worker{
		store: st,
		exec:  ex,
		cfg:   cfg,
		log:   log,
		wake:  make(chan struct{}, 1),
	}
}

// SetOnResult sets a callback triggered when a command completes.
func (w *Worker) SetOnResult(fn func(store.Result)) {
	w.onResult = fn
}

// Wake signals the worker to check the queue immediately.
func (w *Worker) Wake() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// RunOnce claims and executes a single command. It returns didWork=false when
// the queue is empty. Errors specific to one command are recorded on that
// command (not returned) so the loop keeps running; only infrastructure errors
// (e.g. the queue is unreadable/unwritable) are returned.
func (w *Worker) RunOnce(ctx context.Context) (didWork bool, err error) {
	cmd, err := w.store.ClaimNext(w.cfg.MaxAttempts)
	if err != nil {
		return false, err
	}
	if cmd == nil {
		return false, nil
	}

	p, derr := cmd.Decode()
	if derr != nil {
		w.log.Error("invalid payload", "command", cmd.CommandID, "err", derr)
		res := store.Result{
			CommandID:  cmd.CommandID,
			Stderr:     "invalid payload: " + derr.Error(),
			ExitCode:   -1,
			ExecutedAt: time.Now().Unix(),
		}
		_ = w.store.SaveResult(res)
		_ = w.store.Complete(cmd.CommandID, store.StatusFailed)
		if w.onResult != nil {
			w.onResult(res)
		}
		return true, nil
	}

	timeout := w.cfg.Timeout
	if p.TimeoutSec > 0 {
		timeout = time.Duration(p.TimeoutSec) * time.Second
	}

	w.log.Info("executing", "command", cmd.CommandID, "attempt", cmd.Attempts)
	// Detach from ctx so a shutdown signal does not kill an in-flight command;
	// the per-command timeout still bounds execution.
	res := w.exec.Run(context.Background(), p.Command, timeout)

	stResult := store.Result{
		CommandID:  cmd.CommandID,
		Stdout:     res.Stdout,
		Stderr:     res.Stderr,
		ExitCode:   res.ExitCode,
		ExecutedAt: time.Now().Unix(),
	}

	if serr := w.store.SaveResult(stResult); serr != nil {
		// If we cannot persist the result, surface it as an infrastructure error.
		return true, serr
	}

	switch {
	case res.ExitCode == 0:
		_ = w.store.Complete(cmd.CommandID, store.StatusCompleted)
		w.log.Info("completed", "command", cmd.CommandID)
		if w.onResult != nil {
			w.onResult(stResult)
		}
	case cmd.Attempts < w.cfg.MaxAttempts:
		_ = w.store.Requeue(cmd.CommandID)
		w.log.Warn("failed, will retry", "command", cmd.CommandID, "attempt", cmd.Attempts, "exit", res.ExitCode)
	default:
		_ = w.store.Complete(cmd.CommandID, store.StatusFailed)
		w.log.Error("failed permanently", "command", cmd.CommandID, "attempts", cmd.Attempts, "exit", res.ExitCode)
		if w.onResult != nil {
			w.onResult(stResult)
		}
	}
	return true, nil
}

// Run drains the queue, then polls on an interval or wake signal until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) error {
	ticker := time.NewTicker(w.cfg.PollInterval)
	defer ticker.Stop()

	for {
		for { // drain everything currently available
			if ctx.Err() != nil {
				return ctx.Err()
			}
			did, err := w.RunOnce(ctx)
			if err != nil {
				w.log.Error("run once", "err", err)
				break
			}
			if !did {
				break
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-w.wake:
			// Woken up by new command arrival
		case <-ticker.C:
		}
	}
}
