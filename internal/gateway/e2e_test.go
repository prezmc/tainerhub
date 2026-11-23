package gateway_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prezmc/tainerhub/internal/agent"
	"github.com/prezmc/tainerhub/internal/gateway"
	"github.com/prezmc/tainerhub/internal/pki"
	"github.com/prezmc/tainerhub/internal/registry"
	"github.com/prezmc/tainerhub/internal/wire"
)

// ---- in-memory store ----

type memStore struct {
	mu     sync.Mutex
	tokens map[string]*gateway.JoinTokenRecord
	hosts  map[string]*gateway.HostCert

	renewals    int
	hellos      int      // TouchHost calls with a Hello
	enrollments []string // "hostName ip reenrolled", from RecordEnrollment
}

func (m *memStore) GetJoinToken(_ context.Context, id string) (gateway.JoinTokenRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tokens[id]
	if !ok {
		return gateway.JoinTokenRecord{}, gateway.ErrNotFound
	}
	return *t, nil
}

func (m *memStore) CompleteEnrollment(_ context.Context, tokenID, hostID, serial string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.tokens[tokenID]
	if t == nil || t.Used {
		return errors.New("used")
	}
	t.Used = true
	m.hosts[hostID].CertSerial = serial
	return nil
}

func (m *memStore) GetHostCert(_ context.Context, hostID string) (gateway.HostCert, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	h, ok := m.hosts[hostID]
	if !ok {
		return gateway.HostCert{}, gateway.ErrNotFound
	}
	return *h, nil
}

func (m *memStore) RenewHostCert(_ context.Context, hostID, oldSerial, newSerial string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	h, ok := m.hosts[hostID]
	if !ok || oldSerial == "" || h.CertSerial != oldSerial {
		return gateway.ErrSerialChanged
	}
	h.CertSerial = newSerial
	m.renewals++
	return nil
}

func (m *memStore) serial(hostID string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hosts[hostID].CertSerial
}

func (m *memStore) TouchHost(_ context.Context, _ string, hello *wire.Hello, _ time.Time) error {
	if hello != nil {
		m.mu.Lock()
		m.hellos++
		m.mu.Unlock()
	}
	return nil
}

// ---- fake docker ----

type fakeDocker struct {
	mu      sync.Mutex
	running map[string]bool
	events  chan wire.DockerEvent
	lists   int // container list calls, which are costly for a real Docker
}

func (f *fakeDocker) Info(context.Context) (wire.Hello, error) {
	return wire.Hello{Hostname: "server-1", DockerVersion: "28.0.0", NCPU: 4}, nil
}

func (f *fakeDocker) List(context.Context) ([]wire.Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists++
	state := "exited"
	if f.running["abc123"] {
		state = "running"
	}
	return []wire.Container{{ID: "abc123", Name: "web", Image: "nginx", State: state}}, nil
}

func (f *fakeDocker) Events(ctx context.Context, fn func(wire.DockerEvent)) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev := <-f.events:
			fn(ev)
		}
	}
}

func (f *fakeDocker) Action(_ context.Context, id, action string) error {
	if id != "abc123" {
		return errors.New("no such container")
	}
	f.mu.Lock()
	f.running[id] = action == wire.ActionStart || action == wire.ActionRestart
	f.mu.Unlock()
	return nil
}

func (f *fakeDocker) Stats(context.Context, string) (agent.Usage, error) {
	return agent.Usage{CPU: 12.34, Mem: 64 << 20, MemLimit: 1 << 30}, nil
}

func (f *fakeDocker) Operate(_ context.Context, op wire.Operation) (any, error) {
	switch op.Kind {
	case wire.OpContainerInspect:
		return wire.ContainerDetail{ID: op.Target, Name: "web", RestartPolicy: "unless-stopped"}, nil
	case wire.OpImageRemove:
		return nil, errors.New("conflict: image is being used by running container")
	}
	return []string{}, nil
}

func (f *fakeDocker) Logs(ctx context.Context, id string, tail int, follow bool, w io.Writer) error {
	if _, err := io.WriteString(w, "line 1\nline 2\n"); err != nil {
		return err
	}
	if follow {
		<-ctx.Done() // keep following until cancelled
	}
	return nil
}

