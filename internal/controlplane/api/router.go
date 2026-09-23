// Package api implements the REST layer described in ARCHITECTURE.md §E.1.
// Handlers depend only on the storage.Store and events.Recorder interfaces —
// never on a concrete backend — so the storage driver can change without
// touching this package.
package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/nandani/docker-monitor/internal/controlplane/agentregistry"
	"github.com/nandani/docker-monitor/internal/controlplane/auth"
	"github.com/nandani/docker-monitor/internal/controlplane/events"
	"github.com/nandani/docker-monitor/internal/controlplane/monitoring"
	"github.com/nandani/docker-monitor/internal/controlplane/pki"
	"github.com/nandani/docker-monitor/internal/controlplane/storage"
	"github.com/nandani/docker-monitor/internal/controlplane/ws"
	"github.com/nandani/docker-monitor/internal/shared/models"
)

const sessionCookieName = "dm_session"

type Server struct {
	store    storage.Store
	recorder *events.Recorder
	logger   *slog.Logger
	mux      *http.ServeMux

	// registry, ca, and monitor are optional (nil in tests/builds that
	// don't need agent enrollment / live-monitoring endpoints); handlers
	// that use them check for nil and fail loudly rather than panicking.
	registry *agentregistry.Registry
	ca       *pki.CA
	monitor  *monitoring.Engine

	// hub is optional the same way: nil disables the /ws/* routes rather
	// than panicking, so a build that doesn't want WebSocket support
	// (or a unit test exercising only REST handlers) doesn't have to
	// construct one.
	hub *ws.Hub

	// authService is optional for the same reason every other dependency
	// here is: nil falls back to withAuthStub's old permissive
	// "some credential present" check, which is what every existing
	// handler test still builds against. A production Server always calls
	// WithAuth — see cmd/controlplane/main.go. cookieSecure controls the
	// Set-Cookie Secure flag (ARCHITECTURE.md §26); it has no effect when
	// authService is nil, since no cookie is ever set in stub mode.
	authService  *auth.Service
	cookieSecure bool
}

func NewServer(store storage.Store, recorder *events.Recorder, logger *slog.Logger, registry *agentregistry.Registry, ca *pki.CA, monitor *monitoring.Engine) *Server {
	s := &Server{store: store, recorder: recorder, logger: logger, registry: registry, ca: ca, monitor: monitor, mux: http.NewServeMux()}
	s.routes()
	return s
}

// WithAuth enables real session-based authentication and RBAC
// (ARCHITECTURE.md §G, §23) in place of the permissive dev/test stub.
// cookieSecure should be true for any deployment served over HTTPS (i.e.
// almost always) and is only ever false for local HTTP-only development —
// see config.CookieSecure's doc comment. Returns the same *Server so it
// chains after NewServer, matching WithHub's pattern.
func (s *Server) WithAuth(authService *auth.Service, cookieSecure bool) *Server {
	s.authService = authService
	s.cookieSecure = cookieSecure
	return s
}

// WithHub enables the /ws/* real-time routes (ARCHITECTURE.md §E.2) and
// wires this hub into events.Recorder / agentregistry.Registry so that
// recorded events and ingested metrics are actually broadcast, not just
// deliverable-if-someone-were-listening. Returns the same *Server so it
// chains after NewServer, matching WithAgentRegistry's pattern elsewhere
// in this build.
func (s *Server) WithHub(hub *ws.Hub) *Server {
	s.hub = hub
	if s.recorder != nil {
		s.recorder.SetBroadcaster(hub)
	}
	if s.registry != nil {
		s.registry.SetMetricBroadcaster(hub)
	}
	s.registerWSRoutes()
	return s
}

