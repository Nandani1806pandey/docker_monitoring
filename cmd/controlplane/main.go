// Command controlplane is the entrypoint for the Docker Monitor control
// plane described in ARCHITECTURE.md. This build wires up the REST API
// (§E.1) and the in-memory storage backend; the agent protocol (§E.3),
// adaptive monitoring engine (§F), and WebSocket hub (§E.2) land in later
// milestones per the implementation plan (§H).
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"crypto/tls"
	"crypto/x509"

	"github.com/nandani/docker-monitor/internal/controlplane/agentapi"
	"github.com/nandani/docker-monitor/internal/controlplane/agentregistry"
	"github.com/nandani/docker-monitor/internal/controlplane/api"
	"github.com/nandani/docker-monitor/internal/controlplane/auth"
	"github.com/nandani/docker-monitor/internal/controlplane/events"
	"github.com/nandani/docker-monitor/internal/controlplane/monitoring"
	"github.com/nandani/docker-monitor/internal/controlplane/pki"
	"github.com/nandani/docker-monitor/internal/controlplane/storage"
	"github.com/nandani/docker-monitor/internal/controlplane/storage/memory"
	"github.com/nandani/docker-monitor/internal/controlplane/storage/sqlstore"
	"github.com/nandani/docker-monitor/internal/controlplane/ws"
	"github.com/nandani/docker-monitor/internal/shared/config"
)

func tlsCertificate(certPEM, keyPEM []byte) (tls.Certificate, error) {
	return tls.X509KeyPair(certPEM, keyPEM)
}

func x509CertPool(ca *pki.CA) *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(ca.Certificate())
	return pool
}

// loadOrGenerateCA persists the internal CA's cert/key as plain PEM files
// under dir, so a control-plane restart keeps issuing certificates under
// the same root instead of invalidating every already-enrolled agent.
//
// This is a known, documented simplification, not the eventual answer:
// pki.CA's own doc comment says this key "must come from the secrets
// store... never from a plaintext file" in production. A real secrets
// store (Vault, KMS, sealed-and-encrypted-at-rest, whatever the deployment
// target dictates) is out of scope for this milestone; what matters here
// is closing the "every restart nukes the whole fleet's trust" gap, with
// file permissions (0700 dir, 0600 files) that at least keep the key off
// of anything world-readable in the meantime.
func loadOrGenerateCA(dir string, logger *slog.Logger) (*pki.CA, error) {
	certPath := filepath.Join(dir, "ca-cert.pem")
	keyPath := filepath.Join(dir, "ca-key.pem")

	certPEM, certErr := os.ReadFile(certPath)
	keyPEM, keyErr := os.ReadFile(keyPath)
	if certErr == nil && keyErr == nil {
		ca, err := pki.LoadCA(certPEM, keyPEM)
		if err != nil {
			return nil, fmt.Errorf("load persisted CA from %s: %w", dir, err)
		}
		logger.Info("loaded persisted internal CA", "dir", dir,
			"not_after", ca.Certificate().NotAfter.Format(time.RFC3339))
		return ca, nil
	}

	logger.Info("no persisted CA found, generating a new one", "dir", dir)
	ca, err := pki.GenerateCA("docker-monitor-root", 10*365*24*time.Hour)
	if err != nil {
		return nil, fmt.Errorf("generate CA: %w", err)
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create CA directory %s: %w", dir, err)
	}
	newKeyPEM, err := ca.KeyPEM()
	if err != nil {
		return nil, fmt.Errorf("export CA key: %w", err)
	}
	if err := os.WriteFile(keyPath, newKeyPEM, 0o600); err != nil {
		return nil, fmt.Errorf("persist CA key: %w", err)
	}
	if err := os.WriteFile(certPath, ca.CertPEM(), 0o600); err != nil {
		return nil, fmt.Errorf("persist CA certificate: %w", err)
	}
	logger.Info("persisted new internal CA", "dir", dir)
	return ca, nil
}

