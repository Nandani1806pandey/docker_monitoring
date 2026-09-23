package docker

import (
	"context"
	"net/http"
	"net/url"
	"time"

	"github.com/nandani/docker-monitor/internal/shared/models"
)

// networkStat is one interface's entry within StatsRaw.Networks.
type networkStat struct {
	RxBytes uint64 `json:"rx_bytes"`
	TxBytes uint64 `json:"tx_bytes"`
}

// cpuUsage is the nested cpu_usage block within a cpu_stats/precpu_stats
// sample.
type cpuUsage struct {
	TotalUsage  uint64   `json:"total_usage"`
	PercpuUsage []uint64 `json:"percpu_usage,omitempty"`
}

// cpuStatsBlock mirrors the daemon's cpu_stats/precpu_stats shape closely
// enough for the standard docker-cli CPU% algorithm (see cpuPercent below).
type cpuStatsBlock struct {
	CPUUsage       cpuUsage `json:"cpu_usage"`
	SystemCPUUsage uint64   `json:"system_cpu_usage"`
	OnlineCPUs     uint32   `json:"online_cpus"`
}

// memoryStatsBlock mirrors the daemon's memory_stats shape.
type memoryStatsBlock struct {
	Usage uint64            `json:"usage"`
	Limit uint64            `json:"limit"`
	Stats map[string]uint64 `json:"stats"`
}

// StatsRaw is a single non-streaming sample from
// GET /containers/{id}/stats?stream=false, trimmed to the fields needed to
// compute the metrics the control plane cares about (ARCHITECTURE.md §11).
type StatsRaw struct {
	Read        string                 `json:"read"`
	Networks    map[string]networkStat `json:"networks"`
	CPUStats    cpuStatsBlock          `json:"cpu_stats"`
	PreCPUStats cpuStatsBlock          `json:"precpu_stats"`
	MemoryStats memoryStatsBlock       `json:"memory_stats"`
}

// ContainerStatsOnce fetches a single non-streaming stats sample
// (?stream=false). The agent's collection loop (ARCHITECTURE.md §A.2)
// calls this once per interval per entity rather than holding open Docker's
// streaming stats endpoint per container, which is what keeps N-container
// hosts cheap at the default 60s NORMAL interval — streaming would mean N
// permanently-open connections/goroutines regardless of collection cadence.
func (c *Client) ContainerStatsOnce(ctx context.Context, id string) (StatsRaw, error) {
	var out StatsRaw
	err := c.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/stats?stream=false", nil, &out)
	return out, err
}

// ToMetric converts a raw Engine API stats sample into the shared, storage-
// agnostic models.Metric — the only shape that crosses the agent/control
// plane boundary (ARCHITECTURE.md §E.3 StreamTelemetry). mode and
// intervalSeconds come from the collector's current effective_mode
// (ARCHITECTURE.md §F.2), not from anything Docker reports.
func (s StatsRaw) ToMetric(entityType, entityID string, mode models.MonitoringMode, intervalSeconds int) models.Metric {
	var rxTotal, txTotal uint64
	for _, n := range s.Networks {
		rxTotal += n.RxBytes
		txTotal += n.TxBytes
	}

	collectedAt := time.Now().UTC()
	if t, err := time.Parse(time.RFC3339Nano, s.Read); err == nil {
		collectedAt = t
	}

	return models.Metric{
		EntityType:      entityType,
		EntityID:        entityID,
		CPUPercent:      s.cpuPercent(),
		MemUsedBytes:    s.memUsedBytesNoCache(),
		MemLimitBytes:   s.MemoryStats.Limit,
		NetRXBytes:      rxTotal,
		NetTXBytes:      txTotal,
		CollectedAt:     collectedAt,
		Mode:            mode,
		IntervalSeconds: intervalSeconds,
	}
}

// cpuPercent reproduces the standard Docker CLI CPU% algorithm: the ratio
// of container CPU-usage delta to host CPU-usage delta between this sample
// and the previous one (which the daemon includes as precpu_stats),
// scaled by the number of online CPUs. A single stats sample can't yield a
// meaningful CPU% on its own — the daemon computes this delta for us so the
// agent doesn't need to keep its own previous-sample state per container.
func (s StatsRaw) cpuPercent() float64 {
	cpuDelta := float64(s.CPUStats.CPUUsage.TotalUsage) - float64(s.PreCPUStats.CPUUsage.TotalUsage)
	systemDelta := float64(s.CPUStats.SystemCPUUsage) - float64(s.PreCPUStats.SystemCPUUsage)

	if cpuDelta <= 0 || systemDelta <= 0 {
		return 0
	}

	onlineCPUs := s.CPUStats.OnlineCPUs
	if onlineCPUs == 0 {
		onlineCPUs = uint32(len(s.CPUStats.CPUUsage.PercpuUsage))
	}
	if onlineCPUs == 0 {
		onlineCPUs = 1
	}

	return (cpuDelta / systemDelta) * float64(onlineCPUs) * 100.0
}

// memUsedBytesNoCache subtracts page cache from the raw memory.usage
// figure. Without this, container memory% looks alarmingly high and
// mostly reflects the page cache Linux is happy to reclaim under pressure,
// not real application usage — the same adjustment `docker stats` makes.
// Handles both cgroup v1 ("total_inactive_file"/"cache") and cgroup v2
// ("inactive_file") key names.
func (s StatsRaw) memUsedBytesNoCache() uint64 {
	usage := s.MemoryStats.Usage
	stats := s.MemoryStats.Stats

	if v, ok := stats["total_inactive_file"]; ok && v < usage {
		return usage - v
	}
	if v, ok := stats["inactive_file"]; ok && v < usage {
		return usage - v
	}
	if v, ok := stats["cache"]; ok && v < usage {
		return usage - v
	}
	return usage
}
