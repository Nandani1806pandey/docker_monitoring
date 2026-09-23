package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/nandani/docker-monitor/internal/controlplane/storage"
	"github.com/nandani/docker-monitor/internal/shared/models"
)

// handleListEvents implements GET /api/v1/events?host=&container=&type=&severity=&from=&to=&limit=
func (s *Server) handleListEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := storage.EventFilter{Limit: 100}

	if v := q.Get("host"); v != "" {
		filter.HostID = &v
	}
	if v := q.Get("container"); v != "" {
		filter.ContainerID = &v
	}
	if v := q.Get("type"); v != "" {
		filter.Type = &v
	}
	if v := q.Get("severity"); v != "" {
		sev := models.EventSeverity(v)
		filter.Severity = &sev
	}
	if v := q.Get("from"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			filter.From = &t
		} else {
			writeError(w, http.StatusBadRequest, "invalid 'from' timestamp, expected RFC3339")
			return
		}
	}
	if v := q.Get("to"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			filter.To = &t
		} else {
			writeError(w, http.StatusBadRequest, "invalid 'to' timestamp, expected RFC3339")
			return
		}
	}
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			filter.Limit = n
		}
	}

	evts, err := s.store.ListEvents(r.Context(), filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list events")
		return
	}
	writeJSON(w, http.StatusOK, evts)
}
