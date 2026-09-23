// Package collector runs the agent's periodic discover-collect-report
// cycle against the local Docker daemon and reports through a transport
// client. Its polling interval is mutable at runtime (SetInterval) and it
// exposes a non-blocking CollectNow for manual live-inspection requests
// (ARCHITECTURE.md §5-6) — nothing in this package decides *when* those
// should happen; that's the adaptive monitoring FSM's job (a later
// milestone drives SetInterval/CollectNow from FSM transitions).
package collector

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nandani/docker-monitor/internal/agent/docker"
	"github.com/nandani/docker-monitor/internal/agent/hoststats"
	"github.com/nandani/docker-monitor/internal/agent/transport"
	"github.com/nandani/docker-monitor/internal/shared/models"
)

// Collector runs one host's discover -> collect -> report cycle on a
// mutable ticker.
type Collector struct {
	docker    *docker.Client
	transport *transport.Client
	logger    *slog.Logger

	intervalMu sync.Mutex
	interval   time.Duration
	resetTick  chan struct{}
	collectNow chan struct{}

	lastInterval atomic.Int64 // seconds, for observability only
}

func New(dockerClient *docker.Client, transportClient *transport.Client, initialInterval time.Duration, logger *slog.Logger) *Collector {
	c := &Collector{
		docker:     dockerClient,
		transport:  transportClient,
		logger:     logger,
		interval:   initialInterval,
		resetTick:  make(chan struct{}, 1),
		collectNow: make(chan struct{}, 1),
	}
	c.lastInterval.Store(int64(initialInterval / time.Second))
	return c
}

// SetInterval changes the polling cadence and wakes the loop so the new
// interval takes effect immediately rather than after the current tick
// finishes — this is what lets the FSM move a collector from 60s NORMAL to
// 10s CRITICAL without waiting up to 60s for the change to be noticed.
func (c *Collector) SetInterval(d time.Duration) {
	c.intervalMu.Lock()
	c.interval = d
	c.intervalMu.Unlock()
	c.lastInterval.Store(int64(d / time.Second))
	select {
	case c.resetTick <- struct{}{}:
	default:
	}
}

// CollectNow triggers an immediate out-of-band collection (ARCHITECTURE.md
// §5's "user opens Container Details" case) without blocking the caller or
// creating a second concurrent collection loop — it's coalesced with
// whatever the ticker would do next, satisfying §49's "must not create
// duplicate collectors".
func (c *Collector) CollectNow() {
	select {
	case c.collectNow <- struct{}{}:
	default:
	}
}

func (c *Collector) currentInterval() time.Duration {
	c.intervalMu.Lock()
	defer c.intervalMu.Unlock()
	return c.interval
}

// Run blocks until ctx is cancelled, performing one collect-and-report
// cycle per tick (or immediately on CollectNow/SetInterval).
func (c *Collector) Run(ctx context.Context, hostID string) {
	for {
		timer := time.NewTimer(c.currentInterval())
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-c.resetTick:
			timer.Stop()
			continue // interval changed before it fired; restart with the new one
		case <-c.collectNow:
			timer.Stop()
		case <-timer.C:
		}

		if err := c.collectAndReport(ctx, hostID); err != nil {
			c.logger.Error("collection cycle failed", "err", err, "host_id", hostID)
		}
	}
}

