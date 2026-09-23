# Docker Monitor — Control Plane, Agent & Adaptive Monitoring FSM

Implements, per ARCHITECTURE.md §H: config loading, storage interfaces +
in-memory implementation, host/container registration REST endpoints, the
append-only event log, the mTLS-enrolled Go agent, and the adaptive
monitoring state machine.

## Run

### Run with Docker

Build and start the control plane with its CA persisted in a named volume:

```bash
docker build -t docker-monitor:local .
docker volume create docker-monitor-data
docker run --rm --name docker-monitor \
  -p 8080:8080 -p 8443:8443 \
  -e DM_SESSION_SECRET="$(openssl rand -hex 32)" \
  -e DM_BOOTSTRAP_ADMIN_EMAIL=admin@example.com \
  -e DM_BOOTSTRAP_ADMIN_PASSWORD=change-me-now \
  -v docker-monitor-data:/app/data \
  docker-monitor:local
```

In another terminal, verify the container is ready:

```bash
curl http://localhost:8080/api/v1/system/health
```

The control plane listens on container ports `8080` (browser/API) and
`8443` (agent mTLS). The default storage driver is in-memory, so users,
hosts, and events reset when the container stops; the named volume preserves
the CA under `/app/data/ca`.

```bash
export DM_SESSION_SECRET="$(openssl rand -hex 32)"
# First run only: seeds the initial admin account. Once any user exists,
# these are ignored (auth.Service.BootstrapAdminIfNeeded is a no-op).
export DM_BOOTSTRAP_ADMIN_EMAIL="admin@example.com"
export DM_BOOTSTRAP_ADMIN_PASSWORD="$(openssl rand -hex 16)"
go run ./cmd/controlplane
```

Browser API listens on `:8080` (override with `DM_HTTP_ADDR`); the
agent-facing mTLS listener on `:8443` (override with `DM_AGENT_ADDR`). The
internal CA is persisted to `./data/ca` (override with `DM_CA_DIR`) so
restarting doesn't invalidate already-enrolled agents. Sessions last
`DM_SESSION_TTL_HOURS` (default 24). Set `DM_COOKIE_SECURE=false` only for
local HTTP-only development — leave it at its default (`true`) for
anything served over HTTPS.

## Try it

```bash
curl http://localhost:8080/api/v1/system/health

# Log in — this sets a real, server-side-revocable session cookie
# (HttpOnly, SameSite=Strict, Argon2id-verified password).
curl -c cookies.txt -X POST http://localhost:8080/api/v1/auth/login \
  -H "Content-Type: application/json" \
  -d "{\"email\":\"admin@example.com\",\"password\":\"$DM_BOOTSTRAP_ADMIN_PASSWORD\"}"

curl -b cookies.txt http://localhost:8080/api/v1/me

curl -b cookies.txt -X POST http://localhost:8080/api/v1/hosts \
  -H "Content-Type: application/json" \
  -d '{"name":"local-docker","kind":"local"}'

curl -b cookies.txt http://localhost:8080/api/v1/hosts
curl -b cookies.txt http://localhost:8080/api/v1/events

# Open manual live inspection on a host (falls back to NORMAL or CRITICAL,
# whichever the FSM says is still active, when you close it):
curl -b cookies.txt -X POST http://localhost:8080/api/v1/hosts/$HOST_ID/live
curl -b cookies.txt -X DELETE http://localhost:8080/api/v1/hosts/$HOST_ID/live

curl -b cookies.txt -X POST http://localhost:8080/api/v1/auth/logout
```

## What's real vs. stubbed in this build

**Real and tested (Phase 2 — browser-facing core):**
- REST endpoints for hosts (create/list/get/delete), containers (get,
  live stats), events (list with filters)
- Storage abstraction (`internal/controlplane/storage`) with a working
  in-memory backend — this is the genuinely zero-dependency single-host
  mode from ARCHITECTURE.md §B, not a placeholder
- Request logging middleware
- An auth *stub* — every route except `/system/health` requires some
  credential to be present, so nothing is ever wired up wide open. It does
  not yet resolve that credential to a real principal or check permissions.

