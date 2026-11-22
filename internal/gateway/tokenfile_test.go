package gateway_test

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prezmc/tainerhub/internal/agent"
	"github.com/prezmc/tainerhub/internal/gateway"
	"github.com/prezmc/tainerhub/internal/pki"
	"github.com/prezmc/tainerhub/internal/registry"
	"github.com/prezmc/tainerhub/internal/wire"
)

// The hub's own agent starts at the same time as the hub and finds its join
// token in a shared file, possibly only after it has started. It connects by
// a name ("localhost") the agent never configured a certificate for.
func TestAgentEnrollsFromTokenFile(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	dir := t.TempDir()
	ca, err := pki.LoadOrCreateCA(dir + "/pki")
	if err != nil {
		t.Fatal(err)
	}
	const hostID = "22222222-2222-3333-4444-555555555555"
	st := &memStore{tokens: map[string]*gateway.JoinTokenRecord{
		"tokL": {ID: "tokL", HostID: hostID, SecretHash: gateway.HashSecret("local-secret"), ExpiresAt: time.Now().Add(time.Hour)},
	}, hosts: map[string]*gateway.HostCert{hostID: {Name: "this-server"}}}
	reg := registry.New()
	gw := &gateway.Gateway{CA: ca, Store: st, Reg: reg, Log: log, PingEvery: 200 * time.Millisecond, DeadAfter: 2 * time.Second}
	srvCert, err := ca.ServerCert([]string{"tainerhub-gateway.invalid"}) // deliberately not the dialled name
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(gw.Handler())
	srv.TLS = gw.TLSConfig(srvCert)
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())

	tokFile := filepath.Join(dir, "share", "join_token")
	ag, err := agent.New(agent.Config{
		HubAddr: "localhost:" + port, JoinTokenFile: tokFile, DataDir: dir + "/agent", Log: log,
		Docker: &fakeDocker{running: map[string]bool{}, events: make(chan wire.DockerEvent)}, SnapshotEvery: time.Hour, StatsEvery: -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = ag.Run(ctx) }()

	time.Sleep(500 * time.Millisecond) // the agent is waiting; now the hub writes the token
	if err := os.MkdirAll(filepath.Dir(tokFile), 0o755); err != nil {
		t.Fatal(err)
	}
	tok := wire.JoinToken{ID: "tokL", Secret: "local-secret", CAFingerprint: ca.Fingerprint()}.Encode()
	if err := os.WriteFile(tokFile, []byte(tok+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "local agent connected", func() bool { return reg.Get(hostID) != nil })
	if !st.tokens["tokL"].Used {
		t.Fatal("token should be marked used")
	}
}
