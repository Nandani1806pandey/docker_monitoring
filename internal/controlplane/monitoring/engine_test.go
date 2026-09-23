package monitoring

import (
	"testing"
	"time"

	"github.com/nandani/docker-monitor/internal/shared/models"
)

// fakeClock lets tests drive the hysteresis window deterministically
// instead of sleeping through a real 60-second default.
type fakeClock struct{ now time.Time }

func (f *fakeClock) Now() time.Time          { return f.now }
func (f *fakeClock) advance(d time.Duration) { f.now = f.now.Add(d) }

func testConfig() ThresholdConfig {
	cfg := DefaultThresholds()
	cfg.HysteresisWindow = 60 * time.Second
	cfg.MinConsecutiveNormalSamples = 3
	return cfg
}

func metricAt(cpu, memPercent float64) models.Metric {
	return models.Metric{CPUPercent: cpu, MemUsedBytes: uint64(memPercent), MemLimitBytes: 100}
}

func TestNormalToCritical_CPUThreshold(t *testing.T) {
	clock := &fakeClock{now: time.Now()}
	e := NewEngineWithClock(testConfig(), clock)

	if mode := e.EffectiveMode(EntityContainer, "c1"); mode != models.ModeNormal {
		t.Fatalf("expected initial mode NORMAL, got %s", mode)
	}

	transitioned, ev := e.EvaluateMetric(EntityContainer, "c1", metricAt(95, 10))
	if !transitioned {
		t.Fatalf("expected a transition on CPU breach")
	}
	if ev == nil || ev.EnteredMode != models.ModeCritical || ev.Reason != "cpu_threshold" {
		t.Fatalf("unexpected event: %+v", ev)
	}
	if mode := e.EffectiveMode(EntityContainer, "c1"); mode != models.ModeCritical {
		t.Fatalf("expected CRITICAL after breach, got %s", mode)
	}

	// A second breach sample is not itself a new transition.
	transitioned, ev = e.EvaluateMetric(EntityContainer, "c1", metricAt(96, 10))
	if transitioned || ev != nil {
		t.Fatalf("expected no re-transition while already critical, got %v %+v", transitioned, ev)
	}
}

func TestNormalToCritical_MemThreshold(t *testing.T) {
	clock := &fakeClock{now: time.Now()}
	e := NewEngineWithClock(testConfig(), clock)

	// memPercent = 95/100 = 95% >= 90% critical threshold; CPU low.
	m := models.Metric{CPUPercent: 5, MemUsedBytes: 95, MemLimitBytes: 100}
	transitioned, ev := e.EvaluateMetric(EntityHost, "h1", m)
	if !transitioned || ev.Reason != "mem_threshold" {
		t.Fatalf("expected mem_threshold transition, got %v %+v", transitioned, ev)
	}
}

// TestSingleGoodSampleNeverRecovers is the exact scenario §F.4 calls out by
// name: "A single good sample never triggers recovery by itself."
func TestSingleGoodSampleNeverRecovers(t *testing.T) {
	clock := &fakeClock{now: time.Now()}
	e := NewEngineWithClock(testConfig(), clock)

	e.EvaluateMetric(EntityContainer, "c1", metricAt(95, 10)) // -> CRITICAL

	clock.advance(70 * time.Second) // well past the 60s window
	transitioned, ev := e.EvaluateMetric(EntityContainer, "c1", metricAt(10, 10))
	if transitioned || ev != nil {
		t.Fatalf("a single good sample must never recover on its own, got %v %+v", transitioned, ev)
	}
	if mode := e.EffectiveMode(EntityContainer, "c1"); mode != models.ModeCritical {
		t.Fatalf("expected still CRITICAL after one good sample, got %s", mode)
	}
}

