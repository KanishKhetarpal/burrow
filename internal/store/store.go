// Package store is Burrow's persistent state: paired devices, passkey
// credentials, auth sessions and settings.
//
// SQLite via modernc.org/sqlite so the agent stays a single CGO-free binary
// (docs/02-ARCHITECTURE.md §6).
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// ErrNotFound is returned by the Get-style methods when nothing matches.
var ErrNotFound = errors.New("store: not found")

// Store wraps the database handle.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the database at path and applies migrations.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("store dir: %w", err)
		}
	}
	// WAL keeps readers from blocking the single writer; busy_timeout turns a
	// lock collision into a short wait instead of an immediate error.
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	// One writer avoids SQLITE_BUSY churn; the workload is a single user.
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(0)

	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// OpenMemory opens a private in-memory store for tests.
func OpenMemory() (*Store, error) {
	db, err := sql.Open("sqlite", "file::memory:?_pragma=foreign_keys(1)")
	if err != nil {
		return nil, fmt.Errorf("open memory store: %w", err)
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the handle for packages that need their own queries.
func (s *Store) DB() *sql.DB { return s.db }

const schemaVersion = 1

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("migrate: version table: %w", err)
	}
	var have int
	err := s.db.QueryRowContext(ctx, `SELECT version FROM schema_version LIMIT 1`).Scan(&have)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO schema_version (version) VALUES (0)`); err != nil {
			return fmt.Errorf("migrate: seed version: %w", err)
		}
		have = 0
	} else if err != nil {
		return fmt.Errorf("migrate: read version: %w", err)
	}

	for v := have + 1; v <= schemaVersion; v++ {
		stmts, ok := migrations[v]
		if !ok {
			return fmt.Errorf("migrate: no migration for version %d", v)
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("migrate %d: begin: %w", v, err)
		}
		for _, stmt := range stmts {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				tx.Rollback()
				return fmt.Errorf("migrate %d: %w", v, err)
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE schema_version SET version = ?`, v); err != nil {
			tx.Rollback()
			return fmt.Errorf("migrate %d: bump version: %w", v, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("migrate %d: commit: %w", v, err)
		}
	}
	return nil
}

var migrations = map[int][]string{
	1: {
		// A paired client device: the iPhone or the laptop. node_id is the
		// Tailscale StableID; every request is checked against it.
		`CREATE TABLE devices (
			id          TEXT PRIMARY KEY,
			name        TEXT NOT NULL,
			kind        TEXT NOT NULL CHECK (kind IN ('phone','laptop')),
			node_id     TEXT NOT NULL UNIQUE,
			login       TEXT NOT NULL,
			paired_at   TEXT NOT NULL,
			last_seen   TEXT,
			revoked_at  TEXT
		)`,
		// WebAuthn credentials, one or more per device.
		`CREATE TABLE credentials (
			id            TEXT PRIMARY KEY,
			device_id     TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
			public_key    BLOB NOT NULL,
			attestation   TEXT,
			transports    TEXT,
			aaguid        BLOB,
			sign_count    INTEGER NOT NULL DEFAULT 0,
			backup_eligible INTEGER NOT NULL DEFAULT 0,
			backup_state  INTEGER NOT NULL DEFAULT 0,
			created_at    TEXT NOT NULL,
			last_used_at  TEXT
		)`,
		`CREATE INDEX idx_credentials_device ON credentials(device_id)`,
		// Server-side auth sessions. The cookie holds only the id.
		`CREATE TABLE auth_sessions (
			id           TEXT PRIMARY KEY,
			device_id    TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
			node_id      TEXT NOT NULL,
			created_at   TEXT NOT NULL,
			last_seen_at TEXT NOT NULL,
			stepup_at    TEXT,
			revoked_at   TEXT
		)`,
		`CREATE INDEX idx_auth_sessions_device ON auth_sessions(device_id)`,
		// Claude session records, so the list survives an agent restart.
		`CREATE TABLE claude_sessions (
			id              TEXT PRIMARY KEY,
			name            TEXT NOT NULL,
			project_id      TEXT NOT NULL,
			cwd             TEXT NOT NULL,
			mode            TEXT NOT NULL,
			permission_mode TEXT NOT NULL,
			spawn           TEXT,
			status          TEXT NOT NULL,
			remote_url      TEXT,
			pid             INTEGER,
			started_at      TEXT NOT NULL,
			ended_at        TEXT,
			exit_code       INTEGER,
			restarts        INTEGER NOT NULL DEFAULT 0
		)`,
		// Login failure counters, for lockout.
		`CREATE TABLE login_failures (
			node_id   TEXT PRIMARY KEY,
			count     INTEGER NOT NULL DEFAULT 0,
			first_at  TEXT NOT NULL,
			locked_until TEXT
		)`,
		// Small key/value settings (lockdown flag, keep-awake, auto-shutdown).
		`CREATE TABLE settings (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
	},
}

// ---------- settings ----------

// SetSetting stores a JSON-encoded value under key.
func (s *Store) SetSetting(ctx context.Context, key string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode setting %q: %w", key, err)
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO settings (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, string(raw))
	if err != nil {
		return fmt.Errorf("save setting %q: %w", key, err)
	}
	return nil
}

// GetSetting decodes the value stored under key into out.
// It returns ErrNotFound when the key was never set.
func (s *Store) GetSetting(ctx context.Context, key string, out any) error {
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("read setting %q: %w", key, err)
	}
	if err := json.Unmarshal([]byte(raw), out); err != nil {
		return fmt.Errorf("decode setting %q: %w", key, err)
	}
	return nil
}

// ---------- devices ----------

// Device is a paired client device.
type Device struct {
	ID        string
	Name      string
	Kind      string
	NodeID    string
	Login     string
	PairedAt  time.Time
	LastSeen  *time.Time
	RevokedAt *time.Time
}

// AddDevice records a newly paired device.
func (s *Store) AddDevice(ctx context.Context, d Device) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO devices (id, name, kind, node_id, login, paired_at) VALUES (?, ?, ?, ?, ?, ?)`,
		d.ID, d.Name, d.Kind, d.NodeID, d.Login, rfc(d.PairedAt))
	if err != nil {
		return fmt.Errorf("add device: %w", err)
	}
	return nil
}

