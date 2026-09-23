package api

import (
	"errors"
	"net/http"

	"github.com/nandani/docker-monitor/internal/controlplane/events"
	"github.com/nandani/docker-monitor/internal/controlplane/storage"
	"github.com/nandani/docker-monitor/internal/shared/models"
)

func (s *Server) handleGetContainer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	c, err := s.store.GetContainer(r.Context(), id)
	if errors.Is(err, storage.ErrNotFound) {
		writeError(w, http.StatusNotFound, "container not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get container")
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// --- event payload builders (kept here so handlers stay short) ---

func eventsHostRegistered(h *models.Host) events.RecordInput {
	return events.RecordInput{
		Type:     "host.connected",
		Severity: models.SeverityInfo,
		HostID:   &h.ID,
		Message:  "Host " + h.Name + " registered",
	}
}

func eventsHostRemoved(h *models.Host) events.RecordInput {
	return events.RecordInput{
		Type:     "host.disconnected",
		Severity: models.SeverityInfo,
		HostID:   &h.ID,
		Message:  "Host " + h.Name + " removed",
	}
}
