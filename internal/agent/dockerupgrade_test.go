package agent

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestHelperImagePinned(t *testing.T) {
	ref, err := digestRef(helperImage)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ref, "alpine@sha256:") {
		t.Fatalf("helper image ref %q", ref)
	}
}

func TestDigestRef(t *testing.T) {
	dg := "sha256:" + strings.Repeat("a", 64)
	for in, want := range map[string]string{
		"alpine:3.24@" + dg:               "alpine@" + dg,
		"alpine@" + dg:                    "alpine@" + dg,
		"reg.local:5000/x/alpine:1@" + dg: "reg.local:5000/x/alpine@" + dg,
		"reg.local:5000/x/alpine@" + dg:   "reg.local:5000/x/alpine@" + dg,
	} {
		if got, err := digestRef(in); err != nil || got != want {
			t.Errorf("digestRef(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"alpine:3", "alpine:3.24", "alpine@sha256:short", "alpine@md5:" + strings.Repeat("a", 64), "@" + dg} {
		if _, err := digestRef(in); err == nil {
			t.Errorf("digestRef(%q) must fail", in)
		}
	}
}

// A local image that only carries the tag must not be used: ensureImage
// pulls by digest and creates containers from the digest reference.
func TestEnsureImagePullsByDigest(t *testing.T) {
	pinned, _ := digestRef(helperImage)
	var mu sync.Mutex
	pulled := ""
	sock := filepath.Join(t.TempDir(), "d.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /images/{ref}/json", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.PathValue("ref") == pinned && pulled != "":
			_ = json.NewEncoder(w).Encode(map[string]any{"RepoDigests": []string{pinned}})
		case r.PathValue("ref") == pinned:
			// The engine resolves the digest to a same-content image that was
			// only tagged locally: no matching repo digest.
			_ = json.NewEncoder(w).Encode(map[string]any{"RepoTags": []string{"alpine:3.24"}, "RepoDigests": []string{}})
		default:
			http.Error(w, `{"message":"no such image"}`, 404)
		}
	})
	mux.HandleFunc("POST /images/create", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		pulled = r.URL.Query().Get("fromImage")
		mu.Unlock()
		_, _ = w.Write([]byte(`{"status":"Status: Downloaded newer image"}` + "\n"))
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(l) }()
	defer srv.Close()

	got, err := NewSocketDocker(sock).ensureImage(context.Background(), helperImage)
	if err != nil {
		t.Fatal(err)
	}
	if got != pinned || pulled != pinned {
		t.Fatalf("ensureImage = %q, pulled %q; want both %q", got, pulled, pinned)
	}
	if _, err := NewSocketDocker(sock).ensureImage(context.Background(), "alpine:3"); err == nil {
		t.Fatal("an unpinned reference must be refused")
	}
}

func TestHasRepoDigest(t *testing.T) {
	dg := "sha256:" + strings.Repeat("b", 64)
	if !hasRepoDigest([]string{"docker.io/library/alpine@" + dg}, "alpine@"+dg) || !hasRepoDigest([]string{"alpine@" + dg}, "alpine@"+dg) {
		t.Fatal("matching repo digest not recognised")
	}
	if hasRepoDigest([]string{"evil/alpine@" + dg, "alpine@sha256:" + strings.Repeat("c", 64)}, "alpine@"+dg) {
		t.Fatal("non-matching repo digest accepted")
	}
}

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
	p = parsePlan("systemd=yes\nmanager=apt\ninstalled=5:29.10.0-1\ncandidate=5:29.10.0-1\nrefresh_failed=yes\nrefresh_error=W: Failed to fetch x\n")
	if !p.Supported || !p.RefreshFailed || p.RefreshError != "W: Failed to fetch x" || p.Reason == "" {
		t.Fatalf("apt refresh failed: %+v", p)
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