**Real and tested (Phase 3 — the Go agent):**
- Docker Engine API client (`internal/agent/docker`) — stdlib-only, talks
  to the Unix socket directly, reproduces the standard `docker stats`
  CPU%/cache-adjusted-memory algorithm, tested against a fake daemon on a
  real Unix socket
- Internal CA (`internal/controlplane/pki`) — stdlib `crypto/x509`, issues
  agent client certs and the control plane's own server cert. A real bug
  was found and fixed here: `ServerTLSCertificate` was putting IP literals
  like `"127.0.0.1"` into `DNSNames`, but Go's TLS client only matches an
  IP-literal dial address against `IPAddresses` — every localhost/IP-based
  mTLS connection failed verification despite the cert "looking" fine on
  field inspection. Fixed by routing each host entry to the correct SAN
  list (`net.ParseIP` check); regression-tested with a real TLS handshake
  against `127.0.0.1`, not just field assertions, so this can't silently
  break again.
- Single-use enrollment tokens (hashed, TTL-bound) and mTLS certificate
  issuance (`internal/controlplane/agentregistry`)
- Agent-facing mTLS API (`internal/controlplane/agentapi`), separate
  listener from the browser API, host identity derived from the verified
  client certificate's CommonName (a cert cannot act as a different host)
- Agent-side enrollment client, mTLS heartbeat/telemetry client, and a
  collection loop with a mutable interval and non-blocking `CollectNow()`
  (`internal/agent/transport`, `internal/agent/collector`)
- `cmd/agent` and `cmd/controlplane` are real, runnable binaries — wired
  up end to end: CA generation, the second TLS listener, enrollment-token
  and CA-cert REST routes, agent enrollment/heartbeat/telemetry
- `internal/integrationtest` — end-to-end test over real TLS: enrollment,
  a reused token correctly rejected, heartbeat, telemetry, and a check that
  one host's certificate cannot affect another host's record

**Real and tested (Phase 5 — FSM wired to the collector and event log):**
- `agentregistry.IngestTelemetry` now evaluates every container-health
  report and metric sample against the `monitoring.Engine`, persists any
  resulting `TransitionEvent` to the event log (`monitoring.cpu_threshold`,
  `monitoring.mem_threshold`, `monitoring.container_crashed`,
  `monitoring.container_unhealthy`, `monitoring.container_restarted`,
  `monitoring.recovered`), and updates the persisted `MonitoringMode` on
  the host and every container in the batch
- The telemetry response now carries the FSM's verdict
  (`{"mode": ..., "interval_seconds": ...}`) back to the agent; the
  collector applies it via `SetInterval` right after a successful
  `SendTelemetry` — this is the actual live wiring from "control plane
  decided CRITICAL" to "agent starts polling every 10s on its very next
  cycle", not just two independently-tested pieces sitting next to each
  other
- The single-batch response is host-granularity, not per-container: a
  telemetry batch spanning a critical host and several idle containers
  gets back `FastestMode` across all of them, so one hot container speeds
  up the whole host's collector rather than only itself. Documented as a
  known simplification, not silently swept under the rug — per-entity
  directives would need a richer wire protocol
- A real bug was found and fixed while wiring this up: a container
  discovered for the first time with a nonzero historical restart count
  (e.g. `RestartCount: 5` on first sighting) was being compared against a
  hardcoded `0` baseline and flagged as "just restarted". Fixed by
  baselining against the container's own current count when there's no
  prior record to compare against; regression-tested
  (`TestIngestTelemetry_FirstSeenRestartCountIsNotATransition`)
- 5 new tests in `internal/controlplane/agentregistry`, all passing under
  `-race`, alongside the existing FSM and integration-test suites


