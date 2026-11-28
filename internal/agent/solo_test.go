package agent

import (
	"context"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prezmc/tainerhub/internal/wire"
)

func TestMarkSolo(t *testing.T) {
	cs := []wire.Container{
		{Name: "plain"},
		{Name: "radarr", Project: "radarr"},
		{Name: "vpn", Project: "media"},
		{Name: "sonarr", Project: "media"},
	}
	markSolo(cs)
	got := map[string]bool{}
	for _, c := range cs {
		got[c.Name] = c.Solo
	}
	if got["plain"] || !got["radarr"] || got["vpn"] || got["sonarr"] {
		t.Fatalf("solo %v; want only radarr", got)
	}
	if !wire.UpdatesAlone(cs[0], false) || !wire.UpdatesAlone(cs[1], false) || wire.UpdatesAlone(cs[1], true) || wire.UpdatesAlone(cs[2], false) {
		t.Fatal("UpdatesAlone: want plain and unmanaged radarr only")
	}
}

// A container that shares its compose project with others is refused
// before anything is pulled or stopped.
func TestRecreateRefusesContainerOfSeveral(t *testing.T) {
	dir, err := os.MkdirTemp("", "rs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /containers/sonarr/json", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"Id":"s1","Name":"/sonarr","Image":"sha256:old","Config":{"Image":"sonarr:4","Labels":{"com.docker.compose.project":"media"}}}`))
	})
	mux.HandleFunc("GET /containers/json", func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Query().Get("filters"), "com.docker.compose.project=media") {
			t.Errorf("unfiltered list: %s", r.URL.RawQuery)
		}
		w.Write([]byte(`[{"Id":"s1"},{"Id":"v1"}]`))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })

	_, err = NewSocketDocker(sock).recreate(context.Background(), "sonarr", nil)
	if err == nil || !strings.Contains(err.Error(), "one of 2 containers in the media compose stack") {
		t.Fatalf("got %v", err)
	}
}

// TestRecreateSoloComposeRealDocker: the only container of a compose
// project started by hand updates on its own, and compose still treats it
// as the service's container afterwards (`up -d` leaves it alone). Needs a
// registry at localhost:5001, like TestRecreateRealDocker.
//
//	TAINERHUB_REAL_REGISTRY=1 go test ./internal/agent -run SoloComposeReal -v
func TestRecreateSoloComposeRealDocker(t *testing.T) {
	if os.Getenv("TAINERHUB_REAL_REGISTRY") != "1" {
		t.Skip("set TAINERHUB_REAL_REGISTRY=1 (and run a registry on localhost:5001)")
	}
	socket := os.Getenv("DOCKER_SOCKET")
	if socket == "" {
		socket = "/var/run/docker.sock"
	}
	const img, project = "localhost:5001/thsolo:latest", "thsolo"
	dir := t.TempDir()
	compose := filepath.Join(dir, "compose.yml")
	if err := os.WriteFile(compose, []byte("services:\n  app:\n    image: "+img+"\n    command: [\"sleep\", \"3600\"]\n    environment: [MINE=kept]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("docker", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("docker %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	build := func(v string) {
		cmd := exec.Command("docker", "build", "-q", "-t", img, "-")
		cmd.Stdin = strings.NewReader("FROM alpine:3\nRUN echo " + v + " > /version\n")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v %s", v, err, out)
		}
		run("push", "-q", img)
	}
	cleanup := func() {
		_ = exec.Command("docker", "compose", "-p", project, "-f", compose, "down").Run()
		_ = exec.Command("docker", "rmi", img, "thsolo-v1:keep").Run()
	}
	cleanup()
	defer cleanup()
	build("v1")
	run("compose", "-p", project, "-f", compose, "up", "-d")
	name := run("ps", "-q", "--filter", "label=com.docker.compose.project="+project)
	run("tag", img, "thsolo-v1:keep")
	build("v2")
	run("tag", "thsolo-v1:keep", img) // the registry has v2, this host's tag still v1

	d := NewSocketDocker(socket)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cs, err := d.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cs {
		if c.Project == project && !c.Solo {
			t.Fatalf("%s should be solo", c.Name)
		}
	}
	res, err := d.recreate(ctx, name, nil)
	if err != nil || res.Status != "updated" {
		t.Fatalf("recreate: %+v %v", res, err)
	}
	after := run("ps", "-q", "--no-trunc", "--filter", "label=com.docker.compose.project="+project)
	if v := run("exec", after[:12], "cat", "/version"); v != "v2" {
		t.Fatalf("runs %s, want v2", v)
	}
	if env := run("inspect", "-f", "{{json .Config.Env}}", after); !strings.Contains(env, "MINE=kept") {
		t.Fatalf("env lost: %s", env)
	}
	run("compose", "-p", project, "-f", compose, "up", "-d")
	if again := run("ps", "-q", "--no-trunc", "--filter", "label=com.docker.compose.project="+project); again != after {
		t.Fatalf("compose up replaced the updated container (%s → %s)", after[:12], again[:12])
	}
}
