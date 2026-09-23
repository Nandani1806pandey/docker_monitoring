package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nandani/docker-monitor/internal/controlplane/auth"
	"github.com/nandani/docker-monitor/internal/controlplane/events"
	"github.com/nandani/docker-monitor/internal/controlplane/monitoring"
	"github.com/nandani/docker-monitor/internal/controlplane/storage/memory"
	"github.com/nandani/docker-monitor/internal/shared/models"
)

// newAuthedTestServer builds a Server with a real auth.Service wired (via
// WithAuth), plus one admin and one viewer account already created —
// unlike newTestServer (live_test.go), which stays in permissive stub
// mode. Tests in this file are specifically about what changes once real
// auth is active.
func newAuthedTestServer(t *testing.T) (*Server, *memory.Store, *auth.Service) {
	t.Helper()
	store := memory.New()
	recorder := events.NewRecorder(store)
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 100}))
	engine := monitoring.NewEngine(monitoring.DefaultThresholds())
	authSvc := auth.NewService(store, time.Hour)

	if _, err := authSvc.CreateUser(context.Background(), "admin@example.com", "Admin", "admin-password-123", models.RoleAdmin); err != nil {
		t.Fatalf("seed admin: %v", err)
	}
	if _, err := authSvc.CreateUser(context.Background(), "viewer@example.com", "Viewer", "viewer-password-123", models.RoleViewer); err != nil {
		t.Fatalf("seed viewer: %v", err)
	}

	server := NewServer(store, recorder, logger, nil, nil, engine).WithAuth(authSvc, false)
	return server, store, authSvc
}

func doJSON(t *testing.T, server *Server, method, path string, body interface{}, token string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	return rec
}

func loginAs(t *testing.T, server *Server, email, password string) (token string, cookie *http.Cookie) {
	t.Helper()
	rec := doJSON(t, server, http.MethodPost, "/api/v1/auth/login", loginRequest{Email: email, Password: password}, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("login failed: %d %s", rec.Code, rec.Body.String())
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName {
			return c.Value, c
		}
	}
	t.Fatalf("login response had no session cookie")
	return "", nil
}

func TestLogin_Success_SetsCookie(t *testing.T) {
	server, _, _ := newAuthedTestServer(t)
	token, cookie := loginAs(t, server, "admin@example.com", "admin-password-123")
	if token == "" {
		t.Fatalf("expected a non-empty session token in the cookie")
	}
	if !cookie.HttpOnly {
		t.Fatalf("expected session cookie to be HttpOnly")
	}
	if cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("expected SameSite=Strict, got %v", cookie.SameSite)
	}
}

func TestLogin_WrongPassword_Rejected(t *testing.T) {
	server, _, _ := newAuthedTestServer(t)
	rec := doJSON(t, server, http.MethodPost, "/api/v1/auth/login",
		loginRequest{Email: "admin@example.com", Password: "totally-wrong"}, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestUnauthenticatedRequest_Rejected(t *testing.T) {
	server, _, _ := newAuthedTestServer(t)
	rec := doJSON(t, server, http.MethodGet, "/api/v1/hosts", nil, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for a request with no session, got %d", rec.Code)
	}
}

func TestBogusBearerToken_NoLongerAccepted(t *testing.T) {
	// Regression guard: before real auth was wired, any non-empty bearer
	// value (e.g. "Bearer dev") was accepted by the stub. Once a real
	// auth.Service is active, only an actual issued session token works.
	server, _, _ := newAuthedTestServer(t)
	rec := doJSON(t, server, http.MethodGet, "/api/v1/hosts", nil, "dev")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected a made-up bearer token to be rejected once real auth is wired, got %d", rec.Code)
	}
}

func TestAuthenticatedRequest_Succeeds(t *testing.T) {
	server, _, _ := newAuthedTestServer(t)
	token, _ := loginAs(t, server, "viewer@example.com", "viewer-password-123")
	rec := doJSON(t, server, http.MethodGet, "/api/v1/hosts", nil, token)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for an authenticated read, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestMe_ReturnsCallerProfile(t *testing.T) {
	server, _, _ := newAuthedTestServer(t)
	token, _ := loginAs(t, server, "admin@example.com", "admin-password-123")
	rec := doJSON(t, server, http.MethodGet, "/api/v1/me", nil, token)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp loginResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Email != "admin@example.com" || resp.Role != "admin" {
		t.Fatalf("unexpected /me response: %+v", resp)
	}
}

func TestLogout_InvalidatesSession(t *testing.T) {
	server, _, _ := newAuthedTestServer(t)
	token, _ := loginAs(t, server, "admin@example.com", "admin-password-123")

	rec := doJSON(t, server, http.MethodPost, "/api/v1/auth/logout", nil, token)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204 from logout, got %d", rec.Code)
	}

	rec2 := doJSON(t, server, http.MethodGet, "/api/v1/hosts", nil, token)
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("expected the logged-out session to be rejected, got %d", rec2.Code)
	}
}

func TestRBAC_ViewerCannotCreateHost(t *testing.T) {
	server, _, _ := newAuthedTestServer(t)
	token, _ := loginAs(t, server, "viewer@example.com", "viewer-password-123")

	rec := doJSON(t, server, http.MethodPost, "/api/v1/hosts",
		map[string]string{"name": "sneaky-host", "kind": "local"}, token)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for a viewer trying to create a host, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestRBAC_AdminCanCreateHost(t *testing.T) {
	server, _, _ := newAuthedTestServer(t)
	token, _ := loginAs(t, server, "admin@example.com", "admin-password-123")

	rec := doJSON(t, server, http.MethodPost, "/api/v1/hosts",
		map[string]string{"name": "admin-host", "kind": "local"}, token)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201 for an admin creating a host, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestRBAC_ViewerCanReadHosts(t *testing.T) {
	server, _, _ := newAuthedTestServer(t)
	token, _ := loginAs(t, server, "viewer@example.com", "viewer-password-123")

	rec := doJSON(t, server, http.MethodGet, "/api/v1/hosts", nil, token)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected a viewer to be able to read hosts, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHealthEndpoint_NeverRequiresAuth(t *testing.T) {
	server, _, _ := newAuthedTestServer(t)
	rec := doJSON(t, server, http.MethodGet, "/api/v1/system/health", nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected health check to work with no auth at all, got %d", rec.Code)
	}
}
