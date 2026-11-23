package gateway_test

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
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

// volumeDocker is fakeDocker plus in-memory volumes that can be copied:
// each volume is kept as the tar stream Docker would produce for it.
type volumeDocker struct {
	fakeDocker
	mu      sync.Mutex
	volumes map[string][]byte // name -> tar of its contents under th-data/
	labels  map[string]map[string]string
	helpers map[string]string // helper id -> volume
	n       int
}

func newVolumeDocker() *volumeDocker {
	return &volumeDocker{
		fakeDocker: fakeDocker{running: map[string]bool{}, events: make(chan wire.DockerEvent)},
		volumes:    map[string][]byte{}, labels: map[string]map[string]string{}, helpers: map[string]string{},
	}
}

func (d *volumeDocker) HelperImage(context.Context) (string, error) { return "img", nil }

func (d *volumeDocker) CreateHelper(_ context.Context, _ string, m agent.HelperMount) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.n++
	id := "h" + string(rune('0'+d.n))
	d.helpers[id] = m.Volume
	return id, nil
}

func (d *volumeDocker) RemoveContainer(_ context.Context, id string) error {
	d.mu.Lock()
	delete(d.helpers, id)
	d.mu.Unlock()
	return nil
}

func (d *volumeDocker) ArchiveGet(_ context.Context, id, _ string) (io.ReadCloser, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return io.NopCloser(bytes.NewReader(d.volumes[d.helpers[id]])), nil
}

func (d *volumeDocker) ArchivePut(_ context.Context, id, _ string, r io.Reader) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	d.mu.Lock()
	d.volumes[d.helpers[id]] = b
	d.mu.Unlock()
	return nil
}

func (d *volumeDocker) VolumeInfo(_ context.Context, name string) (agent.VolumeInfo, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.volumes[name]
	return agent.VolumeInfo{Driver: "local", Labels: d.labels[name]}, ok, nil
}

func (d *volumeDocker) CreateVolume(_ context.Context, name string, labels map[string]string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.volumes[name]; ok {
		return errors.New("exists")
	}
	d.volumes[name], d.labels[name] = nil, labels
	return nil
}

func (d *volumeDocker) RemoveVolume(_ context.Context, name string) error {
	d.mu.Lock()
	delete(d.volumes, name)
	d.mu.Unlock()
	return nil
}

// files lists a volume's entries as "name:content".
func (d *volumeDocker) files(t *testing.T, name string) []string {
	t.Helper()
	d.mu.Lock()
	b := d.volumes[name]
	d.mu.Unlock()
	var out []string
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		c, _ := io.ReadAll(tr)
		out = append(out, h.Name+":"+string(c))
	}
}

func TestTransferThroughGateway(t *testing.T) {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	dir := t.TempDir()
	ca, err := pki.LoadOrCreateCA(dir + "/pki")
	if err != nil {
		t.Fatal(err)
	}
	const hostA, hostB = "aaaaaaaa-0000-0000-0000-000000000001", "bbbbbbbb-0000-0000-0000-000000000002"
	store := &memStore{
		tokens: map[string]*gateway.JoinTokenRecord{
			"ta": {ID: "ta", HostID: hostA, SecretHash: gateway.HashSecret("sa"), ExpiresAt: time.Now().Add(time.Hour)},
			"tb": {ID: "tb", HostID: hostB, SecretHash: gateway.HashSecret("sb"), ExpiresAt: time.Now().Add(time.Hour)},
		},
		hosts: map[string]*gateway.HostCert{hostA: {Name: "a"}, hostB: {Name: "b"}},
	}
	reg := registry.New()
	gw := &gateway.Gateway{CA: ca, Store: store, Reg: reg, Log: log, PingEvery: 200 * time.Millisecond, DeadAfter: 5 * time.Second}
	srvCert, err := ca.ServerCert([]string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(gw.Handler())
	srv.TLS = gw.TLSConfig(srvCert)
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())

	// Host A has a volume with a sizeable file; host B has nothing.
	var vol bytes.Buffer
	tw := tar.NewWriter(&vol)
	big := strings.Repeat("0123456789abcdef", 256<<10) // 4 MiB, more than one HTTP/2 window
	_ = tw.WriteHeader(&tar.Header{Name: "th-data/", Typeflag: tar.TypeDir, Mode: 0o755})
	_ = tw.WriteHeader(&tar.Header{Name: "th-data/pg/", Typeflag: tar.TypeDir, Mode: 0o700, Uid: 999, Gid: 999})
	_ = tw.WriteHeader(&tar.Header{Name: "th-data/pg/base", Typeflag: tar.TypeReg, Mode: 0o600, Uid: 999, Gid: 999, Size: int64(len(big))})
	_, _ = io.WriteString(tw, big)
	_ = tw.Close()
	dockA, dockB := newVolumeDocker(), newVolumeDocker()
	dockA.volumes["app_db"] = vol.Bytes()
	dockA.labels["app_db"] = map[string]string{"com.docker.compose.project": "app"}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for _, h := range []struct {
		id, tok, secret string
		d               *volumeDocker
	}{{hostA, "ta", "sa", dockA}, {hostB, "tb", "sb", dockB}} {
		ag, err := agent.New(agent.Config{
			HubAddr: "127.0.0.1:" + port, DataDir: dir + "/" + h.tok, Log: log, Docker: h.d,
			JoinToken: wire.JoinToken{ID: h.tok, Secret: h.secret, CAFingerprint: ca.Fingerprint()}.Encode(),
			PingEvery: 200 * time.Millisecond, DeadAfter: 5 * time.Second, SnapshotEvery: time.Hour, StatsEvery: -1,
		})
		if err != nil {
			t.Fatal(err)
		}
		go func() { _ = ag.Run(ctx) }()
	}
	for _, id := range []string{hostA, hostB} {
		waitFor(t, "agent "+id, func() bool {
			a := reg.Get(id)
			return a != nil && a.Status().Hello != nil && a.Status().Hello.Transfer
		})
	}

	item := wire.TransferItem{Kind: wire.TransferVolume, Name: "app_db", Labels: dockA.labels["app_db"]}
	res, err := reg.Copy(ctx, hostA, hostB, item, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Files != 3 || res.Bytes < int64(len(big)) {
		t.Errorf("result %+v", res)
	}
	if got, want := dockB.files(t, "app_db"), dockA.files(t, "app_db"); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("copy differs: %d entries vs %d", len(got), len(want))
	}
	if dockB.labels["app_db"]["com.docker.compose.project"] != "app" {
		t.Error("labels not copied")
	}

	// The same volume again: host B refuses to overwrite it, and the error says why.
	if _, err := reg.Copy(ctx, hostA, hostB, item, 0, nil); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("second copy: %v", err)
	}
	if len(dockB.files(t, "app_db")) != 3 {
		t.Error("the existing volume was changed")
	}

	// Outside a transfer the endpoints answer nothing useful, and never without a client certificate.
	resp, err := srv.Client().Get(srv.URL + wire.PathTransfer + strings.Repeat("0", 32))
	if err == nil {
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("anonymous download: %s", resp.Status)
		}
	}
}
