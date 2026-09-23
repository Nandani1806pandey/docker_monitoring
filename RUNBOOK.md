# Runbook

Operational procedures for the current state of the system. Every command
below has been run in this environment as part of writing this document —
none are speculative. As new phases land (see
`docs/PROJECT_COMPLETION_ROADMAP.md`), this file grows; nothing here is
final.

## Installation

```bash
git clone <repo>
cd docker-monitor
go build ./...
```

No external dependencies to install beyond the Go toolchain — the
in-memory storage backend has zero runtime dependencies. `golang.org/x/
crypto` is fetched via a GitHub-mirror `replace` in `go.mod` (see
`BRAIN.md` for why); if `go build`/`go mod tidy` needs to re-fetch it and
`golang.org` itself is unreachable from your network, that replace
directive is what makes it work anyway.

## Startup

```bash
export DM_SESSION_SECRET="$(openssl rand -hex 32)"
export DM_BOOTSTRAP_ADMIN_EMAIL="admin@example.com"
export DM_BOOTSTRAP_ADMIN_PASSWORD="$(openssl rand -hex 16)"
go run ./cmd/controlplane
```

The bootstrap admin variables only take effect the *first* time the
process runs against an empty user store — with the in-memory backend,
that's every process start (nothing persists across restarts yet; see
Gap Analysis Section C on persistence). Save the generated password if you
used `openssl rand` for it — it is not logged or recoverable otherwise.

Confirm it's up:
```bash
curl http://localhost:8080/api/v1/system/health
# {"status":"ok"}
```

## Shutdown

`Ctrl-C` (SIGINT) or `SIGTERM` triggers a graceful shutdown with a 10s
timeout for in-flight requests (`cmd/controlplane/main.go`). No separate
stop command is needed.

## Restart

With the current in-memory backend, a restart **loses all data**
(users, hosts, sessions, events) except the CA, which is persisted to
`DM_CA_DIR` (default `./data/ca`) specifically so agent enrollment
survives restarts even though nothing else does yet. Once Phase A
(persistence) lands, this section gets rewritten to describe a real
restart-with-data-intact procedure.

## Agent enrollment

1. Log in and create a host record:
   ```bash
   curl -c cookies.txt -X POST http://localhost:8080/api/v1/auth/login \
     -H "Content-Type: application/json" \
     -d '{"email":"admin@example.com","password":"<your-password>"}'

   curl -b cookies.txt -X POST http://localhost:8080/api/v1/hosts \
     -H "Content-Type: application/json" \
     -d '{"name":"my-docker-host","kind":"remote","address":"192.0.2.10"}'
   ```
2. Issue an enrollment token for that host (valid 15 minutes, single use):
   ```bash
   curl -b cookies.txt -X POST http://localhost:8080/api/v1/hosts/$HOST_ID/enrollment-token
   ```
3. Run the agent on the target host with that token — see
   `cmd/agent/main.go` for its current flag/env-var surface (a dedicated
   `Try the agent locally` section already exists in `README.md`; that's
   the authoritative up-to-date version of this step, kept there rather
   than duplicated here to avoid drift).

## Adding hosts / removing hosts

Adding: see enrollment above (for `remote`), or a bare
`POST /api/v1/hosts` with `"kind":"local"` for a host the control plane
itself runs alongside (no enrollment/mTLS needed for `local`).

Removing: `DELETE /api/v1/hosts/{id}` (admin-only). This does not currently
cascade to revoke an already-issued agent certificate (see Gap Analysis
Section B.3 — certificate revocation doesn't exist yet); a removed host's
agent, if still running, can still authenticate at the mTLS listener until
its certificate expires. This is a known gap, not intended behavior — see
Phase I in the roadmap.

## Database backup / restore

Not applicable yet — no persistent database exists (Phase A). This
section will be written, with tested commands, when that phase lands.

## Certificate renewal / troubleshooting

The internal CA is generated on first run and persisted to `DM_CA_DIR` as
`ca-cert.pem`/`ca-key.pem` (0600 permissions, directory 0700). An agent's
own certificate is issued at enrollment, valid 90 days, with no rotation
mechanism yet (Phase I) — a cert nearing/past expiry currently requires
re-running the enrollment flow from scratch (a new token, a new
`POST /api/v1/hosts/{id}/enrollment-token`).

To inspect the CA:
```bash
openssl x509 -in ./data/ca/ca-cert.pem -noout -text | head -20
```

If an agent fails to connect with a TLS handshake error, check:
1. Is the agent using a certificate signed by *this* control plane's CA
   (i.e. did the CA regenerate because `DM_CA_DIR` was empty/wrong on a
   restart)? Compare `openssl x509 -in <agent-cert> -noout -issuer`
   against the control plane's CA subject.
2. Is the agent's certificate expired? `openssl x509 -in <agent-cert>
   -noout -enddate`.

## Docker troubleshooting (agent side)

The agent talks to the Docker Engine API over the local Unix socket
(default `/var/run/docker.sock`). If the agent reports it can't reach
Docker:
```bash
ls -l /var/run/docker.sock   # confirm it exists and is reachable
docker info                  # confirm the daemon itself is up
```
`internal/agent/docker/client.go` doesn't retry a fully-down daemon
indefinitely with backoff yet beyond whatever the collector's own loop
does — see `internal/agent/collector/collector.go` for the current retry
behavior before assuming a specific resilience guarantee that isn't
written down elsewhere.

## WebSocket troubleshooting

`/ws/events`, `/ws/hosts/{id}/stats`, `/ws/containers/{id}/stats` are
plain WebSocket upgrades over the same HTTP port as the REST API — no
separate port. If a client isn't receiving broadcasts:
1. Confirm the hub is actually wired in — `WithHub(...)` must have been
   called (`cmd/controlplane/main.go` always calls it; a test server
   built via `NewServer(...)` alone, without `WithHub`, will 404 every
   `/ws/*` route by design — see `router.go`'s `registerWSRoutes` doc
   comment).
2. Confirm you subscribed to the topic *before* the event you're waiting
   for occurred — the hub does not replay history to a newly-connected
   client (this is documented, current behavior, not a bug).

## Alert / notification troubleshooting

Not applicable yet — neither exists (Phases E/F).

## Emergency recovery / rollback

With the current in-memory backend, "recovery" from a bad state is
restarting the process (which also resets all data — see Restart above).
Once Phase A lands, this section becomes about database-level rollback
(point-in-time restore) instead, and will be rewritten then.
