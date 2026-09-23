package agentregistry

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/nandani/docker-monitor/internal/controlplane/events"
	"github.com/nandani/docker-monitor/internal/controlplane/monitoring"
	"github.com/nandani/docker-monitor/internal/controlplane/pki"
	"github.com/nandani/docker-monitor/internal/controlplane/storage/sqlstore"
	"github.com/nandani/docker-monitor/internal/shared/models"
)

// TestIngestTelemetry_HistoricalStoreActuallyPersists is an end-to-end
// regression test for a gap that unit tests alone don't catch: sqlstore.Store
// implementing HistoricalMetricStore correctly (verified by
// historical_metric_test.go) says nothing about whether anything in the
// running application actually *calls* SetHistoricalStore and routes real
// telemetry into it. This test exercises the real wiring end-to-end — a real
// *sqlstore.Store backing both storage.Store and Registry's historical
// store — and confirms a sample landed by reading it back via QueryRange,
// not just by checking that RecordHistoricalSample didn't error.
func TestIngestTelemetry_HistoricalStoreActuallyPersists(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "wiring.db")
	ctx := context.Background()

	store, err := sqlstore.Open(ctx, "sqlite", dsn)
	if err != nil {
		t.Fatalf("sqlstore.Open: %v", err)
	}
	defer store.Close()

	recorder := events.NewRecorder(store)
	ca, err := pki.GenerateCA("test-ca", time.Hour)
	if err != nil {
		t.Fatalf("GenerateCA: %v", err)
	}
	engine := monitoring.NewEngine(monitoring.DefaultThresholds())
	registry := New(store, ca, recorder, engine)

	// This is the line main.go must execute when DM_HISTORICAL_METRICS_ENABLED
	// is true; the test's entire point is to fail if that wiring is ever
	// missing or silently reverted.
	registry.SetHistoricalStore(store)

	host := &models.Host{ID: "host-1", Name: "host-1", Kind: models.HostKindLocal}
	if err := store.CreateHost(ctx, host); err != nil {
		t.Fatalf("CreateHost: %v", err)
	}

	now := time.Now().UTC()
	report := TelemetryReport{
		Metrics: []models.Metric{
			{EntityType: "host", CPUPercent: 42.5, MemUsedBytes: 123456, CollectedAt: now},
		},
	}
	if _, err := registry.IngestTelemetry(ctx, "host-1", report); err != nil {
		t.Fatalf("IngestTelemetry: %v", err)
	}

	points, err := store.QueryRange(ctx, "host", "host-1", now.Add(-time.Minute), now.Add(time.Minute))
	if err != nil {
		t.Fatalf("QueryRange: %v", err)
	}
	if len(points) != 1 {
		t.Fatalf("expected IngestTelemetry to have written exactly one historical bucket via the wired store, got %d", len(points))
	}
	if points[0].CPUPercentAvg != 42.5 {
		t.Fatalf("expected the historical bucket to reflect the ingested sample (cpu=42.5), got %+v", points[0])
	}
}

// TestIngestTelemetry_WithoutHistoricalStoreDoesNotPersist is the negative
// case: confirms historical persistence is genuinely opt-in — a Registry
// that never had SetHistoricalStore called must not write to metric_history,
// so the "disabled by default" claim in SetHistoricalStore's doc comment is
// actually true of the wired system, not just of the unwired method.
func TestIngestTelemetry_WithoutHistoricalStoreDoesNotPersist(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "unwired.db")
	ctx := context.Background()

	store, err := sqlstore.Open(ctx, "sqlite", dsn)
	if err != nil {
		t.Fatalf("sqlstore.Open: %v", err)
	}
	defer store.Close()

	recorder := events.NewRecorder(store)
	ca, err := pki.GenerateCA("test-ca", time.Hour)
	if err != nil {
		t.Fatalf("GenerateCA: %v", err)
	}
	engine := monitoring.NewEngine(monitoring.DefaultThresholds())
	registry := New(store, ca, recorder, engine) // note: SetHistoricalStore NOT called

	host := &models.Host{ID: "host-1", Name: "host-1", Kind: models.HostKindLocal}
	if err := store.CreateHost(ctx, host); err != nil {
		t.Fatalf("CreateHost: %v", err)
	}

	now := time.Now().UTC()
	report := TelemetryReport{
		Metrics: []models.Metric{
			{EntityType: "host", CPUPercent: 10, MemUsedBytes: 1, CollectedAt: now},
		},
	}
	if _, err := registry.IngestTelemetry(ctx, "host-1", report); err != nil {
		t.Fatalf("IngestTelemetry: %v", err)
	}

	points, err := store.QueryRange(ctx, "host", "host-1", now.Add(-time.Minute), now.Add(time.Minute))
	if err != nil {
		t.Fatalf("QueryRange: %v", err)
	}
	if len(points) != 0 {
		t.Fatalf("expected no historical data without SetHistoricalStore, got %d points", len(points))
	}
}
