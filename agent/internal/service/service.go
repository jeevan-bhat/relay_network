// Package service adapts the worker to run as a Windows service (or
// interactively) via kardianos/service.
package service

import (
	"context"
	"errors"
	"log/slog"
	"time"

	ksvc "github.com/kardianos/service"

	"terminalagent/internal/config"
	"terminalagent/internal/executor"
	"terminalagent/internal/store"
	"terminalagent/internal/worker"
)

var svcConfig = &ksvc.Config{
	Name:        "TerminalAgent",
	DisplayName: "Terminal Agent",
	Description: "Resilient remote-terminal agent (Phase 1: local PowerShell executor + SQLite queue).",
}

type program struct {
	cfg    config.Config
	log    *slog.Logger
	cancel context.CancelFunc
	done   chan struct{}
}

func (p *program) Start(s ksvc.Service) error {
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.done = make(chan struct{})
	go p.run(ctx)
	return nil
}

func (p *program) run(ctx context.Context) {
	defer close(p.done)

	st, err := store.Open(p.cfg.DBPath)
	if err != nil {
		p.log.Error("open store", "err", err)
		return
	}
	defer st.Close()

	if n, err := st.RecoverStale(p.cfg.MaxAttempts); err != nil {
		p.log.Error("recover stale", "err", err)
	} else if n > 0 {
		p.log.Info("recovered stale commands", "count", n)
	}

	w := worker.New(st, executor.New(p.cfg.ExecPolicy), p.cfg, p.log)
	p.log.Info("agent started", "db", p.cfg.DBPath)
	if err := w.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		p.log.Error("worker stopped", "err", err)
	}
}

func (p *program) Stop(s ksvc.Service) error {
	if p.cancel != nil {
		p.cancel()
	}
	if p.done != nil {
		select {
		case <-p.done:
		case <-time.After(15 * time.Second):
			p.log.Warn("worker did not stop within 15s")
		}
	}
	return nil
}

func build(cfg config.Config, log *slog.Logger) (ksvc.Service, error) {
	return ksvc.New(&program{cfg: cfg, log: log}, svcConfig)
}

// Interactive reports whether the process is running interactively (not under
// the service control manager).
func Interactive() bool { return ksvc.Interactive() }

// Run runs the agent under the service manager (or interactively).
func Run(cfg config.Config, log *slog.Logger) error {
	svc, err := build(cfg, log)
	if err != nil {
		return err
	}
	return svc.Run()
}

// Control performs a service action: install, uninstall, start, stop, restart.
func Control(action string, cfg config.Config, log *slog.Logger) error {
	svc, err := build(cfg, log)
	if err != nil {
		return err
	}
	return ksvc.Control(svc, action)
}
