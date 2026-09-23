package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nandani/docker-monitor/internal/controlplane/events"
	"github.com/nandani/docker-monitor/internal/controlplane/monitoring"
	"github.com/nandani/docker-monitor/internal/controlplane/storage/memory"
	"github.com/nandani/docker-monitor/internal/shared/models"
)

func newTestServer(t *testing.T) (*Server, *memory.Store) {
	t.Helper()
	store := memory.New()
	recorder := events.NewRecorder(store)
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 100})) // discard
	engine := monitoring.NewEngine(monitoring.DefaultThresholds())
	return NewServer(store, recorder, logger, nil, nil, engine), store
}

func authedRequest(method, path string) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("Authorization", "Bearer dev")
	return req
}

func decodeLiveResponse(t *testing.T, rec *httptest.ResponseRecorder) liveResponse {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var out liveResponse
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return out
}

func TestHostLiveOpenAndClose(t *testing.T) {
	server, store := newTestServer(t)
	host := &models.Host{ID: "host-1", Name: "host-1", Kind: models.HostKindLocal}
	if err := store.CreateHost(context.Background(), host); err != nil {
		t.Fatalf("CreateHost: %v", err)
	}

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, authedRequest(http.MethodPost, "/api/v1/hosts/host-1/live"))
	out := decodeLiveResponse(t, rec)
	if out.Mode != models.ModeManualRealtime || out.IntervalSeconds != 2 {
		t.Fatalf("expected MANUAL_REALTIME/2s after opening, got %+v", out)
	}

	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, authedRequest(http.MethodDelete, "/api/v1/hosts/host-1/live"))
	out = decodeLiveResponse(t, rec)
	if out.Mode != models.ModeNormal || out.IntervalSeconds != 60 {
		t.Fatalf("expected NORMAL/60s after closing with no critical condition, got %+v", out)
	}
}

// TestHostLiveClose_FallsBackToCriticalNotNormal mirrors the FSM's own
// TestManualViewer_FallsBackToCriticalNotNormal at the HTTP layer: closing
// a live view on a host that's still critical must not silently report
// NORMAL.
func TestHostLiveClose_FallsBackToCriticalNotNormal(t *testing.T) {
	server, store := newTestServer(t)
	host := &models.Host{ID: "host-1", Name: "host-1", Kind: models.HostKindLocal}
	if err := store.CreateHost(context.Background(), host); err != nil {
		t.Fatalf("CreateHost: %v", err)
	}
	server.monitor.EvaluateMetric(monitoring.EntityHost, "host-1", models.Metric{CPUPercent: 95, CollectedAt: time.Now()})

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, authedRequest(http.MethodPost, "/api/v1/hosts/host-1/live"))
	out := decodeLiveResponse(t, rec)
	if out.Mode != models.ModeManualRealtime {
		t.Fatalf("expected MANUAL_REALTIME to win over CRITICAL while open, got %+v", out)
	}

	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, authedRequest(http.MethodDelete, "/api/v1/hosts/host-1/live"))
	out = decodeLiveResponse(t, rec)
	if out.Mode != models.ModeCritical || out.IntervalSeconds != 10 {
		t.Fatalf("expected fallback to CRITICAL/10s (still active), not NORMAL, got %+v", out)
	}
}

func TestHostLiveOpen_UnknownHostIs404(t *testing.T) {
	server, _ := newTestServer(t)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, authedRequest(http.MethodPost, "/api/v1/hosts/does-not-exist/live"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown host, got %d", rec.Code)
	}
}

func TestContainerLiveOpenAndClose(t *testing.T) {
	server, store := newTestServer(t)
	container := &models.Container{ID: "c1", HostID: "host-1", DockerContainerID: "abc123", Name: "web"}
	if err := store.UpsertContainer(context.Background(), container); err != nil {
		t.Fatalf("UpsertContainer: %v", err)
	}

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, authedRequest(http.MethodPost, "/api/v1/containers/c1/live"))
	out := decodeLiveResponse(t, rec)
	if out.Mode != models.ModeManualRealtime {
		t.Fatalf("expected MANUAL_REALTIME, got %+v", out)
	}

	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, authedRequest(http.MethodDelete, "/api/v1/containers/c1/live"))
	out = decodeLiveResponse(t, rec)
	if out.Mode != models.ModeNormal {
		t.Fatalf("expected NORMAL after closing, got %+v", out)
	}
}

func TestContainerLiveOpen_UnknownContainerIs404(t *testing.T) {
	server, _ := newTestServer(t)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, authedRequest(http.MethodPost, "/api/v1/containers/does-not-exist/live"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown container, got %d", rec.Code)
	}
}

// TestLiveEndpoints_501WithoutMonitor covers a server built without a
// monitoring.Engine (e.g. a future minimal/embedded build) — the handlers
// must fail loudly rather than nil-pointer panicking.
func TestLiveEndpoints_501WithoutMonitor(t *testing.T) {
	store := memory.New()
	recorder := events.NewRecorder(store)
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 100}))
	server := NewServer(store, recorder, logger, nil, nil, nil)

	host := &models.Host{ID: "host-1", Name: "host-1", Kind: models.HostKindLocal}
	if err := store.CreateHost(context.Background(), host); err != nil {
		t.Fatalf("CreateHost: %v", err)
	}

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, authedRequest(http.MethodPost, "/api/v1/hosts/host-1/live"))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501 without a monitor engine, got %d", rec.Code)
	}
}
