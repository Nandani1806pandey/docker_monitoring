package api

import "net/http"

type healthResponse struct {
	Status string `json:"status"`
}

// handleHealth is intentionally unauthenticated (liveness/readiness probes
// shouldn't need credentials) and intentionally minimal for now. Milestone
// 12 expands this into the full §37 observability payload (agent count,
// WS connections, queue sizes, collection latency) once those subsystems
// exist.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, healthResponse{Status: "ok"})
}
