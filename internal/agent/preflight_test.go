package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/prezmc/tainerhub/internal/wire"
)

// What `docker compose config --format json` (v2/v5) writes for these keys.
const preflightJSON = `{
 "services": {
  "a": {
   "image": "nginx", "container_name": "web1",
   "ports": [{"mode":"ingress","host_ip":"127.0.0.1","target":80,"published":"8080","protocol":"tcp"},
             {"mode":"ingress","target":53,"published":"53","protocol":"udp"},
             {"mode":"ingress","target":3000,"protocol":"tcp"}],
   "cpus": 1.5, "mem_limit": "536870912",
   "devices": [{"source":"/dev/dri","target":"/dev/dri","permissions":"rwm"}, "/dev/ttyUSB0:/dev/ttyUSB0"],
   "deploy": {"resources": {"reservations": {"devices": [{"capabilities":["gpu"],"driver":"nvidia","count":-1}]}}},
   "volumes": [{"type":"bind","source":"/opt/tainerhub/stacks/s/data","target":"/data"},
               {"type":"bind","source":"/srv/media","target":"/media"},
               {"type":"volume","source":"vol","target":"/v"}],
   "networks": {"own": null, "proxy": null},
   "environment": {"DB_PASSWORD": "changeme", "TZ": "Europe/London", "EMPTY": null, "URL": "https://<your-domain>"}
  },
  "b": {"image": "redis", "gpus": [{"count": -1}], "network_mode": "host",
        "deploy": {"resources": {"limits": {"cpus": "2", "memory": "1073741824"}}}},
  "c": {"build": {"context": "."}, "image": "s-c"}
 },
 "networks": {"own": {"name": "s_own"}, "proxy": {"name": "proxy", "external": true}},
 "volumes": {"vol": {"name": "s_vol"}, "ext": {"name": "shared-data", "external": true}}
}`

func TestParsePreflight(t *testing.T) {
	var out wire.StackPreflight
	if err := parsePreflight([]byte(preflightJSON), "/opt/tainerhub/stacks/s", &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Services) != 3 {
		t.Fatalf("%+v", out.Services)
	}
	a, b, c := out.Services[0], out.Services[1], out.Services[2]
	if a.ContainerName != "web1" || a.CPUs != 1.5 || a.Memory != 512<<20 || !a.GPU || len(a.Ports) != 3 {
		t.Errorf("a: %+v", a)
	}
	if a.Ports[0] != (wire.PreflightPort{HostIP: "127.0.0.1", Published: "8080", Target: "80", Protocol: "tcp"}) || a.Ports[2].Published != "" {
		t.Errorf("ports: %+v", a.Ports)
	}
	if len(a.Devices) != 2 || a.Devices[0] != "/dev/dri" || a.Devices[1] != "/dev/ttyUSB0" {
		t.Errorf("devices: %+v", a.Devices)
	}
	if len(a.Binds) != 2 || !a.Binds[0].InStackDir || a.Binds[1].InStackDir {
		t.Errorf("binds: %+v", a.Binds)
	}
	if len(a.PlaceholderEnv) != 2 || a.PlaceholderEnv[0] != "DB_PASSWORD" || a.PlaceholderEnv[1] != "URL" {
		t.Errorf("placeholder env: %v", a.PlaceholderEnv)
	}
	if !b.GPU || !b.HostNetwork || b.CPUs != 2 || b.Memory != 1<<30 {
		t.Errorf("b: %+v", b)
	}
	if !c.Build {
		t.Errorf("c: %+v", c)
	}
	if len(out.Networks) != 1 || out.Networks[0].Name != "proxy" || len(out.Volumes) != 1 || out.Volumes[0].Name != "shared-data" {
		t.Errorf("external: %+v %+v", out.Networks, out.Volumes)
	}
}

type fakePreflightDocker struct{ asked []string }

func (f *fakePreflightDocker) preflightHost(context.Context) (hostFacts, error) {
	return hostFacts{}, nil
}
func (f *fakePreflightDocker) checkImage(_ context.Context, ref string, _ []wire.RegistryAuth) wire.ImageCheck {
	f.asked = append(f.asked, ref)
	return wire.ImageCheck{Remote: "found"}
}

func TestFillPreflight(t *testing.T) {
	out := wire.StackPreflight{
		Services: []wire.PreflightService{{Name: "a", Image: "nginx"}, {Name: "b", Image: "nginx"}, {Name: "c", Image: "s-c", Build: true}, {Name: "d", Image: "bad ref!"}},
		Networks: []wire.PreflightResource{{Name: "proxy"}, {Name: "gone"}},
	}
	d := &fakePreflightDocker{}
	fillPreflight(context.Background(), &out, hostFacts{Arch: "arm64", GPU: true, Networks: map[string]bool{"proxy": true}}, d, nil)
	if len(d.asked) != 1 || out.Services[1].ImageCheck == nil || out.Services[2].ImageCheck != nil || out.Services[3].ImageCheck.Remote != "error" {
		t.Errorf("asked %v, services %+v", d.asked, out.Services)
	}
	if !out.Networks[0].Exists || out.Networks[1].Exists || out.Arch != "arm64" || !out.GPURuntime {
		t.Errorf("%+v", out)
	}
}

func TestClassifyRegistryError(t *testing.T) {
	for in, want := range map[string]string{
		"docker: manifest unknown: manifest unknown":                                                                 "not_found",
		"docker: errors:\ndenied: requested access to the resource is denied\nunauthorized: authentication required": "denied",
		`docker: Head "https://ghcr.io/v2/x/y/manifests/1": denied`:                                                  "denied",
		"docker: Get https://registry: dial tcp: i/o timeout":                                                        "error",
	} {
		if got, _ := classifyRegistryError(errors.New(in)); got != want {
			t.Errorf("%q: %s, want %s", in, got, want)
		}
	}
	if got, _ := classifyRegistryError(nil); got != "found" {
		t.Error(got)
	}
	if dockerArch("aarch64") != "arm64" || dockerArch("x86_64") != "amd64" || dockerArch("armv7l") != "arm/v7" {
		t.Error("dockerArch")
	}
}