// TestRecovery_BothConditionsRequired shows time-elapsed alone is
// insufficient (2 samples spanning well over 60s still doesn't recover —
// the 3-sample floor isn't met); the 3rd consecutive good sample recovers it.
func TestRecovery_BothConditionsRequired(t *testing.T) {
	clock := &fakeClock{now: time.Now()}
	e := NewEngineWithClock(testConfig(), clock)

	e.EvaluateMetric(EntityContainer, "c1", metricAt(95, 10)) // -> CRITICAL

	if transitioned, _ := e.EvaluateMetric(EntityContainer, "c1", metricAt(10, 10)); transitioned {
		t.Fatalf("sample 1 of 3 must not recover")
	}
	// 65s have now elapsed since the first good (recovery-window-starting)
	// sample — well past the 60s window — but only 2 consecutive samples.
	clock.advance(65 * time.Second)
	if transitioned, _ := e.EvaluateMetric(EntityContainer, "c1", metricAt(10, 10)); transitioned {
		t.Fatalf("sample 2 of 3 must not recover even though >60s has elapsed (count floor not met)")
	}
	if mode := e.EffectiveMode(EntityContainer, "c1"); mode != models.ModeCritical {
		t.Fatalf("expected still CRITICAL at the 2-sample boundary, got %s", mode)
	}

	clock.advance(1 * time.Second)
	transitioned, ev := e.EvaluateMetric(EntityContainer, "c1", metricAt(10, 10))
	if !transitioned || ev.EnteredMode != models.ModeNormal {
		t.Fatalf("expected recovery on the 3rd consecutive good sample past the window, got %v %+v", transitioned, ev)
	}
	if mode := e.EffectiveMode(EntityContainer, "c1"); mode != models.ModeNormal {
		t.Fatalf("expected NORMAL after recovery, got %s", mode)
	}
}

// TestDeadbandSampleResetsRecoveryProgress covers the anti-flap deadband
// (§F.4): a sample that's below critical but NOT below the (lower)
// recovery threshold must reset progress, not count toward recovery.
func TestDeadbandSampleResetsRecoveryProgress(t *testing.T) {
	clock := &fakeClock{now: time.Now()}
	e := NewEngineWithClock(testConfig(), clock)
	e.EvaluateMetric(EntityContainer, "c1", metricAt(95, 10)) // -> CRITICAL, critical=90 recovery=75

	clock.advance(60 * time.Second)
	e.EvaluateMetric(EntityContainer, "c1", metricAt(10, 10)) // sample 1: clean recovery reading
	e.EvaluateMetric(EntityContainer, "c1", metricAt(10, 10)) // sample 2: clean recovery reading

	// Deadband: 80% is below the 90% critical threshold but NOT below the
	// 75% recovery threshold — must reset the streak entirely.
	clock.advance(1 * time.Second)
	transitioned, _ := e.EvaluateMetric(EntityContainer, "c1", metricAt(80, 10))
	if transitioned {
		t.Fatalf("a deadband sample must never itself cause recovery")
	}

	// Two more clean samples immediately after: this must NOT be enough
	// to recover, since the deadband sample should have reset the count.
	clock.advance(1 * time.Second)
	e.EvaluateMetric(EntityContainer, "c1", metricAt(10, 10))
	clock.advance(1 * time.Second)
	transitioned, _ = e.EvaluateMetric(EntityContainer, "c1", metricAt(10, 10))
	if transitioned {
		t.Fatalf("recovery streak should have been reset by the deadband sample")
	}
	if mode := e.EffectiveMode(EntityContainer, "c1"); mode != models.ModeCritical {
		t.Fatalf("expected still CRITICAL, got %s", mode)
	}
}

func TestManualViewer_OverridesNormal(t *testing.T) {
	e := NewEngine(testConfig())
	e.ManualViewerOpen(EntityContainer, "c1")
	if mode := e.EffectiveMode(EntityContainer, "c1"); mode != models.ModeManualRealtime {
		t.Fatalf("expected MANUAL_REALTIME, got %s", mode)
	}
	e.ManualViewerClose(EntityContainer, "c1")
	if mode := e.EffectiveMode(EntityContainer, "c1"); mode != models.ModeNormal {
		t.Fatalf("expected NORMAL after viewer closes with no critical condition, got %s", mode)
	}
}

// TestManualViewer_FallsBackToCriticalNotNormal is §F.3's
// "MANUAL_REALTIME -> {CRITICAL | NORMAL}: falls back to CRITICAL if still
// active, else NORMAL" — the case that's easy to get wrong by always
// falling back to NORMAL.
func TestManualViewer_FallsBackToCriticalNotNormal(t *testing.T) {
	e := NewEngine(testConfig())
	e.EvaluateMetric(EntityContainer, "c1", metricAt(95, 10)) // critical_active = true

	e.ManualViewerOpen(EntityContainer, "c1")
	if mode := e.EffectiveMode(EntityContainer, "c1"); mode != models.ModeManualRealtime {
		t.Fatalf("expected MANUAL_REALTIME to win over CRITICAL, got %s", mode)
	}

	e.ManualViewerClose(EntityContainer, "c1")
	if mode := e.EffectiveMode(EntityContainer, "c1"); mode != models.ModeCritical {
		t.Fatalf("expected fallback to CRITICAL (still active), not NORMAL, got %s", mode)
	}
}

