package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prezmc/tainerhub/internal/store"
	"github.com/prezmc/tainerhub/internal/wire"
)

type memHosts struct {
	hosts  map[string]*store.Host
	tokens map[string]string // token ID -> host ID
	n      int
}

func (m *memHosts) CreateHost(_ context.Context, name string) (store.Host, error) {
	for _, h := range m.hosts {
		if h.Name == name {
			return store.Host{}, store.ErrConflict
		}
	}
	m.n++
	h := &store.Host{ID: string(rune('a' + m.n)), Name: name}
	m.hosts[h.ID] = h
	return *h, nil
}
func (m *memHosts) GetHost(_ context.Context, id string) (store.Host, error) {
	h, ok := m.hosts[id]
	if !ok {
		return store.Host{}, store.ErrNotFound
	}
	return *h, nil
}
func (m *memHosts) CreateJoinToken(_ context.Context, id, hostID string, _ []byte, _ time.Time) error {
	m.tokens[id] = hostID
	return nil
}
func (m *memHosts) RenameHost(_ context.Context, id, name string) error {
	for _, h := range m.hosts {
		if h.Name == name && h.ID != id {
			return store.ErrConflict
		}
	}
	m.hosts[id].Name = name
	return nil
}

func setupLocal(t *testing.T) (*localAgent, *memHosts, *time.Time) {
	t.Helper()
	m := &memHosts{hosts: map[string]*store.Host{}, tokens: map[string]string{}}
	now := time.Now()
	la := &localAgent{
		st: m, caFP: strings.Repeat("ab", 32), dataDir: t.TempDir(), shareDir: t.TempDir(),
		log: slog.New(slog.NewTextHandler(io.Discard, nil)), now: func() time.Time { return now },
	}
	return la, m, &now
}

func TestLocalAgentLifecycle(t *testing.T) {
	ctx := context.Background()
	la, m, now := setupLocal(t)
	tokFile := filepath.Join(la.shareDir, localTokenFile)

	// First start: a host and a join token for the local agent.
	if done, err := la.step(ctx); done || err != nil {
		t.Fatalf("step 1: done=%v err=%v", done, err)
	}
	if len(m.hosts) != 1 {
		t.Fatalf("hosts = %d", len(m.hosts))
	}
	b, err := os.ReadFile(tokFile)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := wire.ParseJoinToken(string(b))
	if err != nil || tok.CAFingerprint != la.caFP {
		t.Fatalf("token file: %v %+v", err, tok)
	}
	var hostID string
	for id := range m.hosts {
		hostID = id
	}
	if m.tokens[tok.ID] != hostID {
		t.Fatal("token not issued for the local host")
	}

	// A restart doesn't create a second host or churn the token.
	if _, err := la.step(ctx); err != nil {
		t.Fatal(err)
	}
	if len(m.hosts) != 1 || len(m.tokens) != 1 {
		t.Fatalf("restart duplicated things: hosts=%d tokens=%d", len(m.hosts), len(m.tokens))
	}

	// An old token is replaced before it can expire.
	old := now.Add(-localTokenRefresh - time.Minute)
	_ = os.Chtimes(tokFile, old, old)
	if _, err := la.step(ctx); err != nil {
		t.Fatal(err)
	}
	if len(m.tokens) != 2 {
		t.Fatal("stale token not refreshed")
	}

	// The agent enrolls and says hello: token removed, host renamed.
	enrolled := *now
	m.hosts[hostID].EnrolledAt = &enrolled
	if done, _ := la.step(ctx); done {
		t.Fatal("should wait for the agent's hello before naming the host")
	}
	if _, err := os.Stat(tokFile); !os.IsNotExist(err) {
		t.Fatal("token file kept after enrollment")
	}
	m.hosts[hostID].Info, _ = json.Marshal(wire.Hello{Hostname: "container1"})
	if done, err := la.step(ctx); !done || err != nil {
		t.Fatalf("done=%v err=%v", done, err)
	}
	if m.hosts[hostID].Name != "container1" {
		t.Fatalf("name = %q", m.hosts[hostID].Name)
	}
}

func TestLocalAgentRespectsRemoval(t *testing.T) {
	ctx := context.Background()
	la, m, _ := setupLocal(t)
	if _, err := la.step(ctx); err != nil {
		t.Fatal(err)
	}
	for id := range m.hosts {
		delete(m.hosts, id) // admin removed it
	}
	if done, err := la.step(ctx); !done || err != nil {
		t.Fatalf("done=%v err=%v", done, err)
	}
	if len(m.hosts) != 0 {
		t.Fatal("removed local host was recreated")
	}
	if _, err := os.Stat(filepath.Join(la.shareDir, localTokenFile)); !os.IsNotExist(err) {
		t.Fatal("token left behind")
	}
}

func TestLocalAgentNameConflictKeepsPlaceholder(t *testing.T) {
	ctx := context.Background()
	la, m, now := setupLocal(t)
	_, _ = m.CreateHost(ctx, "container1") // added by hand earlier
	if _, err := la.step(ctx); err != nil {
		t.Fatal(err)
	}
	for _, h := range m.hosts {
		if h.Name == localPlaceholder {
			e := *now
			h.EnrolledAt = &e
			h.Info, _ = json.Marshal(wire.Hello{Hostname: "container1"})
		}
	}
	if done, err := la.step(ctx); !done || err != nil {
		t.Fatalf("done=%v err=%v", done, err)
	}
	names := map[string]bool{}
	for _, h := range m.hosts {
		names[h.Name] = true
	}
	if !names["container1"] || !names[localPlaceholder] {
		t.Fatalf("names = %v", names)
	}
}
