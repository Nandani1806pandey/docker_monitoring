// Package agentapi implements the agent-facing mTLS listener
// (ARCHITECTURE.md §E.3, §G). This is a separate HTTP server/port from the
// browser-facing REST API in internal/controlplane/api: different trust
// model (mTLS client certs, not sessions/API keys), different audience
// (agents, not browsers).
//
// Wire format note: ARCHITECTURE.md §E.3 specifies gRPC over mTLS. This
// build uses JSON-over-HTTPS with the same mTLS identity model instead —
// see README.md "Wire format note" for why. The mTLS identity model
// (verified client cert -> hostID) is what actually matters for the
// security boundary; swapping the wire format later only touches this
// package and internal/agent/transport.
package agentapi

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/nandani/docker-monitor/internal/controlplane/agentregistry"
	"github.com/nandani/docker-monitor/internal/shared/models"
)

type Server struct {
	registry *agentregistry.Registry
	logger   *slog.Logger
	mux      *http.ServeMux
}

func NewServer(registry *agentregistry.Registry, logger *slog.Logger) *Server {
	s := &Server{registry: registry, logger: logger, mux: http.NewServeMux()}
	s.routes()
	return s
}

func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) routes() {
	s.mux.HandleFunc("POST /agent/v1/enroll", s.handleEnroll)
	s.mux.HandleFunc("POST /agent/v1/heartbeat", s.requireClientCert(s.handleHeartbeat))
	s.mux.HandleFunc("POST /agent/v1/telemetry", s.requireClientCert(s.handleTelemetry))
}

// TLSConfig builds the *tls.Config for the agent-facing listener.
// ClientAuth is VerifyClientCertIfGiven rather than RequireAndVerifyClientCert
// at the TLS layer: /enroll is called by an agent that doesn't have a cert
// yet (that's the whole point of enrollment), so the listener can't demand
// one unconditionally. Per-route enforcement (requireClientCert below)
// is what actually protects /heartbeat and /telemetry — a request that
// reaches those handlers without a verified cert is rejected there.
func TLSConfig(serverCert tls.Certificate, clientCAs *x509.CertPool) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientAuth:   tls.VerifyClientCertIfGiven,
		ClientCAs:    clientCAs,
		MinVersion:   tls.VersionTLS12,
	}
}

// requireClientCert enforces that the connection presented a client
// certificate verified against the CA (guaranteed by VerifyClientCertIfGiven
// in TLSConfig — an unverifiable cert never reaches the handler at all,
// the TLS layer already dropped the connection). It derives hostID from
// the verified certificate's CommonName rather than trusting anything the
// request body claims, so a valid certificate for host-A can never act as
// host-B (ARCHITECTURE.md §G).
func (s *Server) requireClientCert(next func(w http.ResponseWriter, r *http.Request, hostID string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			writeError(w, http.StatusUnauthorized, "client certificate required")
			return
		}
		hostID := r.TLS.PeerCertificates[0].Subject.CommonName
		if hostID == "" {
			writeError(w, http.StatusUnauthorized, "client certificate missing identity")
			return
		}
		next(w, r, hostID)
	}
}

// --- /enroll ---

type enrollRequest struct {
	HostID string `json:"host_id"`
	Token  string `json:"token"`
	CSRPEM []byte `json:"csr_pem"`
}

type enrollResponse struct {
	CertPEM []byte `json:"cert_pem"`
}

func (s *Server) handleEnroll(w http.ResponseWriter, r *http.Request) {
	var req enrollRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.HostID == "" || req.Token == "" || len(req.CSRPEM) == 0 {
		writeError(w, http.StatusBadRequest, "host_id, token, and csr_pem are required")
		return
	}

	csrDER := decodeCSRPEM(req.CSRPEM)
	if csrDER == nil {
		writeError(w, http.StatusBadRequest, "csr_pem is not valid PEM")
		return
	}

	certPEM, err := s.registry.Enroll(r.Context(), req.HostID, req.Token, csrDER)
	if err != nil {
		if errors.Is(err, agentregistry.ErrTokenInvalid) || errors.Is(err, agentregistry.ErrHostNotFound) {
			writeError(w, http.StatusUnauthorized, "enrollment failed")
			return
		}
		s.logger.Error("enroll failed", "err", err)
		writeError(w, http.StatusInternalServerError, "enrollment failed")
		return
	}
	writeJSON(w, http.StatusOK, enrollResponse{CertPEM: certPEM})
}

// --- /heartbeat ---

type heartbeatRequest struct {
	DockerVersion string `json:"docker_version"`
	OSInfo        string `json:"os_info"`
}

func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request, hostID string) {
	var req heartbeatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := s.registry.Heartbeat(r.Context(), hostID, req.DockerVersion, req.OSInfo); err != nil {
		if errors.Is(err, agentregistry.ErrHostNotFound) {
			writeError(w, http.StatusNotFound, "host not found")
			return
		}
		s.logger.Error("heartbeat failed", "err", err, "host_id", hostID)
		writeError(w, http.StatusInternalServerError, "heartbeat failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- /telemetry ---

type telemetryContainer struct {
	DockerContainerID string                 `json:"docker_container_id"`
	Name              string                 `json:"name"`
	Image             string                 `json:"image"`
	Status            models.ContainerStatus `json:"status"`
	HealthStatus      models.HealthStatus    `json:"health_status"`
	RestartCount      int                    `json:"restart_count"`
}

type telemetryRequest struct {
	Containers []telemetryContainer `json:"containers"`
	Metrics    []models.Metric      `json:"metrics"`
}

type telemetryResponse struct {
	Mode            models.MonitoringMode `json:"mode"`
	IntervalSeconds int                   `json:"interval_seconds"`
}

func (s *Server) handleTelemetry(w http.ResponseWriter, r *http.Request, hostID string) {
	var req telemetryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	report := agentregistry.TelemetryReport{
		Containers: make([]agentregistry.ContainerReport, 0, len(req.Containers)),
		Metrics:    req.Metrics,
	}
	for _, c := range req.Containers {
		report.Containers = append(report.Containers, agentregistry.ContainerReport{
			DockerContainerID: c.DockerContainerID,
			Name:              c.Name,
			Image:             c.Image,
			Status:            c.Status,
			HealthStatus:      c.HealthStatus,
			RestartCount:      c.RestartCount,
		})
	}

	result, err := s.registry.IngestTelemetry(r.Context(), hostID, report)
	if err != nil {
		if errors.Is(err, agentregistry.ErrHostNotFound) {
			writeError(w, http.StatusNotFound, "host not found")
			return
		}
		s.logger.Error("telemetry ingest failed", "err", err, "host_id", hostID)
		writeError(w, http.StatusInternalServerError, "telemetry ingest failed")
		return
	}
	// Telling the agent what interval to use next is how the control
	// plane's FSM decision (ARCHITECTURE.md §F) actually reaches the
	// agent's collector — see internal/agent/collector's SetInterval call
	// after a successful SendTelemetry.
	writeJSON(w, http.StatusOK, telemetryResponse{Mode: result.Mode, IntervalSeconds: result.IntervalSeconds})
}

// --- helpers ---

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

type apiError struct {
	Error string `json:"error"`
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, apiError{Error: msg})
}
