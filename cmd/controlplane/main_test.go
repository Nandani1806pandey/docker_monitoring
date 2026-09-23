package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/nandani/docker-monitor/internal/controlplane/monitoring"
	"github.com/nandani/docker-monitor/internal/shared/config"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestLoadOrGenerateCA_GeneratesAndPersists(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ca")
	ca, err := loadOrGenerateCA(dir, discardLogger())
	if err != nil {
		t.Fatalf("loadOrGenerateCA: %v", err)
	}
	if !ca.Certificate().IsCA {
		t.Fatalf("expected a CA certificate")
	}

	certPath := filepath.Join(dir, "ca-cert.pem")
	keyPath := filepath.Join(dir, "ca-key.pem")
	if _, err := os.Stat(certPath); err != nil {
		t.Fatalf("expected cert file to be written: %v", err)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("expected key file to be written: %v", err)
	}

	if runtime.GOOS != "windows" {
		info, err := os.Stat(keyPath)
		if err != nil {
			t.Fatalf("stat key file: %v", err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("expected CA key file to be 0600, got %o", perm)
		}
	}
}

// TestLoadOrGenerateCA_ReloadsSameCAOnSecondBoot is the actual point of
// this milestone: a second "boot" against the same directory must return
// the SAME CA (same serial, same key), not silently generate a new one —
// otherwise every restart still invalidates every enrolled agent, just
// with extra steps.
func TestLoadOrGenerateCA_ReloadsSameCAOnSecondBoot(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ca")

	first, err := loadOrGenerateCA(dir, discardLogger())
	if err != nil {
		t.Fatalf("loadOrGenerateCA (first boot): %v", err)
	}

	second, err := loadOrGenerateCA(dir, discardLogger())
	if err != nil {
		t.Fatalf("loadOrGenerateCA (second boot): %v", err)
	}

	if first.Certificate().SerialNumber.Cmp(second.Certificate().SerialNumber) != 0 {
		t.Fatalf("expected the second boot to reload the same CA (same serial), got a different one")
	}
	if !first.Certificate().Equal(second.Certificate()) {
		t.Fatalf("expected the second boot's certificate to be identical to the first")
	}
}

// TestThresholdsFromConfig_UsesConfiguredIntervals is a regression test:
// config.Load() reads DM_NORMAL_INTERVAL_SECONDS et al. from the
// environment, but main() used to call monitoring.DefaultThresholds()
// directly and never passed cfg through at all — so a configured
// non-default interval was parsed successfully and then silently ignored.
func TestThresholdsFromConfig_UsesConfiguredIntervals(t *testing.T) {
	t.Setenv("DM_NORMAL_INTERVAL_SECONDS", "45")
	t.Setenv("DM_CRITICAL_INTERVAL_SECONDS", "5")
	t.Setenv("DM_MANUAL_INTERVAL_SECONDS", "1")
	t.Setenv("DM_HYSTERESIS_SECONDS", "90")
	// SessionSecret is required by config.Load(); irrelevant to this test.
	t.Setenv("DM_SESSION_SECRET", "test-secret")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	thresholds := thresholdsFromConfig(cfg)
	if thresholds.NormalIntervalSeconds != 45 {
		t.Fatalf("expected NormalIntervalSeconds=45 from env, got %d (config value discarded)", thresholds.NormalIntervalSeconds)
	}
	if thresholds.CriticalIntervalSeconds != 5 {
		t.Fatalf("expected CriticalIntervalSeconds=5 from env, got %d", thresholds.CriticalIntervalSeconds)
	}
	if thresholds.ManualIntervalSeconds != 1 {
		t.Fatalf("expected ManualIntervalSeconds=1 from env, got %d", thresholds.ManualIntervalSeconds)
	}
	if thresholds.HysteresisWindow != 90*time.Second {
		t.Fatalf("expected HysteresisWindow=90s from env, got %s", thresholds.HysteresisWindow)
	}

	// CPU/mem percentages have no env var yet — must still come from
	// DefaultThresholds(), not zero values.
	defaults := monitoring.DefaultThresholds()
	if thresholds.CPUCriticalPercent != defaults.CPUCriticalPercent {
		t.Fatalf("expected CPUCriticalPercent to fall back to the default (%v), got %v",
			defaults.CPUCriticalPercent, thresholds.CPUCriticalPercent)
	}
}

// TestOpenStore_Memory, TestOpenStore_SQLite, TestOpenStore_SQLite_DefaultPath,
// and TestOpenStore_UnknownDriver are a regression guard for the bug the
// gap analysis flagged: config.Load accepted "postgres"/"sqlite" as a
// driver value while main() unconditionally hard-exited on anything but
// "memory". openStore must now actually honor cfg.StorageDriver for every
// value config.Load accepts.
func TestOpenStore_Memory(t *testing.T) {
	cfg := &config.Config{StorageDriver: "memory"}
	store, err := openStore(context.Background(), cfg, discardLogger())
	if err != nil {
		t.Fatalf("openStore(memory): %v", err)
	}
	defer store.Close()
	if _, err := store.ListHosts(context.Background()); err != nil {
		t.Fatalf("memory store not usable: %v", err)
	}
}

func TestOpenStore_SQLite(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	cfg := &config.Config{StorageDriver: "sqlite", DatabaseURL: dsn}
	store, err := openStore(context.Background(), cfg, discardLogger())
	if err != nil {
		t.Fatalf("openStore(sqlite): %v", err)
	}
	defer store.Close()
	if _, err := store.ListHosts(context.Background()); err != nil {
		t.Fatalf("sqlite store not usable: %v", err)
	}
	if _, err := os.Stat(dsn); err != nil {
		t.Fatalf("expected sqlite file to be created at %s: %v", dsn, err)
	}
}

func TestOpenStore_SQLite_DefaultPath(t *testing.T) {
	dir := t.TempDir()
	orig, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer os.Chdir(orig)

	cfg := &config.Config{StorageDriver: "sqlite"} // DatabaseURL left empty on purpose
	store, err := openStore(context.Background(), cfg, discardLogger())
	if err != nil {
		t.Fatalf("openStore(sqlite, no DatabaseURL): %v", err)
	}
	defer store.Close()
	if _, err := os.Stat(filepath.Join(dir, "data", "dockermonitor.db")); err != nil {
		t.Fatalf("expected default sqlite path to be created: %v", err)
	}
}

func TestOpenStore_UnknownDriver(t *testing.T) {
	cfg := &config.Config{StorageDriver: "mongodb"}
	if _, err := openStore(context.Background(), cfg, discardLogger()); err == nil {
		t.Fatalf("expected an error for an unsupported storage driver")
	}
}
