package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/nandani/docker-monitor/internal/controlplane/storage"
	"github.com/nandani/docker-monitor/internal/shared/models"
)

func (s *Store) CreateSession(ctx context.Context, sess *models.Session) error {
	if sess.CreatedAt.IsZero() {
		sess.CreatedAt = timeNow()
	}
	_, err := s.db.ExecContext(ctx, s.q(`
		INSERT INTO sessions (id, user_id, token_hash, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?)`),
		sess.ID, sess.UserID, sess.TokenHash, formatTime(sess.CreatedAt), formatTime(sess.ExpiresAt),
	)
	if err != nil {
		return fmt.Errorf("insert session: %w", err)
	}
	return nil
}

func (s *Store) GetSessionByTokenHash(ctx context.Context, tokenHash string) (*models.Session, error) {
	row := s.db.QueryRowContext(ctx, s.q(`
		SELECT id, user_id, token_hash, created_at, expires_at
		FROM sessions WHERE token_hash = ?`), tokenHash)

	var sess models.Session
	var createdAt, expiresAt string
	if err := row.Scan(&sess.ID, &sess.UserID, &sess.TokenHash, &createdAt, &expiresAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, storage.ErrNotFound
		}
		return nil, fmt.Errorf("get session: %w", err)
	}

	ct, err := parseTime(createdAt)
	if err != nil {
		return nil, fmt.Errorf("parse created_at: %w", err)
	}
	sess.CreatedAt = ct

	et, err := parseTime(expiresAt)
	if err != nil {
		return nil, fmt.Errorf("parse expires_at: %w", err)
	}
	sess.ExpiresAt = et

	return &sess, nil
}

func (s *Store) DeleteSession(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, s.q(`DELETE FROM sessions WHERE id = ?`), id)
	if err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return checkRowsAffected(res, "delete session")
}

// DeleteSessionsForUser is not required to error on "zero sessions
// deleted" — unlike DeleteSession (revoking one specific, presumably
// still-live session), "log out everywhere" for a user with no active
// sessions is a legitimate no-op, not a not-found condition.
func (s *Store) DeleteSessionsForUser(ctx context.Context, userID string) error {
	if _, err := s.db.ExecContext(ctx, s.q(`DELETE FROM sessions WHERE user_id = ?`), userID); err != nil {
		return fmt.Errorf("delete sessions for user: %w", err)
	}
	return nil
}