func (c *Collector) collectAndReport(ctx context.Context, hostID string) error {
	mode := modeForInterval(c.currentInterval())
	intervalSeconds := int(c.currentInterval() / time.Second)

	if err := c.docker.Ping(ctx); err != nil {
		return err // daemon unreachable; caller logs, FSM (later) reacts
	}

	summaries, err := c.docker.ListContainers(ctx)
	if err != nil {
		return err
	}

	containers := make([]transport.TelemetryContainer, 0, len(summaries))
	metrics := make([]models.Metric, 0, len(summaries)+1)

	// Host-level stats (ARCHITECTURE.md §9's "Collect host statistics" —
	// previously entirely missing from this agent; only container stats
	// existed). A failure here (e.g. /proc unavailable, permission
	// issues) is logged and skipped for this cycle rather than failing
	// the whole batch — exactly the same non-fatal handling a single
	// flaky container's inspect/stats failure already gets below, so one
	// missing host sample doesn't blind the dashboard to every container
	// on the host.
	if hostSample, err := hoststats.Collect(ctx, hoststats.DefaultPaths); err != nil {
		c.logger.Warn("host stats collection failed, skipping host metric this cycle", "err", err)
	} else {
		metrics = append(metrics, models.Metric{
			EntityType: "host",
			// EntityID intentionally left blank: the control plane's
			// IngestTelemetry always overwrites a "host" metric's
			// EntityID with the hostID from the authenticated mTLS
			// connection (agentregistry.go), the same way it overwrites
			// Mode with the FSM's authoritative verdict — the agent has
			// no reason to know or assert its own control-plane ID.
			CPUPercent:      hostSample.CPUPercent,
			MemUsedBytes:    hostSample.MemUsedBytes,
			MemLimitBytes:   hostSample.MemTotalBytes,
			NetRXBytes:      hostSample.NetRXBytes,
			NetTXBytes:      hostSample.NetTXBytes,
			CollectedAt:     time.Now().UTC(),
			Mode:            mode,
			IntervalSeconds: intervalSeconds,
		})
	}

	for _, s := range summaries {
		insp, err := c.docker.InspectContainer(ctx, s.ID)
		if err != nil {
			c.logger.Warn("inspect failed, skipping container this cycle", "id", s.ID, "err", err)
			continue
		}
		health := models.HealthStatusNone
		if insp.State.Health != nil {
			if insp.State.Health.Status == "healthy" {
				health = models.HealthStatusHealthy
			} else {
				health = models.HealthStatusUnhealthy
			}
		}
		containers = append(containers, transport.TelemetryContainer{
			DockerContainerID: s.ID,
			Name:              firstName(s.Names),
			Image:             s.Image,
			Status:            containerStatus(insp.State.Status),
			HealthStatus:      health,
			RestartCount:      insp.RestartCount,
		})

		if insp.State.Running {
			raw, err := c.docker.ContainerStatsOnce(ctx, s.ID)
			if err != nil {
				c.logger.Warn("stats failed, skipping metric this cycle", "id", s.ID, "err", err)
				continue
			}
			metrics = append(metrics, raw.ToMetric("container", s.ID, mode, intervalSeconds))
		}
	}

	result, err := c.transport.SendTelemetry(ctx, containers, metrics)
	if err != nil {
		return err
	}
	// This is the actual FSM -> collector wiring (ARCHITECTURE.md §F): the
	// control plane evaluated this batch against the monitoring engine and
	// is telling us what cadence to use next. Applying it here means a
	// CRITICAL verdict takes effect on the very next tick, not up to 60s
	// later — SetInterval already wakes the loop immediately rather than
	// waiting for the current timer to fire.
	c.SetInterval(time.Duration(result.IntervalSeconds) * time.Second)
	return nil
}

func firstName(names []string) string {
	if len(names) == 0 {
		return ""
	}
	// Docker's /containers/json prefixes names with "/"; strip it for
	// display purposes.
	n := names[0]
	if len(n) > 0 && n[0] == '/' {
		return n[1:]
	}
	return n
}

func containerStatus(dockerState string) models.ContainerStatus {
	switch dockerState {
	case "running":
		return models.ContainerStatusRunning
	case "exited", "dead":
		return models.ContainerStatusExited
	case "paused":
		return models.ContainerStatusPaused
	default:
		return models.ContainerStatusUnknown
	}
}

func modeForInterval(d time.Duration) models.MonitoringMode {
	switch {
	case d <= 2*time.Second:
		return models.ModeManualRealtime
	case d <= 10*time.Second:
		return models.ModeCritical
	default:
		return models.ModeNormal
	}
}
