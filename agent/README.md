# Terminal Agent (Phase 1)

A Windows background service (Go) that executes PowerShell commands from a durable,
crash-resilient local SQLite queue. Runs standalone — no relay required yet.

## What it does

- Pulls commands from a local `queue.db` (`CommandQueue` table).
- Executes each via `powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass -Command …`,
  capturing stdout/stderr with a hard timeout (default 60s).
- Persists results to `ResultQueue` (ready to sync to a relay in Phase 3).
- Survives crashes: commands left mid-flight (`EXECUTING`) are recovered on startup;
  failures retry up to 3 times before being marked `FAILED`.

## Build

```bash
go build ./...
```

The SQLite driver is pure Go (`modernc.org/sqlite`) — **no C compiler / cgo required.**

## Try it locally (no admin, no relay)

Use a repo-local database so you don't need write access to `%ProgramData%`. `run --drain`
processes everything currently queued and then exits (handy for scripting and testing):

```bash
go run ./cmd/terminal-agent enqueue --db ./data/queue.db "Get-Date"
go run ./cmd/terminal-agent run     --db ./data/queue.db --drain
go run ./cmd/terminal-agent results --db ./data/queue.db
go run ./cmd/terminal-agent queue   --db ./data/queue.db
```

Drop `--drain` to run the worker continuously (polls the queue; Ctrl+C to stop).

## Run as a Windows service (elevated shell)

By default the service uses `%ProgramData%\TerminalAgent\queue.db` and logs to `agent.log`
beside it.

```bash
go build -o terminal-agent.exe ./cmd/terminal-agent
./terminal-agent.exe install
./terminal-agent.exe start
# … enqueue against %ProgramData%\TerminalAgent\queue.db …
./terminal-agent.exe stop
./terminal-agent.exe uninstall
```

## Test

```bash
go test ./...
```

Store/queue tests run everywhere; executor/worker tests that spawn PowerShell are skipped
on non-Windows.

## Configuration

| Setting         | Default                                | Override                                                   |
| --------------- | -------------------------------------- | ---------------------------------------------------------- |
| Database path   | `%ProgramData%\TerminalAgent\queue.db` | `--db`, or `TERMINAL_AGENT_DB`                             |
| Command timeout | 60s                                    | `--timeout` (run) / `--timeoutSec` per command (enqueue)   |
| Poll interval   | 1s                                     | `--poll` (run)                                             |
| Max attempts    | 3                                      | compile-time default (`config.Default`)                    |

## Layout

```
cmd/terminal-agent/   CLI + service entrypoint
internal/config/      configuration and defaults
internal/store/       SQLite queue (CommandQueue, ResultQueue) + crash recovery
internal/executor/    PowerShell subprocess with timeout + captured output
internal/worker/      claim → execute → persist → retry loop
internal/service/     kardianos/service adapter (install/start/stop)
```

## Known limitations (Phase 1)

- **Single worker** — commands run one at a time.
- **Timeout kills `powershell.exe` only**, not a spawned child-process tree (job-object
  tree-kill is a Phase-3 hardening item).
- **`ExecutionPolicy Bypass`** is the default for convenience; tighten it for production.
  The agent trusts whatever is in its local queue — there is no authentication until the
  relay lands.
```
