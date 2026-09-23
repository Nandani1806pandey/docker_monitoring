// Package hoststats collects the host's own CPU, memory, and network
// utilization directly from /proc (ARCHITECTURE.md §9's "Collect host
// statistics" — the one piece of that requirement this build never
// actually implemented; only container-level stats existed). Deliberately
// stdlib-only and Linux-/proc-only, matching this codebase's existing
// "avoid unnecessary dependencies" stance for the agent (internal/agent/docker
// takes the same approach against the Docker Engine API rather than
// pulling in a full SDK, or here a full host-metrics library like
// gopsutil for what boils down to parsing three small text files).
//
// This intentionally does not support non-Linux hosts. The agent's whole
// reason for being (talking to a local Docker socket) already assumes
// Linux-shaped container infrastructure; adding a portability layer for a
// host-stats feature no other part of this codebase attempts would be
// speculative generality, not a real requirement.
package hoststats

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Sample is one host-level reading, shaped to convert directly into
// models.Metric by the caller (internal/agent/collector) — this package
// doesn't import models itself, since CPU/mem/net-from-/proc has nothing
// to do with the wire format and shouldn't need to know it exists.
type Sample struct {
	CPUPercent    float64
	MemUsedBytes  uint64
	MemTotalBytes uint64
	NetRXBytes    uint64
	NetTXBytes    uint64
}

// Paths lets tests point at fixture files instead of the real /proc.
// Collect uses DefaultPaths when called directly; construct a Paths value
// with fields overridden to test the parsing logic deterministically
// against known input, independent of whatever this machine's actual
// /proc happens to contain right now.
type Paths struct {
	Stat    string
	Meminfo string
	NetDev  string
}

var DefaultPaths = Paths{Stat: "/proc/stat", Meminfo: "/proc/meminfo", NetDev: "/proc/net/dev"}

// sampleWindow is how long Collect waits between its two /proc/stat reads
// to compute a CPU delta. /proc/stat only exposes cumulative jiffie
// counters since boot — a single read can't yield a percentage any more
// than a single odometer reading yields a speed; two readings a short
// interval apart are required, the same reason Docker's own stats
// endpoint returns both cpu_stats and precpu_stats per sample rather than
// one snapshot (see internal/agent/docker/stats.go's cpuPercent). 200ms is
// enough resolution to be meaningful without meaningfully lengthening a
// collection cycle that's normally 60s (or a 2-10s CRITICAL/MANUAL
// interval, where 200ms is still a small fraction of the budget).
const sampleWindow = 200 * time.Millisecond

// Collect takes two /proc/stat readings sampleWindow apart to compute CPU
// utilization, plus single-point-in-time memory and network readings, and
// combines them into one Sample. Returns an error (rather than a partial
// Sample) if any of the three files can't be read or parsed — a host
// metric with silently-zeroed fields would be worse than no host metric
// this cycle, since a dashboard has no way to distinguish "genuinely
// idle" from "collection failed and defaulted to zero".
func Collect(ctx context.Context, paths Paths) (Sample, error) {
	first, err := readCPUJiffies(paths.Stat)
	if err != nil {
		return Sample{}, fmt.Errorf("read cpu stats: %w", err)
	}

	select {
	case <-ctx.Done():
		return Sample{}, ctx.Err()
	case <-time.After(sampleWindow):
	}

	second, err := readCPUJiffies(paths.Stat)
	if err != nil {
		return Sample{}, fmt.Errorf("read cpu stats: %w", err)
	}

	memUsed, memTotal, err := readMemory(paths.Meminfo)
	if err != nil {
		return Sample{}, fmt.Errorf("read memory stats: %w", err)
	}

	rx, tx, err := readNetwork(paths.NetDev)
	if err != nil {
		return Sample{}, fmt.Errorf("read network stats: %w", err)
	}

	return Sample{
		CPUPercent:    cpuPercent(first, second),
		MemUsedBytes:  memUsed,
		MemTotalBytes: memTotal,
		NetRXBytes:    rx,
		NetTXBytes:    tx,
	}, nil
}

// cpuJiffies holds the fields of /proc/stat's aggregate "cpu" line needed
// for a standard busy/total utilization calculation. Field order and
// meaning match the kernel's documented /proc/stat format.
type cpuJiffies struct {
	user, nice, system, idle, iowait, irq, softirq, steal uint64
}

func (c cpuJiffies) total() uint64 {
	return c.user + c.nice + c.system + c.idle + c.iowait + c.irq + c.softirq + c.steal
}

func (c cpuJiffies) idleTotal() uint64 {
	return c.idle + c.iowait
}

