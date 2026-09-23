// Package agentregistry implements the control-plane side of agent
// lifecycle management (ARCHITECTURE.md §8-9, §G): issuing single-use
// enrollment tokens, verifying them and signing the agent's mTLS
// certificate, and ingesting heartbeats/telemetry from already-enrolled
// agents.
package agentregistry

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/nandani/docker-monitor/internal/controlplane/events"
	"github.com/nandani/docker-monitor/internal/controlplane/monitoring"
	"github.com/nandani/docker-monitor/internal/controlplane/pki"
	"github.com/nandani/docker-monitor/internal/controlplane/storage"
	"github.com/nandani/docker-monitor/internal/shared/models"
)

var (
	ErrTokenInvalid = errors.New("enrollment token invalid or already used")
	ErrHostNotFound = errors.New("host not found")
)

const (
	enrollmentTokenTTL   = 15 * time.Minute
	agentCertValidFor    = 90 * 24 * time.Hour // 90 days; rotation handled by re-enrollment for this milestone (see Registry doc comment)
	enrollmentTokenBytes = 32
)

// HistoricalMetricStore persists a time series of metric samples,
// separate from storage.MetricStore's latest-only cache (ARCHITECTURE.md
// §12/Phase B). Registry treats it exactly like MetricBroadcaster: wholly
// optional, nil-safe, and failures here must never abort telemetry
// ingestion — a dashboard losing history for a few samples during a DB
// hiccup is acceptable; losing live monitoring because history couldn't
// write is not.
type HistoricalMetricStore interface {
	RecordHistoricalSample(ctx context.Context, m *models.Metric) error
}

type Registry struct {
	store    storage.Store
	ca       *pki.CA
	recorder *events.Recorder
	monitor  *monitoring.Engine

	metricBroadcaster MetricBroadcaster     // optional; see SetMetricBroadcaster
	historicalStore   HistoricalMetricStore // optional; see SetHistoricalStore
}

// MetricBroadcaster is the narrow slice of ws.Hub that Registry needs for
// live per-entity stats streams. Declared locally for the same reason
// events.Broadcaster is: this package shouldn't need to import the
// WebSocket layer just to type this field.
type MetricBroadcaster interface {
	Broadcast(topic string, v interface{})
}

// New builds a Registry. monitor may be nil — in which case IngestTelemetry
// skips all FSM evaluation and always reports NORMAL/60s back to the agent
// — but every production call site should pass a real Engine so telemetry
// actually drives the adaptive monitoring behaviour described in
// ARCHITECTURE.md §F.
func New(store storage.Store, ca *pki.CA, recorder *events.Recorder, monitor *monitoring.Engine) *Registry {
	return &Registry{store: store, ca: ca, recorder: recorder, monitor: monitor}
}

// SetMetricBroadcaster enables live push of incoming metrics over
// /ws/hosts/{id}/stats and /ws/containers/{id}/stats (ARCHITECTURE.md
// §E.2). Optional and nil-safe, same rationale as events.Recorder's
// SetBroadcaster: additive wiring, no constructor change, every existing
// test keeps working untouched.
func (r *Registry) SetMetricBroadcaster(b MetricBroadcaster) {
	r.metricBroadcaster = b
}

// SetHistoricalStore enables opt-in historical persistence (§12, §27:
// disabled by default — the caller is responsible for only calling this
// when DM_HISTORICAL_METRICS_ENABLED is true, Registry itself has no
// config awareness). Additive wiring, same rationale as
// SetMetricBroadcaster: no constructor change, every existing test keeps
// working with historicalStore nil.
func (r *Registry) SetHistoricalStore(h HistoricalMetricStore) {
	r.historicalStore = h
}

func metricTopic(kind monitoring.EntityKind, id string) string {
	switch kind {
	case monitoring.EntityHost:
		return "hosts/" + id + "/stats"
	default:
		return "containers/" + id + "/stats"
	}
}

