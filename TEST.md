# Test Checklist

Updated after every implementation phase. A checked box means it was
**actually run and observed passing in this environment**, not that code
exists which is expected to pass. Last verified: this inspection pass
(Phase 0 — no code changed since the prior session's live smoke test).

## Current state (Phase 0 — pre-roadmap, backend-only)

- [x] Backend builds (`go build ./...`)
- [x] Agent builds (part of the same `go build ./...` — `cmd/agent` is in
      the module)
- [x] Unit tests pass (`go test ./...`)
- [x] Race tests pass (`go test ./... -race`)
- [x] Integration tests pass (`internal/integrationtest`, real mTLS
      agent↔control-plane round trip)
- [x] Authentication works (verified both by `auth`/`api` package tests
      *and* a live `curl` smoke test: real login → real cookie → `/me` →
      logout → session genuinely dead afterward)
- [x] RBAC works (two-tier admin/viewer, verified by `TestRBAC_*` in
      `internal/controlplane/api/auth_test.go` and live: admin creates a
      host, viewer is denied the same request)
- [x] Agent enrollment works (`registry_test.go`,
      `internal/integrationtest`)
- [x] mTLS works (real certificate issuance and handshake in the
      integration test — not mocked)
- [x] Docker metrics work (`internal/agent/docker/docker_test.go` against
      a fake daemon on a real Unix socket)
- [x] Host metrics work (`internal/agent/hoststats/hoststats_test.go`,
      including a sanity check against this machine's real `/proc`)
- [x] WebSockets work (`internal/controlplane/ws/hub_test.go`; live
      verification is recorded in `README.md`'s Phase 8/9 notes from
      earlier in this project's history)
- [ ] Historical metrics work — **not implemented yet** (Roadmap Phase B)
- [ ] Alerts work — **not implemented yet** (Roadmap Phase E)
- [ ] Notifications work — **not implemented yet** (Roadmap Phase F)
- [ ] Frontend builds — **no frontend exists yet** (Roadmap Phase K)
- [ ] E2E tests pass — **no E2E suite exists yet** (Roadmap Phase P;
      blocked on K for a real E2E pass, per the roadmap)
- [ ] Security tests pass — partially: the `auth` package's own tests
      cover wrong-password/expired-session/unknown-token rejection, but
      the master prompt's fuller list (privilege escalation attempts,
      revoked API key — no API keys exist yet, invalid certificate,
      permission bypass, CSRF) is **not** a dedicated suite yet (Roadmap
      Phase P)
- [ ] Load tests pass — **not run** (Roadmap Phase Q)
- [ ] Persistent-storage tests pass — **N/A, no persistent backend yet**
      (Roadmap Phase A)

## How to run what exists today

```bash
# Full suite, race-checked (what CI should run on every change):
export GOPROXY=direct GOSUMDB=off   # only needed if golang.org/x/crypto
                                     # hasn't been fetched into the module
                                     # cache yet in this environment
go build ./...
go vet ./...
gofmt -l .            # should print nothing
go test ./... -race

# Just the auth package (14 tests: 5 password, 9 service):
go test ./internal/controlplane/auth/... -v

# Just the router-level auth/RBAC tests (18 tests):
go test ./internal/controlplane/api/... -run 'TestLogin|TestRBAC|TestMe|TestLogout|TestUnauthenticated|TestBogusBearer|TestAuthenticatedRequest|TestHealthEndpoint' -v

# Live smoke test (real running binary, real curl, real cookie):
# see RUNBOOK.md's "Startup" and "Agent enrollment" sections for the
# exact commands — this is the same sequence used to verify the auth
# milestone live before it was considered done.
```

## Test count history

| Milestone | Package | Tests added | Cumulative notes |
|---|---|---|---|
| Auth (Argon2id + sessions) | `internal/controlplane/auth` | 14 | password hash/verify (5), service login/logout/session lifecycle/bootstrap (9) |
| Auth (router wiring) | `internal/controlplane/api` | 18 | cookie attributes, stub-token regression guard, RBAC allow/deny, health-stays-public |
| (everything before auth) | various | — | not re-counted here; see each package's own `_test.go` file count in the repo — nothing above claims a total project-wide count, since that number would need to be recomputed after every phase and is easy to let go stale |

This table is updated, not replaced, as each roadmap phase lands — add a
row, don't overwrite history.
