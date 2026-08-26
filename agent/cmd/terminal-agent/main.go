// Command terminal-agent is the Windows background agent: it executes PowerShell
// commands pulled from a local SQLite queue and persists their results.
//
// Usage:
//
//	terminal-agent run     [--db PATH] [--timeout DUR] [--poll DUR] [--drain]  run the worker (dev)
//	terminal-agent enqueue [--db PATH] [--timeoutSec N] "<command>"            add a command to the queue
//	terminal-agent results [--db PATH] [--id ID] [--limit N]                   show execution results
//	terminal-agent queue   [--db PATH] [--limit N]                            show queued commands
//	terminal-agent install | uninstall | start | stop | restart               manage the Windows service
//
// With no arguments it runs under the Windows Service Control Manager.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"terminalagent/internal/config"
	"terminalagent/internal/executor"
	"terminalagent/internal/netclient"
	"terminalagent/internal/service"
	"terminalagent/internal/store"
	"terminalagent/internal/worker"
)

func main() {
	if len(os.Args) < 2 {
		runService()
		return
	}
	switch os.Args[1] {
	case "run":
		cmdRun(os.Args[2:])
	case "enqueue":
		cmdEnqueue(os.Args[2:])
	case "results":
		cmdResults(os.Args[2:])
	case "queue":
		cmdQueue(os.Args[2:])
	case "install", "uninstall", "start", "stop", "restart":
		cmdService(os.Args[1])
	case "-h", "--help", "help":
		usage()
	default:
		// Unknown token: assume the service manager launched us.
		runService()
	}
}

func usage() {
	fmt.Print(`terminal-agent — resilient remote-terminal agent (Phase 2 & 3: Relay + Resilience)

Commands:
  run       [--db PATH] [--relay URL] [--device-id ID] [--token SEC] [--timeout DUR] [--poll DUR] [--drain]
            run the worker in the foreground (dev / connected mode)
  enqueue   [--db PATH] [--timeoutSec N] "<command>"             add a PowerShell command to the queue
  results   [--db PATH] [--id ID] [--limit N]                    show execution results
  queue     [--db PATH] [--limit N]                              show queued commands
  install | uninstall | start | stop | restart                   manage the Windows service

With no arguments, runs under the Windows Service Control Manager.
`)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}

func stderrLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

func openStore(path string) *store.Store {
	st, err := store.Open(path)
	if err != nil {
		fatal(err)
	}
	return st
}

func cmdRun(args []string) {
	def := config.Default()
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	dbPath := fs.String("db", def.DBPath, "path to the queue database")
	relayURL := fs.String("relay", def.RelayURL, "Relay WebSocket URL (e.g. ws://localhost:8080/ws)")
	deviceID := fs.String("device-id", def.DeviceID, "unique device identifier for this agent")
	token := fs.String("token", def.AuthToken, "secret token for relay authentication")
	timeout := fs.Duration("timeout", def.Timeout, "per-command hard timeout")
	poll := fs.Duration("poll", def.PollInterval, "queue poll interval when idle")
	drain := fs.Bool("drain", false, "process the queue until empty, then exit (no polling)")
	_ = fs.Parse(args)

	cfg := def
	cfg.DBPath = *dbPath
	cfg.RelayURL = *relayURL
	cfg.DeviceID = *deviceID
	cfg.AuthToken = *token
	cfg.Timeout = *timeout
	cfg.PollInterval = *poll

	log := stderrLogger()
	st := openStore(cfg.DBPath)
	defer st.Close()

	if n, err := st.RecoverStale(cfg.MaxAttempts); err != nil {
		log.Error("recover stale", "err", err)
	} else if n > 0 {
		log.Info("recovered stale commands", "count", n)
	}

	w := worker.New(st, executor.New(cfg.ExecPolicy), cfg, log)

	if *drain {
		for {
			did, err := w.RunOnce(context.Background())
			if err != nil {
				fatal(err)
			}
			if !did {
				break
			}
		}
		log.Info("drain complete")
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if cfg.RelayURL != "" {
		client := netclient.New(st, cfg, func() {
			w.Wake()
		}, log)
		w.SetOnResult(func(res store.Result) {
			client.ReportResult(res)
		})
		go client.Run(ctx)
		log.Info("relay client started", "relay", cfg.RelayURL, "deviceId", cfg.DeviceID)
	}

	log.Info("worker running (ctrl+c to stop)", "db", cfg.DBPath, "timeout", cfg.Timeout)
	if err := w.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		fatal(err)
	}
	log.Info("worker stopped")
}