// openStore honors cfg.StorageDriver for every value config.Load accepts
// ("memory", "sqlite", "postgres") — closing the gap where config.Load
// validated DM_DATABASE_URL for postgres while this function used to
// hard-exit on anything but "memory". sqlite defaults to
// ./data/dockermonitor.db when DM_DATABASE_URL is unset, matching a
// zero-extra-infra self-hosted default; postgres requires
// DM_DATABASE_URL to already be set (enforced in config.Load).
func openStore(ctx context.Context, cfg *config.Config, logger *slog.Logger) (storage.Store, error) {
	switch cfg.StorageDriver {
	case "memory", "":
		return memory.New(), nil
	case "sqlite":
		dsn := cfg.DatabaseURL
		if dsn == "" {
			dsn = filepath.Join("data", "dockermonitor.db")
		}
		if dir := filepath.Dir(dsn); dir != "." {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return nil, fmt.Errorf("create sqlite data directory %s: %w", dir, err)
			}
		}
		logger.Info("opening sqlite storage", "path", dsn)
		return sqlstore.Open(ctx, "sqlite", dsn)
	case "postgres":
		logger.Info("opening postgres storage")
		return sqlstore.Open(ctx, "postgres", cfg.DatabaseURL)
	default:
		return nil, fmt.Errorf("unsupported storage driver %q (want \"memory\", \"sqlite\", or \"postgres\")", cfg.StorageDriver)
	}
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	cfg, err := config.Load()
	if err != nil {
		logger.Error("config error", "err", err)
		os.Exit(1)
	}

	store, err := openStore(context.Background(), cfg, logger)
	if err != nil {
		logger.Error("failed to open storage", "driver", cfg.StorageDriver, "err", err)
		os.Exit(1)
	}
	defer store.Close()

	recorder := events.NewRecorder(store)

	// Persisted to cfg.CADir so a restart keeps the same root instead of
	// invalidating every already-enrolled agent (see loadOrGenerateCA's
	// doc comment for what this is and isn't).
	ca, err := loadOrGenerateCA(cfg.CADir, logger)
	if err != nil {
		logger.Error("failed to load or generate internal CA", "err", err)
		os.Exit(1)
	}

	// Shared between the registry (which evaluates telemetry against it)
	// and the browser API (which lets a user open/close manual live
	// inspection on it) — one Engine per process, not one per package.
	//
	// thresholdsFromConfig, not monitoring.DefaultThresholds() directly:
	// config.Load() already reads DM_NORMAL_INTERVAL_SECONDS,
	// DM_CRITICAL_INTERVAL_SECONDS, DM_MANUAL_INTERVAL_SECONDS, and
	// DM_HYSTERESIS_SECONDS from the environment, but nothing was ever
	// passing those values through — this used to call DefaultThresholds()
	// unconditionally, so those env vars were parsed and then silently
	// discarded. CPU/mem percentages have no env var yet (config.Config
	// doesn't expose them), so those two still come from the defaults.
	monitor := monitoring.NewEngine(thresholdsFromConfig(cfg))

	registry := agentregistry.New(store, ca, recorder, monitor)

	// Historical metrics (§12/§27) are opt-in and only meaningful on top of
	// real persistence: HistoricalMetricStore is a narrower interface than
	// storage.Store, and only *sqlstore.Store implements it (the memory
	// backend has nowhere durable to put a time series). If the operator
	// has enabled it but is running on the memory backend, that's a
	// misconfiguration worth surfacing loudly rather than silently
	// no-op'ing — the whole point of DM_HISTORICAL_METRICS_ENABLED is that
	// history should actually be recorded.
	if cfg.HistoricalMetricsEnabled {
		hstore, ok := store.(agentregistry.HistoricalMetricStore)
		if !ok {
			logger.Error("DM_HISTORICAL_METRICS_ENABLED is true but the configured storage driver has no historical store",
				"driver", cfg.StorageDriver)
			os.Exit(1)
		}
		registry.SetHistoricalStore(hstore)

		// Periodic retention: rolls up 1-minute buckets older than 6h into
		// 5-minute buckets and deletes anything past the configured
		// retention window. Runs once at startup (in case the process was
		// down past a boundary) and then hourly — retention doesn't need
		// to be exact to the minute, and RunRetention is documented as
		// idempotent/safe to call repeatedly.
		type retentionRunner interface {
			RunRetention(ctx context.Context, now time.Time, retentionDays int) error
		}
		if runner, ok := hstore.(retentionRunner); ok {
			go func() {
				runRetentionOnce := func() {
					ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					defer cancel()
					if err := runner.RunRetention(ctx, time.Now().UTC(), cfg.HistoricalRetentionDays); err != nil {
						logger.Error("historical metrics retention pass failed", "err", err)
					}
				}
				runRetentionOnce()
				ticker := time.NewTicker(time.Hour)
				defer ticker.Stop()
				for range ticker.C {
					runRetentionOnce()
				}
			}()
			logger.Info("historical metrics enabled", "retention_days", cfg.HistoricalRetentionDays)
		}
	}

	// Milestone 9: sessions/RBAC (§G, §26). BootstrapAdminIfNeeded is a
	// no-op once any user exists — see its doc comment — so it's safe to
	// call unconditionally on every startup rather than only on first
	// run.
	authService := auth.NewService(store, time.Duration(cfg.SessionTTLHours)*time.Hour)
	if created, err := authService.BootstrapAdminIfNeeded(context.Background(), cfg.BootstrapAdminEmail, cfg.BootstrapAdminPassword); err != nil {
		logger.Error("failed to bootstrap admin user", "err", err)
		os.Exit(1)
	} else if created {
		logger.Info("bootstrap admin account created", "email", cfg.BootstrapAdminEmail)
	} else if cfg.BootstrapAdminEmail == "" || cfg.BootstrapAdminPassword == "" {
		logger.Warn("no DM_BOOTSTRAP_ADMIN_EMAIL/DM_BOOTSTRAP_ADMIN_PASSWORD set and no users exist yet — nobody will be able to log in until an account is created some other way")
	}

	server := api.NewServer(store, recorder, logger, registry, ca, monitor).
		WithHub(ws.NewHub(logger)).
		WithAuth(authService, cfg.CookieSecure)
	httpServer := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	// Agent-facing mTLS listener: a separate port from the browser API
	// above, with a different trust model (client certs, not
	// sessions/cookies) and different SANs (whatever address agents will
	// actually dial this host at).
	agentServer := agentapi.NewServer(registry, logger)
	agentListener, err := net.Listen("tcp", cfg.AgentAddr)
	if err != nil {
		logger.Error("failed to bind agent listener", "addr", cfg.AgentAddr, "err", err)
		os.Exit(1)
	}
	serverCertPEM, serverKeyPEM, err := ca.ServerTLSCertificate([]string{"localhost", "127.0.0.1"}, 825*24*time.Hour)
	if err != nil {
		logger.Error("failed to issue agent-listener server certificate", "err", err)
		os.Exit(1)
	}
	serverCert, err := tlsCertificate(serverCertPEM, serverKeyPEM)
	if err != nil {
		logger.Error("failed to load agent-listener server certificate", "err", err)
		os.Exit(1)
	}
	caPool := x509CertPool(ca)
	agentHTTPServer := &http.Server{
		Handler:           agentServer.Handler(),
		TLSConfig:         agentapi.TLSConfig(serverCert, caPool),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		logger.Info("control plane listening", "addr", cfg.HTTPAddr, "storage", cfg.StorageDriver,
			"historical_metrics_enabled", cfg.HistoricalMetricsEnabled)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server error", "err", err)
			os.Exit(1)
		}
	}()

	go func() {
		logger.Info("agent mTLS listener listening", "addr", cfg.AgentAddr)
		if err := agentHTTPServer.ServeTLS(agentListener, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("agent server error", "err", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	logger.Info("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(ctx); err != nil {
		logger.Error("graceful shutdown failed", "err", err)
	}
	if err := agentHTTPServer.Shutdown(ctx); err != nil {
		logger.Error("agent server graceful shutdown failed", "err", err)
	}
}

// thresholdsFromConfig builds the monitoring engine's ThresholdConfig from
// loaded configuration, falling back to monitoring.DefaultThresholds() for
// the two fields config.Config doesn't expose yet (CPU/mem percentages —
// no DM_* env var for those exists). Isolated in its own function so the
// mapping is easy to check against config.go at a glance, rather than
// buried inline in main().
func thresholdsFromConfig(cfg *config.Config) monitoring.ThresholdConfig {
	t := monitoring.DefaultThresholds()
	t.NormalIntervalSeconds = cfg.NormalIntervalSeconds
	t.CriticalIntervalSeconds = cfg.CriticalIntervalSeconds
	t.ManualIntervalSeconds = cfg.ManualIntervalSeconds
	t.HysteresisWindow = time.Duration(cfg.HysteresisSeconds) * time.Second
	return t
}
