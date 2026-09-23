package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/nandani/docker-monitor/internal/controlplane/storage"
	"github.com/nandani/docker-monitor/internal/shared/models"
)

var (
	// ErrInvalidCredentials is returned for both "no such user" and
	// "wrong password" — deliberately the same error either way. A
	// caller-visible difference between the two would let an attacker
	// enumerate which emails have accounts (ARCHITECTURE.md §43 — "auth
	// bypass" and account-enumeration are both in scope for what this
	// build's security testing should catch).
	ErrInvalidCredentials = errors.New("invalid email or password")
	ErrSessionInvalid     = errors.New("session invalid or expired")
	ErrEmailTaken         = errors.New("a user with that email already exists")
)

type Service struct {
	store      storage.Store
	sessionTTL time.Duration
}

func NewService(store storage.Store, sessionTTL time.Duration) *Service {
	return &Service{store: store, sessionTTL: sessionTTL}
}

// CreateUser hashes password and persists a new account. Returns
// ErrEmailTaken if the email is already in use — checked here rather than
// relying on a storage-layer uniqueness constraint, since the in-memory
// backend has none (a real SQL backend would additionally enforce this at
// the DB level; this check stays regardless, since relying solely on a
// DB constraint would mean this method's error contract differs by
// backend).
func (s *Service) CreateUser(ctx context.Context, email, displayName, password string, role models.Role) (*models.User, error) {
	if _, err := s.store.GetUserByEmail(ctx, email); err == nil {
		return nil, ErrEmailTaken
	} else if !errors.Is(err, storage.ErrNotFound) {
		return nil, fmt.Errorf("check existing user: %w", err)
	}

	hash, err := HashPassword(password)
	if err != nil {
		return nil, fmt.Errorf("hash password: %w", err)
	}

	u := &models.User{
		ID:           uuid.NewString(),
		Email:        email,
		DisplayName:  displayName,
		PasswordHash: hash,
		Role:         role,
	}
	if err := s.store.CreateUser(ctx, u); err != nil {
		return nil, fmt.Errorf("create user: %w", err)
	}
	return u, nil
}

// Login verifies credentials and, on success, creates a new server-side
// session. Returns the plaintext token (for the Set-Cookie header — this
// is the only place it's ever available) and the session record.
func (s *Service) Login(ctx context.Context, email, password string) (token string, session *models.Session, err error) {
	u, err := s.store.GetUserByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			// Still run VerifyPassword against a fixed dummy hash so a
			// nonexistent-email request takes roughly the same time as a
			// wrong-password one — otherwise response timing itself
			// becomes an account-enumeration oracle even though the
			// returned error is identical.
			_, _ = VerifyPassword(password, dummyHashForTimingParity)
			return "", nil, ErrInvalidCredentials
		}
		return "", nil, fmt.Errorf("look up user: %w", err)
	}

	ok, err := VerifyPassword(password, u.PasswordHash)
	if err != nil {
		return "", nil, fmt.Errorf("verify password: %w", err)
	}
	if !ok {
		return "", nil, ErrInvalidCredentials
	}

	plaintext, err := generateSessionToken()
	if err != nil {
		return "", nil, fmt.Errorf("generate session token: %w", err)
	}
	now := time.Now().UTC()
	sess := &models.Session{
		ID:        uuid.NewString(),
		UserID:    u.ID,
		TokenHash: hashToken(plaintext),
		CreatedAt: now,
		ExpiresAt: now.Add(s.sessionTTL),
	}
	if err := s.store.CreateSession(ctx, sess); err != nil {
		return "", nil, fmt.Errorf("create session: %w", err)
	}

	u.LastLoginAt = &now
	if err := s.store.UpdateUser(ctx, u); err != nil {
		// Non-fatal: the session is already created and valid. Losing
		// LastLoginAt bookkeeping shouldn't fail the login itself.
		return plaintext, sess, nil
	}

	return plaintext, sess, nil
}

// Logout revokes the session identified by the given plaintext token. A
// token that doesn't match any session is treated as already-logged-out
// (no error) — logout is idempotent by design.
func (s *Service) Logout(ctx context.Context, token string) error {
	sess, err := s.store.GetSessionByTokenHash(ctx, hashToken(token))
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("look up session: %w", err)
	}
	return s.store.DeleteSession(ctx, sess.ID)
}

// Authenticate resolves a plaintext session token to its User, or
// ErrSessionInvalid if the token doesn't match a live, unexpired session.
// This is what the HTTP middleware calls on every request.
func (s *Service) Authenticate(ctx context.Context, token string) (*models.User, error) {
	if token == "" {
		return nil, ErrSessionInvalid
	}
	sess, err := s.store.GetSessionByTokenHash(ctx, hashToken(token))
	if errors.Is(err, storage.ErrNotFound) {
		return nil, ErrSessionInvalid
	}
	if err != nil {
		return nil, fmt.Errorf("look up session: %w", err)
	}
	if time.Now().UTC().After(sess.ExpiresAt) {
		// Expired: clean it up opportunistically rather than leaving it
		// for a background reaper this build doesn't have yet (a
		// documented gap — see Service doc / README). Deletion failure
		// here still correctly rejects the request either way.
		_ = s.store.DeleteSession(ctx, sess.ID)
		return nil, ErrSessionInvalid
	}
	u, err := s.store.GetUser(ctx, sess.UserID)
	if err != nil {
		return nil, fmt.Errorf("look up session user: %w", err)
	}
	return u, nil
}

// BootstrapAdminIfNeeded creates an initial admin account if and only if
// the user store is completely empty — never touches an existing account
// set, so this is safe to call unconditionally on every startup. Returns
// (false, nil) if a bootstrap was skipped because users already exist.
func (s *Service) BootstrapAdminIfNeeded(ctx context.Context, email, password string) (created bool, err error) {
	count, err := s.store.CountUsers(ctx)
	if err != nil {
		return false, fmt.Errorf("count users: %w", err)
	}
	if count > 0 {
		return false, nil
	}
	if email == "" || password == "" {
		return false, nil
	}
	if _, err := s.CreateUser(ctx, email, "Administrator", password, models.RoleAdmin); err != nil {
		return false, err
	}
	return true, nil
}

// dummyHashForTimingParity is a real Argon2id hash of an arbitrary fixed
// password, computed once at package init so Login's not-found path can
// spend roughly the same CPU time as its wrong-password path.
var dummyHashForTimingParity = func() string {
	h, err := HashPassword("dummy-password-for-timing-parity-only")
	if err != nil {
		// HashPassword only fails if crypto/rand is broken, in which case
		// the process has much bigger problems than this fallback string
		// being unparsable — VerifyPassword will just return an error
		// that Login already ignores on this path.
		return ""
	}
	return h
}()
