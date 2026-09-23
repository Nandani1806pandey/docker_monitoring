package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nandani/docker-monitor/internal/controlplane/storage/memory"
	"github.com/nandani/docker-monitor/internal/shared/models"
)

func TestLogin_Success(t *testing.T) {
	store := memory.New()
	svc := NewService(store, time.Hour)
	ctx := context.Background()

	if _, err := svc.CreateUser(ctx, "alice@example.com", "Alice", "hunter2-but-longer", models.RoleAdmin); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	token, sess, err := svc.Login(ctx, "alice@example.com", "hunter2-but-longer")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if token == "" {
		t.Fatalf("expected non-empty token")
	}
	if sess.UserID == "" {
		t.Fatalf("expected session to reference a user")
	}

	u, err := svc.Authenticate(ctx, token)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if u.Email != "alice@example.com" {
		t.Fatalf("expected authenticated user alice@example.com, got %s", u.Email)
	}
	if u.LastLoginAt == nil {
		t.Fatalf("expected LastLoginAt to be set after login")
	}
}

func TestLogin_WrongPassword(t *testing.T) {
	store := memory.New()
	svc := NewService(store, time.Hour)
	ctx := context.Background()
	if _, err := svc.CreateUser(ctx, "bob@example.com", "Bob", "correct-password", models.RoleViewer); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	_, _, err := svc.Login(ctx, "bob@example.com", "wrong-password")
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("expected ErrInvalidCredentials, got %v", err)
	}
}

func TestLogin_UnknownEmail_SameErrorAsWrongPassword(t *testing.T) {
	store := memory.New()
	svc := NewService(store, time.Hour)
	ctx := context.Background()

	_, _, err := svc.Login(ctx, "nobody@example.com", "irrelevant")
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("expected ErrInvalidCredentials for unknown email (must not leak which emails exist), got %v", err)
	}
}

func TestCreateUser_DuplicateEmailRejected(t *testing.T) {
	store := memory.New()
	svc := NewService(store, time.Hour)
	ctx := context.Background()

	if _, err := svc.CreateUser(ctx, "dup@example.com", "First", "password-one", models.RoleViewer); err != nil {
		t.Fatalf("first CreateUser: %v", err)
	}
	_, err := svc.CreateUser(ctx, "dup@example.com", "Second", "password-two", models.RoleViewer)
	if !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("expected ErrEmailTaken, got %v", err)
	}
}

func TestAuthenticate_ExpiredSessionRejected(t *testing.T) {
	store := memory.New()
	// Negative TTL: any session created is already expired.
	svc := NewService(store, -time.Hour)
	ctx := context.Background()

	if _, err := svc.CreateUser(ctx, "carol@example.com", "Carol", "a-decent-password", models.RoleAdmin); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	token, _, err := svc.Login(ctx, "carol@example.com", "a-decent-password")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	if _, err := svc.Authenticate(ctx, token); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("expected ErrSessionInvalid for an expired session, got %v", err)
	}
}

func TestAuthenticate_UnknownTokenRejected(t *testing.T) {
	store := memory.New()
	svc := NewService(store, time.Hour)
	if _, err := svc.Authenticate(context.Background(), "totally-made-up-token"); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("expected ErrSessionInvalid, got %v", err)
	}
}

func TestLogout_RevokesSession(t *testing.T) {
	store := memory.New()
	svc := NewService(store, time.Hour)
	ctx := context.Background()

	if _, err := svc.CreateUser(ctx, "dave@example.com", "Dave", "another-password", models.RoleViewer); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	token, _, err := svc.Login(ctx, "dave@example.com", "another-password")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	if err := svc.Logout(ctx, token); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if _, err := svc.Authenticate(ctx, token); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("expected session to be invalid after logout, got %v", err)
	}
}

func TestLogout_UnknownTokenIsIdempotent(t *testing.T) {
	store := memory.New()
	svc := NewService(store, time.Hour)
	if err := svc.Logout(context.Background(), "never-existed"); err != nil {
		t.Fatalf("expected Logout on an unknown token to be a no-op, got %v", err)
	}
}

func TestBootstrapAdminIfNeeded_CreatesOnlyWhenEmpty(t *testing.T) {
	store := memory.New()
	svc := NewService(store, time.Hour)
	ctx := context.Background()

	created, err := svc.BootstrapAdminIfNeeded(ctx, "admin@example.com", "bootstrap-password")
	if err != nil {
		t.Fatalf("BootstrapAdminIfNeeded: %v", err)
	}
	if !created {
		t.Fatalf("expected bootstrap to create an admin on an empty store")
	}

	// A second call must not create a second admin or touch the first.
	created2, err := svc.BootstrapAdminIfNeeded(ctx, "someone-else@example.com", "different-password")
	if err != nil {
		t.Fatalf("second BootstrapAdminIfNeeded: %v", err)
	}
	if created2 {
		t.Fatalf("expected bootstrap to skip when a user already exists")
	}

	users, err := store.ListUsers(ctx)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(users) != 1 || users[0].Email != "admin@example.com" {
		t.Fatalf("expected exactly one user (the original admin), got %+v", users)
	}
}

func TestBootstrapAdminIfNeeded_SkipsWithoutCredentials(t *testing.T) {
	store := memory.New()
	svc := NewService(store, time.Hour)
	created, err := svc.BootstrapAdminIfNeeded(context.Background(), "", "")
	if err != nil {
		t.Fatalf("BootstrapAdminIfNeeded: %v", err)
	}
	if created {
		t.Fatalf("expected no bootstrap admin created with empty credentials")
	}
}
