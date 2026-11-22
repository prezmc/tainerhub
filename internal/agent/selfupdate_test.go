package agent

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/prezmc/tainerhub/internal/wire"
)

func selfUpdateEngine(t *testing.T, labels map[string]string) (*SocketDocker, *map[string]any, *bool) {
	t.Helper()
	dir, err := os.MkdirTemp("", "su") // short: unix socket paths are limited to ~104 bytes on macOS
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	created := map[string]any{}
	started := false
	mux := http.NewServeMux()
	mux.HandleFunc("GET /containers/{id}/json", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"Config": map[string]any{"Image": "ghcr.io/prezmc/tainerhub-agent:latest", "Labels": labels},
			"Mounts": []map[string]string{{"Source": "/run/user-docker.sock", "Destination": "/var/run/docker.sock"}},
		})
	})
	mux.HandleFunc("POST /containers/create", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		_ = json.NewDecoder(r.Body).Decode(&created)
		created["_name"] = r.URL.Query().Get("name")
		mu.Unlock()
		_, _ = w.Write([]byte(`{"Id":"helper123"}`))
	})
	mux.HandleFunc("POST /containers/helper123/start", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		started = true
		mu.Unlock()
		w.WriteHeader(204)
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { srv.Close() })
	d := NewSocketDocker(sock)
	d.socket = "/var/run/docker.sock" // the path inside the agent container
	return d, &created, &started
}

func TestSelfUpdateStartsComposeHelper(t *testing.T) {
	d, created, started := selfUpdateEngine(t, map[string]string{
		"com.docker.compose.project":              "tainerhub",
		"com.docker.compose.project.working_dir":  "/root/tainerhub",
		"com.docker.compose.project.config_files": "/root/tainerhub/compose.yml,/etc/extra/override.yml",
	})
	out, err := d.Operate(context.Background(), wire.Operation{Kind: wire.OpSelfUpdate})
	if err != nil {
		t.Fatal(err)
	}
	res := out.(wire.SelfUpdateResult)
	if res.Project != "tainerhub" || res.Dir != "/root/tainerhub" || !strings.HasPrefix(res.Helper, "tainerhub-updater-") {
		t.Fatalf("result %+v", res)
	}
	if !*started {
		t.Fatal("helper not started")
	}
	c := *created
	if c["Image"] != "ghcr.io/prezmc/tainerhub-agent:latest" {
		t.Fatalf("helper image %v", c["Image"])
	}
	var cmd []string
	for _, x := range c["Cmd"].([]any) {
		cmd = append(cmd, x.(string))
	}
	joined := strings.Join(cmd, " ")
	for _, want := range []string{"--project-name tainerhub", "--project-directory /root/tainerhub", "--file /root/tainerhub/compose.yml", "--file /etc/extra/override.yml", "up --detach --pull always"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("cmd %q missing %q", joined, want)
		}
	}
	hc := c["HostConfig"].(map[string]any)
	var binds []string
	for _, b := range hc["Binds"].([]any) {
		binds = append(binds, b.(string))
	}
	for _, want := range []string{"/run/user-docker.sock:/var/run/docker.sock", "/root/tainerhub:/root/tainerhub:ro", "/etc/extra:/etc/extra:ro"} {
		if !slices.Contains(binds, want) {
			t.Fatalf("binds %v missing %q", binds, want)
		}
	}
	if hc["AutoRemove"] != true {
		t.Fatal("helper must remove itself")
	}
}

func TestSelfUpdateNeedsCompose(t *testing.T) {
	d, _, started := selfUpdateEngine(t, map[string]string{})
	_, err := d.Operate(context.Background(), wire.Operation{Kind: wire.OpSelfUpdate})
	if err == nil || !strings.Contains(err.Error(), "docker compose") || *started {
		t.Fatalf("err=%v started=%v", err, *started)
	}
}