// IssueEnrollmentToken generates a single-use token for a remote host that
// was just registered via the browser API (§8, §45 — this is what an
// operator pastes into the agent's config/systemd unit alongside the CA
// cert). Only the hash is persisted; the plaintext is returned once, same
// handling as an API key (§25).
func (r *Registry) IssueEnrollmentToken(ctx context.Context, hostID string) (string, error) {
	host, err := r.store.GetHost(ctx, hostID)
	if err != nil {
		return "", ErrHostNotFound
	}
	raw := make([]byte, enrollmentTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	hash := hashToken(token)

	now := time.Now().UTC()
	host.EnrollmentTokenHash = hash
	host.EnrollmentIssuedAt = &now
	host.AgentEnrolled = false
	if err := r.store.UpdateHost(ctx, host); err != nil {
		return "", fmt.Errorf("persist enrollment token: %w", err)
	}
	return token, nil
}

// Enroll validates a presented token against the host record, signs the
// agent's CSR, and marks the token consumed (single-use — a second attempt
// with the same token fails even if it hasn't expired). CommonName on the
// issued cert is always hostID, never taken from the CSR, so the agent
// cannot request an identity other than the one it was enrolled for.
func (r *Registry) Enroll(ctx context.Context, hostID, token string, csrDER []byte) (certPEM []byte, err error) {
	host, err := r.store.GetHost(ctx, hostID)
	if err != nil {
		return nil, ErrHostNotFound
	}
	if host.EnrollmentTokenHash == "" || host.AgentEnrolled {
		return nil, ErrTokenInvalid
	}
	if host.EnrollmentIssuedAt == nil || time.Since(*host.EnrollmentIssuedAt) > enrollmentTokenTTL {
		return nil, ErrTokenInvalid
	}
	if subtle.ConstantTimeCompare([]byte(hashToken(token)), []byte(host.EnrollmentTokenHash)) != 1 {
		return nil, ErrTokenInvalid
	}

	certPEM, err = r.ca.IssueAgentCert(csrDER, hostID, agentCertValidFor)
	if err != nil {
		return nil, fmt.Errorf("issue certificate: %w", err)
	}

	host.AgentEnrolled = true
	host.EnrollmentTokenHash = "" // single-use: burn it immediately
	host.CertFingerprint = fingerprint(certPEM)
	host.ConnectionStatus = models.ConnectionStatusUnknown // becomes "online" on first heartbeat
	if err := r.store.UpdateHost(ctx, host); err != nil {
		return nil, fmt.Errorf("persist enrollment: %w", err)
	}

	_ = r.recorder.Record(ctx, events.RecordInput{
		Type:     "host.agent_enrolled",
		Severity: models.SeverityInfo,
		HostID:   &host.ID,
		Message:  "Agent enrolled for host " + host.Name,
	})
	return certPEM, nil
}

// Heartbeat records that an already-enrolled agent is alive. The caller
// (agentapi handler) is responsible for having already verified the mTLS
// client certificate and extracted hostID from its CommonName — this
// method trusts the hostID it's given.
func (r *Registry) Heartbeat(ctx context.Context, hostID, dockerVersion, osInfo string) error {
	host, err := r.store.GetHost(ctx, hostID)
	if err != nil {
		return ErrHostNotFound
	}
	wasOffline := host.ConnectionStatus != models.ConnectionStatusOnline
	now := time.Now().UTC()
	host.LastHeartbeatAt = &now
	host.ConnectionStatus = models.ConnectionStatusOnline
	if dockerVersion != "" {
		host.DockerVersion = dockerVersion
	}
	if osInfo != "" {
		host.OSInfo = osInfo
	}
	if err := r.store.UpdateHost(ctx, host); err != nil {
		return fmt.Errorf("persist heartbeat: %w", err)
	}
	if wasOffline {
		_ = r.recorder.Record(ctx, events.RecordInput{
			Type:     "host.connected",
			Severity: models.SeverityInfo,
			HostID:   &host.ID,
			Message:  "Host " + host.Name + " came online",
		})
	}
	return nil
}

// ContainerReport is one container's discovery snapshot as sent by the
// agent's telemetry batch (ARCHITECTURE.md §E.3 StreamTelemetry).
type ContainerReport struct {
	DockerContainerID string
	Name              string
	Image             string
	Status            models.ContainerStatus
	HealthStatus      models.HealthStatus
	RestartCount      int
}

// TelemetryReport is one batch from the agent: container discovery plus
// metrics for the host and/or its containers, collected at whatever
// interval the entity's current effective_mode dictated (§F.2).
type TelemetryReport struct {
	Containers []ContainerReport
	Metrics    []models.Metric
}

// TelemetryResult is what IngestTelemetry hands back to the caller (the
// agentapi handler, which relays it to the agent) — the single effective
// polling mode/interval this agent should use for its next cycle.
//
// This is a host-granularity answer, not per-container: the wire protocol
// carries one telemetry batch and one response per host per cycle, so
// per-entity intervals aren't representable yet the way ARCHITECTURE.md §3
// envisions ("only the affected host/container/resource should enter
// high-frequency monitoring"). FastestMode across the host and every
// container in this batch is used instead — a single hot container makes
// the whole host's collector poll faster, which is conservative (never
// slower than the FSM wants) at the cost of some unnecessary bandwidth on
// otherwise-quiet containers sharing that host. Splitting this into
// per-container directives is future work, not a design endpoint.
type TelemetryResult struct {
	Mode            models.MonitoringMode
	IntervalSeconds int
}

// IngestTelemetry upserts discovered containers, records metrics for a
// single batch from one host's agent, and evaluates every container-health
// and metric sample against the monitoring FSM (ARCHITECTURE.md §F).
// Transitions are persisted to the event log; the returned TelemetryResult
// tells the agent what polling cadence to use next.
func (r *Registry) IngestTelemetry(ctx context.Context, hostID string, report TelemetryReport) (*TelemetryResult, error) {
	host, err := r.store.GetHost(ctx, hostID)
	if err != nil {
		return nil, ErrHostNotFound
	}

	// The agent only ever knows a container by Docker's own ID — it has no
	// reason to know the control plane's internal ID scheme. Metrics in the
	// same batch are keyed by that same Docker ID (see collector.go), so we
	// build the Docker-ID -> internal-ID mapping up front and use it to
	// translate metric entity IDs, before anything is persisted. Without
	// this translation, GET .../containers/{id}/stats would never find a
	// metric recorded under a different key than the container it's
	// supposedly for.
	internalID := make(map[string]string, len(report.Containers))
	existingByDockerID := make(map[string]*models.Container, len(report.Containers))
	for _, cr := range report.Containers {
		existing, _ := findContainerByDockerID(ctx, r.store, hostID, cr.DockerContainerID)
		// Deterministic (not random) so re-ingesting telemetry for the same
		// container always resolves to the same ID without needing a
		// lookup-then-create race window, and URL-safe — unlike an earlier
		// version of this method, which concatenated hostID+"/"+dockerID
		// and broke every REST route that takes a container ID as a single
		// path segment.
		id := uuid.NewSHA1(uuid.NameSpaceOID, []byte(hostID+"/"+cr.DockerContainerID)).String()
		internalID[cr.DockerContainerID] = id
		existingByDockerID[cr.DockerContainerID] = existing
	}

	// Evaluate container-health transitions (crash / unhealthy / repeated
	// restart) before upserting, so the upsert below can write the
	// resulting effective mode in one pass rather than updating twice.
	if r.monitor != nil {
		for _, cr := range report.Containers {
			id := internalID[cr.DockerContainerID]
			// Baseline for restart-count comparison: if we've never seen
			// this container before, its current RestartCount is history,
			// not something that just happened — comparing against a
			// hardcoded 0 baseline would flag every newly discovered
			// container with a nonzero restart count as "just restarted".
			// Baseline against its own current count instead, so only a
			// count that increases *after* this point counts as an event.
			prevRestart := cr.RestartCount
			if existing := existingByDockerID[cr.DockerContainerID]; existing != nil {
				prevRestart = existing.RestartCount
			}
			if transitioned, ev := r.monitor.EvaluateContainerHealth(id, cr.Status, cr.HealthStatus, cr.RestartCount, prevRestart); transitioned {
				r.recordTransition(ctx, hostID, id, ev)
			}
		}
	}

	// Translate and record metrics, evaluating each against the FSM.
	for i := range report.Metrics {
		m := &report.Metrics[i]
		switch m.EntityType {
		case "host":
			m.EntityID = hostID
			if r.monitor != nil {
				if transitioned, ev := r.monitor.EvaluateMetric(monitoring.EntityHost, hostID, *m); transitioned {
					r.recordTransition(ctx, hostID, hostID, ev)
				}
				// Overwrite whatever mode the agent's own metric batch
				// carried with the control plane's authoritative,
				// just-evaluated EffectiveMode. The agent labels metrics
				// from its own last-known interval (see
				// internal/agent/collector's modeForInterval) purely as a
				// local approximation for its own bookkeeping — it has no
				// visibility into the FSM's actual critical_active/
				// manual_viewers state, and that approximation can be
				// wrong whenever two configured interval values collide
				// (e.g. NormalIntervalSeconds == ManualIntervalSeconds).
				// This is the one place that state is actually known, so
				// this is the one place the label should be assigned —
				// anything stored or broadcast to a dashboard must never
				// show a guessed mode when the real one is sitting right
				// here (§50: never imply something about freshness/state
				// that isn't true).
				m.Mode = r.monitor.EffectiveMode(monitoring.EntityHost, hostID)
			}
			if r.metricBroadcaster != nil {
				r.metricBroadcaster.Broadcast(metricTopic(monitoring.EntityHost, hostID), m)
			}
		case "container":
			mapped, ok := internalID[m.EntityID]
			if !ok {
				// Metric for a container that wasn't in this batch's
				// discovery list (e.g. it exited between discovery and
				// stats collection) — nothing to translate against, so
				// there's no stable ID to store it under. Skip rather than
				// record an orphaned metric no REST route can ever reach.
				continue
			}
			m.EntityID = mapped
			if r.monitor != nil {
				if transitioned, ev := r.monitor.EvaluateMetric(monitoring.EntityContainer, mapped, *m); transitioned {
					r.recordTransition(ctx, hostID, mapped, ev)
				}
				// Same reasoning as the host case above.
				m.Mode = r.monitor.EffectiveMode(monitoring.EntityContainer, mapped)
			}
			if r.metricBroadcaster != nil {
				r.metricBroadcaster.Broadcast(metricTopic(monitoring.EntityContainer, mapped), m)
			}
		}
		if err := r.store.RecordMetric(ctx, m); err != nil {
			return nil, fmt.Errorf("record metric: %w", err)
		}
		// Historical persistence is best-effort and additive: a failure
		// here must never fail telemetry ingestion or the agent's
		// polling contract (§12: "the monitoring engine must continue
		// functioning even if historical persistence temporarily
		// fails"). Errors are swallowed rather than logged through r
		// itself since Registry has no logger field; callers wanting
		// visibility into historical-write failures should wrap
		// HistoricalMetricStore with their own logging decorator.
		if r.historicalStore != nil {
			_ = r.historicalStore.RecordHistoricalSample(ctx, m)
		}
	}

	containerModes := make([]models.MonitoringMode, 0, len(report.Containers))
	for _, cr := range report.Containers {
		id := internalID[cr.DockerContainerID]
		mode := models.ModeNormal
		if r.monitor != nil {
			mode = r.monitor.EffectiveMode(monitoring.EntityContainer, id)
		}
		containerModes = append(containerModes, mode)

		now := time.Now().UTC()
		c := &models.Container{
			ID:                id,
			HostID:            hostID,
			DockerContainerID: cr.DockerContainerID,
			Name:              cr.Name,
			Image:             cr.Image,
			Status:            cr.Status,
			HealthStatus:      cr.HealthStatus,
			RestartCount:      cr.RestartCount,
			MonitoringMode:    mode,
			LastSeenAt:        &now,
		}
		if existing := existingByDockerID[cr.DockerContainerID]; existing != nil {
			c.CreatedAt = existing.CreatedAt
			c.Tags = existing.Tags
		}
		if err := r.store.UpsertContainer(ctx, c); err != nil {
			return nil, fmt.Errorf("upsert container %s: %w", cr.DockerContainerID, err)
		}
	}

	hostMode := models.ModeNormal
	if r.monitor != nil {
		hostMode = r.monitor.EffectiveMode(monitoring.EntityHost, hostID)
	}
	if host.MonitoringMode != hostMode {
		host.MonitoringMode = hostMode
		if err := r.store.UpdateHost(ctx, host); err != nil {
			return nil, fmt.Errorf("persist host monitoring mode: %w", err)
		}
	}

	fastest := monitoring.FastestMode(append([]models.MonitoringMode{hostMode}, containerModes...)...)
	intervalSeconds := 60
	if r.monitor != nil {
		intervalSeconds = r.monitor.IntervalSeconds(fastest)
	}
	return &TelemetryResult{Mode: fastest, IntervalSeconds: intervalSeconds}, nil
}

// recordTransition maps one FSM TransitionEvent onto the event log. Every
// entry into CRITICAL is CRITICAL severity; recovery back to NORMAL is
// INFO — matching the severities already used for host connect/disconnect
// events elsewhere in this package.
func (r *Registry) recordTransition(ctx context.Context, hostID, entityID string, ev *monitoring.TransitionEvent) {
	severity := models.SeverityInfo
	if ev.EnteredMode == models.ModeCritical {
		severity = models.SeverityCritical
	}
	var containerID *string
	if ev.EntityKind == monitoring.EntityContainer {
		containerID = &entityID
	}
	hID := hostID
	_ = r.recorder.Record(ctx, events.RecordInput{
		Type:        "monitoring." + ev.Reason,
		Severity:    severity,
		HostID:      &hID,
		ContainerID: containerID,
		Message:     fmt.Sprintf("%s %s entered %s (%s)", ev.EntityKind, entityID, ev.EnteredMode, ev.Reason),
	})
}

func findContainerByDockerID(ctx context.Context, store storage.Store, hostID, dockerID string) (*models.Container, error) {
	containers, err := store.ListContainersByHost(ctx, hostID)
	if err != nil {
		return nil, err
	}
	for _, c := range containers {
		if c.DockerContainerID == dockerID {
			return c, nil
		}
	}
	return nil, storage.ErrNotFound
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func fingerprint(certPEM []byte) string {
	sum := sha256.Sum256(certPEM)
	return hex.EncodeToString(sum[:])
}
