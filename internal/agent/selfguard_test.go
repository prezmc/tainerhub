package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/prezmc/tainerhub/internal/wire"
)

const (
	selfID  = "aaaaaaaaaaaa1111111111111111111111111111111111111111111111111111"
	hubID   = "bbbbbbbbbbbb2222222222222222222222222222222222222222222222222222"
	otherID = "cccccccccccc3333333333333333333333333333333333333333333333333333"
)

// guardEngine fakes a Docker API with the agent's own container, the hub in
// the same compose project, and an unrelated app. It records which
// containers were actually stopped or removed.
func guardEngine(t *testing.T, inspectFails bool) (*SocketDocker, func() []string) {
	t.Helper()
	old, oldProc := selfHostname, selfProcFiles
	selfHostname = func() (string, error) { return selfID[:12], nil }
	selfProcFiles = nil // the test machine may itself be a container
	t.Cleanup(func() { selfHostname, selfProcFiles = old, oldProc })

	containers := map[string]map[string]any{}
	add := func(id, name, project string) {
		c := map[string]any{"Id": id, "Name": "/" + name, "Config": map[string]any{"Labels": map[string]string{}}}
		if project != "" {
			c["Config"] = map[string]any{"Labels": map[string]string{"com.docker.compose.project": project}}
		}
		containers[id[:12]], containers[name], containers[id] = c, c, c
	}
	add(selfID, "tainerhub-agent-1", "tainerhub")
	add(hubID, "tainerhub-tainerhub-1", "tainerhub")
	add(otherID, "web", "site")

	var mu sync.Mutex
	var done []string
	dir, err := os.MkdirTemp("", "sg") // short: macOS limits socket paths to 104 bytes
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
	mux.HandleFunc("GET /containers/{id}/json", func(w http.ResponseWriter, r *http.Request) {
		if inspectFails && r.PathValue("id") == selfID[:12] {
			w.WriteHeader(500)
			_, _ = w.Write([]byte(`{"message":"boom"}`))
			return
		}
		c, ok := containers[r.PathValue("id")]
		if !ok {
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"message":"No such container"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(c)
	})
	record := func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		done = append(done, r.Method+" "+r.PathValue("id"))
		mu.Unlock()
		w.WriteHeader(204)
	}
	mux.HandleFunc("POST /containers/{id}/{action}", record)
	mux.HandleFunc("DELETE /containers/{id}", record)
	mux.HandleFunc("GET /containers/json", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`[]`)) })
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { srv.Close() })
	return NewSocketDocker(sock), func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), done...)
	}
}

func TestActionRefusesToStopTainerHub(t *testing.T) {
	d, done := guardEngine(t, false)
	ctx := context.Background()
	for _, target := range []string{selfID[:12], selfID, "tainerhub-agent-1", "tainerhub-tainerhub-1", hubID[:12]} {
		for _, a := range []string{wire.ActionStop, wire.ActionKill, wire.ActionRemove, wire.ActionPause} {
			err := d.Action(ctx, target, a)
			if err == nil || !strings.Contains(err.Error(), "TainerHub itself") {
				t.Fatalf("%s %s: expected refusal, got %v", a, target, err)
			}
		}
		if _, err := d.Operate(ctx, wire.Operation{Kind: wire.OpContainerRemoveVolumes, Target: target}); err == nil || !strings.Contains(err.Error(), "TainerHub itself") {
			t.Fatalf("remove with volumes %s: expected refusal, got %v", target, err)
		}
	}
	if got := done(); len(got) != 0 {
		t.Fatalf("nothing should have reached Docker: %v", got)
	}
	// restart comes back, so it's allowed; so are start and unpause
	for _, a := range []string{wire.ActionRestart, wire.ActionStart, wire.ActionUnpause} {
		if err := d.Action(ctx, "tainerhub-tainerhub-1", a); err != nil {
			t.Fatalf("%s should be allowed: %v", a, err)
		}
	}
	// other containers are unaffected
	if err := d.Action(ctx, "web", wire.ActionStop); err != nil {
		t.Fatal(err)
	}
	if err := d.Action(ctx, "web", wire.ActionRemove); err != nil {
		t.Fatal(err)
	}
	if got := done(); len(got) != 5 {
		t.Fatalf("expected 5 calls to Docker, got %v", got)
	}
	if p := d.OwnProject(ctx); p != "tainerhub" {
		t.Fatalf("own project = %q", p)
	}
}

func TestActionProtectsOwnIDWhenSelfInspectFails(t *testing.T) {
	d, _ := guardEngine(t, true)
	ctx := context.Background()
	if err := d.Action(ctx, "tainerhub-agent-1", wire.ActionStop); err == nil {
		t.Fatal("own container should still be protected by its ID")
	}
	// without the project label, the rest of the project can't be recognized
	if err := d.Action(ctx, "tainerhub-tainerhub-1", wire.ActionStop); err != nil {
		t.Fatal(err)
	}
	if p := d.OwnProject(ctx); p != "" {
		t.Fatalf("own project should be unknown, got %q", p)
	}
}