func cmdEnqueue(args []string) {
	def := config.Default()
	fs := flag.NewFlagSet("enqueue", flag.ExitOnError)
	dbPath := fs.String("db", def.DBPath, "path to the queue database")
	timeoutSec := fs.Int("timeoutSec", 0, "per-command timeout override in seconds (0 = use default)")
	_ = fs.Parse(args)

	command := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if command == "" {
		fatal(errors.New(`usage: terminal-agent enqueue [--db PATH] [--timeoutSec N] "<powershell command>"`))
	}

	st := openStore(*dbPath)
	defer st.Close()

	id, err := st.Enqueue(store.Payload{Command: command, TimeoutSec: *timeoutSec})
	if err != nil {
		fatal(err)
	}
	fmt.Println(id)
}

func cmdResults(args []string) {
	def := config.Default()
	fs := flag.NewFlagSet("results", flag.ExitOnError)
	dbPath := fs.String("db", def.DBPath, "path to the queue database")
	id := fs.String("id", "", "filter by CommandId")
	limit := fs.Int("limit", 50, "maximum rows to show")
	_ = fs.Parse(args)

	st := openStore(*dbPath)
	defer st.Close()

	results, err := st.ListResults(*id, *limit)
	if err != nil {
		fatal(err)
	}
	if len(results) == 0 {
		fmt.Println("(no results)")
		return
	}
	for _, r := range results {
		ts := time.Unix(r.ExecutedAt, 0).Format(time.RFC3339)
		fmt.Printf("-- %s  exit=%d  synced=%t  %s\n", r.CommandID, r.ExitCode, r.Synced, ts)
		if s := strings.TrimRight(r.Stdout, "\r\n"); s != "" {
			fmt.Printf("   stdout: %s\n", indent(s))
		}
		if s := strings.TrimRight(r.Stderr, "\r\n"); s != "" {
			fmt.Printf("   stderr: %s\n", indent(s))
		}
	}
}

func cmdQueue(args []string) {
	def := config.Default()
	fs := flag.NewFlagSet("queue", flag.ExitOnError)
	dbPath := fs.String("db", def.DBPath, "path to the queue database")
	limit := fs.Int("limit", 50, "maximum rows to show")
	_ = fs.Parse(args)

	st := openStore(*dbPath)
	defer st.Close()

	cmds, err := st.ListCommands(*limit)
	if err != nil {
		fatal(err)
	}
	if len(cmds) == 0 {
		fmt.Println("(queue empty)")
		return
	}
	for _, c := range cmds {
		ts := time.Unix(c.CreatedAt, 0).Format(time.RFC3339)
		p, _ := c.Decode()
		fmt.Printf("%-10s attempts=%d  %s  %s\n    %s\n", c.Status, c.Attempts, ts, c.CommandID, p.Command)
	}
}

func cmdService(action string) {
	if err := service.Control(action, config.Default(), stderrLogger()); err != nil {
		fatal(err)
	}
	fmt.Printf("service: %s ok\n", action)
}

func runService() {
	cfg := config.Default()
	log := serviceLogger(cfg)
	if err := service.Run(cfg, log); err != nil {
		log.Error("service run", "err", err)
		os.Exit(1)
	}
}

// serviceLogger logs to stderr when interactive, otherwise to a file next to the
// queue database (a Windows service has no console).
func serviceLogger(cfg config.Config) *slog.Logger {
	if service.Interactive() {
		return stderrLogger()
	}
	_ = os.MkdirAll(cfg.DataDir(), 0o755)
	f, err := os.OpenFile(filepath.Join(cfg.DataDir(), "agent.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return stderrLogger()
	}
	return slog.New(slog.NewTextHandler(f, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

func indent(s string) string {
	return strings.ReplaceAll(s, "\n", "\n           ")
}
