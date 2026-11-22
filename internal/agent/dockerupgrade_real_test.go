package agent

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/prezmc/tainerhub/internal/wire"
)

// TestDockerUpgradeReal upgrades Docker on the machine it runs on. Only run
// it on a throwaway server: TAINERHUB_REAL_DOCKER_UPGRADE=1.
func TestDockerUpgradeReal(t *testing.T) {
	if os.Getenv("TAINERHUB_REAL_DOCKER_UPGRADE") != "1" {
		t.Skip("upgrades this machine's Docker: set TAINERHUB_REAL_DOCKER_UPGRADE=1 on a throwaway server")
	}
	d := NewSocketDocker("/var/run/docker.sock")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	before, err := d.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = exec.Command("docker", "rm", "-f", "survivor").Run()
	if out, err := exec.Command("docker", "run", "-d", "--name", "survivor", "--restart", "unless-stopped", "alpine:3", "sleep", "3600").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}

	out, err := d.Operate(ctx, wire.Operation{Kind: wire.OpDockerPlan})
	if err != nil {
		t.Fatal(err)
	}
	p := out.(wire.DockerPlan)
	t.Logf("plan: %+v", p)
	if p.Manager != "apt" || !p.Supported || !p.Available {
		t.Fatalf("expected an apt upgrade to be possible: %+v", p)
	}
	if _, err := d.Operate(ctx, wire.Operation{Kind: wire.OpDockerUpgrade}); err == nil {
		t.Fatal("an upgrade without the explicit OK must be refused")
	}
	if _, err := d.Operate(ctx, wire.Operation{Kind: wire.OpDockerUpgrade, Arg: "restart-all"}); err != nil {
		t.Fatal(err)
	}
	t.Logf("upgrade started; waiting for Docker to come back (was %s)", before.DockerVersion)
	var after wire.Hello
	for i := 0; i < 150; i++ {
		time.Sleep(2 * time.Second)
		if h, err := d.Info(ctx); err == nil && h.DockerVersion != before.DockerVersion {
			after = h
			break
		}
	}
	if after.DockerVersion == "" {
		log, _ := os.ReadFile("/var/log/tainerhub-docker-upgrade.log")
		t.Fatalf("Docker didn't come back on a new version; upgrade log:\n%s", log)
	}
	t.Logf("Docker %s -> %s", before.DockerVersion, after.DockerVersion)
	time.Sleep(3 * time.Second)
	st, _ := exec.Command("docker", "inspect", "survivor", "--format", "{{.State.Running}}").Output()
	if strings.TrimSpace(string(st)) != "true" {
		t.Fatalf("container with a restart policy didn't come back: %s", st)
	}
	if again, _ := d.Operate(ctx, wire.Operation{Kind: wire.OpDockerPlan}); again.(wire.DockerPlan).Available {
		t.Fatalf("after upgrading, no newer version should be offered: %+v", again)
	}
}
