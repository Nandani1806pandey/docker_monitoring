# Project Completion Roadmap

This roadmap sequences the 25 phases from the master completion prompt
against the actual current state documented in `PROJECT_GAP_ANALYSIS.md`.
It reorders strictly by **dependency**, not by the master prompt's numeric
order, because several later-numbered phases are prerequisites for
earlier-numbered ones as written (e.g. alerting/Phase 10 cannot persist
alert state without Phase 2's database; the frontend/Phase 5 cannot show
historical charts without Phase 3).

Each phase below lists: what it depends on, what it delivers, its exit
criteria (how we know it's actually done, not just coded), and its risk
level against existing working code.

Per the master prompt's own execution rule, phases are implemented **one
at a time**, each fully tested and documented before the next begins. This
document is the plan; `TEST.md` is the running checklist; `CHANGELOG.md`
(to be created at Phase 2) records what actually shipped per phase.

---

## Ordering rationale

```
Phase A (persistence)
   → Phase B (historical metrics)      [needs A's DB]
   → Phase C (full RBAC + audit log)   [needs A's DB for groups/roles/permissions/audit_logs tables]
   → Phase D (API keys + rate limiting)[needs C's permission model to scope keys]
   → Phase E (alerting)                [needs A for alert_rules storage, B for threshold evaluation against real history]
   → Phase F (notifications)           [needs E — nothing to notify about without alerts]
   → Phase G (container/Docker mgmt)   [independent of A-F; can run in parallel with them]
   → Phase H (log viewer backend)      [independent, can run in parallel]
   → Phase I (cert lifecycle)          [independent, can run in parallel]
   → Phase J (OIDC/SSO)                [needs C's auth abstraction to slot behind]
   → Phase K (frontend)                [needs A, B, most of C/E/F/G/H to have something real to render]
   → Phase L (frontend customization)  [needs K]
   → Phase M (secrets mgmt abstraction)[independent, ideally before J/D so OIDC secrets and API keys land in it, not around it]
   → Phase N (observability)           [best done alongside A, since DB health becomes part of it]
   → Phase O (deployment configs)      [needs a real DB driver from A to be meaningful]
   → Phase P (testing: security/E2E/failure)  [continuous, but a real pass needs K done for E2E]
   → Phase Q (load/scale testing)      [last — needs the real system, not stubs, to mean anything]
   → Phase R (docs finalization)       [continuous, closed out last]
```

Mapping back to the master prompt's phase numbers: A=Phase 2, B=Phase 3,
C=Phase 12(+18), D=Phase 14+15, E=Phase 10, F=Phase 11, G=Phase 7+8,
H=Phase 9, I=Phase 16, J=Phase 13, K=Phase 5, L=Phase 6, M=Phase 17,
N=Phase 24, O=Phase 23, P=Phase 21 (minus unit tests, which happen inside
every phase), Q=Phase 22, R=Phase 19+25.

Phase 1 (Core Architecture verification) from the master prompt is not a
separate phase here — it's what `PROJECT_GAP_ANALYSIS.md` Section A/D
already did. The one concrete Phase-1 fix identified (the storage-driver
config/runtime mismatch) is folded into Phase A below since it's the same
code path.

---

## Phase A — Real Persistence *(master Phase 2)*

**Depends on:** nothing (first phase).
**Blocks:** B, C, D, E, N (partially), O.

**Scope:**
- Add a Postgres driver implementing the existing `storage.Store`
  interface (`internal/controlplane/storage/postgres`), using
  `database/sql` + a migration tool (e.g. `golang-migrate` — chosen at
  implementation time after confirming it's reachable in this
  environment's network allowlist; if not, a minimal embedded migration
  runner over plain `.sql` files is the fallback, documented as such).
- SQLite as the lightweight option, same interface, for self-hosted
  single-node deployments that don't want a separate Postgres container.
- Migrations for every table the interface (and the phases that will
  build on it) need: `users`, `sessions`, `hosts`, `containers`,
  `container_states` (if state history is tracked separately from
  current state — decide during implementation whether this is a
  separate table or derivable from the metrics/events tables), `events`,
  `alerts`, `alert_rules`, `notification_providers`, `api_keys`,
  `groups`, `roles`, `permissions`, `role_permissions`, `group_roles`,
  `user_groups`, `audit_logs`, `metric_metadata`, `monitoring_config`.
  (Alert/notification/RBAC/API-key/audit tables are created here even
  though those *features* land in later phases, so Phase A is the one
  and only place the schema gets designed holistically instead of
  bolted on table-by-table.)
- Foreign keys with `ON DELETE SET NULL` for audit-relevant references
  (matching the existing in-memory `Event.HostID`/`ContainerID` pattern —
  audit/event data must survive the deletion of what it references) and
  `ON DELETE CASCADE` only where genuinely appropriate (e.g.
  `role_permissions` rows when a role is deleted).
- Indexes on every foreign key and every column used in a `WHERE`/`ORDER
  BY` the existing `EventFilter`-style queries need (host_id,
  container_id, created_at, severity, type at minimum).
- Connection pooling (`sql.DB`'s built-in pool, tuned via
  `SetMaxOpenConns`/`SetMaxIdleConns`/`SetConnMaxLifetime`), transaction
  handling for any multi-statement write (e.g. creating a user + default
  group membership atomically), and graceful degradation: the control
  plane must return `503` with a clear error on a DB outage, not panic or
  hang.
- Fix the identified config/runtime mismatch: `main.go`'s driver-check
  branch is updated in the same change that adds the driver, not before —
  until then, the honest thing is to keep the current hard error for
  `postgres`/`sqlite` rather than pretend they work.
- **Do not remove `storage/memory`** — it stays as the zero-dependency
  default and as the fast path for unit tests; only `main.go`'s allowed
  driver list grows.

**Exit criteria:**
- Every existing test in the repo still passes unmodified against the
  `memory` backend (regression guard).
- A new, equivalent test suite runs the *same* `storage.Store` contract
  tests against Postgres and SQLite (a shared contract-test helper that
  takes a `storage.Store` and exercises it — written once, run three
  times, once per backend — is the right structure here, not three
  independent copies of the same tests).
- A real `docker-compose up` with a real Postgres container, control
  plane pointed at it via `DM_DATABASE_URL`, survives a control-plane
  restart with data intact (hosts/users persist) — verified live, not
  assumed from the migration file existing.
- `docs/DATABASE.md` written, documenting the schema, migration process,
  and backup/restore basics (full backup/restore procedure detail is
  Phase O's job; this doc covers the schema itself).

**Risk to existing code:** Low-medium. The interface boundary
(`storage.Store`) is already clean — no handler code should need to
change. The main risk is `main.go`'s wiring and `config.go`'s validation,
both small, well-isolated files.

---

## Phase B — Historical Metrics *(master Phase 3)*

**Depends on:** A.
**Blocks:** K (frontend charts need real data to show).

**Scope:**
- A new `HistoricalMetricStore` interface (additive, alongside the
  existing latest-only `MetricStore` — not a replacement, since
  live/latest reads have different performance needs than range queries).
- Configurable sampling write-through: every metric ingested via
  `RecordMetric` optionally also appends to history, gated by
  `DM_HISTORICAL_METRICS_ENABLED` (already a config field, currently
  unused — this phase is what finally reads it).
- Downsampling: raw samples at ingestion resolution for a short window
  (e.g. last hour), rolled up to coarser resolution (1-minute, then
  5-minute averages) for older data — the concrete rollup schedule is an
  implementation decision to make explicit in `docs/MONITORING.md`, not
  left implicit in code.
- Retention: a background job (or a query-time filter, decided during
  implementation) enforcing `DM_HISTORICAL_RETENTION_DAYS`.
- Range queries backing exactly the ranges the master prompt lists (5m,
  15m, 1h, 6h, 24h, 7d, custom start/end) as one parameterized query path,
  not seven separate handlers.

**Exit criteria:**
- A load-shaped test: ingest metrics continuously for a simulated period
  (using a fake clock, not a real multi-day wait) and confirm downsampling
  and retention behave as configured.
- API endpoints for range queries return correctly-shaped, correctly
  downsampled data verified against hand-computed expected values for a
  known synthetic dataset.
- Storage growth under continuous ingestion is bounded and measured (a
  concrete number in `docs/PERFORMANCE.md`, even before full Phase Q load
  testing) — this directly answers the master prompt's "do not store
  unlimited high-frequency data."

**Risk:** Low-medium. Additive interface; risk is mostly in getting the
downsampling math right, which is why it needs dedicated tests against
known values, not just "it runs."

---

## Phase C — Full RBAC + Audit Logging *(master Phase 12 + 18)*

**Depends on:** A.
**Blocks:** D, J.

**Scope:**
- Extend `models.go`: `Group`, `Permission` (a fixed catalog, e.g.
  `hosts.read`, `hosts.write`, `containers.start`, ... — the master
  prompt's own list is the starting catalog), `Role` becomes a named,
  DB-backed entity with an attached permission set rather than the
  current fixed two-value enum. **This is a breaking change to the
  existing `models.Role`/`User.Role` shape** — handled carefully per the
  Vibe-Coding Safety Rule: read every caller of `models.Role`/`RoleAdmin`/
  `RoleViewer` first (`router.go`'s `requireAdmin`, `auth/service.go`'s
  `CreateUser`, every test in `auth_test.go`/`service_test.go`), migrate
  them one at a time, keep the two-role default behavior as
  auto-provisioned built-in `admin`/`viewer` roles so no operator's
  existing account gets locked out during upgrade.
- `EffectivePermissions(user) []string` resolving group→role→permission,
  replacing `requireAdmin`'s single boolean check with per-route
  `requirePermission(perm)`.
- Audit log: a new `AuditStore` distinct from the existing operational
  `EventStore` (they serve different consumers and retention policies —
  don't overload one table for both). Record exactly the actions the
  master prompt lists (login, logout, failed login, user/role/permission
  changes, host/container lifecycle actions, config changes, API key
  create/revoke), with actor/action/resource/result/timestamp, and an
  explicit guard (a test, not just a code review) that no audit entry
  ever contains a password, token, or secret value.

**Exit criteria:**
- Every existing RBAC test (`TestRBAC_*` in `auth_test.go`) still passes,
  now expressed against the permission-based check instead of the old
  role check, proving the migration preserved behavior.
- New tests: a custom role with a narrow permission set (e.g.
  `containers.read` only, no `hosts.write`) is denied exactly the routes
  it should be and allowed exactly the ones it should be.
- An audit-log completeness test: perform one of every listed
  security-sensitive action against a test server, assert exactly one
  matching audit row was written for each, with no secret material in any
  of them.

**Risk:** Medium-high — this is the one phase that changes an existing,
tested, working data shape (`models.Role`) rather than only adding new
ones. Do it as its own isolated change with the full existing test suite
re-run before and after, not bundled with unrelated work.

---

## Phase D — API Keys + Rate Limiting *(master Phase 14 + 15)*

**Depends on:** C (keys are scoped to a subset of their creator's
permissions, so the permission model must exist first).
**Blocks:** nothing downstream, but pairs naturally with J (OIDC) since
both are "additional auth surface" work.

**Scope:**
- `APIKey` model: hash-only storage (never the raw key past issuance),
  expiration, permission subset, last-used timestamp — this is a smaller,
  scoped version of a design already sketched once during this project's
  history (see `BRAIN.md`) and can reuse those ideas directly rather than
  redesigning from zero.
- A general per-IP token-bucket rate limiter (also previously sketched —
  see `BRAIN.md`) applied first and specifically to `/api/v1/auth/login`
  (tight limit — brute-force protection is the actual goal there), then
  as a lighter backstop across the whole API. Tuned so legitimate agent
  telemetry traffic (which is frequent and expected) is never
  rate-limited — agent traffic goes through the separate mTLS listener,
  not this HTTP API, so this should be naturally non-conflicting, but
  it's an explicit thing to verify, not assume.

**Exit criteria:**
- A revoked or expired key is rejected; a valid key scoped to fewer
  permissions than its creator currently has is rejected for anything
  outside that scope, and re-checked against the creator's *current*
  permissions (not just permissions at issuance) so a demoted user's
  already-issued keys don't outlive the demotion.
- A brute-force simulation (many rapid failed logins from one source)
  gets rate-limited with the correct HTTP status (`429`) and
  `Retry-After`; a normal login rate is never affected.

**Risk:** Low — purely additive surface area on top of C's permission
model.

---

## Phase E — Alerting Engine *(master Phase 10)*

**Depends on:** A (rule/state storage), B (threshold evaluation needs
real historical context for "sustained for duration X", not just an
instantaneous reading).
**Blocks:** F.

**Scope:**
- `AlertRule` model and evaluator: threshold conditions (CPU/mem/network),
  state conditions (container stopped/unhealthy/restarting, host/agent
  offline, Docker unavailable, excessive restart count), each with
  duration (must persist for N seconds before firing, not fire on a
  single noisy sample), cooldown, grouping/deduplication (one alert per
  condition per entity, not one per sample), recovery notification, an
  enabled/disabled flag, and blackout windows.
- The evaluator runs against the adaptive monitoring engine's existing
  telemetry ingestion path (`agentregistry.IngestTelemetry` is the
  natural hook point) rather than polling metrics separately — reuse the
  existing data flow, don't build a parallel one.

**Exit criteria:**
- Threshold + duration test: a metric crossing a threshold for less than
  the configured duration does not fire; crossing it for longer does;
  recovering below threshold for the recovery condition fires a recovery
  notification exactly once.
- Deduplication test: a sustained breach produces one alert, not one per
  sample, until it resolves.
- Blackout window test: a condition that would otherwise fire during a
  configured blackout window does not.

**Risk:** Medium — needs care to avoid double-counting with the existing
`Event` log (an alert firing is itself worth an event, but the alert
engine's own state — active/resolved/cooldown — is not the same thing as
the event log and shouldn't be modeled as one).

---

## Phase F — Notification Providers *(master Phase 11)*

**Depends on:** E.

**Scope:**
- A `NotificationProvider` interface (`Send(ctx, alert) error`) so the
  alert engine never imports a specific provider package directly.
- Discord, Slack, Telegram, Pushover, Gotify, SMTP implementations behind
  that interface, each with its own config (webhook URL / bot token /
  SMTP host+creds, etc.), a "send test notification" endpoint, templated
  message bodies, retry-with-backoff, and timeout handling.
- Provider health tracking (last successful send, last error) surfaced
  via an API, not just logged.

**Exit criteria:**
- Each provider has a unit test against a fake HTTP endpoint (a local
  `httptest.Server` standing in for Discord/Slack/etc.'s real webhook) —
  no test depends on reaching a real external service, since this
  environment's network egress is restricted and real provider
  credentials won't exist in CI anyway.
- A provider failure (fake endpoint returns 500, or times out) is
  retried per policy and eventually marked unhealthy without crashing the
  alert engine or blocking other providers.
- Secrets (webhook URLs, bot tokens, SMTP passwords) never appear in logs
  — an explicit test asserting this, following the same pattern as
  Phase C's audit-log secret-leak guard.

**Risk:** Low — new, isolated package; the interface boundary keeps this
from touching the alert engine's core logic once the interface is fixed.

---

## Phase G — Container Actions + Docker/Compose Management *(master Phase 7 + 8)*

**Depends on:** nothing new (uses the existing agent↔control-plane
transport and `internal/agent/docker` client); can run in parallel with
A–F.

**Scope:**
- Container lifecycle actions (start/stop/restart/pause/unpause/remove)
  and bulk variants, routed control-plane → agent → Docker Engine API,
  reusing `internal/agent/docker`'s existing client rather than building
  a second one.
- Server-side authorization on every action (`containers.start`,
  `containers.stop`, etc. — permissions defined in Phase C; if G lands
  before C in practice, it temporarily gates on `RoleAdmin` and is
  migrated to fine-grained permissions once C exists, the same way
  `requireAdmin` is handled today).
- Image list/inspect/pull/remove and Compose stack list/start/stop/restart
  and status, again via the agent's Docker client.
- Confirmation is a frontend concern (Phase K) but the backend still
  requires an explicit, unambiguous request body for destructive actions
  (no accidental bulk-remove-all via a missing filter).

**Exit criteria:**
- Each action verified against a real Docker daemon in this sandbox (the
  same "fake daemon on a real Unix socket" pattern `docker_test.go`
  already uses, extended to cover write operations, not just reads).
- Authorization test: a viewer-equivalent role is denied every mutating
  action; an admin-equivalent role can perform all of them.

**Risk:** Low-medium. New handlers and new agent-side Docker client
methods, but built on an already-tested client and transport.

---

## Phase H — Log Viewer (backend) *(master Phase 9, backend half)*

**Depends on:** nothing new; can run in parallel with A–G.

**Scope:**
- A streaming logs endpoint (container logs via the agent's Docker
  client, follow mode, since-timestamp, tail-N) with an explicit,
  enforced line/byte cap so a runaway container's logs can't be pulled
  unbounded into either the control plane or the browser.
- Search/filter is likely more practical as a frontend-side operation over
  a bounded, paginated backend stream — decided explicitly during
  implementation and documented, not left ambiguous.

**Exit criteria:**
- A container producing logs faster than the configured cap is correctly
  truncated/paginated, not OOMing the control plane or hanging the
  request.

**Risk:** Low.

---

## Phase I — Certificate Lifecycle *(master Phase 16)*

**Depends on:** nothing new; can run in parallel with A–H.

**Scope:**
- Expiration monitoring (a scheduled check against every enrolled agent's
  cert `NotAfter`), proactive rotation/renewal before expiry, and a real
  revocation mechanism (likely a short revocation list the agent-facing
  mTLS listener checks per-connection, since this environment's agent
  count is expected to be small enough that a CRL-style approach is
  practical without needing OCSP infrastructure).
- Re-enrollment flow for a cert nearing expiry, reusing the existing
  enrollment-token mechanism rather than inventing a second one.

**Exit criteria:**
- A live test: an agent's cert artificially set to expire soon triggers
  rotation, receives a new cert, and its mTLS connection continues
  working with zero manual intervention — verified against a real running
  agent+control-plane pair, the same way Phase 7/9 of the existing
  README were verified live.
- A revoked cert is rejected by the mTLS listener on its very next
  connection attempt.

**Risk:** Medium-high — same trust-boundary caution as any PKI work;
existing enrolled agents (in any live deployment) must not be silently
invalidated by a rotation-mechanism bug.

---

## Phase J — OIDC / SSO *(master Phase 13)*

**Depends on:** C (needs a permission/group model to map OIDC group
claims onto).

**Scope:**
- An `Authenticator` interface behind which local password auth (already
  built) and OIDC both sit, so `auth.Service` stops being the only
  authentication path.
- Standard-library-only OIDC discovery/token-exchange/JWKS verification —
  this exact implementation was already sketched once during this
  project's history (see `BRAIN.md` for why `golang.org/x/oauth2` isn't
  used: `golang.org` itself is blocked in this build environment, the
  same constraint documented in the current `password.go`, worked around
  there via a GitHub-mirror `replace` directive; OIDC's dependency
  surface is different enough — RS256 JWT verification, JWKS — that a
  from-scratch stdlib implementation was judged simpler than chasing
  mirrors for a whole OAuth2 library, but that decision should be
  revisited at implementation time against whatever's reachable then).

**Exit criteria:**
- A real OIDC login round-trip against a local, mock discovery-compliant
  IdP in a test (not a live third-party IdP, for the same
  network-restriction reasons Phase F's providers are tested against
  fakes).
- Group-claim sync test: a user's OIDC groups map onto existing internal
  groups (and *only* existing ones — an unprovisioned claimed group must
  not silently grant access, matching the caution already documented in
  this project's discarded-but-instructive earlier OIDC sketch).

**Risk:** Medium.

---

## Phase K — React + TypeScript Dashboard *(master Phase 5)*

**Depends on:** A, B, and enough of C/E/F/G/H to have real data and real
actions to render — implemented incrementally screen-by-screen against
whatever backend phases have landed, not blocked entirely until every
other phase is 100% done.

**Scope:** exactly the widget/screen list in the master prompt (global
dashboard, host dashboard, container dashboard, container details with
live+historical charts+logs+events+restart history). Standard modern
React+TypeScript tooling (framework/bundler choice made at implementation
time), REST for CRUD, the existing WebSocket hub for live updates —
no new backend push mechanism needed here since the hub already exists.

**Exit criteria:** each screen backed by a real running control plane in
a manual verification pass (screenshot or recorded interaction), plus
component-level tests. Full E2E (Phase P) comes after this phase has
something to drive.

**Risk:** Low to existing backend code (frontend is additive), but this
is the single largest phase by volume of new code.

---

## Phase L — Dashboard Customization *(master Phase 6)*

**Depends on:** K.

**Scope:** drag/drop, resize, show/hide, per-user saved layouts (needs a
`user_dashboard_layouts` table — small addition to Phase A's schema, add
it there retroactively via a new migration rather than papering over it
in K).

**Risk:** Low.

---

## Phase M — Secrets Management Abstraction *(master Phase 17)*

**Depends on:** nothing structurally, but most valuable once there are
several kinds of secrets to manage (OIDC client secret, notification
provider credentials, API key hashes are already one-way so not a concern
here) — best scheduled around the same time as J and F so their secrets
land directly in the abstraction rather than needing retrofitting.

**Scope:** a `SecretStore` interface (env-var backend as the zero-dependency
default, matching this project's existing "memory store first" pattern;
a real secrets-backend implementation, e.g. Vault, as a documented
optional driver, not a hard requirement). Explicit `go vet`/lint-style
checks (or at minimum, a documented manual review step) confirming no
secret ever reaches a log line, API response, or committed config example.

**Risk:** Low-medium — mostly about discipline (auditing every place a
secret currently flows) rather than complex new logic.

---

## Phase N — Observability *(master Phase 24)*

**Depends on:** best done alongside A (DB health becomes part of the
health check almost immediately).

**Scope:** `/healthz` (liveness), `/readyz` (readiness — actually checks
DB connectivity, not a hardcoded OK like today's `/api/v1/system/health`),
a metrics endpoint (Prometheus-format, exposing request latency, error
counts, active-agent count, active-WebSocket count, metric-ingestion
rate, alert-processing rate), and structured logs with request-ID
propagation (closing the gap noted in the gap analysis).

**Risk:** Low.

---

## Phase O — Production Deployment *(master Phase 23)*

**Depends on:** A (a real DB driver has to exist for a Postgres-backed
Compose file to mean anything).

**Scope:** `docker-compose.yml` (control plane + Postgres + reverse proxy
with TLS termination), health-check directives, named volumes for
Postgres data and the CA directory, a documented backup strategy (`pg_dump`
on a schedule, or volume snapshots — decided and documented, not left
vague) and a *tested* restore procedure (restore into a fresh container,
verify data), plus agent deployment as both a Docker container and a
systemd unit.

**Exit criteria:** a full `docker-compose up` from a clean checkout
reaches a working, logged-in dashboard with zero manual steps beyond
setting the documented environment variables.

**Risk:** Low to existing code (pure ops/config), but easy to get subtly
wrong (health-check timing, volume permissions) — must be tested live,
not just written.

---

## Phase P — Security / E2E / Failure Testing *(master Phase 21, the parts
not already covered by each phase's own exit criteria)*

**Depends on:** K for a real E2E pass (login→add host→enrollment→...→
recovery notification, exactly the workflow the master prompt lists);
otherwise runs incrementally alongside every phase above.

**Scope:** dedicated security tests beyond each phase's own (unauthorized
API access across every route, privilege escalation attempts, expired/
revoked session and API key rejection, expired/revoked certificate
rejection, CSRF where cookies are used); failure-injection tests (agent
offline, Docker unavailable, DB unavailable, WebSocket disconnect/
reconnect, notification provider down) verifying graceful degradation
rather than cascading failure.

**Risk:** N/A (testing phase).

---

## Phase Q — Load / Scale Testing *(master Phase 22)*

**Depends on:** everything above being real (there is no honest way to
load-test a system with in-memory storage and claim it reflects
production behavior).

**Scope:** exactly the 10/25/50/100-host matrix the master prompt
specifies, measured (not estimated) CPU/RAM/network/DB load/WebSocket
connection count/ingestion rate/API latency/alert-processing time/
frontend performance, written up honestly in `docs/PERFORMANCE.md`
**including where the system falls short**, if it does — the master
prompt is explicit that 100-host support must not be claimed without
having tested it, and that instruction is taken at face value: if 100
hosts isn't achieved cleanly, the roadmap and this doc get updated to say
so rather than the claim being softened.

**Risk:** N/A (testing phase), but this is where earlier architectural
decisions (e.g. Phase A's connection pooling, Phase D's rate limiter
tuning) get validated or found wanting — expect this phase to occasionally
send work backward into an earlier phase, which is normal and should be
tracked in `CHANGELOG.md` when it happens, not hidden.

---

## Phase R — Documentation Finalization *(master Phase 19 + 25)*

**Ongoing throughout, closed out last:** `API.md` (OpenAPI-generated where
practical), `SECURITY.md`, `MONITORING.md`, `ALERTING.md`, `TESTING.md`,
`TROUBLESHOOTING.md`, `CHANGELOG.md` — each written as its corresponding
phase lands, not deferred to the end and reconstructed from memory.
`ARCHITECTURE.md` specifically needs to be **created** (see Gap Analysis
Section D — it's cited everywhere but doesn't exist in this tree) rather
than "updated," and should be treated as the authoritative source the
existing code's `§`-numbered comments already assume exists.

---

## What happens next

Per the master prompt's own execution rule, implementation proceeds one
phase at a time, in the order above, each fully tested and reported
before the next begins. **No implementation has started yet** — this
document and `PROJECT_GAP_ANALYSIS.md`, plus `RUNBOOK.md`/`BRAIN.md`/
`TEST.md`/`TIPS.md`, are the complete Phase-0 deliverable. Awaiting
confirmation before beginning Phase A, since Phase A includes one
change to already-shipped, tested code (`main.go`'s storage-driver
branch, `config.go`'s validation) alongside new code, and per the
Vibe-Coding Safety Rule that warrants an explicit go-ahead rather than
being bundled silently into "just getting started."
