// Command agent is the entrypoint for the Docker Monitor Go agent
// (ARCHITECTURE.md §8-9). On first run it enrolls against the control
// plane using a one-time token, caching the issued mTLS certificate to
// disk so subsequent runs skip enrollment. It then runs the adaptive
// collection loop (currently at a fixed interval — the FSM drives
// SetInterval/CollectNow in a later milestone) against the local Docker
// socket.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/nandani/docker-monitor/internal/agent/collector"
	"github.com/nandani/docker-monitor/internal/agent/docker"
	"github.com/nandani/docker-monitor/internal/agent/transport"
)

type agentConfig struct {
	ControlPlaneAddr string
	HostID           string
	DockerSocket     string
	CertDir          string
	EnrollmentToken  string
	NormalInterval   time.Duration
}

func loadConfig() agentConfig {
	return agentConfig{
		ControlPlaneAddr: getEnv("DM_AGENT_CONTROL_PLANE_ADDR", "localhost:8443"),
		HostID:           getEnv("DM_AGENT_HOST_ID", ""),
		DockerSocket:     getEnv("DM_AGENT_DOCKER_SOCKET", "/var/run/docker.sock"),
		CertDir:          getEnv("DM_AGENT_CERT_DIR", "/etc/docker-monitor-agent/certs"),
		EnrollmentToken:  getEnv("DM_AGENT_ENROLLMENT_TOKEN", ""),
		// The control plane's own DM_NORMAL_INTERVAL_SECONDS default is
		// 60s, so this matches that default — but was previously
		// hardcoded here with no override at all, unlike every other
		// agent setting. A shorter interval is genuinely useful for local
		// testing (waiting a full minute for the first real metric is a
		// bad loop when iterating), and there's no reason the control
		// plane's config should be adjustable but the agent's starting
		// point shouldn't be — the FSM overrides this within one cycle
		// anyway via SetInterval, so this only controls the very first tick.
		NormalInterval: time.Duration(getEnvInt("DM_AGENT_NORMAL_INTERVAL_SECONDS", 60)) * time.Second,
	}
}

func getEnv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	var n int
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil || n <= 0 {
		return fallback
	}
	return n
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg := loadConfig()

	if cfg.HostID == "" {
		logger.Error("DM_AGENT_HOST_ID is required")
		os.Exit(1)
	}

	caPEM, err := os.ReadFile(filepath.Join(cfg.CertDir, "ca.pem"))
	if err != nil {
		logger.Error("failed to read CA certificate — fetch it from the control plane's "+
			"/api/v1/system/ca-certificate endpoint and save it to $DM_AGENT_CERT_DIR/ca.pem first",
			"path", filepath.Join(cfg.CertDir, "ca.pem"), "err", err)
		os.Exit(1)
	}

	certPath := filepath.Join(cfg.CertDir, "agent-cert.pem")
	keyPath := filepath.Join(cfg.CertDir, "agent-key.pem")

	certPEM, certErr := os.ReadFile(certPath)
	keyPEM, keyErr := os.ReadFile(keyPath)
	if certErr != nil || keyErr != nil {
		if cfg.EnrollmentToken == "" {
			logger.Error("no cached agent certificate found and DM_AGENT_ENROLLMENT_TOKEN is not set")
			os.Exit(1)
		}
		logger.Info("no cached certificate found, enrolling", "control_plane", cfg.ControlPlaneAddr, "host_id", cfg.HostID)

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		result, err := transport.Enroll(ctx, transport.EnrollConfig{
			ControlPlaneAddr: cfg.ControlPlaneAddr,
			HostID:           cfg.HostID,
			Token:            cfg.EnrollmentToken,
			CAPEM:            caPEM,
		})
		cancel()
		if err != nil {
			logger.Error("enrollment failed", "err", err)
			os.Exit(1)
		}

		if err := os.MkdirAll(cfg.CertDir, 0o700); err != nil {
			logger.Error("failed to create cert directory", "err", err)
			os.Exit(1)
		}
		if err := os.WriteFile(certPath, result.CertPEM, 0o600); err != nil {
			logger.Error("failed to persist agent certificate", "err", err)
			os.Exit(1)
		}
		if err := os.WriteFile(keyPath, result.KeyPEM, 0o600); err != nil {
			logger.Error("failed to persist agent private key", "err", err)
			os.Exit(1)
		}
		certPEM, keyPEM = result.CertPEM, result.KeyPEM
		logger.Info("enrollment complete, certificate cached", "path", cfg.CertDir)
	} else {
		logger.Info("using cached agent certificate", "path", cfg.CertDir)
	}

	transportClient, err := transport.NewClient(cfg.ControlPlaneAddr, certPEM, keyPEM, caPEM)
	if err != nil {
		logger.Error("failed to build mTLS client", "err", err)
		os.Exit(1)
	}

	dockerClient := docker.NewClient(cfg.DockerSocket)
	{
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := dockerClient.Ping(ctx); err != nil {
			logger.Error("failed to reach Docker daemon", "socket", cfg.DockerSocket, "err", err)
			cancel()
			os.Exit(1)
		}
		cancel()
	}

	col := collector.New(dockerClient, transportClient, cfg.NormalInterval, logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Heartbeat on its own lightweight ticker, independent of the
	// (potentially much slower) collection interval — the control plane
	// needs to know the agent is alive even during a 60s NORMAL window.
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		sendHeartbeat := func() {
			hbCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			version, verr := dockerClient.Version(hbCtx)
			dockerVersion, osInfo := "", ""
			if verr == nil {
				dockerVersion, osInfo = version.Version, version.Os
			}
			if err := transportClient.Heartbeat(hbCtx, dockerVersion, osInfo); err != nil {
				logger.Warn("heartbeat failed", "err", err)
			}
		}
		sendHeartbeat()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				sendHeartbeat()
			}
		}
	}()

	logger.Info("agent started", "host_id", cfg.HostID, "control_plane", cfg.ControlPlaneAddr,
		"normal_interval", cfg.NormalInterval)
	// Report the initial container inventory immediately rather than
	// waiting up to a full NormalInterval — CollectNow is exactly the
	// mechanism §5's manual-live-inspection flow uses for "don't wait for
	// the next cycle", and a fresh agent boot deserves the same treatment:
	// a dashboard showing nothing for up to a minute after every restart
	// (agent upgrade, host reboot, crash-restart) reads as broken, not
	// as "waiting for its next scheduled poll".
	col.CollectNow()
	col.Run(ctx, cfg.HostID)
	logger.Info("agent shutting down")
}
