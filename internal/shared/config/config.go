// Package config loads control-plane configuration from environment
// variables, with safe and lightweight defaults (see ARCHITECTURE.md §38-39).
// No config value here silently enables telemetry, external calls, or
// heavier-than-default polling.
package config

import (
	"fmt"
	"os"
	"strconv"
)

type Config struct {
	// Server
	HTTPAddr string // e.g. ":8080"

	// Storage — "memory" is the zero-dependency default used in this
	// skeleton; "postgres" is the documented production path (see
	// ARCHITECTURE.md §B) and is wired in behind the same Store interface
	// once the driver is added — no handler code changes when it lands.
	StorageDriver string
	DatabaseURL   string

	// Adaptive monitoring defaults (§39)
	NormalIntervalSeconds   int
	CriticalIntervalSeconds int
	ManualIntervalSeconds   int
	HysteresisSeconds       int

	// Historical metrics — disabled by default (§12, §27)
	HistoricalMetricsEnabled bool
	HistoricalRetentionDays  int

	// Session cookie secret (random per deployment; do not commit a real one)
	SessionSecret string

	// Agent-facing mTLS listener (§G, §45). The CA's cert/key are persisted
	// as PEM files under CADir (see cmd/controlplane's loadOrGenerateCA) —
	// a documented simplification, not a real secrets store.
	AgentAddr string
	CADir     string

	// Milestone 9: sessions/RBAC (§G, §26). SessionTTLHours controls how
	// long a login stays valid before re-authentication is required.
	// CookieSecure should be true for any HTTPS deployment (i.e. almost
	// always) and is only ever false for local HTTP-only development.
	// BootstrapAdmin{Email,Password} seed the very first account — see
	// auth.Service.BootstrapAdminIfNeeded's doc comment: a no-op once any
	// user exists, and a no-op here too if either is left empty (an
	// operator who wants no admin auto-created just doesn't set them and
	// creates the first account some other way).
	SessionTTLHours        int
	CookieSecure           bool
	BootstrapAdminEmail    string
	BootstrapAdminPassword string
}

func Load() (*Config, error) {
	cfg := &Config{
		HTTPAddr:                 getEnv("DM_HTTP_ADDR", ":8080"),
		StorageDriver:            getEnv("DM_STORAGE_DRIVER", "memory"),
		DatabaseURL:              getEnv("DM_DATABASE_URL", ""),
		NormalIntervalSeconds:    getEnvInt("DM_NORMAL_INTERVAL_SECONDS", 60),
		CriticalIntervalSeconds:  getEnvInt("DM_CRITICAL_INTERVAL_SECONDS", 10),
		ManualIntervalSeconds:    getEnvInt("DM_MANUAL_INTERVAL_SECONDS", 2),
		HysteresisSeconds:        getEnvInt("DM_HYSTERESIS_SECONDS", 60),
		HistoricalMetricsEnabled: getEnvBool("DM_HISTORICAL_METRICS_ENABLED", false),
		HistoricalRetentionDays:  getEnvInt("DM_HISTORICAL_RETENTION_DAYS", 90),
		SessionSecret:            getEnv("DM_SESSION_SECRET", ""),
		AgentAddr:                getEnv("DM_AGENT_ADDR", ":8443"),
		CADir:                    getEnv("DM_CA_DIR", "./data/ca"),

		BootstrapAdminEmail:    getEnv("DM_BOOTSTRAP_ADMIN_EMAIL", ""),
		BootstrapAdminPassword: getEnv("DM_BOOTSTRAP_ADMIN_PASSWORD", ""),

		SessionTTLHours: getEnvInt("DM_SESSION_TTL_HOURS", 24),
		CookieSecure:    getEnvBool("DM_COOKIE_SECURE", true),
	}

	if cfg.StorageDriver == "postgres" && cfg.DatabaseURL == "" {
		return nil, fmt.Errorf("DM_DATABASE_URL is required when DM_STORAGE_DRIVER=postgres")
	}
	if cfg.SessionSecret == "" {
		return nil, fmt.Errorf("DM_SESSION_SECRET must be set to a random value (do not use a default in production)")
	}
	return cfg, nil
}

func getEnv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	if v, ok := os.LookupEnv(key); ok {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func getEnvBool(key string, fallback bool) bool {
	if v, ok := os.LookupEnv(key); ok {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return fallback
}
