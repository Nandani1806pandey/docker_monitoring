# Brain — Architectural Memory

This file is the project's institutional memory: why things are the way
they are, what was tried and reverted, and what future work should know
before repeating already-settled decisions (or already-discarded
mistakes). Update this whenever a major architectural decision is made or
changed — not just when code is added.

## Current architecture (as of this writing)

```
cmd/controlplane/main.go      — process entrypoint, wiring, CA persistence
cmd/agent/main.go             — agent entrypoint

internal/controlplane/
  api/          — REST handlers, router, session-auth middleware
  auth/         — password hashing (Argon2id), session Service
  agentapi/     — the mTLS-facing HTTP server agents talk to
  agentregistry/— enrollment token issuance, telemetry ingestion, FSM hookup
  pki/          — internal CA, cert issuance
  monitoring/   — adaptive monitoring FSM (NORMAL/CRITICAL/MANUAL_REALTIME)
  events/       — append-only operational event log + broadcast hook
  ws/           — WebSocket hub (topic pub/sub)
  storage/      — Store interface + memory/ implementation

internal/agent/
  docker/       — Docker Engine API client (stdlib, Unix socket)
  hoststats/    — /proc-based host CPU/mem/net
  collector/    — ties docker+hoststats together, reports to control plane
  transport/    — agent-side HTTP client over mTLS

internal/shared/
  config/       — env-var configuration
  models/       — domain types (storage-agnostic)

internal/integrationtest/ — real mTLS agent<->control-plane round trip
```

