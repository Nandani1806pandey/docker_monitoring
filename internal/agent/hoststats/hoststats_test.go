package hoststats

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeFixture(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write fixture %s: %v", name, err)
	}
	return path
}

// TestCollect_KnownFixtures drives Collect end-to-end against fixture
// files with known, hand-computed expected results — the deterministic
// counterpart to the real-/proc sanity test below, isolating this
// package's parsing/math from whatever this machine's real host happens
// to be doing right now.
func TestCollect_KnownFixtures(t *testing.T) {
	dir := t.TempDir()

	// meminfo: total=4,000,000 kB, available=1,000,000 kB
	// -> used = 3,000,000 kB = 3,072,000,000 bytes, total = 4,096,000,000 bytes
	meminfo := "MemTotal:        4000000 kB\nMemFree:          800000 kB\nMemAvailable:    1000000 kB\nBuffers:            5000 kB\n"
	meminfoPath := writeFixture(t, dir, "meminfo", meminfo)

	// net/dev: two header lines, then lo (must be excluded) and eth0.
	// eth0: rx=1000 bytes (field 0), tx=2000 bytes (field 8 of the 8 tx columns).
	netdev := "Inter-|   Receive                                                |  Transmit\n" +
		" face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed\n" +
		"    lo:  999999       1    0    0    0     0          0         0   999999       1    0    0    0     0       0          0\n" +
		"  eth0:    1000       5    0    0    0     0          0         0     2000       5    0    0    0     0       0          0\n"
	netdevPath := writeFixture(t, dir, "net_dev", netdev)

	// stat: two snapshots representing 100 total jiffies elapsed, 25 of
	// them idle -> 75% busy. Values chosen so the math is easy to verify
	// by hand: user+nice+system+irq+softirq+steal all zero except one
	// field, idle+iowait carries the "idle" 25.
	statContent := "cpu  0 0 0 0 0 0 0 0\nintr 12345\n"
	statPath := writeFixture(t, dir, "stat", statContent)

	// Collect reads Stat twice sampleWindow apart; rewrite it with the
	// "after" values partway through via a background goroutine timed
	// just past the first read, since Collect doesn't accept two separate
	// paths for its two reads.
	go func() {
		time.Sleep(sampleWindow / 2)
		after := "cpu  75 0 0 25 0 0 0 0\nintr 12345\n"
		_ = os.WriteFile(statPath, []byte(after), 0o644)
	}()

	sample, err := Collect(context.Background(), Paths{Stat: statPath, Meminfo: meminfoPath, NetDev: netdevPath})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	if sample.CPUPercent < 74.9 || sample.CPUPercent > 75.1 {
		t.Fatalf("expected ~75%% CPU, got %f", sample.CPUPercent)
	}
	if sample.MemTotalBytes != 4000000*1024 {
		t.Fatalf("expected MemTotalBytes=%d, got %d", 4000000*1024, sample.MemTotalBytes)
	}
	wantUsed := uint64(4000000-1000000) * 1024
	if sample.MemUsedBytes != wantUsed {
		t.Fatalf("expected MemUsedBytes=%d (total-available, not total-free), got %d", wantUsed, sample.MemUsedBytes)
	}
	if sample.NetRXBytes != 1000 || sample.NetTXBytes != 2000 {
		t.Fatalf("expected eth0-only rx=1000 tx=2000 (lo excluded), got rx=%d tx=%d", sample.NetRXBytes, sample.NetTXBytes)
	}
}

func TestCpuPercent_ZeroDeltaDoesNotDivideByZero(t *testing.T) {
	a := cpuJiffies{user: 100, idle: 100}
	b := cpuJiffies{user: 100, idle: 100} // identical: no elapsed time
	if got := cpuPercent(a, b); got != 0 {
		t.Fatalf("expected 0 for a zero total delta, got %f", got)
	}
}

func TestReadMemory_MissingFieldsErrors(t *testing.T) {
	dir := t.TempDir()
	path := writeFixture(t, dir, "meminfo", "MemTotal: 1000 kB\n") // no MemAvailable
	if _, _, err := readMemory(path); err == nil {
		t.Fatalf("expected an error when MemAvailable is missing, not a silently wrong result")
	}
}

func TestReadNetwork_ExcludesLoopback(t *testing.T) {
	dir := t.TempDir()
	netdev := "Inter-|   Receive                                                |  Transmit\n" +
		" face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed\n" +
		"    lo:   50000       1    0    0    0     0          0         0    50000       1    0    0    0     0       0          0\n"
	path := writeFixture(t, dir, "net_dev", netdev)
	rx, tx, err := readNetwork(path)
	if err != nil {
		t.Fatalf("readNetwork: %v", err)
	}
	if rx != 0 || tx != 0 {
		t.Fatalf("expected loopback-only traffic to be excluded entirely, got rx=%d tx=%d", rx, tx)
	}
}

// TestCollect_RealProc is a sanity check against this machine's actual
// /proc, confirming the parsing logic works against a real live kernel's
// format, not just controlled fixtures. It only asserts the result is
// plausible (no error, sane ranges) since the real values are whatever
// this machine happens to be doing right now.
func TestCollect_RealProc(t *testing.T) {
	if _, err := os.Stat("/proc/stat"); err != nil {
		t.Skip("no /proc/stat on this system")
	}
	sample, err := Collect(context.Background(), DefaultPaths)
	if err != nil {
		t.Fatalf("Collect against real /proc: %v", err)
	}
	if sample.CPUPercent < 0 || sample.CPUPercent > 100 {
		t.Fatalf("CPUPercent out of plausible range: %f", sample.CPUPercent)
	}
	if sample.MemTotalBytes == 0 {
		t.Fatalf("expected nonzero MemTotalBytes on a real system")
	}
	if sample.MemUsedBytes > sample.MemTotalBytes {
		t.Fatalf("MemUsedBytes (%d) exceeds MemTotalBytes (%d)", sample.MemUsedBytes, sample.MemTotalBytes)
	}
}
