package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Credential is a stored WebAuthn passkey.
type Credential struct {
	// ID is the raw credential ID, base64url-encoded for storage.
	ID             string
	DeviceID       string
	PublicKey      []byte
	Attestation    string
	Transports     []string
	AAGUID         []byte
	SignCount      uint32
	BackupEligible bool
	BackupState    bool
	CreatedAt      time.Time
	LastUsedAt     *time.Time
}

// AddCredential stores a new passkey for a device.
func (s *Store) AddCredential(ctx context.Context, c Credential) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO credentials
		   (id, device_id, public_key, attestation, transports, aaguid,
		    sign_count, backup_eligible, backup_state, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.DeviceID, c.PublicKey, c.Attestation,
		strings.Join(c.Transports, ","), c.AAGUID,
		c.SignCount, boolInt(c.BackupEligible), boolInt(c.BackupState), rfc(c.CreatedAt))
	if err != nil {
		return fmt.Errorf("add credential: %w", err)
	}
	return nil
}

// CredentialByID looks up one passkey. Credentials of revoked devices are not
// returned, so revoking a device immediately stops its passkey working.
func (s *Store) CredentialByID(ctx context.Context, id string) (Credential, error) {
	return scanCredential(s.db.QueryRowContext(ctx,
		`SELECT c.id, c.device_id, c.public_key, c.attestation, c.transports, c.aaguid,
		        c.sign_count, c.backup_eligible, c.backup_state, c.created_at, c.last_used_at
		   FROM credentials c
		   JOIN devices d ON d.id = c.device_id
		  WHERE c.id = ? AND d.revoked_at IS NULL`, id))
}

// ListCredentials returns every passkey belonging to live devices. Used to
// build allowCredentials for login.
func (s *Store) ListCredentials(ctx context.Context) ([]Credential, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT c.id, c.device_id, c.public_key, c.attestation, c.transports, c.aaguid,
		        c.sign_count, c.backup_eligible, c.backup_state, c.created_at, c.last_used_at
		   FROM credentials c
		   JOIN devices d ON d.id = c.device_id
		  WHERE d.revoked_at IS NULL
		  ORDER BY c.created_at`)
	if err != nil {
		return nil, fmt.Errorf("list credentials: %w", err)
	}
	defer rows.Close()
	var out []Credential
	for rows.Next() {
		c, err := scanCredential(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ListCredentialsForDevice returns one device's passkeys.
func (s *Store) ListCredentialsForDevice(ctx context.Context, deviceID string) ([]Credential, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, device_id, public_key, attestation, transports, aaguid,
		        sign_count, backup_eligible, backup_state, created_at, last_used_at
		   FROM credentials WHERE device_id = ? ORDER BY created_at`, deviceID)
	if err != nil {
		return nil, fmt.Errorf("list device credentials: %w", err)
	}
	defer rows.Close()
	var out []Credential
	for rows.Next() {
		c, err := scanCredential(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// UpdateCredentialUse records a successful assertion: the authenticator's sign
// counter and the time. A counter that goes backwards is a cloned-authenticator
// signal, which the auth layer checks before calling this.
func (s *Store) UpdateCredentialUse(ctx context.Context, id string, signCount uint32, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE credentials SET sign_count = ?, last_used_at = ? WHERE id = ?`,
		signCount, rfc(at), id)
	if err != nil {
		return fmt.Errorf("update credential use: %w", err)
	}
	return nil
}

func scanCredential(row rowScanner) (Credential, error) {
	var (
		c                           Credential
		transports                  string
		createdAt                   string
		lastUsed                    sql.NullString
		backupEligible, backupState int
	)
	err := row.Scan(&c.ID, &c.DeviceID, &c.PublicKey, &c.Attestation, &transports, &c.AAGUID,
		&c.SignCount, &backupEligible, &backupState, &createdAt, &lastUsed)
	if errors.Is(err, sql.ErrNoRows) {
		return Credential{}, ErrNotFound
	}
	if err != nil {
		return Credential{}, fmt.Errorf("scan credential: %w", err)
	}
	if transports != "" {
		c.Transports = strings.Split(transports, ",")
	}
	c.BackupEligible = backupEligible == 1
	c.BackupState = backupState == 1
	if c.CreatedAt, err = parseTime(createdAt); err != nil {
		return Credential{}, err
	}
	if c.LastUsedAt, err = parseNullTime(lastUsed); err != nil {
		return Credential{}, err
	}
	return c, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
