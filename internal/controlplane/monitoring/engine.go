// Package monitoring implements the adaptive monitoring state machine
// described in ARCHITECTURE.md §F. It is deliberately storage- and
// transport-agnostic: EvaluateMetric/EvaluateContainerHealth are pure
// functions of (current state, new sample) plus a clock, so the entire FSM
// is unit-testable without a database, an agent, or real time passing
// (see NewEngineWithClock).
//
// One Engine instance tracks independent state per (EntityKind, entityID)
// pair — a host and one of its containers can be in different effective
// modes simultaneously (§F.5), and nothing here creates duplicate
// collectors for an entity that's both critical and manually viewed (§49):
// EffectiveMode always returns a single answer per entity, and it's the
// caller's job to drive one collector per entity off of that answer.
package monitoring

import (
	"sync"
	"time"

	"github.com/nandani/docker-monitor/internal/shared/models"
)

// EntityKind distinguishes hosts from containers in the FSM's state map —
// the same ID string could otherwise collide between a host and container
// keyed independently.
type EntityKind string

const (
	EntityHost      EntityKind = "host"
	EntityContainer EntityKind = "container"
)

// ThresholdConfig holds the tunable parameters for critical-condition
// detection and hysteresis-based recovery (ARCHITECTURE.md §3-4, §39).
type ThresholdConfig struct {
	CPUCriticalPercent float64
	CPURecoveryPercent float64
	MemCriticalPercent float64
	MemRecoveryPercent float64

	// HysteresisWindow and MinConsecutiveNormalSamples together prevent
	// 60s -> 10s -> 60s -> 10s flapping from small metric fluctuations
	// (§4): recovery requires BOTH at least MinConsecutiveNormalSamples
	// consecutive good samples AND that the elapsed time since the first
	// of those samples is >= HysteresisWindow. Either alone is
	// insufficient — see the engine tests for the exact boundary cases
	// this is meant to cover.
	HysteresisWindow            time.Duration
	MinConsecutiveNormalSamples int

	NormalIntervalSeconds   int
	CriticalIntervalSeconds int
	ManualIntervalSeconds   int
}

// DefaultThresholds returns the documented safe defaults (§39).
func DefaultThresholds() ThresholdConfig {
	return ThresholdConfig{
		CPUCriticalPercent:          90,
		CPURecoveryPercent:          75,
		MemCriticalPercent:          90,
		MemRecoveryPercent:          75,
		HysteresisWindow:            60 * time.Second,
		MinConsecutiveNormalSamples: 3,
		NormalIntervalSeconds:       60,
		CriticalIntervalSeconds:     10,
		ManualIntervalSeconds:       2,
	}
}

// Clock is satisfied by time.Time.Now and by tests' fake clocks, so
// hysteresis boundary conditions can be driven deterministically instead
// of sleeping through a real 60-second window.
type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// TransitionEvent describes one FSM state change, suitable for feeding
// directly into the event log/alert engine (later milestones).
type TransitionEvent struct {
	EntityKind  EntityKind
	EntityID    string
	EnteredMode models.MonitoringMode
	Reason      string // "cpu_threshold" | "mem_threshold" | "recovered" |
	// "container_crashed" | "container_unhealthy" | "container_restarted"
	At time.Time
}

type entityState struct {
	criticalActive bool

	// Recovery streak tracking (§F.4): count resets to 0 the moment a
	// sample isn't clean (either still critical, or in the deadband
	// between the recovery and critical thresholds), so a flappy metric
	// can't accumulate partial credit across a bad sample.
	goodStreakCount int
	goodStreakStart time.Time

	manualViewers int
}

// Engine holds per-entity FSM state. Safe for concurrent use.
type Engine struct {
	cfg   ThresholdConfig
	clock Clock

	mu    sync.Mutex
	state map[string]*entityState
}

// NewEngine builds an Engine using the real wall clock.
func NewEngine(cfg ThresholdConfig) *Engine {
	return NewEngineWithClock(cfg, realClock{})
}

// NewEngineWithClock builds an Engine driven by the given clock — used by
// tests to control elapsed time deterministically.
func NewEngineWithClock(cfg ThresholdConfig, clock Clock) *Engine {
	return &Engine{cfg: cfg, clock: clock, state: make(map[string]*entityState)}
}

func stateKey(kind EntityKind, id string) string {
	return string(kind) + "/" + id
}

func (e *Engine) getOrCreate(kind EntityKind, id string) *entityState {
	key := stateKey(kind, id)
	st, ok := e.state[key]
	if !ok {
		st = &entityState{}
		e.state[key] = st
	}
	return st
}

// EffectiveMode returns an entity's current monitoring mode. An entity
// never evaluated before is NORMAL — that's the FSM's initial state
// (§F.1), not an error.
func (e *Engine) EffectiveMode(kind EntityKind, id string) models.MonitoringMode {
	e.mu.Lock()
	defer e.mu.Unlock()
	st, ok := e.state[stateKey(kind, id)]
	if !ok {
		return models.ModeNormal
	}
	switch {
	case st.manualViewers > 0:
		// MANUAL_REALTIME always wins while a viewer is open (§F.3), even
		// over an active critical condition — the user is already getting
		// the fastest cadence available.
		return models.ModeManualRealtime
	case st.criticalActive:
		return models.ModeCritical
	default:
		return models.ModeNormal
	}
}

func memPercent(m models.Metric) float64 {
	if m.MemLimitBytes == 0 {
		return 0
	}
	return float64(m.MemUsedBytes) / float64(m.MemLimitBytes) * 100
}

