package agent

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prezmc/tainerhub/internal/wire"
)

// TestStackRealDocker runs a full stack lifecycle against a real Docker
// daemon and compose binary. It is opt-in:
//
//	TAINERHUB_REAL_DOCKER=1 DOCKER_SOCKET=/var/run/docker.sock go test ./internal/agent -run Real -v
func TestStackRealDocker(t *testing.T) {
	if os.Getenv("TAINERHUB_REAL_DOCKER") != "1" {
		t.Skip("set TAINERHUB_REAL_DOCKER=1 to run against a real Docker daemon")
	}
	bin := os.Getenv("TAINERHUB_COMPOSE_BIN")
	if bin == "" {
		var err error
		if bin, err = exec.LookPath("docker-compose"); err != nil {
			t.Skip("docker-compose not found")
		}
	}
	socket := os.Getenv("DOCKER_SOCKET")
	if socket == "" {
		socket = "/var/run/docker.sock"
	}
	tmp, err := os.MkdirTemp("", "th-stacks-") // short path: Docker Desktop shares /var/folders
	if err != nil {
		t.Fatal(err)
	}
	tmp, _ = filepath.EvalSymlinks(tmp)
	defer os.RemoveAll(tmp)
	r := &StackRunner{Dir: tmp + "/stacks", Bin: bin, Socket: socket, WorkDir: tmp + "/work"}
	d := NewSocketDocker(socket)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	const name = "tainerhub-realtest"
	compose := `services:
  app:
    image: alpine:3
    command: ["sh", "-c", "echo $${GREETING} > /out/hello && sleep 3600"]
    environment:
      GREETING: ${GREETING}
    volumes:
      - ./out:/out
`
	run := func(c wire.StackCommand) string {
		t.Helper()
		var out bytes.Buffer
		c.Name = name
		if err := r.Run(ctx, c, &out); err != nil {
			t.Fatalf("%s: %v\n%s", c.Action, err, out.String())
		}
		return out.String()
	}
	defer r.Run(context.Background(), wire.StackCommand{Action: wire.StackRemove, Name: name, Volumes: true}, &bytes.Buffer{})

	t.Log(run(wire.StackCommand{Action: wire.StackDeploy, Compose: compose, Env: "GREETING=hello-from-tainerhub\n", Pull: true}))

	// The relative bind mount resolved to the stack folder and .env was used.
	hello := filepath.Join(r.Dir, name, "out", "hello")
	var b []byte
	for i := 0; i < 50 && len(b) == 0; i++ {
		b, _ = os.ReadFile(hello)
		time.Sleep(100 * time.Millisecond)
	}
	if strings.TrimSpace(string(b)) != "hello-from-tainerhub" {
		t.Fatalf("bind mount / .env not applied: %q", b)
	}

	state := func() string {
		cs, err := d.List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range cs {
			if c.Project == name {
				return c.State
			}
		}
		return "absent"
	}
	if s := state(); s != "running" {
		t.Fatalf("after deploy: %s", s)
	}
	run(wire.StackCommand{Action: wire.StackStop})
	if s := state(); s != "exited" {
		t.Fatalf("after stop: %s", s)
	}
	run(wire.StackCommand{Action: wire.StackStart})
	if s := state(); s != "running" {
		t.Fatalf("after start: %s", s)
	}

	// A broken file is refused and the running stack is untouched.
	var out bytes.Buffer
	err = r.Run(ctx, wire.StackCommand{Action: wire.StackDeploy, Name: name, Compose: "services:\n  app:\n    image: [\n"}, &out)
	if err == nil {
		t.Fatal("broken compose file accepted")
	}
	t.Log(out.String())
	if s := state(); s != "running" {
		t.Fatalf("after rejected deploy: %s", s)
	}

	t.Log(run(wire.StackCommand{Action: wire.StackRemove, Volumes: true}))
	if s := state(); s != "absent" {
		t.Fatalf("after remove: %s", s)
	}
	if _, err := os.Stat(hello); err != nil {
		t.Fatal("remove deleted bind-mounted data")
	}
}

