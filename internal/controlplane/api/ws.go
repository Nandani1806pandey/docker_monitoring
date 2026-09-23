// This file implements ARCHITECTURE.md §E.2's WebSocket channels for the
// browser dashboard. Each route just validates the entity exists (so a
// typo'd container ID fails fast with a normal HTTP error instead of
// silently upgrading to a socket that will never receive anything) and
// then hands the connection to the hub for its lifetime — the hub owns
// everything about the connection from that point on.
package api

import (
	"errors"
	"net/http"

	"github.com/nandani/docker-monitor/internal/controlplane/storage"
)

func (s *Server) handleWSEvents(w http.ResponseWriter, r *http.Request) {
	if s.hub == nil {
		writeError(w, http.StatusNotImplemented, "real-time updates are not enabled on this server")
		return
	}
	if err := s.hub.ServeTopic(w, r, "events"); err != nil {
		s.logger.Warn("ws: events upgrade failed", "err", err)
	}
}

func (s *Server) handleWSHostStats(w http.ResponseWriter, r *http.Request) {
	if s.hub == nil {
		writeError(w, http.StatusNotImplemented, "real-time updates are not enabled on this server")
		return
	}
	id := r.PathValue("id")
	if _, err := s.store.GetHost(r.Context(), id); errors.Is(err, storage.ErrNotFound) {
		writeError(w, http.StatusNotFound, "host not found")
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get host")
		return
	}
	if err := s.hub.ServeTopic(w, r, "hosts/"+id+"/stats"); err != nil {
		s.logger.Warn("ws: host stats upgrade failed", "host_id", id, "err", err)
	}
}

func (s *Server) handleWSContainerStats(w http.ResponseWriter, r *http.Request) {
	if s.hub == nil {
		writeError(w, http.StatusNotImplemented, "real-time updates are not enabled on this server")
		return
	}
	id := r.PathValue("id")
	if _, err := s.store.GetContainer(r.Context(), id); errors.Is(err, storage.ErrNotFound) {
		writeError(w, http.StatusNotFound, "container not found")
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get container")
		return
	}
	if err := s.hub.ServeTopic(w, r, "containers/"+id+"/stats"); err != nil {
		s.logger.Warn("ws: container stats upgrade failed", "container_id", id, "err", err)
	}
}