// EvaluateMetric feeds one metric sample into the FSM for (kind, id) and
// returns whether it caused a transition, plus the event describing it (nil
// if no transition occurred). This is the threshold + hysteresis logic
// from ARCHITECTURE.md §3-4.
func (e *Engine) EvaluateMetric(kind EntityKind, id string, m models.Metric) (bool, *TransitionEvent) {
	e.mu.Lock()
	defer e.mu.Unlock()

	st := e.getOrCreate(kind, id)
	now := e.clock.Now()
	cpuPct := m.CPUPercent
	memPct := memPercent(m)

	if !st.criticalActive {
		switch {
		case cpuPct >= e.cfg.CPUCriticalPercent:
			st.criticalActive = true
			st.goodStreakCount = 0
			return true, &TransitionEvent{EntityKind: kind, EntityID: id, EnteredMode: models.ModeCritical, Reason: "cpu_threshold", At: now}
		case memPct >= e.cfg.MemCriticalPercent:
			st.criticalActive = true
			st.goodStreakCount = 0
			return true, &TransitionEvent{EntityKind: kind, EntityID: id, EnteredMode: models.ModeCritical, Reason: "mem_threshold", At: now}
		default:
			return false, nil
		}
	}

	// Already CRITICAL: evaluate recovery. A "clean" sample must be below
	// the (lower) recovery threshold on both dimensions — the gap between
	// recovery and critical thresholds is a deliberate deadband so a
	// metric hovering just under the critical line doesn't count as
	// recovering (§F.4).
	isClean := cpuPct < e.cfg.CPURecoveryPercent && memPct < e.cfg.MemRecoveryPercent
	if !isClean {
		st.goodStreakCount = 0
		st.goodStreakStart = time.Time{}
		return false, nil
	}

	if st.goodStreakCount == 0 {
		st.goodStreakStart = now
	}
	st.goodStreakCount++

	if st.goodStreakCount >= e.cfg.MinConsecutiveNormalSamples && now.Sub(st.goodStreakStart) >= e.cfg.HysteresisWindow {
		st.criticalActive = false
		st.goodStreakCount = 0
		st.goodStreakStart = time.Time{}
		return true, &TransitionEvent{EntityKind: kind, EntityID: id, EnteredMode: models.ModeNormal, Reason: "recovered", At: now}
	}
	return false, nil
}

// EvaluateContainerHealth feeds a container's inspect-derived state into
// the FSM (crash/unhealthy/repeated-restart detection, §3). previousRestartCount
// is supplied by the caller (the collector tracks the last-seen count per
// container) rather than by the Engine, since the Engine only holds FSM
// mode state, not container inventory.
func (e *Engine) EvaluateContainerHealth(id string, status models.ContainerStatus, health models.HealthStatus, restartCount, previousRestartCount int) (bool, *TransitionEvent) {
	e.mu.Lock()
	defer e.mu.Unlock()

	st := e.getOrCreate(EntityContainer, id)
	if st.criticalActive {
		return false, nil
	}
	now := e.clock.Now()

	switch {
	case status == models.ContainerStatusExited:
		st.criticalActive = true
		return true, &TransitionEvent{EntityKind: EntityContainer, EntityID: id, EnteredMode: models.ModeCritical, Reason: "container_crashed", At: now}
	case health == models.HealthStatusUnhealthy:
		st.criticalActive = true
		return true, &TransitionEvent{EntityKind: EntityContainer, EntityID: id, EnteredMode: models.ModeCritical, Reason: "container_unhealthy", At: now}
	case restartCount > previousRestartCount:
		st.criticalActive = true
		return true, &TransitionEvent{EntityKind: EntityContainer, EntityID: id, EnteredMode: models.ModeCritical, Reason: "container_restarted", At: now}
	default:
		return false, nil
	}
}

// ManualViewerOpen registers that a user opened live inspection for this
// entity (§5). Multiple simultaneous viewers are coalesced into a single
// counter — the Nth viewer opening/closing doesn't create or destroy a
// collector, it just adjusts the refcount (§49: no duplicate collectors).
func (e *Engine) ManualViewerOpen(kind EntityKind, id string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	st := e.getOrCreate(kind, id)
	st.manualViewers++
}

// ManualViewerClose unregisters one viewer. The count never goes negative
// even under mismatched open/close calls.
func (e *Engine) ManualViewerClose(kind EntityKind, id string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	st := e.getOrCreate(kind, id)
	if st.manualViewers > 0 {
		st.manualViewers--
	}
}

// IntervalSeconds maps a mode to its configured polling interval (§39).
func (e *Engine) IntervalSeconds(mode models.MonitoringMode) int {
	switch mode {
	case models.ModeCritical:
		return e.cfg.CriticalIntervalSeconds
	case models.ModeManualRealtime:
		return e.cfg.ManualIntervalSeconds
	default:
		return e.cfg.NormalIntervalSeconds
	}
}

// modeRank orders modes by polling frequency, fastest last, so FastestMode
// can reduce over a slice with a simple max-by-rank.
var modeRank = map[models.MonitoringMode]int{
	models.ModeNormal:         0,
	models.ModeCritical:       1,
	models.ModeManualRealtime: 2,
}

// FastestMode returns whichever of the given modes polls most frequently
// (MANUAL_REALTIME > CRITICAL > NORMAL). Used when a single collector must
// serve an entity that's simultaneously critical and manually viewed, or
// when a host's effective rate must account for its fastest container
// (§49, §6 "the appropriate higher-frequency mode"). An empty input
// defaults to NORMAL — the least surprising answer for "no constraints".
func FastestMode(modes ...models.MonitoringMode) models.MonitoringMode {
	if len(modes) == 0 {
		return models.ModeNormal
	}
	best := modes[0]
	for _, m := range modes[1:] {
		if modeRank[m] > modeRank[best] {
			best = m
		}
	}
	return best
}
