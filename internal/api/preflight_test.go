package api

import (
	"strings"
	"testing"

	"github.com/prezmc/tainerhub/internal/store"
	"github.com/prezmc/tainerhub/internal/wire"
)

const pfHost = "11111111-1111-1111-1111-111111111111"

func pfItem(rep preflightReport, key string) *preflightItem {
	for i := range rep.Items {
		if rep.Items[i].Key == key {
			return &rep.Items[i]
		}
	}
	return nil
}

func pfHostWith(cs ...wire.Container) preflightHost {
	return preflightHost{Name: "nas", HostID: pfHost, Hello: &wire.Hello{NCPU: 4, MemTotal: 8 << 30}, Containers: cs,
		Stacks: []store.Stack{{HostID: pfHost, Name: "jellyfin"}, {HostID: "other", Name: "web"}}}
}

func TestPreflightAllGood(t *testing.T) {
	p := wire.StackPreflight{Arch: "arm64", GPURuntime: true, Services: []wire.PreflightService{{
		Name: "web", ContainerName: "web", Image: "nginx:1.27",
		Ports:      []wire.PreflightPort{{Published: "8080", Target: "80", Protocol: "tcp"}},
		ImageCheck: &wire.ImageCheck{Remote: "found", Platforms: []string{"linux/amd64", "linux/arm64/v8"}},
	}}, Networks: []wire.PreflightResource{{Name: "proxy", Exists: true}}}
	rep := buildPreflightReport(p, pfHostWith(wire.Container{Name: "other", Ports: []string{"9000->9000/tcp"}}), "web")
	if rep.Blocked {
		t.Fatalf("blocked: %+v", rep.Items)
	}
	for _, k := range []string{"config", "ports", "network:proxy", "image:nginx:1.27"} {
		if it := pfItem(rep, k); it == nil || it.Level != levelOK {
			t.Errorf("%s: %+v", k, it)
		}
	}
}

func TestPreflightProblemsAndFixes(t *testing.T) {
	h := pfHostWith(
		wire.Container{Name: "old-jf", Ports: []string{"8096->8096/tcp", "8097->8097/tcp"}},
		wire.Container{Name: "jellyfin-2", Project: ""},
	)
	h.SuggestTags = func(repo, want, arch string) []string {
		if repo != "example/app" || want != "1.0" || arch != "arm64" {
			t.Errorf("SuggestTags(%q, %q, %q)", repo, want, arch)
		}
		return []string{"latest", "1.2"}
	}
	p := wire.StackPreflight{Arch: "arm64", Warnings: []string{`The "X" variable is not set.`}, Services: []wire.PreflightService{{
		Name: "jellyfin", ContainerName: "old-jf", Image: "example/app:1.0",
		Ports: []wire.PreflightPort{{Published: "8096", Target: "8096", Protocol: "tcp"}, {Published: "7359", Target: "7359", Protocol: "udp"}},
		CPUs:  6, Memory: 16 << 30, GPU: true, Devices: []string{"/dev/dri"},
		Binds:      []wire.StackBind{{Source: "/srv/media"}, {Source: "/etc/localtime"}, {Source: "/opt/tainerhub/stacks/jellyfin/config", InStackDir: true}},
		ImageCheck: &wire.ImageCheck{Remote: "found", Platforms: []string{"linux/amd64"}},
	}}, Networks: []wire.PreflightResource{{Name: "proxy"}}, Volumes: []wire.PreflightResource{{Name: "media"}}}
	rep := buildPreflightReport(p, h, "jellyfin")
	if !rep.Blocked {
		t.Fatal("not blocked")
	}
	want := map[string]string{
		"name": levelBlock, "container-name:old-jf": levelBlock, "port:8096/tcp": levelBlock, "ports": levelOK,
		"network:proxy": levelBlock, "volume:media": levelBlock, "image:example/app:1.0": levelBlock,
		"cpus:jellyfin": levelBlock, "memory:jellyfin": levelWarn, "gpu:jellyfin": levelWarn,
		"devices:jellyfin": levelWarn, "binds:jellyfin": levelWarn, "compose-warning": levelWarn,
	}
	for k, lvl := range want {
		if it := pfItem(rep, k); it == nil || it.Level != lvl {
			t.Errorf("%s: want %s, got %+v", k, lvl, it)
		}
	}
	fix := func(key string) []preflightFix { return pfItem(rep, key).Fixes }
	if f := fix("name"); len(f) != 1 || f[0].Value != "jellyfin-3" { // jellyfin-2 is a container's name
		t.Errorf("name fix %+v", f)
	}
	if f := fix("port:8096/tcp"); len(f) != 1 || f[0].Kind != "port" || f[0].Target != "8096/tcp" || f[0].Value != "8098" {
		t.Errorf("port fix %+v", f)
	}
	if f := fix("cpus:jellyfin"); len(f) != 1 || f[0].Value != "4" {
		t.Errorf("cpus fix %+v", f)
	}
	if f := fix("memory:jellyfin"); len(f) != 1 || f[0].Value != "4096" {
		t.Errorf("memory fix %+v", f)
	}
	if f := fix("gpu:jellyfin"); len(f) != 1 || f[0].Kind != "gpu-off" {
		t.Errorf("gpu fix %+v", f)
	}
	if f := fix("network:proxy"); len(f) != 2 || f[0].Kind != "network-create" || f[1].Kind != "network-drop" {
		t.Errorf("network fixes %+v", f)
	}
	if f := fix("image:example/app:1.0"); len(f) != 2 || f[0].Value != "latest" || f[1].Kind != "tag" {
		t.Errorf("tag fixes %+v", f)
	}
	if b := pfItem(rep, "binds:jellyfin"); !strings.Contains(b.Title, "/srv/media") || strings.Contains(b.Title, "localtime") || strings.Contains(b.Title, "config") {
		t.Errorf("binds: %s", b.Title)
	}
	// blockers first
	if rep.Items[0].Level != levelBlock || rep.Items[len(rep.Items)-1].Level == levelBlock {
		t.Error("not sorted by level")
	}
}

