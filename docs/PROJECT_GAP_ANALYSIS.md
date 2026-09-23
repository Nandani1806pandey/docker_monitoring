# Project Gap Analysis

Generated from a direct inspection of the repository as it exists right now
(43 Go files, ~7,100 lines, `go build ./...` / `go vet ./...` / `go test
./... -race` all currently green). This is a Go backend only: **there is
no frontend of any kind in this repository yet** — no `package.json`, no
`.tsx`/`.jsx`, no `web/` or `frontend/` directory. Every "Phase 5/6/9/20"
item in the master prompt (React dashboard, drag-and-drop, log viewer UI,
frontend security) is 0% started, not partially started, and is called out
that way below rather than folded into "partial."

This document is a snapshot, not a promise. Every "Status" below reflects
what the code and tests actually do today, verified by reading the file,
not by the presence of a plausible-looking function name.

---

## A. Implemented

| Requirement | File(s) | Function/Component | Test coverage | Status |
|---|---|---|---|---|
| Config loading from env | `internal/shared/config/config.go` | `Load()` | none dedicated (exercised indirectly via `cmd/controlplane/main_test.go`) | **Done** for the fields that exist |
| In-memory storage | `internal/controlplane/storage/memory/memory.go` | `Store` | exercised via every package's tests that use `memory.New()` | **Done**, explicitly zero-persistence by design |
| Storage interface abstraction | `internal/controlplane/storage/store.go` | `HostStore`/`ContainerStore`/`MetricStore`/`EventStore`/`UserStore`/`SessionStore` | via memory backend tests | **Done** — genuinely backend-agnostic; no handler imports `memory` directly except tests and `main.go`'s wiring |
| Host REST API | `internal/controlplane/api/hosts.go` | create/get/list/delete, enrollment token issuance | `live_test.go` | **Done** for CRUD; no update/PATCH endpoint |
| Container REST API | `internal/controlplane/api/containers.go` | get container, list by host | `live_test.go` | **Done** for read-only; no start/stop/restart/remove (Phase 7 territory) |
| Event log API | `internal/controlplane/api/events.go` | list with filters (host/container/type/severity/from/to) | `live_test.go` | **Done**, append-only, in-memory only |
| Docker Engine API client (agent) | `internal/agent/docker/*.go` | container discovery, stats, CPU%/mem algorithm matching `docker stats` | `docker_test.go` against a fake daemon on a real Unix socket | **Done** and genuinely tested, not mocked-and-assumed |
| Host-level metrics (agent) | `internal/agent/hoststats/hoststats.go` | CPU/mem/net from `/proc` | `hoststats_test.go`, including a live sanity check against this machine's real `/proc` | **Done**, Linux-only by design |
| Internal CA / mTLS issuance | `internal/controlplane/pki/ca.go` | `GenerateCA`, agent cert issuance, server cert issuance | `ca_test.go` | **Done** for issuance; **not done** for rotation/revocation (Phase 16) |
| CA persistence across restarts | `cmd/controlplane/main.go` (`loadOrGenerateCA`) | PEM files under `DM_CA_DIR` | exercised in `main_test.go` | **Done**, but plaintext-file key storage is explicitly flagged as not a real secrets store (Phase 17 gap) |
| Agent enrollment (token → cert) | `internal/controlplane/agentregistry/registry.go`, `internal/controlplane/agentapi/csr.go` | single-use, 15-minute TTL tokens; CSR signing | `registry_test.go`, `internal/integrationtest/agent_protocol_test.go` | **Done** and verified with a real mTLS handshake in the integration test |
| Heartbeat / telemetry ingestion | `agentregistry.Registry.IngestTelemetry` | — | `registry_test.go`, integration test | **Done** |
| Adaptive monitoring FSM | `internal/controlplane/monitoring/engine.go` | NORMAL/CRITICAL/MANUAL_REALTIME states, hysteresis window | `engine_test.go` | **Done** for the state machine itself — see Section B for what's incomplete around it |
| WebSocket hub | `internal/controlplane/ws/hub.go`, `internal/controlplane/api/ws.go` | topic-based pub/sub, `/ws/events`, `/ws/hosts/{id}/stats`, `/ws/containers/{id}/stats` | `hub_test.go`, live-verified in README's Phase 8/9 notes | **Done** |
| Session-based authentication | `internal/controlplane/auth/service.go`, `password.go` | `Login`/`Logout`/`Authenticate`/`BootstrapAdminIfNeeded` | `service_test.go`, `password_test.go` (14 tests) | **Done** for local password auth |
| Argon2id password hashing | `internal/controlplane/auth/password.go` | real `golang.org/x/crypto/argon2`, PHC-format storage | `password_test.go` | **Done** |
| Two-tier RBAC (admin/viewer) | `internal/controlplane/api/router.go` (`requireAdmin`) | route-level gating | `auth_test.go` | **Done**, but only two roles — see gap vs. master prompt's Phase 12 permission list |
| Session cookie hygiene | `internal/controlplane/api/auth.go` | `HttpOnly`, `SameSite=Strict`, `Secure` (configurable) | `auth_test.go` | **Done** |
| Manual live-inspection open/close | `internal/controlplane/api/live.go` | `POST`/`DELETE /api/v1/hosts/{id}/live`, same for containers | `live_test.go` | **Done** server-side; agent-side push still bounded by next telemetry cycle (see Section B) |
| Integration tests (agent↔control-plane, real mTLS) | `internal/integrationtest/*.go` | full enrollment → heartbeat → telemetry round trip | self | **Done** |
| Request logging middleware | `internal/controlplane/api/router.go` (`withLogging`) | structured `slog` per-request | — | **Done**, minimal (method/path/duration only — no request ID) |

