package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// ---------------- registry logins ----------------

// RegistryCredential is a login for one private registry. Secret is the
// encrypted password; it never leaves the hub (see api/registries.go).
type RegistryCredential struct {
	ID         string     `json:"id"`
	Registry   string     `json:"registry"`
	Username   string     `json:"username"`
	Secret     []byte     `json:"-"`
	CreatedBy  string     `json:"createdBy,omitempty"` // username, if the user still exists
	CreatedAt  time.Time  `json:"createdAt"`
	UpdatedAt  time.Time  `json:"updatedAt"`
	LastUsedAt *time.Time `json:"lastUsedAt"`
}

const registryCredCols = `r.id::text, r.registry, r.username, r.secret, COALESCE(u.username, ''), r.created_at, r.updated_at, r.last_used_at`
const registryCredFrom = ` FROM registry_credentials r LEFT JOIN users u ON u.id = r.created_by`

func scanRegistryCred(row pgx.Row) (RegistryCredential, error) {
	var c RegistryCredential
	err := row.Scan(&c.ID, &c.Registry, &c.Username, &c.Secret, &c.CreatedBy, &c.CreatedAt, &c.UpdatedAt, &c.LastUsedAt)
	return c, notFound(err)
}

func (s *Store) queryRegistryCreds(ctx context.Context, where string, args ...any) ([]RegistryCredential, error) {
	rows, err := s.db.Query(ctx, `SELECT `+registryCredCols+registryCredFrom+where+` ORDER BY r.registry`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RegistryCredential
	for rows.Next() {
		c, err := scanRegistryCred(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ListRegistryCredentials lists every saved login, by registry.
func (s *Store) ListRegistryCredentials(ctx context.Context) ([]RegistryCredential, error) {
	return s.queryRegistryCreds(ctx, "")
}

// RegistryCredentialsFor loads the logins for these registries (those saved).
func (s *Store) RegistryCredentialsFor(ctx context.Context, registries []string) ([]RegistryCredential, error) {
	if len(registries) == 0 {
		return nil, nil
	}
	return s.queryRegistryCreds(ctx, ` WHERE r.registry = ANY($1)`, registries)
}

func (s *Store) GetRegistryCredential(ctx context.Context, id string) (RegistryCredential, error) {
	return scanRegistryCred(s.db.QueryRow(ctx, `SELECT `+registryCredCols+registryCredFrom+` WHERE r.id = $1::uuid`, id))
}

// CreateRegistryCredential returns ErrConflict if the registry already has a login.
func (s *Store) CreateRegistryCredential(ctx context.Context, registry, username string, secret []byte, createdBy string) (RegistryCredential, error) {
	var id string
	err := s.db.QueryRow(ctx, `INSERT INTO registry_credentials (registry, username, secret, created_by)
		VALUES ($1, $2, $3, NULLIF($4, '')::uuid) RETURNING id::text`, registry, username, secret, createdBy).Scan(&id)
	if isUniqueViolation(err) {
		return RegistryCredential{}, ErrConflict
	}
	if err != nil {
		return RegistryCredential{}, err
	}
	return s.GetRegistryCredential(ctx, id)
}

// UpdateRegistryCredential changes the username and, unless secret is nil,
// the password.
func (s *Store) UpdateRegistryCredential(ctx context.Context, id, username string, secret []byte) (RegistryCredential, error) {
	tag, err := s.db.Exec(ctx, `UPDATE registry_credentials SET username = $2, secret = COALESCE($3, secret), updated_at = now()
		WHERE id = $1::uuid`, id, username, secret)
	if err != nil {
		return RegistryCredential{}, err
	}
	if tag.RowsAffected() == 0 {
		return RegistryCredential{}, ErrNotFound
	}
	return s.GetRegistryCredential(ctx, id)
}

// DeleteRegistryCredential removes a login and returns its registry.
func (s *Store) DeleteRegistryCredential(ctx context.Context, id string) (string, error) {
	var registry string
	err := s.db.QueryRow(ctx, `DELETE FROM registry_credentials WHERE id = $1::uuid RETURNING registry`, id).Scan(&registry)
	return registry, notFound(err)
}

// TouchRegistryCredentials records that these registries' logins were just
// sent to an agent.
func (s *Store) TouchRegistryCredentials(ctx context.Context, registries []string) error {
	if len(registries) == 0 {
		return nil
	}
	_, err := s.db.Exec(ctx, `UPDATE registry_credentials SET last_used_at = now() WHERE registry = ANY($1)`, registries)
	return err
}