func TestPreflightImages(t *testing.T) {
	cases := []struct {
		check wire.ImageCheck
		level string
	}{
		{wire.ImageCheck{Remote: "not_found"}, levelBlock},
		{wire.ImageCheck{Remote: "not_found", Local: true}, levelWarn},
		{wire.ImageCheck{Remote: "denied"}, levelBlock},
		{wire.ImageCheck{Remote: "error", Error: "timeout"}, levelWarn},
		{wire.ImageCheck{Remote: "error", Local: true}, levelOK},
		{wire.ImageCheck{Remote: "found", Platforms: []string{"linux/amd64"}, Local: true}, levelWarn},
	}
	for _, c := range cases {
		c := c
		p := wire.StackPreflight{Arch: "arm64", Services: []wire.PreflightService{{Name: "a", Image: "x/y:1", ImageCheck: &c.check}}}
		if it := pfItem(buildPreflightReport(p, pfHostWith(), "a"), "image:x/y:1"); it == nil || it.Level != c.level {
			t.Errorf("%+v: got %+v", c.check, it)
		}
	}
	// A build is only noted, never looked up.
	p := wire.StackPreflight{Services: []wire.PreflightService{{Name: "a", Image: "x", Build: true}}}
	if it := pfItem(buildPreflightReport(p, pfHostWith(), "a"), "image:a"); it == nil || it.Level != levelWarn {
		t.Errorf("build: %+v", it)
	}
}

func TestPreflightConfigErrorAndDuplicates(t *testing.T) {
	rep := buildPreflightReport(wire.StackPreflight{ConfigError: "services.a: bad"}, pfHostWith(), "a")
	if !rep.Blocked || len(rep.Items) != 1 {
		t.Fatalf("%+v", rep)
	}
	p := wire.StackPreflight{Services: []wire.PreflightService{
		{Name: "a", Ports: []wire.PreflightPort{{Published: "80", Protocol: "tcp"}}},
		{Name: "b", Ports: []wire.PreflightPort{{Published: "80", Protocol: "tcp"}, {Protocol: "tcp", Target: "81"}}},
	}}
	if it := pfItem(buildPreflightReport(p, pfHostWith(), "s"), "port:80/tcp"); it == nil || it.Level != levelBlock || !strings.Contains(it.Title, "twice") {
		t.Errorf("%+v", it)
	}
	// The stack's own containers (a redeploy) don't count as taking its ports.
	p = wire.StackPreflight{Services: []wire.PreflightService{{Name: "a", Ports: []wire.PreflightPort{{Published: "80", Protocol: "tcp"}}}}}
	rep = buildPreflightReport(p, pfHostWith(wire.Container{Name: "s-a-1", Project: "s", Ports: []string{"80->80/tcp"}}), "s")
	if pfItem(rep, "port:80/tcp") != nil || pfItem(rep, "ports") == nil {
		t.Errorf("own port counted: %+v", rep.Items)
	}
}

func TestSupportsArch(t *testing.T) {
	if !supportsArch([]string{"linux/arm64/v8"}, "arm64") || supportsArch([]string{"linux/arm/v7"}, "arm64") || !supportsArch([]string{"linux/arm/v7"}, "arm/v7") {
		t.Error("supportsArch")
	}
}