## B. Partially Implemented

### 1. Adaptive monitoring — directive delivery latency
- **What exists:** The FSM computes the correct next state/interval and returns it in the telemetry response (`agentregistry.IngestTelemetry` → response payload the agent reads on its next poll).
- **What's missing:** There is no out-of-band push channel from control plane to agent. `transport.go` is a request/response protocol driven by the agent; the control plane cannot proactively tell an agent "go to MANUAL_REALTIME now" — the agent only finds out on its *next* telemetry submission. The master prompt's Phase 4 requirement ("must not need to wait unnecessarily for the next telemetry cycle") is **not met**.
- **What must change:** Either (a) a persistent connection from agent→control-plane (the existing mTLS transport could be upgraded to a long-lived stream/SSE-like model), or (b) the agent's telemetry interval must shrink enough during NORMAL that the practical latency is acceptable, which contradicts the point of NORMAL being infrequent.
- **Dependencies:** `internal/agent/transport`, `internal/agent/collector`, `agentregistry.Registry`.
- **Risk of modifying:** Medium — the existing protocol is tested end-to-end in `internal/integrationtest`; a new push channel must not break the existing pull-based contract, since real agents are assumed to be already enrolled against it.

### 2. Per-container (rather than host-level) monitoring mode
- **What exists:** `models.Container.MonitoringMode` exists as a field.
- **What's missing:** `monitoring.Engine` evaluates and returns a mode at the *host* granularity; nothing currently sets a container's mode independently of its host's. The master prompt's Phase 4 ask for "independent entity state" is **not met** for containers specifically (it is met for hosts).
- **Risk:** Low-medium — additive; the existing host-level FSM doesn't need to change, a parallel per-container evaluation path can be added.

### 3. Certificate lifecycle
- **What exists:** Issuance (initial enrollment) and persistence across control-plane restarts.
- **What's missing:** Expiration monitoring, proactive rotation, revocation (no CRL/OCSP equivalent — a compromised agent cert cannot currently be invalidated before its 90-day expiry), re-enrollment flow for an agent whose cert is about to expire.
- **Risk of modifying:** Medium-high — touches the mTLS trust boundary directly; must not invalidate already-enrolled agents (explicitly called out in the master prompt).

### 4. Storage abstraction vs. actual persistence
- **What exists:** A clean `storage.Store` interface that nothing outside `storage/memory` and test code depends on directly — genuinely ready for a second backend to be dropped in.
- **What's missing:** There is no second backend. `config.Load()` *accepts* `DM_STORAGE_DRIVER=postgres` and *validates* that `DM_DATABASE_URL` is set in that case, but `cmd/controlplane/main.go` then unconditionally exits with "unsupported storage driver" for anything other than `memory`. This is a real inconsistency (see Section D) — the config layer promises something the runtime doesn't deliver.
- **Risk:** Low to add a new backend (the interface is the whole point), but every method needs real transaction/error-handling semantics that the in-memory version doesn't need to think about (e.g. `ErrNotFound` mapping, connection failures, retries).