// TestExecRealDocker opens a terminal in a real container, types a line and
// reads the answer. Opt-in like TestStackRealDocker.
func TestExecRealDocker(t *testing.T) {
	if os.Getenv("TAINERHUB_REAL_DOCKER") != "1" {
		t.Skip("set TAINERHUB_REAL_DOCKER=1 to run against a real Docker daemon")
	}
	socket := os.Getenv("DOCKER_SOCKET")
	if socket == "" {
		socket = "/var/run/docker.sock"
	}
	name := "tainerhub-exectest"
	_ = exec.Command("docker", "rm", "-f", name).Run()
	if out, err := exec.Command("docker", "run", "-d", "--name", name, "alpine:3", "sleep", "300").CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	defer exec.Command("docker", "rm", "-f", name).Run()

	d := NewSocketDocker(socket)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sess, err := d.Exec(ctx, name, []string{"sh", "-c", "echo ready; read x; echo got:$x; exit 3"}, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	buf := make([]byte, 4096)
	var out strings.Builder
	readUntil := func(want string) {
		for !strings.Contains(out.String(), want) {
			n, err := sess.Read(buf)
			out.Write(buf[:n])
			if err != nil {
				t.Fatalf("waiting for %q: %v (got %q)", want, err, out.String())
			}
		}
	}
	readUntil("ready")
	if _, err := sess.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	readUntil("got:hello")
	if err := sess.Resize(ctx, 120, 40); err != nil {
		t.Logf("resize after exit is fine to fail: %v", err)
	}
	for { // drain to EOF
		if _, err := sess.Read(buf); err != nil {
			break
		}
	}
	if code, ok := sess.ExitCode(ctx); !ok || code != 3 {
		t.Fatalf("exit code = %d, %v", code, ok)
	}
}

// TestRecreateRealDocker needs a registry at localhost:5001 (docker run -d -p
// 127.0.0.1:5001:5000 registry:2). It publishes v1 of a tiny image, runs a
// container with its own settings, publishes v2, and checks that the update is
// detected and that recreating keeps the settings but takes v2's defaults.
func TestRecreateRealDocker(t *testing.T) {
	if os.Getenv("TAINERHUB_REAL_REGISTRY") != "1" {
		t.Skip("set TAINERHUB_REAL_REGISTRY=1 (and run a registry on localhost:5001)")
	}
	socket := os.Getenv("DOCKER_SOCKET")
	ref := "localhost:5001/thdemo:latest"
	publish := func(v string) {
		t.Helper()
		cmd := exec.Command("docker", "build", "-q", "-t", ref, "-")
		cmd.Stdin = strings.NewReader("FROM alpine:3\nENV DEMO_VERSION=" + v + "\nLABEL demo.version=" + v + "\nCMD [\"sleep\",\"3600\"]\n")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build: %v %s", err, out)
		}
		if out, err := exec.Command("docker", "push", "-q", ref).CombinedOutput(); err != nil {
			t.Fatalf("push: %v %s", err, out)
		}
	}
	name := "thdemo"
	_ = exec.Command("docker", "rm", "-f", name).Run()
	defer exec.Command("docker", "rm", "-f", name).Run()
	publish("1")
	if out, err := exec.Command("docker", "run", "-d", "--name", name, "-e", "MINE=kept", "-p", "127.0.0.1:18089:80", "--label", "mine=yes", "--restart", "unless-stopped", ref).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	d := NewSocketDocker(socket)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	r, err := d.checkUpdates(ctx)
	if err != nil || r.Images[ref] {
		t.Fatalf("v1 should be current: %+v %v", r, err)
	}
	_ = exec.Command("docker", "tag", ref, "thdemo-v1:keep").Run()
	defer exec.Command("docker", "rmi", "thdemo-v1:keep").Run()
	publish("2")
	// The real situation: the registry has v2, this host's tag still points at v1.
	if out, err := exec.Command("docker", "tag", "thdemo-v1:keep", ref).CombinedOutput(); err != nil {
		t.Fatalf("retag v1: %v %s", err, out)
	}
	r, err = d.checkUpdates(ctx)
	if err != nil || !r.Images[ref] {
		t.Fatalf("v2 should be detected: %+v %v", r, err)
	}

	res, err := d.recreate(ctx, name)
	if err != nil || res.Status != "updated" {
		t.Fatalf("recreate: %+v %v", res, err)
	}
	out, _ := exec.Command("docker", "inspect", name, "--format", "{{json .Config.Env}} {{index .Config.Labels \"mine\"}} {{index .Config.Labels \"demo.version\"}} {{json .HostConfig.PortBindings}} {{.HostConfig.RestartPolicy.Name}} {{.State.Running}}").CombinedOutput()
	got := string(out)
	for _, want := range []string{"MINE=kept", "DEMO_VERSION=2", " yes 2 ", "18089", "unless-stopped", "true"} {
		if !strings.Contains(got, want) {
			t.Fatalf("after recreate, %q missing from: %s", want, got)
		}
	}
	if strings.Contains(got, "DEMO_VERSION=1") {
		t.Fatalf("the old image's env was frozen into the new container: %s", got)
	}
	if list, _ := exec.Command("docker", "ps", "-a", "--filter", "name=thdemo-tainerhub-old", "-q").Output(); len(strings.TrimSpace(string(list))) > 0 {
		t.Fatal("old container left behind")
	}
	if res, err := d.recreate(ctx, name); err != nil || res.Status != "current" {
		t.Fatalf("second recreate should be a no-op: %+v %v", res, err)
	}
}
