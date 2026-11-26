package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// ---------------- API tokens ----------------

// APIToken is a token's metadata; the token itself is never stored.
type APIToken struct {
	ID         string     `json:"id"`
	UserID     string     `json:"userId"`
	Username   string     `json:"username,omitempty"` // filled by ListAllAPITokens
	UserRole   string     `json:"userRole,omitempty"` // the owner's current role, ditto
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	Role       string     `json:"role"`
	CreatedAt  time.Time  `json:"createdAt"`
	ExpiresAt  *time.Time `json:"expiresAt"`
	LastUsedAt *time.Time `json:"lastUsedAt"`
	LastUsedIP string     `json:"lastUsedIp"`
}

const apiTokenCols = `t.id::text, t.user_id::text, t.name, t.prefix, t.role, t.created_at, t.expires_at, t.last_used_at, t.last_used_ip`

func scanAPIToken(row pgx.Row, extra ...any) (APIToken, error) {
	var t APIToken
	dest := append([]any{&t.ID, &t.UserID, &t.Name, &t.Prefix, &t.Role, &t.CreatedAt, &t.ExpiresAt, &t.LastUsedAt, &t.LastUsedIP}, extra...)
	return t, notFound(row.Scan(dest...))
}

// CreateAPIToken saves a new token for userID. expires nil means it never expires.
func (s *Store) CreateAPIToken(ctx context.Context, userID, name, prefix, role string, tokenHash []byte, expires *time.Time) (APIToken, error) {
	return scanAPIToken(s.db.QueryRow(ctx, `INSERT INTO api_tokens AS t (user_id, name, prefix, token_hash, role, expires_at)
		VALUES ($1::uuid, $2, $3, $4, $5, $6) RETURNING `+apiTokenCols, userID, name, prefix, tokenHash, role, expires))
}

// ListAPITokens lists one user's tokens, newest first.
func (s *Store) ListAPITokens(ctx context.Context, userID string) ([]APIToken, error) {
	rows, err := s.db.Query(ctx, `SELECT `+apiTokenCols+` FROM api_tokens t WHERE t.user_id = $1::uuid ORDER BY t.created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIToken
	for rows.Next() {
		t, err := scanAPIToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ListAllAPITokens lists every user's tokens with the owner's name and role.
func (s *Store) ListAllAPITokens(ctx context.Context) ([]APIToken, error) {
	rows, err := s.db.Query(ctx, `SELECT `+apiTokenCols+`, u.username, u.role
		FROM api_tokens t JOIN users u ON u.id = t.user_id ORDER BY u.username, t.created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIToken
	for rows.Next() {
		var t APIToken
		var username, role string
		if t, err = scanAPIToken(rows, &username, &role); err != nil {
			return nil, err
		}
		t.Username, t.UserRole = username, role
		out = append(out, t)
	}
	return out, rows.Err()
}

// GetAPIToken loads one token with its owner's name and current role.
func (s *Store) GetAPIToken(ctx context.Context, id string) (APIToken, error) {
	var username, role string
	t, err := scanAPIToken(s.db.QueryRow(ctx, `SELECT `+apiTokenCols+`, u.username, u.role
		FROM api_tokens t JOIN users u ON u.id = t.user_id WHERE t.id = $1::uuid`, id), &username, &role)
	t.Username, t.UserRole = username, role
	return t, err
}

// APITokenUser loads a token and its user in one query: it runs on every
// token-authenticated request.
func (s *Store) APITokenUser(ctx context.Context, tokenHash []byte) (APIToken, User, error) {
	var u User
	// The user's host scope comes along: a token reaches the same hosts.
	t, err := scanAPIToken(s.db.QueryRow(ctx, `SELECT `+apiTokenCols+`, `+userColsAs("u.")+`
		FROM api_tokens t JOIN users u ON u.id = t.user_id WHERE t.token_hash = $1`, tokenHash),
		userDest(&u)...)
	if !u.Scoped {
		u.Hosts = nil
	}
	return t, u, err
}

// TouchAPIToken records when and from where a token was last used.
func (s *Store) TouchAPIToken(ctx context.Context, id, ip string) error {
	_, err := s.db.Exec(ctx, `UPDATE api_tokens SET last_used_at = now(), last_used_ip = $2 WHERE id = $1::uuid`, id, ip)
	return err
}

// DeleteAPIToken revokes one of userID's own tokens, returning its name.
// ErrNotFound if there is no such token for that user.
func (s *Store) DeleteAPIToken(ctx context.Context, id, userID string) (string, error) {
	var name string
	err := s.db.QueryRow(ctx, `DELETE FROM api_tokens WHERE id = $1::uuid AND user_id = $2::uuid RETURNING name`, id, userID).Scan(&name)
	return name, notFound(err)
}

// DeleteAPITokenOf revokes any user's token, but only while its owner's role
// is still expectRole (the role the caller checked permission against), like
// SetUserRole. ErrNotFound if the token is gone, ErrChanged if the owner's
// role moved on.
func (s *Store) DeleteAPITokenOf(ctx context.Context, id, expectRole string) error {
	tag, err := s.db.Exec(ctx, `DELETE FROM api_tokens t USING users u
		WHERE t.id = $1::uuid AND u.id = t.user_id AND u.role = $2`, id, expectRole)
	if err != nil {
		return err
	}
	if tag.RowsAffected() > 0 {
		return nil
	}
	var exists bool
	if err := s.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM api_tokens WHERE id = $1::uuid)`, id).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return ErrNotFound
	}
	return ErrChanged
}
