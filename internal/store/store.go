// Package store is TainerHub's Postgres persistence layer.
package store

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/prezmc/tainerhub/internal/gateway"
	"github.com/prezmc/tainerhub/internal/wire"
)

//go:embed migrations/*.sql
var migrations embed.FS

var (
	ErrNotFound = gateway.ErrNotFound
	ErrConflict = errors.New("already exists")
)

type Store struct{ db *pgxpool.Pool }

// Open connects to Postgres, waiting up to 60s for it to come up (it usually
// starts alongside the hub in the same compose file).
func Open(ctx context.Context, url, password string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("database url: %w", err)
	}
	if password != "" {
		cfg.ConnConfig.Password = password
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		err = pool.Ping(ctx)
		if err == nil {
			return &Store{db: pool}, nil
		}
		if time.Now().After(deadline) {
			pool.Close()
			return nil, fmt.Errorf("database not reachable: %w", err)
		}
		select {
		case <-ctx.Done():
			pool.Close()
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (s *Store) Close() { s.db.Close() }

func (s *Store) Ping(ctx context.Context) error { return s.db.Ping(ctx) }

// Migrate applies embedded migrations in file-name order, once each.
func (s *Store) Migrate(ctx context.Context) error {
	if _, err := s.db.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	for _, name := range names {
		version := strings.TrimSuffix(strings.TrimPrefix(name, "migrations/"), ".sql")
		var exists bool
		if err := s.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, version).Scan(&exists); err != nil {
			return err
		}
		if exists {
			continue
		}
		sqlText, err := migrations.ReadFile(name)
		if err != nil {
			return err
		}
		tx, err := s.db.Begin(ctx)
		if err != nil {
			return err
		}
		// No arguments: pgx uses the simple protocol, so multi-statement files work.
		if _, err := tx.Exec(ctx, string(sqlText)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("migration %s: %w", version, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, version); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
	}
	return nil
}

func notFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func isUniqueViolation(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23505"
}

// ---------------- users ----------------

type User struct {
	ID          string    `json:"id"`
	Username    string    `json:"username"`
	DisplayName string    `json:"displayName"`
	Role        string    `json:"role"`
	WebAuthnID  []byte    `json:"-"`
	CreatedAt   time.Time `json:"createdAt"`
}

const userCols = `id::text, username, display_name, role, webauthn_id, created_at`

func scanUser(row pgx.Row) (User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Username, &u.DisplayName, &u.Role, &u.WebAuthnID, &u.CreatedAt)
	return u, notFound(err)
}

func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&n)
	return n, err
}

func (s *Store) UserByID(ctx context.Context, id string) (User, error) {
	return scanUser(s.db.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE id = $1::uuid`, id))
}

func (s *Store) UserByWebAuthnID(ctx context.Context, waID []byte) (User, error) {
	return scanUser(s.db.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE webauthn_id = $1`, waID))
}

func (s *Store) UserByUsername(ctx context.Context, username string) (User, error) {
	return scanUser(s.db.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE lower(username) = lower($1)`, username))
}

// CreateFirstOwner creates the initial owner with one passkey and recovery
// codes. It fails with ErrConflict if any user already exists.
func (s *Store) CreateFirstOwner(ctx context.Context, u User, pk Passkey, recoveryHashes [][]byte) (User, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return User{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `LOCK TABLE users IN EXCLUSIVE MODE`); err != nil {
		return User{}, err
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&n); err != nil {
		return User{}, err
	}
	if n > 0 {
		return User{}, ErrConflict
	}
	created, err := scanUser(tx.QueryRow(ctx,
		`INSERT INTO users (username, display_name, role, webauthn_id) VALUES ($1, $2, 'owner', $3) RETURNING `+userCols,
		u.Username, u.DisplayName, u.WebAuthnID))
	if err != nil {
		return User{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO passkeys (id, user_id, name, credential) VALUES ($1, $2::uuid, $3, $4)`,
		pk.ID, created.ID, pk.Name, pk.Credential); err != nil {
		return User{}, err
	}
	for _, h := range recoveryHashes {
		if _, err := tx.Exec(ctx, `INSERT INTO recovery_codes (user_id, code_hash) VALUES ($1::uuid, $2)`, created.ID, h); err != nil {
			return User{}, err
		}
	}
	return created, tx.Commit(ctx)
}

// ---------------- passkeys ----------------

type Passkey struct {
	ID         []byte     `json:"-"`
	UserID     string     `json:"-"`
	Name       string     `json:"name"`
	Credential []byte     `json:"-"` // JSON
	CreatedAt  time.Time  `json:"createdAt"`
	LastUsedAt *time.Time `json:"lastUsedAt"`
}

