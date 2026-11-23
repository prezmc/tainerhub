package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/prezmc/tainerhub/internal/store"
)

// The published compose file starts Postgres with a well-known default
// password, so installing needs no settings. On first start the hub swaps it
// for a random one, keeps it in its data volume (next to the CA, so one backup
// covers both) and prints it once for the admin to save.

const dbPasswordFile = "db_password"

type passwordDB interface {
	SetOwnPassword(ctx context.Context, pw string) error
	Close()
}

// openManaged connects to the database, choosing the password:
//
//  1. a password saved by an earlier start (dataDir/db_password);
//  2. otherwise the one in the database URL. If that is the published
//     default, it is replaced with a random password, saved and printed.
//
// If the database volume was recreated while dataDir survived, the saved
// password no longer works but the default does: the saved one is re-applied.
func openManaged[T passwordDB](ctx context.Context, open func(context.Context, string) (T, error), urlPassword, dataDir string, log *slog.Logger) (T, error) {
	var zero T
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return zero, err
	}
	saved := filepath.Join(dataDir, dbPasswordFile)
	pending := saved + ".new"

	if b, err := os.ReadFile(saved); err == nil {
		pw := strings.TrimSpace(string(b))
		db, err := open(ctx, pw)
		if !errors.Is(err, store.ErrAuth) {
			return db, err
		}
		// The saved password is refused: the database was probably recreated.
		if urlPassword != store.DefaultPassword {
			return zero, fmt.Errorf("the database refused the password saved in %s (and the URL's): %w", saved, err)
		}
		db, err = open(ctx, urlPassword)
		if err != nil {
			return zero, fmt.Errorf("the database refused both the password saved in %s and the default: %w", saved, err)
		}
		if err := db.SetOwnPassword(ctx, pw); err != nil {
			db.Close()
			return zero, err
		}
		db.Close()
		log.Info("the database was recreated; re-applied the saved database password", "file", saved)
		return open(ctx, pw)
	}

	db, err := open(ctx, urlPassword)
	if errors.Is(err, store.ErrAuth) {
		// A crash between changing the password and saving it leaves it here.
		if b, rerr := os.ReadFile(pending); rerr == nil {
			pw := strings.TrimSpace(string(b))
			if db, err2 := open(ctx, pw); err2 == nil {
				if err := os.Rename(pending, saved); err != nil {
					db.Close()
					return zero, err
				}
				return db, nil
			}
		}
		return zero, err
	}
	if err != nil || urlPassword != store.DefaultPassword {
		return db, err // the admin chose their own password: leave it alone
	}

	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		db.Close()
		return zero, err
	}
	pw := hex.EncodeToString(b)
	// Save first, then change: whatever happens in between, the password is on disk.
	if err := os.WriteFile(pending, []byte(pw+"\n"), 0o600); err != nil {
		db.Close()
		return zero, err
	}
	if err := db.SetOwnPassword(ctx, pw); err != nil {
		db.Close()
		_ = os.Remove(pending)
		return zero, fmt.Errorf("change the default database password: %w", err)
	}
	db.Close()
	if err := os.Rename(pending, saved); err != nil {
		return zero, err
	}
	// The password itself stays out of the logs, which are often shipped
	// elsewhere; `tainerhub db-password` prints it on demand.
	log.Warn("SAVE THIS: the default database password was replaced with a random one. "+
		"TainerHub keeps it in its data volume and you don't need it day to day, "+
		"but keep a copy (e.g. in your password manager) in case you restore the database without that volume. "+
		"See it with: docker exec tainerhub /tainerhub db-password",
		"saved_in", saved)
	return open(ctx, pw)
}