func TestStackRefusesOwnProject(t *testing.T) {
	r, logs := fakeCompose(t)
	r.OwnProject = func(context.Context) string { return "tainerhub" }
	ctx := context.Background()
	for _, a := range []string{wire.StackDeploy, wire.StackDown, wire.StackRemove, wire.StackStop} {
		var out bytes.Buffer
		err := r.Run(ctx, wire.StackCommand{Name: "tainerhub", Action: a, Compose: goodCompose}, &out)
		if err == nil || !strings.Contains(err.Error(), "TainerHub itself runs in") {
			t.Fatalf("%s: expected refusal, got %v", a, err)
		}
	}
	if c := calls(t, logs); len(c) != 1 || c[0] != "" {
		t.Fatalf("compose should not have run: %v", c)
	}
	var out bytes.Buffer
	if err := r.Run(ctx, wire.StackCommand{Name: "site", Action: wire.StackDeploy, Compose: goodCompose}, &out); err != nil {
		t.Fatalf("other stacks still deploy: %v", err)
	}
}

func TestRecreateRefusesTainerHub(t *testing.T) {
	d, done := guardEngine(t, false)
	ctx := context.Background()
	for _, target := range []string{selfID[:12], "tainerhub-tainerhub-1"} {
		_, err := d.Operate(ctx, wire.Operation{Kind: wire.OpContainerRecreate, Target: target})
		if err == nil || !strings.Contains(err.Error(), "TainerHub itself") {
			t.Fatalf("recreate %s: expected refusal, got %v", target, err)
		}
	}
	if got := done(); len(got) != 0 {
		t.Fatalf("nothing should have reached Docker: %v", got)
	}
}

// With a custom hostname the agent finds its container ID in /proc instead.
func TestSelfFoundFromProcWithCustomHostname(t *testing.T) {
	d, done := guardEngine(t, false)
	selfHostname = func() (string, error) { return "my-agent", nil }
	dir := t.TempDir()
	mountinfo := filepath.Join(dir, "mountinfo")
	lines := "1234 1 0:52 / / rw,relatime - overlay overlay rw,lowerdir=/var/lib/docker/overlay2/" + otherID + "/diff\n" +
		"1300 1234 254:1 /docker/volumes/" + hubID + "/_data /data rw - ext4 /dev/vda1 rw\n" +
		"1301 1234 254:1 /docker/containers/" + selfID + "/hostname /etc/hostname rw,relatime - ext4 /dev/vda1 rw\n"
	mustOK(t, os.WriteFile(mountinfo, []byte(lines), 0o644))
	selfProcFiles = []string{filepath.Join(dir, "missing"), mountinfo}

	ctx := context.Background()
	if err := d.Action(ctx, "tainerhub-agent-1", wire.ActionStop); err == nil {
		t.Fatal("own container should be protected via /proc")
	}
	if err := d.Action(ctx, "tainerhub-tainerhub-1", wire.ActionStop); err == nil {
		t.Fatal("the hub in the agent's project should be protected")
	}
	if err := d.Action(ctx, "web", wire.ActionStop); err != nil {
		t.Fatal(err)
	}
	if got := done(); len(got) != 1 {
		t.Fatalf("expected only web stopped, got %v", got)
	}
}

// A lookup that finds nothing is not remembered, so protection comes back
// once the agent's container can be found.
func TestSelfLookupFailureNotCached(t *testing.T) {
	d, _ := guardEngine(t, false)
	selfHostname = func() (string, error) { return "my-agent", nil }
	cg := filepath.Join(t.TempDir(), "cgroup")
	mustOK(t, os.WriteFile(cg, []byte("0::/\n"), 0o644)) // cgroup v2: no ID here
	selfProcFiles = []string{cg}
	ctx := context.Background()
	if p := d.OwnProject(ctx); p != "" {
		t.Fatalf("own project = %q, want unknown", p)
	}
	mustOK(t, os.WriteFile(cg, []byte("12:cpu:/docker/"+selfID+"\n"), 0o644))
	if p := d.OwnProject(ctx); p != "tainerhub" {
		t.Fatalf("own project = %q once the ID is visible; the failure was cached", p)
	}
}

func TestContainerIDIn(t *testing.T) {
	for line, want := range map[string]string{
		"0::/system.slice/docker-" + selfID + ".scope":                                       selfID,
		"4:memory:/docker/" + selfID:                                                         selfID,
		"500 400 0:1 /containers/" + selfID + "/resolv.conf /etc/resolv.conf rw - ext4 x rw": selfID,
		"overlay / overlay rw,upperdir=/var/lib/docker/overlay2/" + selfID + "/diff":         "",
		"/var/lib/docker/volumes/" + selfID + "/_data":                                       "",
		"/var/lib/docker/containers/" + selfID + "ff/hostname":                               "", // too long
		"/var/lib/docker/containers/" + strings.ToUpper(selfID) + "/hostname":                "",
		"0::/": "",
	} {
		if got := containerIDIn(line); got != want {
			t.Errorf("containerIDIn(%q) = %q, want %q", line, got, want)
		}
	}
}
