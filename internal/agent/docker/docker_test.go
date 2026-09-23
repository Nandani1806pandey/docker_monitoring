package docker

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nandani/docker-monitor/internal/shared/models"
)

// startFakeDaemon serves a minimal stand-in for the Docker Engine API over
// a real Unix socket, so the test exercises the actual DialContext/HTTP
// path the agent uses on a real host, not just the JSON decoding logic.
func startFakeDaemon(t *testing.T, mux *http.ServeMux) string {
	t.Helper()
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "docker.sock")

	l, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix socket: %v", err)
	}
	srv := &http.Server{Handler: mux}
	go srv.Serve(l)
	t.Cleanup(func() {
		srv.Close()
		os.Remove(sockPath)
	})
	return sockPath
}

func TestPingAndVersion(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/_ping", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(VersionInfo{Version: "27.3.1", APIVersion: "1.47", Os: "linux", Arch: "amd64"})
	})
	sock := startFakeDaemon(t, mux)
	c := NewClient(sock)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := c.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	v, err := c.Version(ctx)
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if v.Version != "27.3.1" || v.APIVersion != "1.47" {
		t.Fatalf("unexpected version info: %+v", v)
	}
}

func TestListAndInspectContainer(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/containers/json", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("all") != "true" {
			t.Errorf("expected all=true query param, got %q", r.URL.RawQuery)
		}
		json.NewEncoder(w).Encode([]ContainerSummary{
			{ID: "abc123", Names: []string{"/web"}, Image: "nginx:latest", State: "running"},
		})
	})
	mux.HandleFunc("/containers/abc123/json", func(w http.ResponseWriter, r *http.Request) {
		var resp ContainerInspect
		resp.ID = "abc123"
		resp.State.Status = "running"
		resp.State.Running = true
		resp.State.Health = &struct {
			Status string `json:"Status"`
		}{Status: "healthy"}
		resp.RestartCount = 2
		json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("/containers/missing/json", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"message":"No such container: missing"}`))
	})

	sock := startFakeDaemon(t, mux)
	c := NewClient(sock)
	ctx := context.Background()

	list, err := c.ListContainers(ctx)
	if err != nil {
		t.Fatalf("ListContainers: %v", err)
	}
	if len(list) != 1 || list[0].ID != "abc123" {
		t.Fatalf("unexpected list: %+v", list)
	}

	insp, err := c.InspectContainer(ctx, "abc123")
	if err != nil {
		t.Fatalf("InspectContainer: %v", err)
	}
	if insp.RestartCount != 2 || insp.State.Health.Status != "healthy" {
		t.Fatalf("unexpected inspect: %+v", insp)
	}

	// A 404 from the daemon (e.g. container removed between list and
	// inspect) must surface as a typed APIError, not a generic error the
	// FSM would misclassify as "daemon unreachable".
	_, err = c.InspectContainer(ctx, "missing")
	if err == nil {
		t.Fatalf("expected error for missing container")
	}
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("expected *APIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", apiErr.StatusCode)
	}
}

func TestStatsToMetric(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/containers/abc123/stats", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("stream") != "false" {
			t.Errorf("expected stream=false, got %q", r.URL.RawQuery)
		}
		raw := StatsRaw{
			Read: "2026-09-17T12:00:00.000000000Z",
			Networks: map[string]networkStat{
				"eth0": {RxBytes: 1000, TxBytes: 2000},
				"eth1": {RxBytes: 500, TxBytes: 250},
			},
		}
		raw.CPUStats.CPUUsage.TotalUsage = 2_000_000_000
		raw.PreCPUStats.CPUUsage.TotalUsage = 1_000_000_000
		raw.CPUStats.SystemCPUUsage = 20_000_000_000
		raw.PreCPUStats.SystemCPUUsage = 10_000_000_000
		raw.CPUStats.OnlineCPUs = 4
		raw.MemoryStats.Usage = 500_000_000
		raw.MemoryStats.Limit = 1_000_000_000
		raw.MemoryStats.Stats = map[string]uint64{"total_inactive_file": 100_000_000}
		json.NewEncoder(w).Encode(raw)
	})
	sock := startFakeDaemon(t, mux)
	c := NewClient(sock)

	raw, err := c.ContainerStatsOnce(context.Background(), "abc123")
	if err != nil {
		t.Fatalf("ContainerStatsOnce: %v", err)
	}

	m := raw.ToMetric("container", "abc123", models.ModeNormal, 60)

	// cpuDelta=1e9, systemDelta=1e10, onlineCPUs=4 -> (1e9/1e10)*4*100 = 40%
	if m.CPUPercent < 39.9 || m.CPUPercent > 40.1 {
		t.Fatalf("expected ~40%% CPU, got %f", m.CPUPercent)
	}
	// mem used = 500M - 100M (cache) = 400M
	if m.MemUsedBytes != 400_000_000 {
		t.Fatalf("expected 400000000 mem used (cache-adjusted), got %d", m.MemUsedBytes)
	}
	if m.NetRXBytes != 1500 || m.NetTXBytes != 2250 {
		t.Fatalf("expected summed network across interfaces, got rx=%d tx=%d", m.NetRXBytes, m.NetTXBytes)
	}
	if m.Mode != models.ModeNormal || m.IntervalSeconds != 60 {
		t.Fatalf("expected mode/interval to be passed through, got %+v", m)
	}
	if m.CollectedAt.Year() != 2026 {
		t.Fatalf("expected collected_at parsed from daemon's 'read' field, got %v", m.CollectedAt)
	}
}

func TestDaemonUnreachable(t *testing.T) {
	// Point at a socket path nothing is listening on.
	c := NewClient(filepath.Join(t.TempDir(), "no-daemon.sock"))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := c.Ping(ctx)
	if err == nil {
		t.Fatalf("expected error when daemon socket does not exist")
	}
	if _, ok := err.(*APIError); ok {
		t.Fatalf("expected transport error, not *APIError, for an unreachable daemon")
	}
}
