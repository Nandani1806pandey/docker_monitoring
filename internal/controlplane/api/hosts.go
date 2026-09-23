package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/nandani/docker-monitor/internal/controlplane/storage"
	"github.com/nandani/docker-monitor/internal/shared/models"
)

type createHostRequest struct {
	Name    string          `json:"name"`
	Address string          `json:"address,omitempty"`
	Kind    models.HostKind `json:"kind"`
}

// handleCreateHost registers a new host. For a "remote" host this is the
// first half of agent enrollment (§45): the response includes the host
// record; a follow-up milestone adds the short-lived enrollment token the
// agent exchanges for its mTLS client certificate (§G).
func (s *Server) handleCreateHost(w http.ResponseWriter, r *http.Request) {
	var req createHostRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if req.Kind != models.HostKindLocal && req.Kind != models.HostKindRemote {
		writeError(w, http.StatusBadRequest, "kind must be 'local' or 'remote'")
		return
	}
	if req.Kind == models.HostKindRemote && req.Address == "" {
		writeError(w, http.StatusBadRequest, "address is required for remote hosts")
		return
	}

	h := &models.Host{
		ID:               uuid.NewString(),
		Name:             req.Name,
		Address:          req.Address,
		Kind:             req.Kind,
		ConnectionStatus: models.ConnectionStatusUnknown,
		MonitoringMode:   models.ModeNormal,
		CreatedAt:        time.Now().UTC(),
	}
	if err := s.store.CreateHost(r.Context(), h); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create host")
		return
	}

	_ = s.recorder.Record(r.Context(), eventsHostRegistered(h))
	writeJSON(w, http.StatusCreated, h)
}

func (s *Server) handleListHosts(w http.ResponseWriter, r *http.Request) {
	hosts, err := s.store.ListHosts(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list hosts")
		return
	}
	writeJSON(w, http.StatusOK, hosts)
}

func (s *Server) handleGetHost(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	h, err := s.store.GetHost(r.Context(), id)
	if errors.Is(err, storage.ErrNotFound) {
		writeError(w, http.StatusNotFound, "host not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get host")
		return
	}
	writeJSON(w, http.StatusOK, h)
}

func (s *Server) handleDeleteHost(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	h, err := s.store.GetHost(r.Context(), id)
	if errors.Is(err, storage.ErrNotFound) {
		writeError(w, http.StatusNotFound, "host not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get host")
		return
	}
	if err := s.store.DeleteHost(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete host")
		return
	}
	_ = s.recorder.Record(r.Context(), eventsHostRemoved(h))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListHostContainers(w http.ResponseWriter, r *http.Request) {
	hostID := r.PathValue("id")
	if _, err := s.store.GetHost(r.Context(), hostID); errors.Is(err, storage.ErrNotFound) {
		writeError(w, http.StatusNotFound, "host not found")
		return
	}
	containers, err := s.store.ListContainersByHost(r.Context(), hostID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list containers")
		return
	}
	writeJSON(w, http.StatusOK, containers)
}