### 5. Historical metrics
- **What exists:** `config.Config.HistoricalMetricsEnabled`/`HistoricalRetentionDays` fields, and a `storage.MetricStore` interface whose doc comment explicitly says "historical/downsampled persistence... is a separate interface layered on top of this one in a later milestone — not implemented here." `LatestMetric` only ever holds the single most recent sample per entity.
- **What's missing:** Everything else — actual storage of a metric time series, retention/downsampling, and the required time-range queries (5m/15m/1h/6h/24h/7d/custom).
- **Risk:** Low to add given the existing interface seam, but real design work: this is the single feature most likely to blow up storage size and query cost if done naively (the master prompt itself warns "do not store unlimited high-frequency data").

## C. Missing (0% implemented, not partial)

Grouped by master-prompt phase for direct traceability:

- **Phase 2 — Real persistence:** No Postgres/SQLite driver, no migrations, no models for `alerts`, `alert_rules`, `notification_providers`, `groups`, `permissions` (beyond the two hardcoded roles), `audit_logs`, `api_keys`. `models.go` only defines Host/Container/Metric/Event/User/Session.
- **Phase 3 — Historical metrics:** See B.5 above; the query/aggregation/downsampling layer is fully missing.
- **Phase 5/6 — React+TypeScript dashboard, customization:** No frontend exists at all in this repository.
- **Phase 7 — Container management actions:** No start/stop/restart/pause/unpause/remove/logs endpoints. Only read (get/list) exists.
- **Phase 8 — Docker image/compose management:** Not present anywhere in the agent or control plane.
- **Phase 9 — Log viewer (backend or frontend):** No log-streaming endpoint exists; the agent's Docker client (`internal/agent/docker`) doesn't expose a logs API at all.
- **Phase 10 — Alerting engine:** No alert rule model, no evaluator, no severity/cooldown/dedup/blackout logic anywhere.
- **Phase 11 — Notification providers:** No provider interface, no Discord/Slack/Telegram/Pushover/Gotify/SMTP integration.
- **Phase 12 — Full RBAC (groups/permissions):** Only `RoleAdmin`/`RoleViewer` exist; no `Group`, `Permission`, or per-permission checks (`hosts.read`, `containers.start`, etc.) anywhere in `models.go` or `auth/`.
- **Phase 13 — OIDC/SSO:** No `auth` abstraction beyond the local password `Service`; no OIDC config fields, no discovery/token-exchange code.
- **Phase 14 — API keys:** No `APIKey` model, no issuance/hash/revoke/expiry endpoints. (A prior design iteration of this codebase explored this — see `BRAIN.md` for why it was reverted — but nothing from that iteration is in the current tree.)
- **Phase 15 — Rate limiting:** No rate limiter exists anywhere in the current tree (a prior iteration had a token-bucket limiter; it was removed along with the API-key/OIDC design it supported — see `BRAIN.md`).
- **Phase 16 — Certificate rotation/revocation:** See B.3.
- **Phase 17 — Secrets management abstraction:** Secrets today are plain environment variables and plaintext PEM files on disk; no secrets-backend interface exists.
- **Phase 18 — Audit logging:** The `Event` model/`EventStore` is an operational/monitoring log (host down, container unhealthy, etc.), not a security audit log. There is no record of "who logged in," "who changed what role," etc.
- **Phase 19 — API documentation / OpenAPI:** No `docs/API.md`, no OpenAPI spec.
- **Phase 20 — Frontend security:** N/A until a frontend exists.
- **Phase 21 — Full test pyramid:** Unit and integration tests exist and are strong for what they cover (see Section A). E2E, dedicated security tests (beyond the auth package's own tests), and load/failure-injection tests do not exist.
- **Phase 22 — Load/scale testing:** Not performed. No `docs/PERFORMANCE.md`.
- **Phase 23 — Production deployment configs:** No `Dockerfile`, no `docker-compose.yml`, no reverse-proxy config, no backup/restore scripts.
- **Phase 24 — Observability of the platform itself:** `GET /api/v1/system/health` exists and returns a static `{"status":"ok"}` (see `health.go` — 16 lines, no dependency checks). No `/readyz`, no Prometheus-style metrics endpoint, no active-agent/active-websocket counters exposed.
- **Phase 25 — Full docs tree:** Only `README.md` and `ARCHITECTURE.md` (referenced throughout the code but not present in this zip — see Section D) exist. None of `API.md`, `DEPLOYMENT.md`, `SECURITY.md`, `DATABASE.md`, `MONITORING.md`, `ALERTING.md`, `TESTING.md`, `PERFORMANCE.md`, `TROUBLESHOOTING.md`, `CHANGELOG.md` exist yet.

## D. Broken

- **Storage-driver config/runtime mismatch:** `config.Load()` validates `DM_DATABASE_URL` when `DM_STORAGE_DRIVER=postgres`, implying that's a supported path, but `main.go` immediately `os.Exit(1)`s for any driver other than `memory`. This isn't a crash, but it's a real inconsistency between what the config layer implies is possible and what actually runs — worth fixing (either by removing the postgres validation branch until a real driver exists, or by adding the driver) before anyone reads `config.go` and assumes Postgres works today.
- **`ARCHITECTURE.md` is referenced but not present:** Nearly every doc comment in this codebase cites `ARCHITECTURE.md §<number>` as the source of truth (e.g. `§26`, `§45`, `§G`). That file does not exist anywhere in the uploaded/current tree. This isn't breaking compilation, but it means a large fraction of the codebase's own justification is unverifiable by inspection — worth flagging explicitly rather than silently trusting every `§` citation.
- **No compile errors, no vet failures, no test failures** as of this inspection (`go build ./...`, `go vet ./...`, `go test ./... -race` all clean — verified, not assumed).
- **No race conditions detected** by `-race` across the current suite, but coverage is real-but-partial (see Section C/Phase 21) — absence of a detected race is not proof of absence given untested areas (WebSocket hub under heavy concurrent fan-out, for instance, is tested but not load-tested).
- **`GET /api/v1/system/health` doesn't check anything:** it returns a hardcoded OK regardless of storage/agent-listener state, which will misrepresent actual health once persistence (Phase 2) exists and could be down.

## E. Technical Debt

- **In-memory-only storage** is the single largest piece of debt blocking Phase 2 onward — by design and clearly documented, not accidental, but still the load-bearing gap.
- **Two hardcoded roles** (`RoleAdmin`, `RoleViewer`) instead of a permission matrix — clearly documented as a deliberate scope boundary in `models.go`, not hidden, but it is debt relative to the master prompt's Phase 12 ask.
- **No database migrations directory** — there's nothing to migrate yet, but this should exist as soon as Phase 2 starts, with a real migration tool (not ad hoc SQL scripts) from the first commit.
- **CA key stored as a plaintext PEM file** (`loadOrGenerateCA` in `main.go`) — explicitly flagged in that function's own doc comment as "a documented simplification, not the eventual answer."
- **No request-ID propagation** in `withLogging` — every log line has method/path/duration but nothing to correlate a single request across multiple log lines or components.
- **`internal/controlplane/events` is 71 lines** — a minimal `Recorder` wrapping `EventStore.AppendEvent` plus an optional broadcaster. Fine for what it does today, but it will need to grow once alerting (Phase 10) needs to consume events as an evaluation input rather than just a write-through log.
- **No configuration for CPU/memory percentage thresholds** — `thresholdsFromConfig` in `main.go` explicitly notes that `config.Config` doesn't expose these yet, so they're stuck at `monitoring.DefaultThresholds()` regardless of environment.
- **No `.env.example` or documented full environment-variable reference** — variables are discoverable only by reading `config.go` directly.
- **Two previously-explored auth designs were discarded mid-session** (a full Groups/Roles/Permissions/OIDC/API-keys/rate-limiting design, replaced by the current, simpler, Argon2id + two-role design — see `BRAIN.md` for the full history). Nothing from the discarded design remains in the tree, but it's worth knowing this was a deliberate pivot, not an oversight, when Phase 12–15 work picks the RBAC/OIDC/API-key work back up — the discarded design is a reasonable starting sketch to revisit, not a dead end to avoid.