func TestManualViewerClose_NeverGoesNegative(t *testing.T) {
	e := NewEngine(testConfig())
	e.ManualViewerClose(EntityContainer, "c1") // close with none open
	e.ManualViewerClose(EntityContainer, "c1")
	e.ManualViewerOpen(EntityContainer, "c1")
	e.ManualViewerClose(EntityContainer, "c1")
	if mode := e.EffectiveMode(EntityContainer, "c1"); mode != models.ModeNormal {
		t.Fatalf("expected NORMAL, viewer count must not have gone negative: got %s", mode)
	}
}

// TestCombinedCondition_IndependentEntities is exactly §F.5's example:
// Host A critical + Container A on Host A manually opened are independent
// entities with independent effective modes.
func TestCombinedCondition_IndependentEntities(t *testing.T) {
	e := NewEngine(testConfig())
	e.EvaluateMetric(EntityHost, "host-A", metricAt(95, 10)) // host critical
	e.ManualViewerOpen(EntityContainer, "container-A")       // container manually opened

	if mode := e.EffectiveMode(EntityHost, "host-A"); mode != models.ModeCritical {
		t.Fatalf("expected host-A CRITICAL, got %s", mode)
	}
	if mode := e.EffectiveMode(EntityContainer, "container-A"); mode != models.ModeManualRealtime {
		t.Fatalf("expected container-A MANUAL_REALTIME, got %s", mode)
	}
}

func TestEvaluateContainerHealth_Crash(t *testing.T) {
	e := NewEngine(testConfig())
	transitioned, ev := e.EvaluateContainerHealth("c1", models.ContainerStatusExited, models.HealthStatusNone, 0, 0)
	if !transitioned || ev.Reason != "container_crashed" {
		t.Fatalf("expected container_crashed transition, got %v %+v", transitioned, ev)
	}
}

func TestEvaluateContainerHealth_Unhealthy(t *testing.T) {
	e := NewEngine(testConfig())
	transitioned, ev := e.EvaluateContainerHealth("c1", models.ContainerStatusRunning, models.HealthStatusUnhealthy, 0, 0)
	if !transitioned || ev.Reason != "container_unhealthy" {
		t.Fatalf("expected container_unhealthy transition, got %v %+v", transitioned, ev)
	}
}

func TestEvaluateContainerHealth_RepeatedRestart(t *testing.T) {
	e := NewEngine(testConfig())
	transitioned, ev := e.EvaluateContainerHealth("c1", models.ContainerStatusRunning, models.HealthStatusHealthy, 3, 2)
	if !transitioned || ev.Reason != "container_restarted" {
		t.Fatalf("expected container_restarted transition, got %v %+v", transitioned, ev)
	}
}

func TestIntervalSeconds(t *testing.T) {
	e := NewEngine(testConfig())
	if got := e.IntervalSeconds(models.ModeNormal); got != 60 {
		t.Fatalf("expected 60s for NORMAL, got %d", got)
	}
	if got := e.IntervalSeconds(models.ModeCritical); got != 10 {
		t.Fatalf("expected 10s for CRITICAL, got %d", got)
	}
	if got := e.IntervalSeconds(models.ModeManualRealtime); got != 2 {
		t.Fatalf("expected 2s for MANUAL_REALTIME, got %d", got)
	}
}

func TestFastestMode(t *testing.T) {
	cases := []struct {
		in   []models.MonitoringMode
		want models.MonitoringMode
	}{
		{[]models.MonitoringMode{models.ModeNormal, models.ModeNormal}, models.ModeNormal},
		{[]models.MonitoringMode{models.ModeNormal, models.ModeCritical}, models.ModeCritical},
		{[]models.MonitoringMode{models.ModeCritical, models.ModeManualRealtime}, models.ModeManualRealtime},
		{[]models.MonitoringMode{}, models.ModeNormal},
	}
	for _, c := range cases {
		if got := FastestMode(c.in...); got != c.want {
			t.Fatalf("FastestMode(%v) = %s, want %s", c.in, got, c.want)
		}
	}
}
