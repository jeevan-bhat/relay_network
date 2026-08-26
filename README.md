# terminal-app

A resilient remote-terminal / device-management system: a mobile controller talks to a
Windows background agent through a relay, with offline queuing so commands and results
survive network drops, sleep, and reboots.

> **Status:** Phase 1 complete — the Windows agent runs standalone (no relay yet).

## Architecture (target)

| Component      | Role                                                                  |
| -------------- | --------------------------------------------------------------------- |
| Mobile client  | Dashboard: terminal, file transfer, device-health monitoring          |
| Relay server   | NAT traversal, message routing, cloud-side offline command queue      |
| **Windows agent** | Executes commands, monitors health, local offline queue — **this phase** |

## Roadmap

- [x] **Phase 1 — Core agent & terminal:** Windows service executes PowerShell, captures output, persists to a local SQLite queue with crash recovery and retries. See [`agent/`](agent/).
- [ ] Phase 2 — Relay & WebSocket layer
- [ ] Phase 3 — Resilience layer (heartbeat, reconnection backoff, queue flush/sync)
- [ ] Phase 4 — Mobile interface & polish

## Security & responsible use

This is a remote-administration tool. Deploy the agent **only on machines you own or are
authorized to administer.** Phase 1 trusts its local queue and exposes no network surface;
authentication and end-to-end encryption arrive with the relay in Phases 2–3.

See [`agent/README.md`](agent/README.md) to build and run the agent.
