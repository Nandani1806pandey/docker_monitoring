package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/nandani/docker-monitor/internal/controlplane/storage"
	"github.com/nandani/docker-monitor/internal/shared/models"
)

func (s *Store) CreateUser(ctx context.Context, u *models.User) error {
	if u.CreatedAt.IsZero() {
		u.CreatedAt = timeNow()
	}
	_, err := s.db.ExecContext(ctx, s.q(`
		INSERT INTO users (id, email, display_name, password_hash, role, created_at, last_login_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`),
		u.ID, u.Email, u.DisplayName, u.PasswordHash, string(u.Role), formatTime(u.CreatedAt), nullTime(u.LastLoginAt),
	)
	if err != nil {
		return fmt.Errorf("insert user: %w", err)
	}
	return nil
}

func (s *Store) GetUser(ctx context.Context, id string) (*models.User, error) {
	row := s.db.QueryRowContext(ctx, s.q(`
		SELECT id, email, display_name, password_hash, role, created_at, last_login_at
		FROM users WHERE id = ?`), id)
	u, err := scanUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, storage.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get user: %w", err)
	}
	return u, nil
}

func (s *Store) GetUserByEmail(ctx context.Context, email string) (*models.User, error) {
	row := s.db.QueryRowContext(ctx, s.q(`
		SELECT id, email, display_name, password_hash, role, created_at, last_login_at
		FROM users WHERE email = ?`), email)
	u, err := scanUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, storage.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get user by email: %w", err)
	}
	return u, nil
}

func (s *Store) ListUsers(ctx context.Context) ([]*models.User, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`
		SELECT id, email, display_name, password_hash, role, created_at, last_login_at
		FROM users ORDER BY created_at ASC`))
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()

	var out []*models.User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, fmt.Errorf("scan user: %w", err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	return out, nil
}

func (s *Store) UpdateUser(ctx context.Context, u *models.User) error {
	res, err := s.db.ExecContext(ctx, s.q(`
		UPDATE users SET email=?, display_name=?, password_hash=?, role=?, last_login_at=?
		WHERE id=?`),
		u.Email, u.DisplayName, u.PasswordHash, string(u.Role), nullTime(u.LastLoginAt), u.ID,
	)
	if err != nil {
		return fmt.Errorf("update user: %w", err)
	}
	return checkRowsAffected(res, "update user")
}

func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count users: %w", err)
	}
	return n, nil
}

func scanUser(row rowScanner) (*models.User, error) {
	var u models.User
	var role, createdAt string
	var lastLoginAt sql.NullString
	if err := row.Scan(&u.ID, &u.Email, &u.DisplayName, &u.PasswordHash, &role, &createdAt, &lastLoginAt); err != nil {
		return nil, err
	}
	u.Role = models.Role(role)

	t, err := parseTime(createdAt)
	if err != nil {
		return nil, fmt.Errorf("parse created_at: %w", err)
	}
	u.CreatedAt = t

	if lastLoginAt.Valid {
		t, err := parseTime(lastLoginAt.String)
		if err != nil {
			return nil, fmt.Errorf("parse last_login_at: %w", err)
		}
		u.LastLoginAt = &t
	}
	return &u, nil
}
