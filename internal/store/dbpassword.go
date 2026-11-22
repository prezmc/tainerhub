package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

// DefaultPassword is the database password in the published compose file.
// The hub replaces it with a random one on first start (see cmd/tainerhub).
const DefaultPassword = "tainerhub"

// ErrAuth means Postgres rejected the password.
var ErrAuth = errors.New("the database rejected the password")

func isAuthError(err error) bool {
	var pe *pgconn.PgError
	// 28P01 invalid_password, 28000 invalid_authorization_specification
	return errors.As(err, &pe) && (pe.Code == "28P01" || pe.Code == "28000")
}

// SetOwnPassword changes the password of the role the hub connects as.
// ALTER ROLE can't take a bind parameter, so only letters and digits are
// accepted, which need no quoting.
func (s *Store) SetOwnPassword(ctx context.Context, pw string) error {
	if len(pw) < 16 {
		return errors.New("database password too short")
	}
	for i := 0; i < len(pw); i++ {
		c := pw[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			return errors.New("database password must be letters and digits")
		}
	}
	_, err := s.db.Exec(ctx, `ALTER ROLE CURRENT_USER PASSWORD '`+pw+`'`)
	return err
}
