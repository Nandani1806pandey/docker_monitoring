package agentregistry

import (
	"context"
	"testing"
	"time"

	"github.com/nandani/docker-monitor/internal/controlplane/events"
	"github.com/nandani/docker-monitor/internal/controlplane/monitoring"
	"github.com/nandani/docker-monitor/internal/controlplane/pki"
	"github.com/nandani/docker-monitor/internal/controlplane/storage"
	"github.com/nandani/docker-monitor/internal/controlplane/storage/memory"
	"github.com/nandani/docker-monitor/internal/shared/models"
)

// fakeClock lets these tests drive the FSM's hysteresis window
// deterministically, the same way monitoring's own tests do.
type fakeClock struct{ now time.Time }

func (f *fakeClock) Now() time.Time          { return f.now }
func (f *fakeClock) advance(d time.Duration) { f.now = f.now.Add(d) }

func newTestRegistry(t *testing.T) (*Registry, *memory.Store, *fakeClock) {
	t.Helper()
	store := memory.New()
	recorder := events.NewRecorder(store)
	ca, err := pki.GenerateCA("test-ca", time.Hour)
	if err != nil {
		t.Fatalf("GenerateCA: %v", err)
	}
	clock := &fakeClock{now: time.Now()}
	cfg := monitoring.DefaultThresholds()
	cfg.HysteresisWindow = 60 * time.Second
	cfg.MinConsecutiveNormalSamples = 3
	engine := monitoring.NewEngineWithClock(cfg, clock)
	return New(store, ca, recorder, engine), store, clock
}

func createHost(t *testing.T, store *memory.Store, id string) *models.Host {
	t.Helper()
	h := &models.Host{ID: id, Name: id, Kind: models.HostKindLocal}
	if err := store.CreateHost(context.Background(), h); err != nil {
		t.Fatalf("CreateHost: %v", err)
	}
	return h
}

func hostEvents(hostID string) storage.EventFilter {
	return storage.EventFilter{HostID: &hostID, Limit: 100}
}

// TestIngestTelemetry_StoredMetricUsesAuthoritativeModeNotAgentGuess is a
// regression test: the agent labels the Mode field on outgoing metrics
// from its own last-known interval (internal/agent/collector's
// modeForInterval), a local approximation with no visibility into the
// FSM's actual state. That guess can be wrong — most obviously when two
// configured interval values collide (e.g. a deployment sets
// NormalIntervalSeconds equal to ManualIntervalSeconds) — and there's no
// reason to trust it at all when the control plane has just evaluated the
// authoritative mode for this exact entity a few lines above. The metric
// this test sends claims Mode: ModeManualRealtime (as if the agent's
// interval happened to read that way), but no manual viewer is open and
// nothing is critical — the FSM's real answer is NORMAL, and that's what
// must end up persisted and (if wired) broadcast, not the agent's label.
func TestIngestTelemetry_StoredMetricUsesAuthoritativeModeNotAgentGuess(t *testing.T) {
	registry, store, _ := newTestRegistry(t)
	createHost(t, store, "host-1")

	_, err := registry.IngestTelemetry(context.Background(), "host-1", TelemetryReport{
		Metrics: []models.Metric{
			{
				EntityType:  "host",
				CPUPercent:  5, // nowhere near critical
				CollectedAt: time.Now().UTC(),
				// Deliberately wrong: the agent's own (mis)guess.
				Mode:            models.ModeManualRealtime,
				IntervalSeconds: 2,
			},
		},
	})
	if err != nil {
		t.Fatalf("IngestTelemetry: %v", err)
	}

	stored, err := store.LatestMetric(context.Background(), "host", "host-1")
	if err != nil {
		t.Fatalf("LatestMetric: %v", err)
	}
	if stored.Mode != models.ModeNormal {
		t.Fatalf("expected the stored metric's Mode to be overwritten with the FSM's authoritative NORMAL, "+
			"got %q (the agent's incorrect guess was persisted instead)", stored.Mode)
	}
}

func TestIngestTelemetry_HostCPUBreachReturnsCriticalInterval(t *testing.T) {
	registry, store, _ := newTestRegistry(t)
	createHost(t, store, "host-1")

	result, err := registry.IngestTelemetry(context.Background(), "host-1", TelemetryReport{
		Metrics: []models.Metric{
			{EntityType: "host", CPUPercent: 95, CollectedAt: time.Now().UTC()},
		},
	})
	if err != nil {
		t.Fatalf("IngestTelemetry: %v", err)
	}
	if result.Mode != models.ModeCritical || result.IntervalSeconds != 10 {
		t.Fatalf("expected CRITICAL/10s, got %+v", result)
	}

	host, err := store.GetHost(context.Background(), "host-1")
	if err != nil {
		t.Fatalf("GetHost: %v", err)
	}
	if host.MonitoringMode != models.ModeCritical {
		t.Fatalf("expected host.MonitoringMode to be persisted as CRITICAL, got %s", host.MonitoringMode)
	}

	evs, err := store.ListEvents(context.Background(), hostEvents("host-1"))
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(evs) != 1 || evs[0].Type != "monitoring.cpu_threshold" || evs[0].Severity != models.SeverityCritical {
		t.Fatalf("expected one cpu_threshold critical event, got %+v", evs)
	}
}

