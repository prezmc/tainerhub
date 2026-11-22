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