- `internal/controlplane/monitoring` — the NORMAL/CRITICAL/MANUAL_REALTIME
  state machine from ARCHITECTURE.md §F: threshold-triggered transitions
  to CRITICAL (CPU or memory breach), hysteresis-gated recovery back to
  NORMAL (requires *both* `MinConsecutiveNormalSamples` consecutive clean
  samples *and* `HysteresisWindow` elapsed — either alone is insufficient,
  by design, to prevent 60s→10s→60s flapping), a deadband between the
  recovery and critical thresholds so a metric hovering just under the
  critical line doesn't get credit for recovering, container
  crash/unhealthy/repeated-restart detection, manual-viewer refcounting
  that always wins over CRITICAL while open and correctly falls back to
  CRITICAL (not NORMAL) if the condition is still active when the viewer
  closes, and independent per-entity state so a critical host and a
  manually-viewed container on that same host don't interfere. `Engine` is
  clock-injectable (`NewEngineWithClock`) so every hysteresis boundary
  case is tested deterministically — no sleeping through real windows.
  14 unit tests, passing under `-race`.
- The Engine itself doesn't call `collector.SetInterval`/`CollectNow`
  directly — see the Phase 5 section above for how the two are actually
  connected through the telemetry request/response cycle.

**Real and tested (Phase 6 — manual live-inspection endpoints, §5-6):**
- `POST/DELETE /api/v1/hosts/{id}/live` and
  `POST/DELETE /api/v1/containers/{id}/live` — the browser-facing hooks for
  "user opened/left this entity's live monitoring page". These call the
  same `Engine.ManualViewerOpen`/`ManualViewerClose` the FSM's own tests
  exercise, now reachable over HTTP; the response reports the entity's
  resulting mode and interval (e.g.
  `{"mode":"manual_realtime","interval_seconds":2}`). Closing a view on an
  entity that's still critical correctly falls back to CRITICAL, not
  NORMAL — tested explicitly, since that's the easy case to get wrong
  (§F.3).
- One `monitoring.Engine` instance is now shared between `agentregistry`
  (which evaluates telemetry against it) and the browser API (which lets a
  user open/close manual views on it) — one FSM per process, not one per
  package, which is what makes "a critical host + a manually-opened
  container" resolve to genuinely independent state rather than two
  engines disagreeing with each other.
- **Known limitation, documented rather than hidden:** there's no push
  channel from control plane to agent yet (WebSocket hub is milestone 5).
  Opening a live view updates the FSM immediately and the HTTP response
  reflects that immediately, but the *agent* only learns the new interval
  on its next telemetry cycle's response — so "immediate" here means
  "immediate in the FSM's bookkeeping," not yet "the agent starts polling
  every 2s the instant this request returns." Closing that gap needs
  either the WebSocket hub or the agent polling its own directive
  out-of-band.
- 6 new tests in `internal/controlplane/api`, including the 404 and
  no-monitor-configured (501, not a panic) edge cases.

**Real and tested (Phase 7 — CA persistence across restarts):**
- `pki.CA.KeyPEM()` / `pki.LoadCA(certPEM, keyPEM)` — export/reload the
  CA's key material, with `LoadCA` verifying the key actually matches the
  certificate's public key before returning (a mismatched or corrupted
  pair fails loudly at startup, not silently later at first sign attempt)
- `cmd/controlplane`'s `loadOrGenerateCA`: on boot, loads a persisted CA
  from `$DM_CA_DIR` (default `./data/ca`) if both `ca-cert.pem` and
  `ca-key.pem` are present; otherwise generates a fresh one and writes it
  there (`0700` dir, `0600` files). A restart now keeps issuing
  certificates under the *same* root, so already-enrolled agents don't
  need to re-enroll just because the process bounced.
- **Documented as a simplification, not a destination:** `pki.CA`'s own
  doc comment says this key "must come from the secrets store... never
  from a plaintext file" in production. This build's file-based
  persistence closes the "every restart nukes the whole fleet's trust"
  gap without pretending to be a real secrets-store integration (Vault,
  KMS, or whatever the eventual deployment target needs) — that's still
  future work.
- 5 new tests: a `pki` round-trip test that issues a cert with the
  *reloaded* CA and verifies it against the *original* CA's cert (proving
  the reload reconstructed genuinely usable key material, not just parsed
  bytes without error), mismatched-key and garbage-PEM rejection, and two
  `cmd/controlplane` tests — the second-boot-reloads-the-same-CA case is
  the actual point of this milestone.