func TestIngestTelemetry_HostRecoversAfterHysteresisWindow(t *testing.T) {
	registry, store, clock := newTestRegistry(t)
	createHost(t, store, "host-1")
	ctx := context.Background()

	mustIngest := func(cpu float64) *TelemetryResult {
		t.Helper()
		result, err := registry.IngestTelemetry(ctx, "host-1", TelemetryReport{
			Metrics: []models.Metric{{EntityType: "host", CPUPercent: cpu, CollectedAt: clock.Now()}},
		})
		if err != nil {
			t.Fatalf("IngestTelemetry: %v", err)
		}
		return result
	}

	if r := mustIngest(95); r.Mode != models.ModeCritical {
		t.Fatalf("expected CRITICAL after breach, got %+v", r)
	}

	// Two clean samples immediately after: count floor (3) not met yet.
	mustIngest(10)
	if r := mustIngest(10); r.Mode != models.ModeCritical {
		t.Fatalf("expected still CRITICAL before the sample/window floors are met, got %+v", r)
	}

	clock.advance(61 * time.Second)
	result := mustIngest(10) // 3rd consecutive clean sample, past the window
	if result.Mode != models.ModeNormal || result.IntervalSeconds != 60 {
		t.Fatalf("expected recovery to NORMAL/60s, got %+v", result)
	}

	evs, err := store.ListEvents(ctx, hostEvents("host-1"))
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	foundRecovered := false
	for _, e := range evs {
		if e.Type == "monitoring.recovered" && e.Severity == models.SeverityInfo {
			foundRecovered = true
		}
	}
	if !foundRecovered {
		t.Fatalf("expected a monitoring.recovered INFO event, got %+v", evs)
	}
}

func TestIngestTelemetry_ContainerCrashRecordsEventAndMode(t *testing.T) {
	registry, store, _ := newTestRegistry(t)
	createHost(t, store, "host-1")
	ctx := context.Background()

	result, err := registry.IngestTelemetry(ctx, "host-1", TelemetryReport{
		Containers: []ContainerReport{
			{DockerContainerID: "abc123", Name: "web", Status: models.ContainerStatusExited, HealthStatus: models.HealthStatusNone},
		},
	})
	if err != nil {
		t.Fatalf("IngestTelemetry: %v", err)
	}
	if result.Mode != models.ModeCritical {
		t.Fatalf("expected a crashed container to make the batch result CRITICAL, got %+v", result)
	}

	containers, err := store.ListContainersByHost(ctx, "host-1")
	if err != nil {
		t.Fatalf("ListContainersByHost: %v", err)
	}
	if len(containers) != 1 || containers[0].MonitoringMode != models.ModeCritical {
		t.Fatalf("expected the container's persisted MonitoringMode to be CRITICAL, got %+v", containers)
	}

	evs, err := store.ListEvents(ctx, hostEvents("host-1"))
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(evs) != 1 || evs[0].Type != "monitoring.container_crashed" {
		t.Fatalf("expected one container_crashed event, got %+v", evs)
	}
}

// TestIngestTelemetry_FirstSeenRestartCountIsNotATransition is a
// regression test for a real bug: a container discovered for the first
// time with a nonzero historical RestartCount was being compared against a
// hardcoded 0 baseline, so every freshly discovered container with any
// restart history at all was incorrectly flagged as "just restarted".
func TestIngestTelemetry_FirstSeenRestartCountIsNotATransition(t *testing.T) {
	registry, store, _ := newTestRegistry(t)
	createHost(t, store, "host-1")
	ctx := context.Background()

	result, err := registry.IngestTelemetry(ctx, "host-1", TelemetryReport{
		Containers: []ContainerReport{
			{DockerContainerID: "abc123", Name: "web", Status: models.ContainerStatusRunning,
				HealthStatus: models.HealthStatusHealthy, RestartCount: 5},
		},
	})
	if err != nil {
		t.Fatalf("IngestTelemetry: %v", err)
	}
	if result.Mode != models.ModeNormal {
		t.Fatalf("a container's first-ever sighting must not itself be treated as a restart event, got %+v", result)
	}

	evs, err := store.ListEvents(ctx, hostEvents("host-1"))
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(evs) != 0 {
		t.Fatalf("expected no events on first sighting, got %+v", evs)
	}

	// Now the restart count actually increases on a subsequent batch —
	// THIS must be flagged.
	result, err = registry.IngestTelemetry(ctx, "host-1", TelemetryReport{
		Containers: []ContainerReport{
			{DockerContainerID: "abc123", Name: "web", Status: models.ContainerStatusRunning,
				HealthStatus: models.HealthStatusHealthy, RestartCount: 6},
		},
	})
	if err != nil {
		t.Fatalf("IngestTelemetry (2nd batch): %v", err)
	}
	if result.Mode != models.ModeCritical {
		t.Fatalf("expected a genuine restart-count increase to trip CRITICAL, got %+v", result)
	}
}