func TestRankTags(t *testing.T) {
	got := rankTags([]string{"unstable", "2026112315", "10.12.1", "latest", "10.11.3", "10.11", "9.0", "oracle", "10.11.0-rc1"}, "10.11")
	want := []string{"10.12.1", "10.11.3", "latest"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("got %v, want %v", got, want)
	}
	if got := rankTags([]string{"oraclelinux9", "latest", "8.4", "8.0", "9"}, "5.7"); strings.Join(got, ",") != "latest,8.4,8.0" {
		t.Errorf("got %v", got)
	}
}

func TestPreflightOldAgent(t *testing.T) {
	rep := preflightFromPreview(wire.StackPreview{Warnings: []string{"w"}}, pfHostWith(), "new")
	if rep.Blocked || !rep.Partial || pfItem(rep, "old-agent") == nil || pfItem(rep, "config").Level != levelOK {
		t.Errorf("%+v", rep)
	}
	if rep := preflightFromPreview(wire.StackPreview{ConfigError: "bad"}, pfHostWith(), "new"); !rep.Blocked {
		t.Error("config error not blocking")
	}
}

func TestPreflightPlaceholders(t *testing.T) {
	p := wire.StackPreflight{Services: []wire.PreflightService{{Name: "prowlarr",
		Binds: []wire.StackBind{
			{Source: "/path/to/prowlarr/data", Target: "/config"},
			{Source: "/opt/tainerhub/stacks/p/<downloads>", Target: "/downloads", InStackDir: true},
			{Source: "/srv/media", Target: "/media"},
		},
		PlaceholderEnv: []string{"API_KEY", "DOMAIN"},
	}}}
	rep := buildPreflightReport(p, pfHostWith(), "p")
	if !rep.Blocked {
		t.Fatal("placeholder folder doesn't block")
	}
	it := pfItem(rep, "placeholder:prowlarr:/config")
	if it == nil || len(it.Fixes) != 1 || it.Fixes[0] != (preflightFix{Label: "Use ./config", Kind: "volume-source", Target: "/config", Value: "./config"}) {
		t.Errorf("%+v", it)
	}
	if pfItem(rep, "placeholder:prowlarr:/downloads") == nil {
		t.Error("placeholder inside the stack folder missed")
	}
	if b := pfItem(rep, "binds:prowlarr"); b == nil || strings.Contains(b.Title, "path/to") || !strings.Contains(b.Title, "/srv/media") {
		t.Errorf("binds: %+v", b)
	}
	if e := pfItem(rep, "placeholder-env:prowlarr:API_KEY"); e == nil || e.Level != levelWarn || len(e.Fixes) != 1 || e.Fixes[0].Kind != "env-generate" {
		t.Errorf("%+v", e)
	}
	if e := pfItem(rep, "placeholder-env:prowlarr:DOMAIN"); e == nil || len(e.Fixes) != 0 {
		t.Errorf("%+v", e)
	}
	if suggestSource("/var/lib/App Data:ro") != "./app-data" || suggestSource("/") != "./data" {
		t.Error(suggestSource("/var/lib/App Data:ro"))
	}
}

func TestPreflightPortsOnSharedNetwork(t *testing.T) {
	p := wire.StackPreflight{Services: []wire.PreflightService{
		{Name: "gluetun", Image: "qmcgaw/gluetun", Ports: []wire.PreflightPort{{Published: "8080", Target: "8080", Protocol: "tcp"}}},
		{Name: "app", Image: "example/app", NetworkOf: "gluetun", Ports: []wire.PreflightPort{{Published: "9090", Target: "9090", Protocol: "tcp"}}},
		{Name: "quiet", Image: "example/quiet", NetworkOf: "gluetun"},
	}}
	rep := buildPreflightReport(p, pfHostWith(), "vpn")
	if it := pfItem(rep, "shared-net-ports:app"); it == nil || it.Level != levelBlock || !strings.Contains(it.Title, "network of gluetun") {
		t.Fatalf("app publishing ports in gluetun's network should block: %+v", it)
	}
	if pfItem(rep, "shared-net-ports:quiet") != nil || pfItem(rep, "shared-net-ports:gluetun") != nil {
		t.Errorf("only app is wrong: %+v", rep.Items)
	}
	if it := pfItem(rep, "ports"); it == nil || !strings.Contains(it.Title, "8080/tcp") || strings.Contains(it.Title, "9090") {
		t.Errorf("gluetun's port is checked as usual: %+v", it)
	}
}