Data flow, end to end: agent's `collector` polls Docker + host `/proc`
stats on an interval set by the FSM → POSTs telemetry to the control
plane's mTLS listener (`agentapi`) → `agentregistry.IngestTelemetry`
persists it, feeds it to `monitoring.Engine` for the next FSM decision,
records events, and broadcasts to any subscribed WebSocket clients → the
FSM's decision (interval + mode) comes back in the same response for the
agent to use on its *next* cycle (see the Gap Analysis's note on why this
means "manual realtime" isn't instant).

Authentication flow: browser POSTs credentials to `/api/v1/auth/login` →
`auth.Service.Login` verifies against the stored Argon2id hash → creates a
`Session` row (currently in-memory), returns its token in an
`HttpOnly`/`SameSite=Strict` cookie → every subsequent request's
`withSessionAuth` middleware resolves that cookie back to a `User` via
`auth.Service.Authenticate` → `requireAdmin` gates mutating routes on
`User.Role == RoleAdmin`.

WebSocket flow: `ws.Hub` is a topic-keyed pub/sub. `events.Recorder` and
`agentregistry.Registry` both hold an optional broadcaster reference
(`SetBroadcaster`/`SetMetricBroadcaster`) set once at `WithHub(...)` time
in `main.go` — neither package imports `ws` directly, they depend on a
narrow local interface, so the WebSocket layer stays swappable/optional
(a server built without `WithHub` simply never registers `/ws/*` routes
and nothing else changes).

## Why key design decisions are the way they are

**In-memory storage as the current-and-only backend.** Zero-dependency
single-host operation was a deliberate early goal (see `storage/memory`'s
package doc comment). This is explicitly *not* the final answer — see
`docs/PROJECT_COMPLETION_ROADMAP.md` Phase A — but it means every
interface in `storage.Store` was designed against "what does a real
backend need" from day one, not retrofitted.

**Argon2id via a GitHub-mirror `replace` directive.** `golang.org` itself
(not just `proxy.golang.org`) is blocked by this build environment's
network egress policy — its vanity-import redirect mechanism requires an
HTTP request to `golang.org` *before* the module proxy is even consulted,
so no `golang.org/x/...` import can resolve normally here, regardless of
`GOPROXY` settings. The fix: `go.mod` has
```
require golang.org/x/crypto v0.28.0
replace golang.org/x/crypto => github.com/golang/crypto v0.28.0
replace golang.org/x/sys => github.com/golang/sys v0.26.0
```
`github.com` and `codeload.github.com` *are* reachable, and Google
publishes read-only GitHub mirrors of every `golang.org/x/*` module at
identical import paths internally — so the import statement in
`password.go` is still the real `golang.org/x/crypto/argon2` package; only
where `go get`/`go mod tidy` physically fetches the module *from* changes.
This is the real, standard Argon2id implementation, not a hand-rolled
substitute — worth preserving as the pattern for any future `golang.org/x/
...` dependency (OIDC's JWT/JWKS handling was deliberately kept
stdlib-only instead of reaching for `golang.org/x/oauth2`, partly to avoid
needing a third mirror replace and partly because a from-scratch OIDC
implementation is a well-specified, bounded amount of code — see the
discarded-design note below for what that looked like when it was
sketched once already).

**Sessions are server-side records, not JWTs.** Chosen specifically so
logout/revocation is instant and absolute — a stolen JWT with a long
expiry can't be un-issued short of a blocklist, which is just a
server-side session store with extra steps. `Session.TokenHash` (not the
raw token) is what's persisted, same pattern as the CA/enrollment tokens
already used elsewhere in this codebase — a leaked storage snapshot never
hands out a directly-usable session.

**Timing-parity guard in `Login`.** A nonexistent-email login still runs a
real Argon2id verify against a fixed dummy hash (`dummyHashForTimingParity`
in `service.go`) so response latency can't become an account-enumeration
side channel even though the returned error is already identical either
way. Small detail, deliberately not skipped.

**Two-tier `Role` enum instead of full RBAC, for now.** `models.go`'s own
doc comment calls this out explicitly as a deliberate scope boundary, not
an oversight: it covers "can this user mutate state" but not per-resource
grants. Phase C in the roadmap is where this gets replaced with a real
group/role/permission model — and when it does, every caller of
`models.Role`/`RoleAdmin`/`RoleViewer` needs to be found and migrated
deliberately (see that phase's risk notes), not have the type silently
swapped out from under them.

**Adaptive monitoring interval delivery is pull-based, not push.** The
control plane's FSM decision only reaches the agent in the response to
that agent's *own* telemetry POST. This means "open manual realtime" from
the browser doesn't take effect until the agent's next scheduled
check-in — which, during NORMAL mode (60s interval), could be up to a
minute of lag. This is a known, documented gap (Gap Analysis Section B.1,
Roadmap Phase A's blocking note isn't quite right — it's actually
independent of Phase A and could be tackled any time; corrected in the
roadmap's ordering rationale). A future fix likely needs either a
long-lived connection from agent to control plane, or accepting the
latency as an inherent property of a pull-based protocol and documenting
it as such rather than trying to eliminate it.

## Decisions made, then reverted (important — don't redo this work blind)

During this project's history, a **different, more ambitious milestone-9
auth design** was fully built, tested (32 passing tests, live
end-to-end-verified), and then **entirely discarded** in favor of the
current simpler design, because a separate, independently-developed
version of the same milestone (built in a different session against the
same codebase) converged on a different, better-tested approach and was
adopted instead when the two were compared. The discarded design is worth
knowing about because Phases C/D/J in the roadmap will want to rebuild
most of it — it is a reasonable starting sketch, not a dead end:

- **Groups → Roles → Permissions** (not a two-value enum): `Group` had
  `RoleIDs`, `Role` had `Permissions []string`, a user's effective
  permissions were the union across all their groups' roles. This is
  extremely close to what Phase C's roadmap entry now asks for.
- **API keys**: hash-only storage, permission-subset-of-creator
  constrained *at request time* (not just at issuance) so a demoted
  user's already-issued keys can't outlive the demotion — this exact
  property is called out again in Phase D's exit criteria above.
- **A from-scratch, stdlib-only OIDC client**: discovery, authorization-
  code exchange, RS256 JWT/JWKS verification, all over `net/http` +
  `encoding/json` + `crypto/rsa` — no third-party OIDC library, avoiding
  a second `golang.org/x/...` mirror dependency. Group-claim sync
  deliberately mapped only onto *already-provisioned* internal groups —
  an OIDC-claimed group with no matching internal group was ignored
  rather than auto-created, specifically so SSO login alone could never
  grant access to something an admin hadn't already set up. This
  reasoning is worth reusing verbatim in Phase J.
- **A generic per-IP token-bucket rate limiter**, applied more tightly to
  `/auth/login` than to the rest of the API — the exact shape Phase D
  asks for.
- **PBKDF2-HMAC-SHA256 instead of Argon2id** (in that earlier design) —
  this part was *not* reused; the design that was ultimately kept uses
  real Argon2id via the mirror trick described above, which is a strictly
  better outcome and the reason this note exists at all: don't reintroduce
  PBKDF2 as a "simpler" option in a future pass, it was a workaround for a
  constraint (`golang.org` reachability) that the mirror `replace`
  directive already solves properly.

None of that discarded code exists in the current tree — it was fully
removed, not left as dead files — but the ideas above are sound and
should inform Phases C/D/J rather than being rediscovered from scratch.

## Known limitations (current, not aspirational)

- No persistence beyond the CA's PEM files (Phase A).
- No per-container monitoring mode, only per-host (Phase B... actually
  Gap Analysis B.2 — cross-reference, not a roadmap phase on its own;
  likely folds into Phase B or E's implementation).
- No certificate rotation/revocation (Phase I).
- `ARCHITECTURE.md` is cited throughout existing code comments (`§23`,
  `§45`, etc.) but does not exist in this repository — treat every such
  citation as referring to a document that needs to be *written* (Phase
  R), not one that can be looked up today.

## Assumptions currently baked into the code

- Single-process control plane (the rate limiter design note in the
  discarded auth work, and the in-memory store's own doc comment, both
  say this explicitly: no cross-instance coordination exists anywhere).
- Linux-only agent (`hoststats` reads `/proc` directly, no portability
  shim, matching the existing Docker-socket dependency's own
  Linux-shaped assumption).
- A self-hosted deployment behind, at most, a documented and *configured*
  reverse proxy — `clientIP()`-style logic (in the discarded rate
  limiter, and worth preserving as a principle for Phase D) never trusts
  `X-Forwarded-For` unless a proxy is explicitly configured to be trusted.

## Future decisions to make explicitly (not yet decided)

- Migration tool choice for Phase A (`golang-migrate` vs. a minimal
  embedded runner) — contingent on what's actually reachable from this
  network-restricted environment at implementation time.
- Downsampling schedule specifics for Phase B (exact rollup resolutions
  and retention cutoffs) — sketch exists in the roadmap, exact numbers
  need to be chosen and written into `docs/MONITORING.md` when that
  phase starts.
- Whether certificate revocation (Phase I) is a CRL-style list or
  something else — leaning CRL-style given the expected small agent
  count, but not committed yet.
