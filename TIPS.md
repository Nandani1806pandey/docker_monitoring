# Tips

Practical knowledge for working on this codebase. No secrets here — see
`RUNBOOK.md`/deployment docs for anything credential-shaped.

## Common commands

```bash
# Build everything
go build ./...

# Vet + format check (run both before considering any change done)
go vet ./...
gofmt -l .          # any output = files need `gofmt -w`

# Full test suite with the race detector
go test ./... -race

# One package, verbose (see individual test names and pass/fail)
go test ./internal/controlplane/auth/... -v

# One test by name (regex-matched)
go test ./internal/controlplane/api/... -run TestRBAC_ViewerCannotCreateHost -v
```

## Environment / network quirks specific to this sandbox

- `go build`/`go mod tidy` **cannot** reach `golang.org` or
  `proxy.golang.org` directly — the module proxy default fails with a
  403 "Host not in allowlist" error. Two things fix this depending on
  what you're doing:
  - For an already-`go.sum`-pinned dependency, just set
    `GOPROXY=direct GOSUMDB=off` before running `go build`/`go test` —
    `direct` mode fetches straight from the VCS host (`github.com`,
    which *is* reachable) instead of the proxy.
  - For a **new** `golang.org/x/...` dependency, `direct` mode alone
    still fails, because Go's vanity-import mechanism needs to resolve
    `golang.org` itself first. The fix already in this repo's `go.mod`
    is a `replace` directive pointing at the official GitHub mirror
    (`github.com/golang/crypto`, `github.com/golang/sys`) — copy that
    pattern for any future `golang.org/x/...` need. See `BRAIN.md` for
    the full explanation.
  - If Go itself isn't installed in a fresh container:
    `apt-get install -y golang-go` works (Ubuntu's `archive.ubuntu.com`
    is reachable); it installs an older-but-compatible Go 1.22.x, fine
    for this module's `go 1.22.2` directive.

## Docker commands (for exercising the agent locally)

```bash
# Confirm the daemon and socket are reachable (what the agent needs):
docker info
ls -l /var/run/docker.sock

# Watch what the agent would see:
docker ps
docker stats --no-stream
```

## Debugging commands

```bash
# Inspect the persisted CA:
openssl x509 -in ./data/ca/ca-cert.pem -noout -text

# Decode a session cookie's raw value (it's opaque — this just confirms
# it's non-empty base64url, not a JWT to decode):
echo -n '<cookie-value>' | base64 -d 2>&1 | xxd | head

# Tail structured logs (JSON lines) with a bit of formatting:
go run ./cmd/controlplane 2>&1 | jq .
```

## Common errors and fixes

| Symptom | Likely cause | Fix |
|---|---|---|
| `DM_SESSION_SECRET must be set...` at startup | Env var missing | `export DM_SESSION_SECRET="$(openssl rand -hex 32)"` |
| `unsupported storage driver in this build` | `DM_STORAGE_DRIVER` set to anything but `memory` | Only `memory` exists today (see Gap Analysis Section D) — unset it or set it to `memory` explicitly |
| `403 ... Host not in allowlist: proxy.golang.org` during `go build` | Default module proxy blocked in this network | `export GOPROXY=direct GOSUMDB=off` |
| Agent TLS handshake fails after a control-plane restart | CA regenerated because `DM_CA_DIR` was empty/different this run | Always point `DM_CA_DIR` at the same persistent directory across restarts |
| A `/ws/*` route 404s | Server built without `WithHub(...)` (common in a test server built via `NewServer(...)` alone) | Only `cmd/controlplane/main.go`'s real server calls `WithHub` — this is intentional for test servers that don't need it |
| Login always 401s even with the right password | Comparing against a server built *without* `WithAuth(...)`/`WithAuth(nil, ...)`-equivalent (auth stub mode has no real login) | Use `newAuthedTestServer` (see `auth_test.go`) in tests, or confirm `cmd/controlplane/main.go`'s real wiring in production |

## Environment variables (current, complete list)

See `internal/shared/config/config.go` for the authoritative source; as
of this writing:

`DM_HTTP_ADDR`, `DM_STORAGE_DRIVER`, `DM_DATABASE_URL`,
`DM_NORMAL_INTERVAL_SECONDS`, `DM_CRITICAL_INTERVAL_SECONDS`,
`DM_MANUAL_INTERVAL_SECONDS`, `DM_HYSTERESIS_SECONDS`,
`DM_HISTORICAL_METRICS_ENABLED`, `DM_HISTORICAL_RETENTION_DAYS`,
`DM_SESSION_SECRET` (required), `DM_AGENT_ADDR`, `DM_CA_DIR`,
`DM_BOOTSTRAP_ADMIN_EMAIL`, `DM_BOOTSTRAP_ADMIN_PASSWORD`,
`DM_SESSION_TTL_HOURS`, `DM_COOKIE_SECURE`.

This list will grow substantially as the roadmap's phases land (OIDC,
notification providers, rate limiting, etc. each add their own) — keep
this section in sync with `config.go` rather than letting it drift; a
stale env-var list here is worse than no list.

## Development shortcuts

- The in-memory store means every test starts from a clean slate for
  free (`memory.New()`) — no test fixtures/teardown needed for storage
  state.
- `newAuthedTestServer` (in `internal/controlplane/api/auth_test.go`)
  is the fastest way to get a fully-wired server with a real admin and
  real viewer account already created, for any new handler test that
  needs to check RBAC.
- For a live manual smoke test without polluting `./data/ca`, point
  `DM_CA_DIR` at a `/tmp` path — see `RUNBOOK.md`'s startup example.

## Deployment tips

Not much here yet — see `docs/PROJECT_COMPLETION_ROADMAP.md` Phase O.
This section fills in once real deployment configs exist; anything
written here today would be speculative and likely to go stale.
