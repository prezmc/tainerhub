package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/prezmc/tainerhub/internal/gateway"
	"github.com/prezmc/tainerhub/internal/store"
	"github.com/prezmc/tainerhub/internal/wire"
)

// The hub's compose file also runs an agent for the hub's own server. The
// hub enrolls it without anyone clicking "Add host": it creates a host for
// it once, and keeps a fresh one-time join token in a volume shared only with
// that agent until the agent has enrolled. Then it deletes the token and
// renames the host to the server's real hostname.

const (
	localHostIDFile   = "local_host_id"
	localTokenFile    = "join_token"
	localPlaceholder  = "this-server"
	localTokenTTL     = time.Hour
	localTokenRefresh = 45 * time.Minute
)

type localStore interface {
	CreateHost(ctx context.Context, name string) (store.Host, error)
	GetHost(ctx context.Context, id string) (store.Host, error)
	CreateJoinToken(ctx context.Context, id, hostID string, secretHash []byte, expires time.Time) error
	RenameHost(ctx context.Context, id, name string) error
}

type localAgent struct {
	st       localStore
	caFP     string // hub CA fingerprint, pinned in the join token
	dataDir  string // hub's /data: remembers which host is the local one
	shareDir string // volume shared with the local agent
	log      *slog.Logger
	now      func() time.Time
	// renamed is called after the host is renamed, so the live connection
	// (which carries the name it connected under) can be refreshed.
	renamed func(hostID string)
}

func (l *localAgent) run(ctx context.Context) {
	for {
		done, err := l.step(ctx)
		if err != nil {
			l.log.Warn("local agent setup", "err", err)
		}
		if done {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

// step does one round of the setup and reports whether it's finished.
func (l *localAgent) step(ctx context.Context) (bool, error) {
	idFile := filepath.Join(l.dataDir, localHostIDFile)
	tokFile := filepath.Join(l.shareDir, localTokenFile)

	var h store.Host
	b, err := os.ReadFile(idFile)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if h, err = l.createHost(ctx); err != nil {
			return false, err
		}
		if err := os.WriteFile(idFile, []byte(h.ID+"\n"), 0o600); err != nil {
			return false, err
		}
		l.log.Info("registered this server as a host for its local agent", "host_id", h.ID)
	case err != nil:
		return false, err
	default:
		h, err = l.st.GetHost(ctx, strings.TrimSpace(string(b)))
		if errors.Is(err, store.ErrNotFound) {
			// Removed by an admin: respect that and don't bring it back.
			_ = os.Remove(tokFile)
			return true, nil
		}
		if err != nil {
			return false, err
		}
	}

	if h.EnrolledAt == nil {
		if st, err := os.Stat(tokFile); err == nil && l.now().Sub(st.ModTime()) < localTokenRefresh {
			return false, nil // a valid token is waiting for the agent
		}
		return false, l.writeToken(ctx, h.ID, tokFile)
	}

	_ = os.Remove(tokFile) // enrolled: the token is spent
	if !strings.HasPrefix(h.Name, localPlaceholder) {
		return true, nil
	}
	var hello wire.Hello
	_ = json.Unmarshal(h.Info, &hello)
	name := strings.TrimSpace(hello.Hostname)
	if name == "" || len(name) > 64 {
		return false, nil // wait for the agent's first hello
	}
	switch err := l.st.RenameHost(ctx, h.ID, name); {
	case errors.Is(err, store.ErrConflict):
		l.log.Info("kept the local host's name: another host is already called "+name, "host_id", h.ID)
	case err != nil:
		return false, err
	default:
		l.log.Info("named the local host after its server", "name", name)
		if l.renamed != nil {
			l.renamed(h.ID)
		}
	}
	return true, nil
}

func (l *localAgent) createHost(ctx context.Context) (store.Host, error) {
	for i := 1; i <= 20; i++ {
		name := localPlaceholder
		if i > 1 {
			name = fmt.Sprintf("%s-%d", localPlaceholder, i)
		}
		h, err := l.st.CreateHost(ctx, name)
		if !errors.Is(err, store.ErrConflict) {
			return h, err
		}
	}
	return store.Host{}, errors.New("could not pick a name for the local host")
}

func (l *localAgent) writeToken(ctx context.Context, hostID, path string) error {
	tok := wire.JoinToken{ID: randHex(16), Secret: randHex(32), CAFingerprint: l.caFP}
	if err := l.st.CreateJoinToken(ctx, tok.ID, hostID, gateway.HashSecret(tok.Secret), l.now().Add(localTokenTTL)); err != nil {
		return err
	}
	// 0644: the agent runs as root without CAP_DAC_OVERRIDE, and this
	// volume is mounted only into the hub and its local agent.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(tok.Encode()+"\n"), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
