package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/nandani/docker-monitor/internal/controlplane/auth"
)

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type loginResponse struct {
	UserID      string `json:"user_id"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
}

// handleLogin is the one route reachable with no session at all (see
// isPublicRoute). On success it sets the session cookie
// (HttpOnly/SameSite=Strict, Secure per s.cookieSecure — ARCHITECTURE.md
// §26) and returns the user's own (non-sensitive) profile; the raw
// session token is never present in the JSON body, only the cookie.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if s.authService == nil {
		writeError(w, http.StatusNotImplemented, "authentication is not configured on this server")
		return
	}
	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Email == "" || req.Password == "" {
		writeError(w, http.StatusBadRequest, "email and password are required")
		return
	}

	token, sess, err := s.authService.Login(r.Context(), req.Email, req.Password)
	if err != nil {
		if errors.Is(err, auth.ErrInvalidCredentials) {
			// Same status and message regardless of whether the email
			// exists — see auth.Service.Login's doc comment on why.
			writeError(w, http.StatusUnauthorized, "invalid email or password")
			return
		}
		s.logger.Error("login failed", "err", err)
		writeError(w, http.StatusInternalServerError, "login failed")
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.cookieSecure,
		SameSite: http.SameSiteStrictMode,
		Expires:  sess.ExpiresAt,
	})

	u, err := s.store.GetUser(r.Context(), sess.UserID)
	if err != nil {
		s.logger.Error("failed to load user after login", "err", err)
		writeError(w, http.StatusInternalServerError, "login failed")
		return
	}
	writeJSON(w, http.StatusOK, loginResponse{
		UserID: u.ID, Email: u.Email, DisplayName: u.DisplayName, Role: string(u.Role),
	})
}

// handleLogout revokes the caller's session and clears the cookie.
// Idempotent (see auth.Service.Logout) — calling it with no session, or a
// session that's already gone, is not an error.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if s.authService == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	token := sessionTokenFromRequest(r)
	if token != "" {
		if err := s.authService.Logout(r.Context(), token); err != nil {
			s.logger.Error("logout failed", "err", err)
			writeError(w, http.StatusInternalServerError, "logout failed")
			return
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   s.cookieSecure,
		SameSite: http.SameSiteStrictMode,
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
	})
	w.WriteHeader(http.StatusNoContent)
}

// handleMe returns the caller's own profile — what a dashboard calls on
// load to know who's logged in and whether to show admin-only controls.
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	user, ok := userFromContext(r.Context())
	if !ok {
		// Only reachable in stub mode (no auth.Service wired) — there's no
		// real principal to report.
		writeError(w, http.StatusNotImplemented, "authentication is not configured on this server")
		return
	}
	writeJSON(w, http.StatusOK, loginResponse{
		UserID: user.ID, Email: user.Email, DisplayName: user.DisplayName, Role: string(user.Role),
	})
}
