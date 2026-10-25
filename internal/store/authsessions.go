package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// AuthSession is a logged-in browser session. The cookie carries only the ID;
// everything that decides access lives here, so logout and device revocation
// take effect immediately.
type AuthSession struct {
	ID         string
	DeviceID   string
	NodeID     string
	CreatedAt  time.Time
	LastSeenAt time.Time
	StepupAt   *time.Time
	RevokedAt  *time.Time
}

// CreateAuthSession stores a new session.
func (s *Store) CreateAuthSession(ctx context.Context, a AuthSession) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO auth_sessions (id, device_id, node_id, created_at, last_seen_at, stepup_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		a.ID, a.DeviceID, a.NodeID, rfc(a.CreatedAt), rfc(a.LastSeenAt), nullRFC(a.StepupAt))
	if err != nil {
		return fmt.Errorf("create auth session: %w", err)
	}
	return nil
}

// AuthSessionByID returns a live session belonging to a live device.
func (s *Store) AuthSessionByID(ctx context.Context, id string) (AuthSession, error) {
	var (
		a                 AuthSession
		created, lastSeen string
		stepup, revoked   sql.NullString
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT a.id, a.device_id, a.node_id, a.created_at, a.last_seen_at, a.stepup_at, a.revoked_at
		   FROM auth_sessions a
		   JOIN devices d ON d.id = a.device_id
		  WHERE a.id = ? AND a.revoked_at IS NULL AND d.revoked_at IS NULL`, id).
		Scan(&a.ID, &a.DeviceID, &a.NodeID, &created, &lastSeen, &stepup, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return AuthSession{}, ErrNotFound
	}
	if err != nil {
		return AuthSession{}, fmt.Errorf("read auth session: %w", err)
	}
	if a.CreatedAt, err = parseTime(created); err != nil {
		return AuthSession{}, err
	}
	if a.LastSeenAt, err = parseTime(lastSeen); err != nil {
		return AuthSession{}, err
	}
	if a.StepupAt, err = parseNullTime(stepup); err != nil {
		return AuthSession{}, err
	}
	if a.RevokedAt, err = parseNullTime(revoked); err != nil {
		return AuthSession{}, err
	}
	return a, nil
}

// TouchAuthSession slides the idle timeout forward.
func (s *Store) TouchAuthSession(ctx context.Context, id string, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE auth_sessions SET last_seen_at = ? WHERE id = ?`, rfc(at), id)
	if err != nil {
		return fmt.Errorf("touch auth session: %w", err)
	}
	return nil
}

// MarkStepup records a fresh passkey assertion on a session.
func (s *Store) MarkStepup(ctx context.Context, id string, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE auth_sessions SET stepup_at = ?, last_seen_at = ? WHERE id = ?`,
		rfc(at), rfc(at), id)
	if err != nil {
		return fmt.Errorf("mark stepup: %w", err)
	}
	return nil
}

// RevokeAuthSession ends one session (logout).
func (s *Store) RevokeAuthSession(ctx context.Context, id string, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE auth_sessions SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`, rfc(at), id)
	if err != nil {
		return fmt.Errorf("revoke auth session: %w", err)
	}
	return nil
}

// RevokeAllAuthSessions ends every session (lockdown).
func (s *Store) RevokeAllAuthSessions(ctx context.Context, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE auth_sessions SET revoked_at = ? WHERE revoked_at IS NULL`, rfc(at))
	if err != nil {
		return fmt.Errorf("revoke all auth sessions: %w", err)
	}
	return nil
}

// PurgeExpiredAuthSessions deletes sessions past their absolute lifetime.
// Expiry is enforced on every request regardless; this only keeps the table
// from growing without bound.
func (s *Store) PurgeExpiredAuthSessions(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM auth_sessions WHERE created_at < ?`, rfc(before))
	if err != nil {
		return 0, fmt.Errorf("purge auth sessions: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ---------- login failure tracking / lockout ----------

// LoginFailure is the lockout counter for one node.
type LoginFailure struct {
	NodeID      string
	Count       int
	FirstAt     time.Time
	LockedUntil *time.Time
}

// RecordLoginFailure increments the counter for a node and, once it reaches
// max, sets a lock until now+lockout. Counting per node rather than per
// credential means an attacker cannot reset the counter by varying the
// credential ID.
func (s *Store) RecordLoginFailure(ctx context.Context, nodeID string, now time.Time, max int, lockout time.Duration) (LoginFailure, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return LoginFailure{}, fmt.Errorf("record login failure: begin: %w", err)
	}
	defer tx.Rollback()

	var (
		count   int
		firstAt string
		locked  sql.NullString
	)
	err = tx.QueryRowContext(ctx,
		`SELECT count, first_at, locked_until FROM login_failures WHERE node_id = ?`, nodeID).
		Scan(&count, &firstAt, &locked)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		count = 0
	case err != nil:
		return LoginFailure{}, fmt.Errorf("record login failure: read: %w", err)
	}

	count++
	var lockedUntil *time.Time
	if count >= max {
		u := now.Add(lockout)
		lockedUntil = &u
	}
	first := now
	if count > 1 && firstAt != "" {
		if t, err := parseTime(firstAt); err == nil {
			first = t
		}
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO login_failures (node_id, count, first_at, locked_until) VALUES (?, ?, ?, ?)
		 ON CONFLICT(node_id) DO UPDATE SET count = excluded.count, first_at = excluded.first_at,
		                                   locked_until = excluded.locked_until`,
		nodeID, count, rfc(first), nullRFC(lockedUntil)); err != nil {
		return LoginFailure{}, fmt.Errorf("record login failure: write: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return LoginFailure{}, fmt.Errorf("record login failure: commit: %w", err)
	}
	return LoginFailure{NodeID: nodeID, Count: count, FirstAt: first, LockedUntil: lockedUntil}, nil
}

// LoginLockedUntil reports the lock expiry for a node, or nil if not locked.
func (s *Store) LoginLockedUntil(ctx context.Context, nodeID string) (*time.Time, error) {
	var locked sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT locked_until FROM login_failures WHERE node_id = ?`, nodeID).Scan(&locked)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read login lock: %w", err)
	}
	return parseNullTime(locked)
}

// ClearLoginFailures resets the counter after a successful login.
func (s *Store) ClearLoginFailures(ctx context.Context, nodeID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM login_failures WHERE node_id = ?`, nodeID)
	if err != nil {
		return fmt.Errorf("clear login failures: %w", err)
	}
	return nil
}

func nullRFC(t *time.Time) any {
	if t == nil {
		return nil
	}
	return rfc(*t)
}
