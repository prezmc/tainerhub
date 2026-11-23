package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prezmc/tainerhub/internal/wire"
)

// TestPreviewRealCompose runs the takeover preview through a real compose
// binary. Opt-in: TAINERHUB_REAL_DOCKER=1 go test ./internal/agent -run PreviewReal
func TestPreviewRealCompose(t *testing.T) {
	if os.Getenv("TAINERHUB_REAL_DOCKER") != "1" {
		t.Skip("set TAINERHUB_REAL_DOCKER=1 to run with a real compose binary")
	}
	bin, err := exec.LookPath("docker-compose")
	if err != nil {
		t.Skip("docker-compose not found")
	}
	tmp, _ := filepath.EvalSymlinks(t.TempDir())
	r := &StackRunner{Dir: tmp + "/stacks", Bin: bin, Socket: "/var/run/docker.sock", WorkDir: tmp + "/work"}
	ctx := context.Background()
	compose := `services:
  web:
    image: nginx:alpine
    environment: ["PW=${DB_PASSWORD}"]
    volumes: ["./html:/usr/share/nginx/html", "db:/data", "/srv/x:/x"]
volumes:
  db: {}
`
	p, err := r.Preview(ctx, "app", wire.StackFiles{Compose: compose})
	if err != nil || p.ConfigError != "" {
		t.Fatalf("%v %q", err, p.ConfigError)
	}
	if len(p.Warnings) != 1 || !strings.Contains(p.Warnings[0], `"DB_PASSWORD" variable is not set`) {
		t.Errorf("warnings %q", p.Warnings)
	}
	html := filepath.Join(r.Dir, "app", "html")
	if p.Paths[html] != -1 {
		t.Errorf("paths %v", p.Paths)
	}
	if _, err := os.Stat(filepath.Join(r.Dir, "app")); !os.IsNotExist(err) {
		t.Error("the preview created the stack folder")
	}
	found := 0
	for _, m := range p.Mounts {
		if m.Source == html || m.Source == "app_db" || m.Source == "/srv/x" {
			found++
		}
	}
	if found != 3 {
		t.Errorf("mounts %+v", p.Mounts)
	}

	// A relative env_file that isn't in the stack folder.
	bad := strings.Replace(compose, "    environment:", "    env_file: ./app.env\n    environment:", 1)
	p, err = r.Preview(ctx, "app", wire.StackFiles{Compose: bad, Env: "DB_PASSWORD=x\n"})
	if err != nil || !strings.Contains(p.ConfigError, "app.env") || strings.Contains(p.ConfigError, "level=warning") {
		t.Errorf("%v %q", err, p.ConfigError)
	}
	if len(p.Warnings) != 0 {
		t.Errorf("DB_PASSWORD is set in .env, yet: %q", p.Warnings)
	}
}