// DeviceByNodeID returns the live (non-revoked) device for a node.
func (s *Store) DeviceByNodeID(ctx context.Context, nodeID string) (Device, error) {
	return s.scanDevice(s.db.QueryRowContext(ctx,
		`SELECT id, name, kind, node_id, login, paired_at, last_seen, revoked_at
		   FROM devices WHERE node_id = ? AND revoked_at IS NULL`, nodeID))
}

// DeviceByID returns a device by its own id, revoked or not.
func (s *Store) DeviceByID(ctx context.Context, id string) (Device, error) {
	return s.scanDevice(s.db.QueryRowContext(ctx,
		`SELECT id, name, kind, node_id, login, paired_at, last_seen, revoked_at
		   FROM devices WHERE id = ?`, id))
}

// ListDevices returns every live device, oldest pairing first.
func (s *Store) ListDevices(ctx context.Context) ([]Device, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, kind, node_id, login, paired_at, last_seen, revoked_at
		   FROM devices WHERE revoked_at IS NULL ORDER BY paired_at`)
	if err != nil {
		return nil, fmt.Errorf("list devices: %w", err)
	}
	defer rows.Close()
	var out []Device
	for rows.Next() {
		d, err := s.scanDevice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// CountDevices counts live devices, for the max_devices cap.
func (s *Store) CountDevices(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM devices WHERE revoked_at IS NULL`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count devices: %w", err)
	}
	return n, nil
}

// TouchDevice records that a device was just seen.
func (s *Store) TouchDevice(ctx context.Context, id string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE devices SET last_seen = ? WHERE id = ?`, rfc(at), id)
	if err != nil {
		return fmt.Errorf("touch device: %w", err)
	}
	return nil
}

// RevokeDevice marks a device revoked and kills its sessions. Its credentials
// stay for the audit trail but can no longer authenticate, because every login
// path requires a live device row.
func (s *Store) RevokeDevice(ctx context.Context, id string, at time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("revoke device: begin: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`UPDATE devices SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`, rfc(at), id); err != nil {
		return fmt.Errorf("revoke device: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE auth_sessions SET revoked_at = ? WHERE device_id = ? AND revoked_at IS NULL`,
		rfc(at), id); err != nil {
		return fmt.Errorf("revoke device sessions: %w", err)
	}
	return tx.Commit()
}

type rowScanner interface {
	Scan(dest ...any) error
}

func (s *Store) scanDevice(row rowScanner) (Device, error) {
	var (
		d                   Device
		pairedAt            string
		lastSeen, revokedAt sql.NullString
	)
	err := row.Scan(&d.ID, &d.Name, &d.Kind, &d.NodeID, &d.Login, &pairedAt, &lastSeen, &revokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Device{}, ErrNotFound
	}
	if err != nil {
		return Device{}, fmt.Errorf("scan device: %w", err)
	}
	if d.PairedAt, err = parseTime(pairedAt); err != nil {
		return Device{}, err
	}
	if d.LastSeen, err = parseNullTime(lastSeen); err != nil {
		return Device{}, err
	}
	if d.RevokedAt, err = parseNullTime(revokedAt); err != nil {
		return Device{}, err
	}
	return d, nil
}

// ---------- time helpers ----------

func rfc(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parseTime(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse stored time %q: %w", s, err)
	}
	return t.UTC(), nil
}

func parseNullTime(ns sql.NullString) (*time.Time, error) {
	if !ns.Valid || ns.String == "" {
		return nil, nil
	}
	t, err := parseTime(ns.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}
