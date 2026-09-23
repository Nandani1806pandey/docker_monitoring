// This file implements ARCHITECTURE.md §5's "manual real-time inspection
// mode": when a user opens a host/container's live monitoring page, the
// FSM should immediately move that entity to MANUAL_REALTIME; when they
// leave, it should fall back to CRITICAL (if still active) or NORMAL.
//
// Known limitation: the control plane has no push channel to the agent yet
// (no WebSocket hub — that's milestone 5). Opening a live view here updates
// the FSM's state immediately, but the agent only learns about the new
// mode on its *next* telemetry cycle (the response to that cycle's
// SendTelemetry carries the updated interval — see
// internal/controlplane/agentregistry's TelemetryResult and
// internal/agent/collector's SetInterval call). So "immediate" here means
// "immediate in the FSM's bookkeeping and in what this endpoint reports
// back", not yet "the agent starts polling every 2s the instant this
// request returns" — closing that last gap needs either a push channel or
// the agent polling its own directive out-of-band, neither implemented
// yet. Documented here rather than left as a surprise.
package api

import (
	"net/http"

	"github.com/nandani/docker-monitor/internal/controlplane/monitoring"
	"github.com/nandani/docker-monitor/internal/shared/models"
)

type liveResponse struct {
	Mode            models.MonitoringMode `json:"mode"`
	IntervalSeconds int                   `json:"interval_seconds"`
}

func (s *Server) writeLiveResponse(w http.ResponseWriter, kind monitoring.EntityKind, id string) {
	mode := s.monitor.EffectiveMode(kind, id)
	writeJSON(w, http.StatusOK, liveResponse{Mode: mode, IntervalSeconds: s.monitor.IntervalSeconds(mode)})
}

func (s *Server) handleHostLiveOpen(w http.ResponseWriter, r *http.Request) {
	if s.monitor == nil {
		writeError(w, http.StatusNotImplemented, "adaptive monitoring is not enabled on this server")
		return
	}
	id := r.PathValue("id")
	if _, err := s.store.GetHost(r.Context(), id); err != nil {
		writeError(w, http.StatusNotFound, "host not found")
		return
	}
	s.monitor.ManualViewerOpen(monitoring.EntityHost, id)
	s.writeLiveResponse(w, monitoring.EntityHost, id)
}

func (s *Server) handleHostLiveClose(w http.ResponseWriter, r *http.Request) {
	if s.monitor == nil {
		writeError(w, http.StatusNotImplemented, "adaptive monitoring is not enabled on this server")
		return
	}
	id := r.PathValue("id")
	if _, err := s.store.GetHost(r.Context(), id); err != nil {
		writeError(w, http.StatusNotFound, "host not found")
		return
	}
	s.monitor.ManualViewerClose(monitoring.EntityHost, id)
	s.writeLiveResponse(w, monitoring.EntityHost, id)
}

func (s *Server) handleContainerLiveOpen(w http.ResponseWriter, r *http.Request) {
	if s.monitor == nil {
		writeError(w, http.StatusNotImplemented, "adaptive monitoring is not enabled on this server")
		return
	}
	id := r.PathValue("id")
	if _, err := s.store.GetContainer(r.Context(), id); err != nil {
		writeError(w, http.StatusNotFound, "container not found")
		return
	}
	s.monitor.ManualViewerOpen(monitoring.EntityContainer, id)
	s.writeLiveResponse(w, monitoring.EntityContainer, id)
}

func (s *Server) handleContainerLiveClose(w http.ResponseWriter, r *http.Request) {
	if s.monitor == nil {
		writeError(w, http.StatusNotImplemented, "adaptive monitoring is not enabled on this server")
		return
	}
	id := r.PathValue("id")
	if _, err := s.store.GetContainer(r.Context(), id); err != nil {
		writeError(w, http.StatusNotFound, "container not found")
		return
	}
	s.monitor.ManualViewerClose(monitoring.EntityContainer, id)
	s.writeLiveResponse(w, monitoring.EntityContainer, id)
}
