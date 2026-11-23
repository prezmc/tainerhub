package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/prezmc/tainerhub/internal/wire"
)

// Trimmed output of `docker compose config --format json` (compose v2/v5).
const composeJSON = `{
  "name": "app",
  "services": {
    "web": {
      "image": "nginx:1",
      "ports": [{"mode": "ingress", "target": 80, "published": "8080", "protocol": "tcp"}, {"target": 443}],
      "volumes": [
        {"type": "bind", "source": "/opt/tainerhub/stacks/app/html", "target": "/usr/share/nginx/html", "read_only": true},
        {"type": "bind", "source": "/srv/media", "target": "/media"},
        {"type": "volume", "source": "db", "target": "/cache"}
      ]
    },
    "db": {
      "image": "postgres:17",
      "network_mode": "host",
      "devices": [{"source": "/dev/ttyUSB0", "target": "/dev/ttyUSB0", "permissions": "rwm"}],
      "ports": [{"target": 53, "published": 53, "protocol": "udp"}],
      "volumes": [{"type": "volume", "source": "db", "target": "/var/lib/postgresql/data"}]
    },
    "builder": {"build": {"context": "."}}
  },
  "volumes": {
    "db": {"name": "app_db"},
    "nfs": {"name": "app_nfs", "driver_opts": {"type": "nfs", "o": "addr=10.0.0.2"}},
    "shared": {"name": "shared", "external": true}
  }
}`

func TestParseComposeConfig(t *testing.T) {
	var in wire.StackInspect
	if err := parseComposeConfig([]byte(composeJSON), "/opt/tainerhub/stacks/app", &in); err != nil {
		t.Fatal(err)
	}
	if len(in.Services) != 3 || in.Services[0].Name != "builder" || !in.Services[0].Build {
		t.Errorf("services %+v", in.Services)
	}
	db := in.Services[1]
	if !db.HostNetwork || !db.Devices || db.Image != "postgres:17" {
		t.Errorf("db %+v", db)
	}
	if got := in.Ports; len(got) != 2 || got[0] != "53/udp" || got[1] != "8080/tcp" {
		t.Errorf("ports %v (unpublished ones must be left out)", got)
	}
	if len(in.Binds) != 2 || !in.Binds[0].InStackDir || in.Binds[1].InStackDir || in.Binds[1].Source != "/srv/media" {
		t.Errorf("binds %+v", in.Binds)
	}
	vols := map[string]wire.StackVolume{}
	for _, v := range in.Volumes {
		vols[v.Key] = v
	}
	if vols["db"].Name != "app_db" || vols["db"].Options || vols["db"].Size != -1 {
		t.Errorf("db volume %+v", vols["db"])
	}
	if !vols["nfs"].Options || !vols["shared"].External {
		t.Errorf("nfs %+v shared %+v", vols["nfs"], vols["shared"])
	}

	fillVolumes(&in, map[string]VolumeUsage{"app_db": {Size: 42, Labels: map[string]string{"x": "y"}, Driver: "local"}})
	for _, v := range in.Volumes {
		switch v.Name {
		case "app_db":
			if !v.Exists || v.Size != 42 || v.Labels["x"] != "y" || v.Driver != "local" {
				t.Errorf("filled %+v", v)
			}
		default:
			if v.Exists {
				t.Errorf("%s exists without Docker knowing it", v.Name)
			}
		}
	}
	if err := parseComposeConfig([]byte("not json"), "/x", &in); err == nil {
		t.Error("garbage accepted")
	}
}

func TestScanStackDir(t *testing.T) {
	dir := t.TempDir()
	if files, size, partial := scanStackDir(filepath.Join(dir, "missing")); files != 0 || size != 0 || partial {
		t.Errorf("missing folder: %d %d %v", files, size, partial)
	}
	for name, body := range map[string]string{composeFile: "services: {}", envFile: "A=1", "data/x": "12345", "notes.txt": "abc"} {
		p := filepath.Join(dir, name)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.Mkdir(filepath.Join(dir, stagingDir), 0o700)
	files, size, partial := scanStackDir(dir)
	if files != 2 || partial { // data/ and notes.txt
		t.Errorf("files %d partial %v", files, partial)
	}
	if size != int64(len("services: {}")+len("A=1")+5+3) {
		t.Errorf("size %d", size)
	}
}
