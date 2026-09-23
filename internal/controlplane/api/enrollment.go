package api

import (
	"errors"
	"net/http"

	"github.com/nandani/docker-monitor/internal/controlplane/agentregistry"
)

type enrollmentTokenResponse struct {
	Token string `json:"token"`
}

// handleIssueEnrollmentToken generates the one-time token an operator
// pastes into the agent's config (README "Try the agent locally"). Requires
// the registry to be wired up (it isn't in builds/tests that only exercise
// the browser API in isolation).
func (s *Server) handleIssueEnrollmentToken(w http.ResponseWriter, r *http.Request) {
	if s.registry == nil {
		writeError(w, http.StatusNotImplemented, "agent enrollment is not enabled on this server")
		return
	}
	hostID := r.PathValue("id")
	token, err := s.registry.IssueEnrollmentToken(r.Context(), hostID)
	if err != nil {
		if errors.Is(err, agentregistry.ErrHostNotFound) {
			writeError(w, http.StatusNotFound, "host not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to issue enrollment token")
		return
	}
	writeJSON(w, http.StatusOK, enrollmentTokenResponse{Token: token})
}

// handleCACertificate serves the control plane's CA certificate in PEM
// form — what an agent pins at enrollment time to validate the control
// plane's server identity (ARCHITECTURE.md §G).
func (s *Server) handleCACertificate(w http.ResponseWriter, r *http.Request) {
	if s.ca == nil {
		writeError(w, http.StatusNotImplemented, "agent enrollment is not enabled on this server")
		return
	}
	w.Header().Set("Content-Type", "application/x-pem-file")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(s.ca.CertPEM())
}
