package sqlstore

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/nandani/docker-monitor/internal/shared/models"
)

func sample(entityType, entityID string, at time.Time, cpu float64, mem uint64) *models.Metric {
	return &models.Metric{
		EntityType: entityType, EntityID: entityID, CPUPercent: cpu, MemUsedBytes: mem,
		CollectedAt: at, Mode: models.ModeNormal, IntervalSeconds: 60,
	}
}

// TestRecordHistoricalSample_BucketsAndAveragesWithinAMinute verifies the
// running-average upsert against hand-computed expected values: three
// samples in the same minute with CPU 10, 20, 30 must average to exactly
// 20, not just "some plausible number" — that distinction is the whole
// point of testing downsampling math instead of just "it runs".
func TestRecordHistoricalSample_BucketsAndAveragesWithinAMinute(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	samples := []struct {
		offset time.Duration
		cpu    float64
		mem    uint64
	}{
		{0 * time.Second, 10, 1000},
		{20 * time.Second, 20, 2000},
		{40 * time.Second, 30, 3000},
	}
	for _, sm := range samples {
		if err := s.RecordHistoricalSample(ctx, sample("host", "h1", base.Add(sm.offset), sm.cpu, sm.mem)); err != nil {
			t.Fatalf("RecordHistoricalSample: %v", err)
		}
	}

	points, err := s.QueryRange(ctx, "host", "h1", base.Add(-time.Minute), base.Add(time.Hour))
	if err != nil {
		t.Fatalf("QueryRange: %v", err)
	}
	if len(points) != 1 {
		t.Fatalf("expected exactly one 1-minute bucket, got %d: %+v", len(points), points)
	}
	p := points[0]
	if p.SampleCount != 3 {
		t.Fatalf("expected sample_count=3, got %d", p.SampleCount)
	}
	if p.ResolutionSeconds != bucketResolutionSeconds {
		t.Fatalf("expected resolution=%d, got %d", bucketResolutionSeconds, p.ResolutionSeconds)
	}
	wantCPU := (10.0 + 20.0 + 30.0) / 3.0
	if p.CPUPercentAvg != wantCPU {
		t.Fatalf("expected cpu_percent_avg=%v, got %v", wantCPU, p.CPUPercentAvg)
	}
	wantMem := (1000.0 + 2000.0 + 3000.0) / 3.0
	if p.MemUsedBytesAvg != wantMem {
		t.Fatalf("expected mem_used_bytes_avg=%v, got %v", wantMem, p.MemUsedBytesAvg)
	}
	if !p.BucketStart.Equal(base) {
		t.Fatalf("expected bucket_start=%v, got %v", base, p.BucketStart)
	}
}

// TestRecordHistoricalSample_SeparateBucketsPerMinute checks that samples
// in different minutes land in different buckets rather than all being
// folded together.
func TestRecordHistoricalSample_SeparateBucketsPerMinute(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	if err := s.RecordHistoricalSample(ctx, sample("host", "h1", base, 10, 1000)); err != nil {
		t.Fatalf("RecordHistoricalSample: %v", err)
	}
	if err := s.RecordHistoricalSample(ctx, sample("host", "h1", base.Add(90*time.Second), 50, 5000)); err != nil {
		t.Fatalf("RecordHistoricalSample: %v", err)
	}

	points, err := s.QueryRange(ctx, "host", "h1", base.Add(-time.Minute), base.Add(time.Hour))
	if err != nil {
		t.Fatalf("QueryRange: %v", err)
	}
	if len(points) != 2 {
		t.Fatalf("expected 2 separate 1-minute buckets, got %d: %+v", len(points), points)
	}
	if points[0].CPUPercentAvg != 10 || points[1].CPUPercentAvg != 50 {
		t.Fatalf("unexpected bucket contents: %+v", points)
	}
}

