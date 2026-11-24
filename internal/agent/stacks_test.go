package agent

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prezmc/tainerhub/internal/wire"
)

// fakeCompose writes a shell script standing in for `docker compose`. It logs
// each call's arguments and environment, fails `config` when the compose file
// contains INVALID, and succeeds otherwise.
func fakeCompose(t *testing.T) (*StackRunner, string) {
	t.Helper()
	tmp := t.TempDir()
	logs := filepath.Join(tmp, "logs")
	if err := os.Mkdir(logs, 0o700); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
echo "$*" >> "` + logs + `/calls"
env >> "` + logs + `/env"
f=""; prev=""
for a in "$@"; do [ "$prev" = "--file" ] && f="$a"; prev="$a"; done
case "$*" in
  *"version --short"*) echo "5.8.0"; exit 0 ;;
  *"config --format json"*)
    if grep -q INVALID "$f"; then echo "yaml: line 1: did not find expected key" >&2; exit 15; fi
    if grep -q "build:" "$f"; then echo '{"services":{"web":{"build":{"context":"."}}}}'; else echo '{"services":{"web":{"image":"nginx"}}}'; fi
    exit 0 ;;
esac
echo "fake compose ok"
`
	bin := filepath.Join(tmp, "docker-compose")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	r := &StackRunner{Dir: filepath.Join(tmp, "stacks"), Bin: bin, Socket: "/var/run/docker.sock", WorkDir: filepath.Join(tmp, "work")}
	return r, logs
}

func calls(t *testing.T, logs string) []string {
	b, _ := os.ReadFile(filepath.Join(logs, "calls"))
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

const goodCompose = "services:\n  web:\n    image: nginx\n"

func TestStackDeployLifecycle(t *testing.T) {
	t.Setenv("TAINERHUB_JOIN_TOKEN", "thj1_secret")
	r, logs := fakeCompose(t)
	ctx := context.Background()

	if sup := r.Probe(ctx); sup.Error != "" || sup.ComposeVersion != "5.8.0" {
		t.Fatalf("probe: %+v", sup)
	}

	// start before any deploy is refused
	var out bytes.Buffer
	if err := r.Run(ctx, wire.StackCommand{Action: wire.StackStart, Name: "web"}, &out); err == nil {
		t.Fatal("start before deploy should fail")
	}

	out.Reset()
	err := r.Run(ctx, wire.StackCommand{Action: wire.StackDeploy, Name: "web", Compose: goodCompose, Env: "TAG=1.27\n", Pull: true}, &out)
	if err != nil {
		t.Fatalf("deploy: %v\n%s", err, out.String())
	}
	dir := filepath.Join(r.Dir, "web")
	if b, _ := os.ReadFile(filepath.Join(dir, composeFile)); string(b) != goodCompose {
		t.Fatalf("compose.yaml = %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, envFile)); string(b) != "TAG=1.27\n" {
		t.Fatalf(".env = %q", b)
	}
	if _, err := os.Stat(filepath.Join(dir, stagingDir)); !os.IsNotExist(err) {
		t.Fatal("staging folder left behind")
	}
	c := calls(t, logs)
	last3 := strings.Join(c[len(c)-3:], "\n")
	for _, want := range []string{"config --format json", " pull", "up --detach --remove-orphans"} {
		if !strings.Contains(last3, want) {
			t.Fatalf("expected %q among calls:\n%s", want, last3)
		}
	}
	if !strings.Contains(out.String(), "$ docker compose") || !strings.Contains(out.String(), "fake compose ok") {
		t.Fatalf("output missing command echo or compose output:\n%s", out.String())
	}

	// compose must not see the agent's environment
	env, _ := os.ReadFile(filepath.Join(logs, "env"))
	if strings.Contains(string(env), "TAINERHUB_JOIN_TOKEN") {
		t.Fatal("agent environment leaked into compose")
	}

	// an invalid file is rejected and never replaces the working one
	out.Reset()
	err = r.Run(ctx, wire.StackCommand{Action: wire.StackDeploy, Name: "web", Compose: "INVALID"}, &out)
	if err == nil || !strings.Contains(err.Error(), "not valid") {
		t.Fatalf("invalid deploy: err=%v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, composeFile)); string(b) != goodCompose {
		t.Fatal("invalid compose file replaced the deployed one")
	}
	if !strings.Contains(out.String(), "did not find expected key") {
		t.Fatalf("validation error not shown:\n%s", out.String())
	}

	for _, a := range []string{wire.StackStop, wire.StackStart, wire.StackRestart} {
		if err := r.Run(ctx, wire.StackCommand{Action: a, Name: "web"}, &out); err != nil {
			t.Fatalf("%s: %v", a, err)
		}
	}
	if err := r.Run(ctx, wire.StackCommand{Action: wire.StackDown, Name: "web", Volumes: true}, &out); err != nil {
		t.Fatal(err)
	}
	if c := calls(t, logs); !strings.HasSuffix(c[len(c)-1], "down --remove-orphans --volumes") {
		t.Fatalf("down call: %s", c[len(c)-1])
	}

	// remove keeps the stack's own data
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o700); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := r.Run(ctx, wire.StackCommand{Action: wire.StackRemove, Name: "web"}, &out); err != nil {
		t.Fatal(err)
	}
	if fileExists(filepath.Join(dir, composeFile)) || fileExists(filepath.Join(dir, envFile)) {
		t.Fatal("stack files not removed")
	}
	if _, err := os.Stat(filepath.Join(dir, "data")); err != nil {
		t.Fatal("remove deleted the stack's data folder")
	}
	if !strings.Contains(out.String(), "Kept") {
		t.Fatalf("expected a note about the kept folder:\n%s", out.String())
	}

	// with no data left, remove deletes the folder too
	_ = os.Remove(filepath.Join(dir, "data"))
	if err := r.Run(ctx, wire.StackCommand{Action: wire.StackRemove, Name: "web"}, &out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("empty stack folder not removed")
	}
}

func TestStackRejectsBadInput(t *testing.T) {
	r, _ := fakeCompose(t)
	var out bytes.Buffer
	for _, c := range []wire.StackCommand{
		{Action: wire.StackDeploy, Name: "../etc", Compose: goodCompose},
		{Action: wire.StackDeploy, Name: "Web", Compose: goodCompose},
		{Action: wire.StackDeploy, Name: "web", Compose: "  "},
		{Action: "exec", Name: "web"},
	} {
		if err := r.Run(context.Background(), c, &out); err == nil {
			t.Errorf("%+v: expected an error", c)
		}
	}
	if _, err := os.Stat(filepath.Join(r.Dir, "..", "etc")); !os.IsNotExist(err) {
		t.Fatal("path traversal created a folder")
	}
}

func TestStackLockIsPerStack(t *testing.T) {
	r, _ := fakeCompose(t)
	if !r.lock("a") || r.lock("a") {
		t.Fatal("second lock on the same stack should fail")
	}
	if !r.lock("b") {
		t.Fatal("other stacks must not be blocked")
	}
	r.unlock("a")
	if !r.lock("a") {
		t.Fatal("lock not released")
	}
}

func TestStackProbeReportsMissingCompose(t *testing.T) {
	r, _ := fakeCompose(t)
	r.Bin = filepath.Join(t.TempDir(), "missing")
	if sup := r.Probe(context.Background()); sup.Error == "" {
		t.Fatal("expected an error for a missing compose binary")
	}
}

func TestSafeWriterSwallowsErrors(t *testing.T) {
	w := &safeWriter{w: failingWriter{}}
	if n, err := w.Write([]byte("abc")); n != 3 || err != nil {
		t.Fatalf("got %d, %v", n, err)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, os.ErrClosed }