func (s *Server) Handler() http.Handler {
	if s.authService != nil {
		return withLogging(s.logger, s.withSessionAuth(s.mux))
	}
	return withLogging(s.logger, withAuthStub(s.mux))
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /api/v1/system/health", s.handleHealth)

	s.mux.HandleFunc("POST /api/v1/auth/login", s.handleLogin)
	s.mux.HandleFunc("POST /api/v1/auth/logout", s.handleLogout)
	s.mux.HandleFunc("GET /api/v1/me", s.handleMe)

	s.mux.HandleFunc("GET /api/v1/hosts", s.handleListHosts)
	s.mux.HandleFunc("POST /api/v1/hosts", s.requireAdmin(s.handleCreateHost))
	s.mux.HandleFunc("GET /api/v1/hosts/{id}", s.handleGetHost)
	s.mux.HandleFunc("DELETE /api/v1/hosts/{id}", s.requireAdmin(s.handleDeleteHost))
	s.mux.HandleFunc("GET /api/v1/hosts/{id}/containers", s.handleListHostContainers)
	s.mux.HandleFunc("POST /api/v1/hosts/{id}/enrollment-token", s.requireAdmin(s.handleIssueEnrollmentToken))
	s.mux.HandleFunc("POST /api/v1/hosts/{id}/live", s.requireAdmin(s.handleHostLiveOpen))
	s.mux.HandleFunc("DELETE /api/v1/hosts/{id}/live", s.requireAdmin(s.handleHostLiveClose))

	s.mux.HandleFunc("GET /api/v1/system/ca-certificate", s.handleCACertificate)

	s.mux.HandleFunc("GET /api/v1/containers/{id}", s.handleGetContainer)
	s.mux.HandleFunc("POST /api/v1/containers/{id}/live", s.requireAdmin(s.handleContainerLiveOpen))
	s.mux.HandleFunc("DELETE /api/v1/containers/{id}/live", s.requireAdmin(s.handleContainerLiveClose))

	s.mux.HandleFunc("GET /api/v1/events", s.handleListEvents)
}

// registerWSRoutes adds the real-time push routes (ARCHITECTURE.md §E.2).
// Called only from WithHub, once, after s.hub is set — never from
// routes()/NewServer, so a Server built without WithHub never registers
// these paths at all (a request to them 404s normally, rather than a nil
// s.hub panicking a handler that assumed it was always present).
func (s *Server) registerWSRoutes() {
	s.mux.HandleFunc("GET /ws/events", s.handleWSEvents)
	s.mux.HandleFunc("GET /ws/hosts/{id}/stats", s.handleWSHostStats)
	s.mux.HandleFunc("GET /ws/containers/{id}/stats", s.handleWSContainerStats)
}

// --- JSON helpers ---

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

type apiError struct {
	Error string `json:"error"`
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, apiError{Error: msg})
}

// --- Middleware ---

func withLogging(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		logger.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

// withAuthStub enforces "some form of credential present" for every request
// except health checks and login. This is the fallback used when no
// auth.Service is wired (see WithAuth) — every handler test in this
// codebase that doesn't specifically exercise authentication/RBAC builds
// a Server this way, so those tests keep passing unchanged as real auth
// was layered in. A production Server never runs in this mode; see
// cmd/controlplane/main.go, which always calls WithAuth.
func withAuthStub(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isPublicRoute(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		cookie, cookieErr := r.Cookie(sessionCookieName)
		bearer := r.Header.Get("Authorization")
		if (cookieErr != nil || cookie.Value == "") && bearer == "" {
			writeError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isPublicRoute(path string) bool {
	return path == "/api/v1/system/health" || path == "/api/v1/auth/login"
}

// withSessionAuth is the real authentication middleware (ARCHITECTURE.md
// §G): resolves a session cookie or bearer token to a principal via
// auth.Service.Authenticate, and attaches it to the request context. Every
// route except health and login requires a valid, unexpired session —
// there is no longer an "any credential accepted" fallback once this path
// is active.
func (s *Server) withSessionAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isPublicRoute(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}

		token := sessionTokenFromRequest(r)
		user, err := s.authService.Authenticate(r.Context(), token)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "authentication required")
			return
		}

		ctx := withUser(r.Context(), user)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// sessionTokenFromRequest checks the session cookie first (the browser
// dashboard's path), falling back to an "Authorization: Bearer <token>"
// header for non-browser callers (scripts, the agent's own operator
// tooling, tests) — both carry the same kind of token, just via different
// transport, so both go through the identical Authenticate call.
func sessionTokenFromRequest(r *http.Request) string {
	if cookie, err := r.Cookie(sessionCookieName); err == nil && cookie.Value != "" {
		return cookie.Value
	}
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	return ""
}

type contextKey int

const userContextKey contextKey = iota

func withUser(ctx context.Context, u *models.User) context.Context {
	return context.WithValue(ctx, userContextKey, u)
}

func userFromContext(ctx context.Context) (*models.User, bool) {
	u, ok := ctx.Value(userContextKey).(*models.User)
	return u, ok
}

// requireAdmin gates a handler to RoleAdmin principals (ARCHITECTURE.md
// §23 — "every sensitive backend operation must enforce authorization",
// never just the frontend). When no auth.Service is wired (stub mode,
// see withAuthStub), there is no principal to check, so this is a no-op —
// consistent with stub mode not enforcing anything beyond "some
// credential present" for any route.
func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.authService == nil {
			next(w, r)
			return
		}
		user, ok := userFromContext(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		if user.Role != models.RoleAdmin {
			writeError(w, http.StatusForbidden, "admin role required")
			return
		}
		next(w, r)
	}
}