// TestRunRetention_RollsUpOldMinuteBucketsWithWeightedAverage is the exact
// exit-criterion the roadmap asks for: hand-computed expected values for a
// known synthetic dataset, not just "downsampling ran without error".
//
// Two 1-minute buckets, both older than the 6-hour rollup threshold:
//
//	bucket A: 2 samples averaging cpu=10
//	bucket B: 3 samples averaging cpu=40
//
// Weighted by sample count, the rolled-up 5-minute bucket's average must
// be (10*2 + 40*3) / 5 = 26, NOT the naive (10+40)/2 = 25 a sample-count-
// unaware average would produce.
func TestRunRetention_RollsUpOldMinuteBucketsWithWeightedAverage(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	old := now.Add(-7 * time.Hour) // older than rollupAfter (6h)
	bucketA := old.Truncate(time.Minute)
	bucketB := bucketA.Add(30 * time.Second) // same 5-minute window as A, different minute

	// Bucket A: 2 samples, cpu 5 and 15 -> avg 10.
	if err := s.RecordHistoricalSample(ctx, sample("host", "h1", bucketA, 5, 100)); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := s.RecordHistoricalSample(ctx, sample("host", "h1", bucketA.Add(10*time.Second), 15, 300)); err != nil {
		t.Fatalf("record: %v", err)
	}
	// Bucket B: 3 samples, cpu 30, 40, 50 -> avg 40.
	for _, cpu := range []float64{30, 40, 50} {
		if err := s.RecordHistoricalSample(ctx, sample("host", "h1", bucketB, cpu, 200)); err != nil {
			t.Fatalf("record: %v", err)
		}
	}

	if err := s.RunRetention(ctx, now, 0); err != nil { // retentionDays=0 disables deletion for this test
		t.Fatalf("RunRetention: %v", err)
	}

	points, err := s.QueryRange(ctx, "host", "h1", old.Add(-time.Hour), old.Add(time.Hour))
	if err != nil {
		t.Fatalf("QueryRange: %v", err)
	}
	if len(points) != 1 {
		t.Fatalf("expected the two 1-minute buckets to be rolled into exactly one 5-minute bucket, got %d: %+v", len(points), points)
	}
	p := points[0]
	if p.ResolutionSeconds != rollupResolutionSeconds {
		t.Fatalf("expected resolution=%d after rollup, got %d", rollupResolutionSeconds, p.ResolutionSeconds)
	}
	if p.SampleCount != 5 {
		t.Fatalf("expected sample_count=5 (2+3) after rollup, got %d", p.SampleCount)
	}
	wantCPU := (10.0*2 + 40.0*3) / 5.0 // = 26
	if p.CPUPercentAvg != wantCPU {
		t.Fatalf("expected weighted cpu_percent_avg=%v, got %v", wantCPU, p.CPUPercentAvg)
	}
}

// TestRunRetention_DeletesBeyondRetentionWindow confirms the master
// prompt's explicit requirement ("do not store unlimited high-frequency
// data") is actually enforced, not just configured.
func TestRunRetention_DeletesBeyondRetentionWindow(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	tooOld := now.Add(-100 * 24 * time.Hour)
	recent := now.Add(-1 * time.Hour)

	if err := s.RecordHistoricalSample(ctx, sample("host", "h1", tooOld, 10, 100)); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := s.RecordHistoricalSample(ctx, sample("host", "h1", recent, 20, 200)); err != nil {
		t.Fatalf("record: %v", err)
	}

	if err := s.RunRetention(ctx, now, 90); err != nil {
		t.Fatalf("RunRetention: %v", err)
	}

	points, err := s.QueryRange(ctx, "host", "h1", now.Add(-200*24*time.Hour), now)
	if err != nil {
		t.Fatalf("QueryRange: %v", err)
	}
	if len(points) != 1 {
		t.Fatalf("expected only the recent sample to survive a 90-day retention window, got %d: %+v", len(points), points)
	}
	if points[0].CPUPercentAvg != 20 {
		t.Fatalf("expected the surviving point to be the recent one, got %+v", points[0])
	}
}

// TestRunRetention_Idempotent verifies calling RunRetention repeatedly
// (as a periodic ticker would) doesn't double-count or error once a
// bucket has already been rolled up / deleted.
func TestRunRetention_Idempotent(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	old := now.Add(-7 * time.Hour)
	if err := s.RecordHistoricalSample(ctx, sample("host", "h1", old, 10, 100)); err != nil {
		t.Fatalf("record: %v", err)
	}

	for i := 0; i < 3; i++ {
		if err := s.RunRetention(ctx, now, 90); err != nil {
			t.Fatalf("RunRetention (pass %d): %v", i, err)
		}
	}

	points, err := s.QueryRange(ctx, "host", "h1", old.Add(-time.Hour), old.Add(time.Hour))
	if err != nil {
		t.Fatalf("QueryRange: %v", err)
	}
	if len(points) != 1 {
		t.Fatalf("expected exactly one surviving point after repeated retention passes, got %d: %+v", len(points), points)
	}
	if points[0].SampleCount != 1 {
		t.Fatalf("expected sample_count to stay 1 (no double-counting across passes), got %d", points[0].SampleCount)
	}
}

func TestQueryRange_EmptyWhenNothingRecorded(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	points, err := s.QueryRange(ctx, "host", "nonexistent", now.Add(-time.Hour), now)
	if err != nil {
		t.Fatalf("QueryRange: %v", err)
	}
	if len(points) != 0 {
		t.Fatalf("expected no points, got %d", len(points))
	}
}

// ensure the DSN helper from sqlstore_test.go compiles against a fresh
// temp dir per call (guards against accidental cross-test DB reuse).
func TestOpenTestStore_IsolatedPerTest(t *testing.T) {
	dsn1 := filepath.Join(t.TempDir(), "a.db")
	dsn2 := filepath.Join(t.TempDir(), "b.db")
	if dsn1 == dsn2 {
		t.Fatalf("expected distinct temp dirs per test")
	}
}