// ---- test ----

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestEnrollConnectActLogsRevoke(t *testing.T) {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	dir := t.TempDir()
	ca, err := pki.LoadOrCreateCA(dir + "/pki")
	if err != nil {
		t.Fatal(err)
	}
	const hostID = "11111111-2222-3333-4444-555555555555"
	store := &memStore{
		tokens: map[string]*gateway.JoinTokenRecord{
			"tok1": {ID: "tok1", HostID: hostID, SecretHash: gateway.HashSecret("s3cret"), ExpiresAt: time.Now().Add(time.Hour)},
		},
		hosts: map[string]*gateway.HostCert{hostID: {Name: "server-1"}},
	}
	reg := registry.New()
	var gotEvent sync.WaitGroup
	gotEvent.Add(1)
	var once sync.Once
	reg.OnEvent = func(string, wire.DockerEvent) { once.Do(gotEvent.Done) }
	gw := &gateway.Gateway{CA: ca, Store: store, Reg: reg, Log: log, PingEvery: 200 * time.Millisecond, DeadAfter: 2 * time.Second}

	srvCert, err := ca.ServerCert([]string{"127.0.0.1", "localhost"})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(gw.Handler())
	srv.TLS = gw.TLSConfig(srvCert)
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())

	token := wire.JoinToken{ID: "tok1", Secret: "s3cret", CAFingerprint: ca.Fingerprint()}.Encode()

	// A token pinned to a different CA must be refused by the agent before
	// it sends anything sensitive.
	otherCA, _ := pki.LoadOrCreateCA(dir + "/other")
	bad, _ := agent.New(agent.Config{
		HubAddr: "127.0.0.1:" + port, DataDir: dir + "/bad", Log: log,
		JoinToken: wire.JoinToken{ID: "tok1", Secret: "s3cret", CAFingerprint: otherCA.Fingerprint()}.Encode(),
		Docker:    &fakeDocker{running: map[string]bool{}, events: make(chan wire.DockerEvent)},
	})
	ctxBad, cancelBad := context.WithTimeout(context.Background(), 3*time.Second)
	err = bad.Run(ctxBad)
	cancelBad()
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("expected pin mismatch, got %v", err)
	}

	docker := &fakeDocker{running: map[string]bool{}, events: make(chan wire.DockerEvent, 1)}
	fakeCompose := dir + "/docker-compose"
	if err := os.WriteFile(fakeCompose, []byte("#!/bin/sh\ncase \"$*\" in *version*) echo 5.8.0;; *\" up \"*) echo 'Container web-1  Started';; esac\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ag, err := agent.New(agent.Config{
		HubAddr: "127.0.0.1:" + port, JoinToken: token, DataDir: dir + "/agent", Version: "test",
		Docker: docker, Log: log,
		Stacks: &agent.StackRunner{Dir: dir + "/stacks", Bin: fakeCompose, Socket: "/var/run/docker.sock", WorkDir: dir + "/agent/compose"}, PingEvery: 200 * time.Millisecond, DeadAfter: 2 * time.Second, SnapshotEvery: time.Hour, StatsEvery: 300 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	agentDone := make(chan error, 1)
	go func() { agentDone <- ag.Run(ctx) }()

	// 1. enrollment + connection + hello + first snapshot
	waitFor(t, "agent snapshot", func() bool {
		a := reg.Get(hostID)
		if a == nil {
			return false
		}
		st := a.Status()
		return st.Hello != nil && st.Hello.Hostname == "server-1" && len(st.Containers) == 1
	})
	if !store.tokens["tok1"].Used {
		t.Fatal("join token should be marked used")
	}

	// 2. a second enrollment with the same token is refused
	second, _ := agent.New(agent.Config{HubAddr: "127.0.0.1:" + port, JoinToken: token, DataDir: dir + "/second", Docker: docker, Log: log})
	ctx2, cancel2 := context.WithTimeout(context.Background(), 3*time.Second)
	if err := second.Run(ctx2); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("expected token reuse to be refused, got %v", err)
	}
	cancel2()

	// 3. container action round trip, followed by a fresh snapshot
	a := reg.Get(hostID)
	if err := a.Do(context.Background(), wire.ContainerAction{ContainerID: "abc123", Action: wire.ActionStart}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitFor(t, "running state", func() bool {
		cs := reg.Get(hostID).Status().Containers
		return len(cs) == 1 && cs[0].State == "running"
	})
	if err := a.Do(context.Background(), wire.ContainerAction{ContainerID: "nope", Action: wire.ActionStop}); err == nil || !strings.Contains(err.Error(), "no such container") {
		t.Fatalf("expected agent error to propagate, got %v", err)
	}

	// 3b. a compose stack deploy streams its output and outcome back
	if sup := a.Status().Hello.Stacks; sup == nil || sup.Error != "" || sup.ComposeVersion != "5.8.0" {
		t.Fatalf("stack support not reported: %+v", sup)
	}
	ch, err := a.Stack(context.Background(), wire.StackCommand{Action: wire.StackDeploy, Name: "web", Compose: "services: {}\n"})
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	var last wire.LogChunk
	for c := range ch {
		out.Write(c.Data)
		last = c
	}
	if !last.EOF || last.Error != "" || !strings.Contains(out.String(), "Container web-1  Started") {
		t.Fatalf("stack deploy: final=%+v output:\n%s", last, out.String())
	}
	if b, _ := os.ReadFile(dir + "/stacks/web/compose.yaml"); string(b) != "services: {}\n" {
		t.Fatalf("compose file on host = %q", b)
	}
	ch, _ = a.Stack(context.Background(), wire.StackCommand{Action: wire.StackStart, Name: "never-deployed"})
	for c := range ch {
		last = c
	}
	if !strings.Contains(last.Error, "not been deployed") {
		t.Fatalf("expected agent error for an undeployed stack, got %+v", last)
	}

	// 4. docker events reach the hub
	docker.events <- wire.DockerEvent{Type: "container", Action: "die", ID: "abc123", Name: "web"}
	evDone := make(chan struct{})
	go func() { gotEvent.Wait(); close(evDone) }()
	select {
	case <-evDone:
	case <-time.After(5 * time.Second):
		t.Fatal("event not received")
	}

	// 5. log streaming with follow, then cancel
	ch, stop, err := a.Logs(context.Background(), wire.LogsRequest{ContainerID: "abc123", Tail: 10, Follow: true})
	if err != nil {
		t.Fatal(err)
	}
	var got strings.Builder
	timeout := time.After(5 * time.Second)
	for !strings.Contains(got.String(), "line 2") {
		select {
		case c := <-ch:
			got.Write(c.Data)
		case <-timeout:
			t.Fatalf("logs: got %q", got.String())
		}
	}
	stop()

	// 5a. resource operations: data comes back, agent errors propagate,
	// invalid operations are refused before they leave the hub.
	raw, err := a.Operate(context.Background(), wire.Operation{Kind: wire.OpContainerInspect, Target: "abc123"})
	if err != nil || !strings.Contains(string(raw), `"restartPolicy":"unless-stopped"`) {
		t.Fatalf("inspect: %v %s", err, raw)
	}
	if _, err := a.Operate(context.Background(), wire.Operation{Kind: wire.OpImageRemove, Target: "sha256:0123456789abcdef"}); err == nil || !strings.Contains(err.Error(), "being used") {
		t.Fatalf("expected agent error, got %v", err)
	}
	if _, err := a.Operate(context.Background(), wire.Operation{Kind: "container.exec", Target: "abc123"}); err == nil {
		t.Fatal("unknown operation should be refused")
	}

	// 5b. live CPU / memory arrive for running containers. With nobody
	// watching the agent samples only every minute; opening a fleet stream
	// (a browser tab) makes it sample straight away.
	release := reg.Watch()
	defer release()
	waitFor(t, "container stats", func() bool {
		cs := reg.Get(hostID).Status().Containers
		return len(cs) == 1 && cs[0].CPU == 12.3 && cs[0].Mem == 64<<20
	})
	// Stats rounds reuse the last container list rather than asking Docker
	// again. (Let the refresh that watching starts settle first.)
	time.Sleep(600 * time.Millisecond)
	docker.mu.Lock()
	lists := docker.lists
	docker.mu.Unlock()
	time.Sleep(1200 * time.Millisecond) // about 4 stats rounds
	docker.mu.Lock()
	if docker.lists != lists {
		t.Errorf("stats rounds listed containers %d times; want 0", docker.lists-lists)
	}
	docker.mu.Unlock()

	// 6. the connection survives idle time thanks to pings
	time.Sleep(2500 * time.Millisecond)
	if reg.Get(hostID) == nil {
		t.Fatal("agent dropped while idle")
	}

	// 7. revocation: hub drops the stream and refuses reconnects
	store.mu.Lock()
	store.hosts[hostID].Revoked = true
	store.mu.Unlock()
	reg.Disconnect(hostID)
	waitFor(t, "disconnect", func() bool { return reg.Get(hostID) == nil })
	time.Sleep(1500 * time.Millisecond) // agent retries after 1s backoff
	if reg.Get(hostID) != nil {
		t.Fatal("revoked agent reconnected")
	}

	cancel()
	select {
	case <-agentDone:
	case <-time.After(5 * time.Second):
		t.Fatal("agent did not stop")
	}
}
