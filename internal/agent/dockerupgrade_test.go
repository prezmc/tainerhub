package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestParsePlan(t *testing.T) {
	p := parsePlan("systemd=yes\nmanager=apt\ninstalled=5:29.8.2-1~ubuntu.24.04~noble\ncandidate=5:29.10.0-1~ubuntu.24.04~noble\n")
	if !p.Supported || !p.Available || !strings.Contains(p.Command, "--only-upgrade") {
		t.Fatalf("apt with update: %+v", p)
	}
	p = parsePlan("systemd=yes\nmanager=apt\ninstalled=5:29.10.0-1\ncandidate=5:29.10.0-1\n")
	if !p.Supported || p.Available {
		t.Fatalf("apt up to date: %+v", p)
	}
	p = parsePlan("systemd=yes\nmanager=dnf\ninstalled=29.8.2-1.fc41\ncandidate=\n")
	if p.Available {
		t.Fatalf("dnf with no upgrades listed must be up to date: %+v", p)
	}
	p = parsePlan("systemd=no\nmanager=apt\ninstalled=1\ncandidate=2\n")
	if p.Supported || p.Reason == "" {
		t.Fatalf("no systemd must not be supported: %+v", p)
	}
	for _, m := range []string{"snap", "apt-docker.io", "rpm-distro", "unknown"} {
		if p := parsePlan("systemd=yes\nmanager=" + m + "\n"); p.Supported || p.Reason == "" {
			t.Fatalf("%s: %+v", m, p)
		}
	}
}

// The plan script runs on the host's /bin/sh: it must at least parse.
func TestPlanScriptParses(t *testing.T) {
	f := filepath.Join(t.TempDir(), "plan.sh")
	if err := os.WriteFile(f, []byte(planScript), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("sh", "-n", f).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
}