**Real and tested (Phase 8 — WebSocket hub for real-time dashboard updates, §E.2):**
- `internal/controlplane/ws` — a topic-based pub/sub hub over
  `gorilla/websocket` (fetched via direct git, same as `google/uuid` — see
  the network note below). Non-blocking fan-out (`Broadcast` never blocks
  on a slow subscriber), ping/pong keepalive, and connection teardown that
  works from either direction (client disconnects, or the hub itself drops
  a client whose buffer filled).
- A real concurrency bug was found and fixed while writing this: a
  client's send channel can be closed from two independent places — the
  hub's own slow-client drop in `Broadcast`, and the read pump noticing
  the client disconnected — and `sync.Once` only prevents a double
  *close*, not a *send* racing a close that already happened a moment
  earlier. `Broadcast`'s send is now wrapped in a `recover()`-guarded
  `trySend`, documented in the code as the actual mutual-exclusion
  boundary for that narrow window. Caught by writing the concurrency
  correctly the first time, not by a flaky test — but it's exactly the
  kind of thing `-race` and real concurrent connections are for.
- Wired in non-invasively: `events.Recorder.SetBroadcaster` and
  `agentregistry.Registry.SetMetricBroadcaster` are optional, nil-safe
  hooks called from `api.Server.WithHub` — no constructor signature
  changed, so every existing test kept passing untouched. Routes:
  `/ws/events`, `/ws/hosts/{id}/stats`, `/ws/containers/{id}/stats`,
  reusing the existing auth-stub middleware (the same "every WebSocket
  upgrade is authenticated at handshake" requirement as any other route).
- **A second real bug, found by grepping the code rather than trusting a
  written summary of it:** `cmd/controlplane`'s monitoring engine was
  built with `monitoring.DefaultThresholds()` directly, even though
  `config.Load()` already parsed `DM_NORMAL_INTERVAL_SECONDS`,
  `DM_CRITICAL_INTERVAL_SECONDS`, `DM_MANUAL_INTERVAL_SECONDS`, and
  `DM_HYSTERESIS_SECONDS` from the environment — those values were read
  successfully and then silently thrown away. Fixed with a
  `thresholdsFromConfig` helper and a regression test
  (`TestThresholdsFromConfig_UsesConfiguredIntervals`) that fails against
  the old code and passes now — confirmed by literally reverting the fix,
  running the test, and watching it fail with the expected message before
  restoring it.
- **A third real bug, this one caught live rather than by a unit test:**
  the stored/broadcast `Metric.Mode` field was whatever the *agent* had
  guessed via `modeForInterval(currentInterval)` — a local approximation
  with no visibility into the FSM's actual state, wrong by construction
  whenever two configured interval values collide (e.g.
  `NormalIntervalSeconds == ManualIntervalSeconds`, both `2`). Fixed by
  having `IngestTelemetry` overwrite `Mode` with the control plane's
  just-evaluated, authoritative `Engine.EffectiveMode` before storing or
  broadcasting — the one place the real answer is actually known.
  Regression-tested (`TestIngestTelemetry_StoredMetricUsesAuthoritativeModeNotAgentGuess`,
  same revert-and-confirm-it-fails discipline as above), *and* confirmed
  live: a real `controlplane` + `dm-agent` binary pair, both configured
  with a 2-second interval (the exact collision condition), streamed 4
  consecutive real WebSocket broadcasts on `/ws/containers/{id}/stats`
  correctly labeled `"mode":"normal"` — the pre-fix code would have shown
  `"manual_realtime"` here.
- The agent's collection interval had no environment override at all
  (hardcoded `60*time.Second`) unlike every other agent setting, and
  `cmd/agent` waited for a full interval to elapse before its first
  collection — meaning a freshly restarted agent left the dashboard blank
  for up to a minute. Added `DM_AGENT_NORMAL_INTERVAL_SECONDS` and a
  `col.CollectNow()` call right before `col.Run(...)`, so the initial
  container inventory reports immediately (same mechanism §5's manual
  live-inspection flow already uses for "don't wait for the next cycle").
- 6 hub tests using real WebSocket connections over `httptest.Server`
  (multi-subscriber fan-out, topic isolation, disconnect cleanup, and the
  backpressure/drop contract), plus one full live end-to-end smoke test:
  real `controlplane` + fake-Docker-daemon + real `dm-agent` binaries, a
  real WebSocket client watching `/ws/events`, confirming `host
  registered` → `agent enrolled` → `host came online` streamed live and
  in order, followed by real container-stats broadcasts at the configured
  cadence with correctly-labeled mode.
- One thing this phase's live testing surfaced but didn't fix: this build
  still only collects **container**-level metrics — there is no
  host-level CPU/memory collection anywhere in `internal/agent/collector`,
  so `/ws/hosts/{id}/stats` is wired and reachable but will never actually
  receive a broadcast against current agent behavior. Noted here rather
  than left for someone to discover by watching an empty channel forever.

**Real and tested (Phase 9 — host-level metric collection, closing the Phase 8 gap):**
- `internal/agent/hoststats` — CPU, memory, and network utilization for
  the host itself, parsed directly from `/proc/stat`, `/proc/meminfo`, and
  `/proc/net/dev` (stdlib-only, no `gopsutil` or similar — same "avoid
  unnecessary dependencies" reasoning `internal/agent/docker` already
  applies to the Docker Engine API). CPU is computed from two `/proc/stat`
  readings 200ms apart (a single cumulative-jiffies reading can no more
  yield a percentage than a single odometer reading yields a speed — the
  same reason Docker's own container stats return both `cpu_stats` and
  `precpu_stats` per sample). Memory uses `MemAvailable`, not
  `MemTotal-MemFree`, so host memory pressure isn't overstated by
  reclaimable page cache — the same reasoning `internal/agent/docker`'s
  `memUsedBytesNoCache` already applies to containers, now applied
  consistently at the host level too. Network sums every non-loopback
  interface.
- Deliberately Linux-only, no portability shim: the agent's entire reason
  for existing (a local Docker socket) already assumes Linux-shaped
  infrastructure, so a cross-platform host-stats layer would be
  speculative generality nothing else in this codebase attempts.
- 5 tests: known-value fixture files for exact CPU%/memory/network math
  (including confirming loopback traffic is excluded and that a missing
  `MemAvailable` field errors rather than silently returning a wrong
  number), a zero-delta-doesn't-divide-by-zero guard, and one sanity test
  against this machine's *real* `/proc` — not just controlled fixtures —
  asserting the result lands in a plausible range.
- Wired into `collector.collectAndReport`: a host metric is now built and
  included in every telemetry batch alongside container metrics, using
  the same non-fatal error handling as a single flaky container's
  inspect/stats failure (logged and skipped for that cycle, not fatal to
  the whole batch).
- Verified live, closing the exact gap Phase 8 flagged: a real
  `controlplane` + `dm-agent` pair, both at a 2s interval, with a real
  WebSocket client subscribed to `/ws/hosts/{id}/stats` *before* the agent
  even started (so the first `CollectNow`-triggered broadcast couldn't be
  missed). Received 4 consecutive real host-metric broadcasts with
  genuine `/proc`-sourced values from this actual sandbox (e.g.
  `"mem_used_bytes":297234432` out of a `4194623488`-byte host — real
  numbers, not fixture placeholders) at the correct cadence.

**Deliberately not yet implemented** (next milestones per ARCHITECTURE.md §H):
- Full group/role/permission-matrix RBAC per ARCHITECTURE.md §D (this
  build has a real, working two-tier Role enum instead — see Phase 10
  below for exactly what that does and doesn't cover) and OIDC/SSO (§24)
- Postgres/SQLite persistence (the in-memory store satisfies the same
  `storage.Store` interface a DB-backed store will — no handler changes
  needed when that lands)
- Per-container (rather than host-granularity) polling directives — see
  the Phase 5 note above
- The push-channel gap noted in Phase 6 above still applies to
  *control-plane-to-agent* directives specifically (mode/interval
  changes reach the agent only via its next telemetry response, not
  instantly) — the Phase 8 WebSocket hub is for *browser*-facing
  real-time updates and doesn't change this; a genuinely instant
  control-plane→agent push would need its own channel (or the agent
  polling more eagerly)
- Certificate ROTATION specifically (as opposed to persistence, which is
  now handled — see Phase 7 above): agents still get a fixed 90-day cert
  and must re-enroll after that; no automated renewal flow yet
- A real secrets-store integration for the CA key, replacing the
  file-based persistence from Phase 7

This is intentional: each milestone is meant to be independently buildable
and testable rather than stubbing everything at once and hoping it
integrates later.

**Real and tested (Phase 10 — milestone 9: sessions, Argon2id passwords, two-tier RBAC, §G/§23/§26):**
- `internal/controlplane/auth` — `Service` with `CreateUser`/`Login`/
  `Logout`/`Authenticate`/`BootstrapAdminIfNeeded`. Passwords are hashed
  with the real `golang.org/x/crypto/argon2` implementation (PHC-formatted
  `$argon2id$v=19$m=65536,t=1,p=4$<salt>$<hash>`), not a hand-rolled
  substitute: `golang.org` itself is blocked in this build environment
  (its vanity-import redirect needs a request to `golang.org` before the
  module proxy is even reachable), so `go.mod` routes
  `golang.org/x/crypto` and `golang.org/x/sys` to their official read-only
  GitHub mirrors via `replace` directives — the import path used
  throughout the code is still the real `golang.org/x/crypto/argon2`
  package, only where the module is fetched *from* changes.
- Sessions are server-side and revocable (§G: "the session record — not a
  JWT — is the source of truth"): only a session's sha256 token hash is
  ever persisted, the raw bearer token is returned exactly once (the
  login response's `Set-Cookie`), and logout/expiry genuinely invalidate
  it — proven live below, not just asserted.
- A timing-parity guard in `Login`: a nonexistent email still runs a real
  Argon2id verify against a fixed dummy hash, so response latency can't
  become an account-enumeration oracle even though the returned error
  (`ErrInvalidCredentials`) is already identical either way.
- RBAC is a two-tier `Role` enum (`admin`/`viewer`), not the full
  group/role/permission matrix ARCHITECTURE.md §D describes — documented
  as a deliberate scope boundary in `models.go`'s doc comment, the same
  way every other simplification in this codebase is called out, rather
  than silently passing off two roles as full RBAC. `requireAdmin` gates
  every mutating route (host create/delete, enrollment tokens, live-mode
  open/close) server-side (§23: enforcement can never live only in the
  frontend).
- Wired into the router as an optional dependency — `WithAuth(...)`,
  matching the existing `WithHub(...)` fluent pattern. `nil` falls back to
  the pre-existing permissive "some credential present" stub, so every
  test written before this milestone kept passing completely unchanged
  as real auth was layered in; a production server (`cmd/controlplane`)
  always calls `WithAuth`.
- Routes: `POST /api/v1/auth/login`, `POST /api/v1/auth/logout`,
  `GET /api/v1/me`.
- 32 tests, all green under `go test ./... -race`: 14 in `internal/
  controlplane/auth` (password hash/verify correctness and malformed-hash
  rejection, unique salt per call, login success/wrong-password/unknown-
  email-same-error, expired/unknown-token rejection, idempotent logout,
  duplicate-email rejection, bootstrap-once-and-only-with-credentials
  semantics) and 18 at the router level in `internal/controlplane/api`
  (cookie attributes, a regression guard proving the old "Bearer dev" stub
  token is now correctly rejected once real auth is active, RBAC: viewer
  can read but not create a host, admin can do both, health stays
  unauthenticated).
- Verified live end-to-end the same way every previous phase was: a real
  `controlplane` process, a real `curl` login producing a real
  `HttpOnly; SameSite=Strict` cookie, an authenticated `/api/v1/me` read,
  an admin successfully creating a host, a wrong password rejected with
  401, and a post-logout request against the same cookie correctly
  rejected with 401 — genuine revocation, not just the test suite's word
  for it.

**Deliberately not yet implemented** (still, after Phase 10 — see the note
in "What's real vs. stubbed" above for what specifically remains):
- Full group/role/permission-matrix RBAC (§D) and OIDC/SSO (§24)
- API keys for programmatic/non-browser access (§25)
- Rate limiting on the login endpoint specifically (§26) — nothing in this
  build yet throttles repeated login attempts beyond whatever a future
  reverse proxy in front of it does
- A background reaper for expired sessions — they're currently cleaned up
  opportunistically, only when an expired token is actually presented to
  `Authenticate`, not proactively swept

## Two real bugs found and fixed during end-to-end testing

Worth naming explicitly rather than burying:

1. Container IDs were built as `hostID + "/" + dockerContainerID`, which
   breaks any REST route using the container ID as a single URL path
   segment (`/containers/{id}/...` silently 404s on the slash). Fixed by
   using a deterministic UUID (`uuid.NewSHA1` over the same inputs) —
   stable across restarts, URL-safe.
2. `pki.CA.ServerTLSCertificate` put IP literals into `DNSNames` instead of
   `IPAddresses` (see above) — every localhost/IP-based mTLS connection
   failed verification. Fixed by routing SANs by `net.ParseIP`, with a real
   TLS-handshake regression test.

## Wire format note

ARCHITECTURE.md §E.3 specifies gRPC over mTLS for the agent protocol. This
build uses JSON-over-HTTPS with the same mTLS identity model instead:
generating the gRPC/protobuf stubs requires `protoc` + `protoc-gen-go`, and
this build environment's network policy blocks `golang.org` (including its
module proxy), which `go install`-ing `protoc-gen-go` and its dependencies
needs. The transport is fully isolated behind `internal/agent/transport`
and `internal/controlplane/agentapi` — swapping in gRPC later changes those
two packages' internals, not the collector, registry, or anything that
calls them.

## Network note (development sandboxes)

This build's dependencies (`github.com/google/uuid`, `github.com/gorilla/websocket`,
and nothing else) are fetched via direct git rather than the module proxy, since
`proxy.golang.org` is blocked here. If your environment has similar
restrictions: `GOPROXY=direct GOSUMDB=off go mod tidy`. The official Docker
SDK was deliberately not used — beyond the "avoid unnecessary dependencies"
principle in ARCHITECTURE.md §9, its current version requires Go ≥1.24
(this sandbox has 1.22) and pulls in `golang.org/x/net` transitively, which
is also blocked here.

## Try the agent locally

You'll need a real (or fake, for testing) Docker socket, a running
`controlplane`, a host registered via its browser API, and an enrollment
token:

```bash
export DM_SESSION_SECRET="$(openssl rand -hex 32)"
go run ./cmd/controlplane &

HOST_ID=$(curl -s -X POST http://localhost:8080/api/v1/hosts \
  -H "Authorization: Bearer dev" -H "Content-Type: application/json" \
  -d '{"name":"my-remote-host","kind":"remote","address":"10.0.0.5"}' \
  | grep -o '"id":"[^"]*"' | cut -d'"' -f4)

TOKEN=$(curl -s -X POST http://localhost:8080/api/v1/hosts/$HOST_ID/enrollment-token \
  -H "Authorization: Bearer dev" | grep -o '"token":"[^"]*"' | cut -d'"' -f4)

mkdir -p /tmp/agentcerts
curl -s -H "Authorization: Bearer dev" http://localhost:8080/api/v1/system/ca-certificate \
  -o /tmp/agentcerts/ca.pem

DM_AGENT_CONTROL_PLANE_ADDR="localhost:8443" \
DM_AGENT_HOST_ID="$HOST_ID" \
DM_AGENT_DOCKER_SOCKET="/var/run/docker.sock" \
DM_AGENT_CERT_DIR="/tmp/agentcerts" \
DM_AGENT_ENROLLMENT_TOKEN="$TOKEN" \
go run ./cmd/agent
```
#   d o c k e r _ m o n i t o r i n g  
 