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
		if isAuthError(err) { // retrying won't help
			pool.Close()
			return nil, fmt.Errorf("%w: %v", ErrAuth, err)
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
	// PasswordHash is an encoded argon2id hash, or "" for no password.
	PasswordHash string `json:"-"`
}

const userCols = `id::text, username, display_name, role, webauthn_id, created_at, password_hash`

func scanUser(row pgx.Row) (User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Username, &u.DisplayName, &u.Role, &u.WebAuthnID, &u.CreatedAt, &u.PasswordHash)
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
	// Case-insensitive. On an old install holding two names that differ only
	// in case, the exact spelling wins so each account stays reachable.
	return scanUser(s.db.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE lower(username) = lower($1)
		ORDER BY username = $1 DESC, created_at LIMIT 1`, username))
}

// CreateFirstOwner creates the initial owner with recovery codes and either
// a passkey or a password (u.PasswordHash; pk nil). It fails with
// ErrConflict if any user already exists.
func (s *Store) CreateFirstOwner(ctx context.Context, u User, pk *Passkey, recoveryHashes [][]byte) (User, error) {
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
		`INSERT INTO users (username, display_name, role, webauthn_id, password_hash) VALUES ($1, $2, 'owner', $3, $4) RETURNING `+userCols,
		u.Username, u.DisplayName, u.WebAuthnID, u.PasswordHash))
	if err != nil {
		return User{}, err
	}
	if pk != nil {
		if _, err := tx.Exec(ctx, `INSERT INTO passkeys (id, user_id, name, credential) VALUES ($1, $2::uuid, $3, $4)`,
			pk.ID, created.ID, pk.Name, pk.Credential); err != nil {
			return User{}, err
		}
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

// DeletePasskey removes a passkey unless it is the user's last way to sign
// in (their last passkey and they have no password).
func (s *Store) DeletePasskey(ctx context.Context, userID string, id []byte) error {
	tag, err := s.db.Exec(ctx, `
		DELETE FROM passkeys WHERE id = $2 AND user_id = $1::uuid
		AND ((SELECT count(*) FROM passkeys WHERE user_id = $1::uuid) > 1
		     OR (SELECT password_hash FROM users WHERE id = $1::uuid) <> '')`, userID, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SetPasswordHash sets (or with "" removes) a user's password.
func (s *Store) SetPasswordHash(ctx context.Context, userID, hash string) error {
	tag, err := s.db.Exec(ctx, `UPDATE users SET password_hash = $2 WHERE id = $1::uuid`, userID, hash)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
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

// DeleteOtherSessions signs a user out everywhere except the session whose
// token hash is keep, and returns how many sessions it ended.
func (s *Store) DeleteOtherSessions(ctx context.Context, userID string, keep []byte) (int64, error) {
	if keep == nil {
		keep = []byte{} // nil would be NULL, and "<> NULL" matches nothing
	}
	tag, err := s.db.Exec(ctx, `DELETE FROM sessions WHERE user_id = $1::uuid AND token_hash <> $2`, userID, keep)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// SetPasswordAndRevoke sets a user's password and ends their other sessions
// in one transaction, so a new password always shuts out old sign-ins.
func (s *Store) SetPasswordAndRevoke(ctx context.Context, userID, hash string, keep []byte) error {
	if keep == nil {
		keep = []byte{} // nil would be NULL, and "<> NULL" matches nothing
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	tag, err := tx.Exec(ctx, `UPDATE users SET password_hash = $2 WHERE id = $1::uuid`, userID, hash)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if _, err := tx.Exec(ctx, `DELETE FROM sessions WHERE user_id = $1::uuid AND token_hash <> $2`, userID, keep); err != nil {
		return err
	}
	return tx.Commit(ctx)
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

// RenewHostCert swaps in a renewed certificate's serial only if the host
// still has the one the agent presented, so of two concurrent renewals with
// the same certificate exactly one wins.
func (s *Store) RenewHostCert(ctx context.Context, hostID, oldSerial, newSerial string) error {
	if oldSerial == "" { // an empty serial means not enrolled, never renewable
		return gateway.ErrSerialChanged
	}
	tag, err := s.db.Exec(ctx, `UPDATE hosts SET cert_serial = $3 WHERE id = $1::uuid AND cert_serial = $2`, hostID, oldSerial, newSerial)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return gateway.ErrSerialChanged
	}
	return nil
}

// RecordCertRenewal puts "agent certificate renewed" in the audit log.
func (s *Store) RecordCertRenewal(ctx context.Context, hostID, hostName, remoteIP string) error {
	return s.Audit(ctx, AuditEntry{Actor: "agent", Action: "host.cert_renewed", Target: hostName, IP: remoteIP})
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

// ---------------- stacks ----------------

type Stack struct {
	ID           string     `json:"id"`
	HostID       string     `json:"hostId"`
	Name         string     `json:"name"`
	Compose      string     `json:"compose"`
	Env          string     `json:"env"`
	CreatedAt    time.Time  `json:"createdAt"`
	UpdatedAt    time.Time  `json:"updatedAt"`
	UpdatedBy    string     `json:"updatedBy"`
	DeployedAt   *time.Time `json:"deployedAt"`
	DeployedHash string     `json:"-"`
}

const stackCols = `id::text, host_id::text, name, compose, env, created_at, updated_at, updated_by, deployed_at, deployed_hash`

func scanStack(row pgx.Row) (Stack, error) {
	var st Stack
	err := row.Scan(&st.ID, &st.HostID, &st.Name, &st.Compose, &st.Env, &st.CreatedAt, &st.UpdatedAt, &st.UpdatedBy, &st.DeployedAt, &st.DeployedHash)
	return st, notFound(err)
}

func (s *Store) ListStacks(ctx context.Context) ([]Stack, error) {
	rows, err := s.db.Query(ctx, `SELECT `+stackCols+` FROM stacks ORDER BY name, host_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Stack
	for rows.Next() {
		st, err := scanStack(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

func (s *Store) GetStack(ctx context.Context, id string) (Stack, error) {
	return scanStack(s.db.QueryRow(ctx, `SELECT `+stackCols+` FROM stacks WHERE id = $1::uuid`, id))
}

// CreateStack returns ErrConflict if the host already has a stack with that name.
func (s *Store) CreateStack(ctx context.Context, st Stack) (Stack, error) {
	out, err := scanStack(s.db.QueryRow(ctx,
		`INSERT INTO stacks (host_id, name, compose, env, updated_by) VALUES ($1::uuid, $2, $3, $4, $5) RETURNING `+stackCols,
		st.HostID, st.Name, st.Compose, st.Env, st.UpdatedBy))
	if isUniqueViolation(err) {
		return Stack{}, ErrConflict
	}
	return out, err
}

// ErrStale means the stack changed since the caller loaded it.
var ErrStale = errors.New("stack was changed by someone else")

// UpdateStack saves new contents if the stack still has the updated_at the
// caller loaded (optimistic locking), so two editors can't silently
// overwrite each other.
func (s *Store) UpdateStack(ctx context.Context, id, compose, env, by string, loadedAt time.Time) (Stack, error) {
	st, err := scanStack(s.db.QueryRow(ctx,
		`UPDATE stacks SET compose = $2, env = $3, updated_by = $4, updated_at = now()
		 WHERE id = $1::uuid AND date_trunc('milliseconds', updated_at) = date_trunc('milliseconds', $5::timestamptz) RETURNING `+stackCols,
		id, compose, env, by, loadedAt))
	if errors.Is(err, ErrNotFound) {
		if _, gerr := s.GetStack(ctx, id); gerr == nil {
			return Stack{}, ErrStale
		}
	}
	return st, err
}

// MarkStackDeployed records a successful deploy of the contents with this hash.
func (s *Store) MarkStackDeployed(ctx context.Context, id, hash string) error {
	_, err := s.db.Exec(ctx, `UPDATE stacks SET deployed_at = now(), deployed_hash = $2 WHERE id = $1::uuid`, id, hash)
	return err
}

func (s *Store) DeleteStack(ctx context.Context, id string) error {
	tag, err := s.db.Exec(ctx, `DELETE FROM stacks WHERE id = $1::uuid`, id)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

// RenameHost changes a host's display name. ErrConflict if it's taken.
func (s *Store) RenameHost(ctx context.Context, id, name string) error {
	tag, err := s.db.Exec(ctx, `UPDATE hosts SET name = $2 WHERE id = $1::uuid`, id, name)
	if isUniqueViolation(err) {
		return ErrConflict
	}
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

// ---------------- users and invites ----------------

func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.db.Query(ctx, `SELECT `+userCols+` FROM users ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// SetUserRole changes a user's role. The owner's role can't change here.
func (s *Store) SetUserRole(ctx context.Context, id, role string) error {
	tag, err := s.db.Exec(ctx, `UPDATE users SET role = $2 WHERE id = $1::uuid AND role <> 'owner'`, id, role)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

// DeleteUser removes a user with their sessions, passkeys and recovery codes
// (signing them out everywhere). The owner can't be removed.
func (s *Store) DeleteUser(ctx context.Context, id string) error {
	tag, err := s.db.Exec(ctx, `DELETE FROM users WHERE id = $1::uuid AND role <> 'owner'`, id)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

type Invite struct {
	ID        string     `json:"id"`
	Role      string     `json:"role"`
	Note      string     `json:"note"`
	CreatedBy string     `json:"createdBy"` // username
	CreatedAt time.Time  `json:"createdAt"`
	ExpiresAt time.Time  `json:"expiresAt"`
	UsedAt    *time.Time `json:"usedAt"`
}

func (s *Store) CreateInvite(ctx context.Context, tokenHash []byte, role, note, createdBy string, expires time.Time) (Invite, error) {
	var inv Invite
	err := s.db.QueryRow(ctx, `INSERT INTO invites (token_hash, role, note, created_by, expires_at) VALUES ($1, $2, $3, $4::uuid, $5)
		RETURNING id::text, role, note, created_at, expires_at`, tokenHash, role, note, createdBy, expires).
		Scan(&inv.ID, &inv.Role, &inv.Note, &inv.CreatedAt, &inv.ExpiresAt)
	return inv, err
}

// PendingInvites lists unused, unexpired invites, newest first.
func (s *Store) PendingInvites(ctx context.Context) ([]Invite, error) {
	rows, err := s.db.Query(ctx, `SELECT i.id::text, i.role, i.note, coalesce(u.username, ''), i.created_at, i.expires_at, i.used_at
		FROM invites i LEFT JOIN users u ON u.id = i.created_by
		WHERE i.used_at IS NULL AND i.expires_at > now() ORDER BY i.created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Invite
	for rows.Next() {
		var inv Invite
		if err := rows.Scan(&inv.ID, &inv.Role, &inv.Note, &inv.CreatedBy, &inv.CreatedAt, &inv.ExpiresAt, &inv.UsedAt); err != nil {
			return nil, err
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}

func (s *Store) DeleteInvite(ctx context.Context, id string) error {
	tag, err := s.db.Exec(ctx, `DELETE FROM invites WHERE id = $1::uuid AND used_at IS NULL`, id)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

// InviteRole returns the role of a usable invite, or ErrNotFound.
func (s *Store) InviteRole(ctx context.Context, tokenHash []byte) (string, error) {
	var role string
	err := s.db.QueryRow(ctx, `SELECT role FROM invites WHERE token_hash = $1 AND used_at IS NULL AND expires_at > now()`, tokenHash).Scan(&role)
	return role, notFound(err)
}

// AcceptInvite creates the user an invite is for and marks the invite used,
// in one transaction, so a link works exactly once. ErrNotFound if the invite
// is unknown, used or expired; ErrConflict if the username is taken.
func (s *Store) AcceptInvite(ctx context.Context, tokenHash []byte, u User, recoveryHashes [][]byte) (User, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return User{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var inviteID, role string
	err = tx.QueryRow(ctx, `SELECT id::text, role FROM invites WHERE token_hash = $1 AND used_at IS NULL AND expires_at > now() FOR UPDATE`, tokenHash).Scan(&inviteID, &role)
	if err != nil {
		return User{}, notFound(err)
	}
	// Names must differ in more than case. The unique index on lower(username)
	// normally enforces this; the lock and check cover installs where the
	// index couldn't be built (see migration 0006). Invites are rare, so
	// briefly serialising user creation costs nothing.
	if _, err := tx.Exec(ctx, `LOCK TABLE users IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		return User{}, err
	}
	var taken bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE lower(username) = lower($1))`, u.Username).Scan(&taken); err != nil {
		return User{}, err
	}
	if taken {
		return User{}, ErrConflict
	}
	created, err := scanUser(tx.QueryRow(ctx,
		`INSERT INTO users (username, display_name, role, webauthn_id, password_hash) VALUES ($1, $2, $3, $4, $5) RETURNING `+userCols,
		u.Username, u.DisplayName, role, u.WebAuthnID, u.PasswordHash))
	if isUniqueViolation(err) {
		return User{}, ErrConflict
	}
	if err != nil {
		return User{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE invites SET used_at = now(), used_by = $2::uuid WHERE id = $1::uuid`, inviteID, created.ID); err != nil {
		return User{}, err
	}
	for _, h := range recoveryHashes {
		if _, err := tx.Exec(ctx, `INSERT INTO recovery_codes (user_id, code_hash) VALUES ($1::uuid, $2)`, created.ID, h); err != nil {
			return User{}, err
		}
	}
	return created, tx.Commit(ctx)
}

// ---------------- settings ----------------

// Setting returns a UI-changed setting, or "" if it isn't set.
func (s *Store) Setting(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRow(ctx, `SELECT value FROM settings WHERE key = $1`, key).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return v, err
}

func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.Exec(ctx, `INSERT INTO settings (key, value) VALUES ($1, $2)
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`, key, value)
	return err
}

func (s *Store) DeleteSetting(ctx context.Context, key string) error {
	_, err := s.db.Exec(ctx, `DELETE FROM settings WHERE key = $1`, key)
	return err
}