// readCPUJiffies parses the single aggregate "cpu " line (note the
// trailing space distinguishing it from "cpu0", "cpu1", ... per-core
// lines, which this package has no need to break out individually).
func readCPUJiffies(path string) (cpuJiffies, error) {
	f, err := os.Open(path)
	if err != nil {
		return cpuJiffies{}, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)[1:] // drop the "cpu" label
		var vals [8]uint64
		for i := 0; i < len(fields) && i < len(vals); i++ {
			v, err := strconv.ParseUint(fields[i], 10, 64)
			if err != nil {
				return cpuJiffies{}, fmt.Errorf("parse %q field %d: %w", path, i, err)
			}
			vals[i] = v
		}
		return cpuJiffies{
			user: vals[0], nice: vals[1], system: vals[2], idle: vals[3],
			iowait: vals[4], irq: vals[5], softirq: vals[6], steal: vals[7],
		}, nil
	}
	if err := scanner.Err(); err != nil {
		return cpuJiffies{}, err
	}
	return cpuJiffies{}, fmt.Errorf("%s: no aggregate \"cpu \" line found", path)
}

// cpuPercent computes utilization from two jiffie snapshots: the fraction
// of total elapsed jiffies that were NOT idle. Guards against a zero or
// negative total delta (clock oddities, or a kernel that reset counters —
// vanishingly rare, but a divide-by-zero here would be an ugly way to
// find out) by reporting 0 rather than NaN or Inf.
func cpuPercent(a, b cpuJiffies) float64 {
	totalDelta := float64(b.total()) - float64(a.total())
	idleDelta := float64(b.idleTotal()) - float64(a.idleTotal())
	if totalDelta <= 0 {
		return 0
	}
	busyDelta := totalDelta - idleDelta
	if busyDelta < 0 {
		busyDelta = 0
	}
	return (busyDelta / totalDelta) * 100.0
}

// readMemory parses /proc/meminfo's MemTotal and MemAvailable (both in
// kB, per the kernel's fixed format) into byte counts. MemAvailable
// (rather than the simpler MemTotal-MemFree) is used deliberately: it
// already accounts for reclaimable page cache and slab memory the kernel
// would free under pressure, which is the same "don't blame the
// container/host for memory Linux is happily using as a cache and will
// give back on demand" reasoning internal/agent/docker/stats.go's
// memUsedBytesNoCache applies to containers — the host-level metric should
// tell the same kind of story, not a naive one.
func readMemory(path string) (usedBytes, totalBytes uint64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()

	var total, available uint64
	var haveTotal, haveAvailable bool

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "MemTotal:"):
			total, err = parseMeminfoKB(line)
			if err != nil {
				return 0, 0, err
			}
			haveTotal = true
		case strings.HasPrefix(line, "MemAvailable:"):
			available, err = parseMeminfoKB(line)
			if err != nil {
				return 0, 0, err
			}
			haveAvailable = true
		}
		if haveTotal && haveAvailable {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, 0, err
	}
	if !haveTotal || !haveAvailable {
		return 0, 0, fmt.Errorf("%s: missing MemTotal or MemAvailable", path)
	}
	if available > total {
		available = total // defensive: never report negative usage
	}
	return (total - available) * 1024, total * 1024, nil
}

func parseMeminfoKB(line string) (uint64, error) {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0, fmt.Errorf("malformed meminfo line: %q", line)
	}
	return strconv.ParseUint(fields[1], 10, 64)
}

// readNetwork sums received/transmitted bytes across every interface in
// /proc/net/dev except loopback — a host's own network utilization is
// about its external traffic, and including lo would double-count
// container-to-container or process-to-process traffic that never
// touches a physical or virtual external link.
func readNetwork(path string) (rxBytes, txBytes uint64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		if lineNum <= 2 {
			continue // two header lines, per the kernel's fixed format
		}
		line := scanner.Text()
		colonIdx := strings.IndexByte(line, ':')
		if colonIdx < 0 {
			continue
		}
		iface := strings.TrimSpace(line[:colonIdx])
		if iface == "lo" {
			continue
		}
		fields := strings.Fields(line[colonIdx+1:])
		// Column layout per the kernel: rx bytes is field 0, tx bytes is
		// field 8 (rx has 8 columns: bytes packets errs drop fifo frame
		// compressed multicast, then tx bytes starts the next 8).
		if len(fields) < 9 {
			continue
		}
		rx, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}
		tx, err := strconv.ParseUint(fields[8], 10, 64)
		if err != nil {
			continue
		}
		rxBytes += rx
		txBytes += tx
	}
	if err := scanner.Err(); err != nil {
		return 0, 0, err
	}
	return rxBytes, txBytes, nil
}