func (s *Store) ListPasskeys(ctx context.Context, userID string) ([]Passkey, error) {
	rows, err := s.db.Query(ctx, `SELECT id, user_id::text, name, credential, created_at, last_used_at FROM passkeys WHERE user_id = $1::uuid ORDER BY created_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Passkey
	for rows.Next() {
		var p Passkey
		if err := rows.Scan(&p.ID, &p.UserID, &p.Name, &p.Credential, &p.CreatedAt, &p.LastUsedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) AddPasskey(ctx context.Context, p Passkey) error {
	_, err := s.db.Exec(ctx, `INSERT INTO passkeys (id, user_id, name, credential) VALUES ($1, $2::uuid, $3, $4)`, p.ID, p.UserID, p.Name, p.Credential)
	if isUniqueViolation(err) {
		return ErrConflict
	}
	return err
}

func (s *Store) UpdatePasskeyAfterLogin(ctx context.Context, id []byte, credential []byte) error {
	_, err := s.db.Exec(ctx, `UPDATE passkeys SET credential = $2, last_used_at = now() WHERE id = $1`, id, credential)
	return err
}

// DeletePasskey removes a passkey unless it is the user's last one.
func (s *Store) DeletePasskey(ctx context.Context, userID string, id []byte) error {
	tag, err := s.db.Exec(ctx, `
		DELETE FROM passkeys WHERE id = $2 AND user_id = $1::uuid
		AND (SELECT count(*) FROM passkeys WHERE user_id = $1::uuid) > 1`, userID, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ---------------- recovery codes ----------------

func (s *Store) UseRecoveryCode(ctx context.Context, userID string, hash []byte) (bool, error) {
	tag, err := s.db.Exec(ctx, `UPDATE recovery_codes SET used_at = now() WHERE user_id = $1::uuid AND code_hash = $2 AND used_at IS NULL`, userID, hash)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (s *Store) ReplaceRecoveryCodes(ctx context.Context, userID string, hashes [][]byte) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `DELETE FROM recovery_codes WHERE user_id = $1::uuid`, userID); err != nil {
		return err
	}
	for _, h := range hashes {
		if _, err := tx.Exec(ctx, `INSERT INTO recovery_codes (user_id, code_hash) VALUES ($1::uuid, $2)`, userID, h); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *Store) CountRecoveryCodes(ctx context.Context, userID string) (int, error) {
	var n int
	err := s.db.QueryRow(ctx, `SELECT count(*) FROM recovery_codes WHERE user_id = $1::uuid AND used_at IS NULL`, userID).Scan(&n)
	return n, err
}

// ---------------- sessions ----------------

type Session struct {
	UserID     string
	CreatedAt  time.Time
	LastSeenAt time.Time
	ExpiresAt  time.Time
}

func (s *Store) CreateSession(ctx context.Context, tokenHash []byte, userID string, expires time.Time, ip, ua string) error {
	_, err := s.db.Exec(ctx, `INSERT INTO sessions (token_hash, user_id, expires_at, ip, user_agent) VALUES ($1, $2::uuid, $3, $4, $5)`,
		tokenHash, userID, expires, ip, ua)
	return err
}

func (s *Store) GetSession(ctx context.Context, tokenHash []byte) (Session, error) {
	var ss Session
	err := s.db.QueryRow(ctx, `SELECT user_id::text, created_at, last_seen_at, expires_at FROM sessions WHERE token_hash = $1`, tokenHash).
		Scan(&ss.UserID, &ss.CreatedAt, &ss.LastSeenAt, &ss.ExpiresAt)
	return ss, notFound(err)
}

func (s *Store) TouchSession(ctx context.Context, tokenHash []byte) error {
	_, err := s.db.Exec(ctx, `UPDATE sessions SET last_seen_at = now() WHERE token_hash = $1`, tokenHash)
	return err
}

func (s *Store) DeleteSession(ctx context.Context, tokenHash []byte) error {
	_, err := s.db.Exec(ctx, `DELETE FROM sessions WHERE token_hash = $1`, tokenHash)
	return err
}

func (s *Store) PurgeSessions(ctx context.Context, idleCutoff time.Time) error {
	_, err := s.db.Exec(ctx, `DELETE FROM sessions WHERE expires_at < now() OR last_seen_at < $1`, idleCutoff)
	return err
}

// ---------------- hosts ----------------

type Host struct {
	ID         string          `json:"id"`
	Name       string          `json:"name"`
	CreatedAt  time.Time       `json:"createdAt"`
	EnrolledAt *time.Time      `json:"enrolledAt"`
	LastSeenAt *time.Time      `json:"lastSeenAt"`
	Info       json.RawMessage `json:"info"`
}

const hostCols = `id::text, name, created_at, enrolled_at, last_seen_at, info`

func scanHost(row pgx.Row) (Host, error) {
	var h Host
	var info []byte
	err := row.Scan(&h.ID, &h.Name, &h.CreatedAt, &h.EnrolledAt, &h.LastSeenAt, &info)
	if len(info) > 0 {
		h.Info = info
	}
	return h, notFound(err)
}

func (s *Store) CreateHost(ctx context.Context, name string) (Host, error) {
	h, err := scanHost(s.db.QueryRow(ctx, `INSERT INTO hosts (name) VALUES ($1) RETURNING `+hostCols, name))
	if isUniqueViolation(err) {
		return Host{}, ErrConflict
	}
	return h, err
}

func (s *Store) GetHost(ctx context.Context, id string) (Host, error) {
	return scanHost(s.db.QueryRow(ctx, `SELECT `+hostCols+` FROM hosts WHERE id = $1::uuid`, id))
}

func (s *Store) ListHosts(ctx context.Context) ([]Host, error) {
	rows, err := s.db.Query(ctx, `SELECT `+hostCols+` FROM hosts ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Host
	for rows.Next() {
		h, err := scanHost(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// DeleteHost removes a host; its certificate stops being accepted at once.
func (s *Store) DeleteHost(ctx context.Context, id string) error {
	tag, err := s.db.Exec(ctx, `DELETE FROM hosts WHERE id = $1::uuid`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// CreateJoinToken replaces any unused token for the host.
func (s *Store) CreateJoinToken(ctx context.Context, id, hostID string, secretHash []byte, expires time.Time) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `DELETE FROM join_tokens WHERE host_id = $1::uuid AND used_at IS NULL`, hostID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO join_tokens (id, host_id, secret_hash, expires_at) VALUES ($1, $2::uuid, $3, $4)`,
		id, hostID, secretHash, expires); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ---- gateway.Store ----

func (s *Store) GetJoinToken(ctx context.Context, id string) (gateway.JoinTokenRecord, error) {
	var r gateway.JoinTokenRecord
	var used *time.Time
	err := s.db.QueryRow(ctx, `SELECT id, host_id::text, secret_hash, expires_at, used_at FROM join_tokens WHERE id = $1`, id).
		Scan(&r.ID, &r.HostID, &r.SecretHash, &r.ExpiresAt, &used)
	r.Used = used != nil
	return r, notFound(err)
}

func (s *Store) CompleteEnrollment(ctx context.Context, tokenID, hostID, certSerial string) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	tag, err := tx.Exec(ctx, `UPDATE join_tokens SET used_at = now() WHERE id = $1 AND host_id = $2::uuid AND used_at IS NULL AND expires_at > now()`, tokenID, hostID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errors.New("join token already used")
	}
	if _, err := tx.Exec(ctx, `UPDATE hosts SET cert_serial = $2, enrolled_at = now() WHERE id = $1::uuid`, hostID, certSerial); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) GetHostCert(ctx context.Context, hostID string) (gateway.HostCert, error) {
	var hc gateway.HostCert
	err := s.db.QueryRow(ctx, `SELECT name, cert_serial FROM hosts WHERE id = $1::uuid`, hostID).Scan(&hc.Name, &hc.CertSerial)
	return hc, notFound(err)
}

func (s *Store) TouchHost(ctx context.Context, hostID string, hello *wire.Hello, at time.Time) error {
	if hello == nil {
		_, err := s.db.Exec(ctx, `UPDATE hosts SET last_seen_at = $2 WHERE id = $1::uuid`, hostID, at)
		return err
	}
	info, _ := json.Marshal(hello)
	_, err := s.db.Exec(ctx, `UPDATE hosts SET last_seen_at = $2, info = $3 WHERE id = $1::uuid`, hostID, at, info)
	return err
}

// ---------------- audit ----------------

type AuditEntry struct {
	ID     int64           `json:"id"`
	At     time.Time       `json:"at"`
	UserID *string         `json:"userId"`
	Actor  string          `json:"actor"`
	Action string          `json:"action"`
	Target string          `json:"target"`
	Detail json.RawMessage `json:"detail,omitempty"`
	IP     string          `json:"ip"`
}

func (s *Store) Audit(ctx context.Context, e AuditEntry) error {
	var detail []byte
	if len(e.Detail) > 0 {
		detail = e.Detail
	}
	_, err := s.db.Exec(ctx, `INSERT INTO audit_log (user_id, actor, action, target, detail, ip) VALUES ($1::uuid, $2, $3, $4, $5, $6)`,
		e.UserID, e.Actor, e.Action, e.Target, detail, e.IP)
	return err
}

func (s *Store) ListAudit(ctx context.Context, limit int) ([]AuditEntry, error) {
	rows, err := s.db.Query(ctx, `SELECT id, at, user_id::text, actor, action, target, detail, ip FROM audit_log ORDER BY at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var detail []byte
		if err := rows.Scan(&e.ID, &e.At, &e.UserID, &e.Actor, &e.Action, &e.Target, &detail, &e.IP); err != nil {
			return nil, err
		}
		if len(detail) > 0 {
			e.Detail = detail
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
