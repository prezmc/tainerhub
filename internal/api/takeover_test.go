package api

import (
	"strings"
	"testing"

	"github.com/prezmc/tainerhub/internal/wire"
)

const tdir = "/opt/tainerhub/stacks/app"

// running is the project as it was started by hand from ~/apps/app.
func running() []wire.RunningContainer {
	return []wire.RunningContainer{
		{Name: "app-web-1", Service: "web", Mounts: []wire.DetailMount{
			{Type: "bind", Source: "/home/me/apps/app/html", Destination: "/usr/share/nginx/html"},
			{Type: "bind", Source: "/srv/media", Destination: "/media"},
		}},
		{Name: "app-db-1", Service: "db", Mounts: []wire.DetailMount{
			{Type: "volume", Name: "app_db", Source: "/var/lib/docker/volumes/app_db/_data", Destination: "/var/lib/postgresql/data"},
			{Type: "volume", Name: strings.Repeat("ab", 32), Destination: "/tmp/anon"},
		}},
		{Name: "app-worker-1", Service: "worker"},
	}
}

func issue(rep takeoverReport, prefix string) *takeoverIssue {
	for i := range rep.Issues {
		if strings.HasPrefix(rep.Issues[i].Key, prefix) {
			return &rep.Issues[i]
		}
	}
	return nil
}

func TestTakeoverReportFindsChanges(t *testing.T) {
	p := wire.StackPreview{
		Dir:      tdir,
		Services: []wire.StackService{{Name: "web"}, {Name: "db"}},
		Mounts: []wire.StackMount{
			{Service: "web", Type: "bind", Source: tdir + "/html", Target: "/usr/share/nginx/html"},
			{Service: "web", Type: "bind", Source: "/srv/media", Target: "/media"},
			{Service: "db", Type: "volume", Source: "app_pgdata", Target: "/var/lib/postgresql/data"},
		},
		Paths:       map[string]int{tdir + "/html": -1},
		Running:     running(),
		WorkingDir:  "/home/me/apps/app",
		ConfigFiles: []string{"/home/me/apps/app/compose.yaml", "/home/me/apps/app/compose.override.yml"},
		Warnings:    []string{`The "DB_PASSWORD" variable is not set. Defaulting to a blank string.`, "something else"},
	}
	rep := buildTakeoverReport(p, "")
	if rep.Blocked {
		t.Fatalf("blocked: %+v", rep.Issues)
	}
	bind := issue(rep, "bind:web:/usr/share/nginx/html")
	if bind == nil || !strings.Contains(bind.Detail, "doesn't exist yet") ||
		bind.Fix != "sudo mkdir -p /opt/tainerhub/stacks/app && sudo cp -a /home/me/apps/app/html /opt/tainerhub/stacks/app/html" {
		t.Errorf("moved bind: %+v", bind)
	}
	if issue(rep, "bind:web:/media") != nil {
		t.Error("an unchanged absolute bind was flagged")
	}
	if v := issue(rep, "volume:db:"); v == nil || !strings.Contains(v.Title, "app_pgdata instead of app_db") {
		t.Errorf("swapped volume: %+v", v)
	}
	if issue(rep, "gone:db:/tmp/anon") != nil {
		t.Error("anonymous volume flagged")
	}
	if o := issue(rep, "orphan:worker"); o == nil || !strings.Contains(o.Detail, "app-worker-1 would be removed") {
		t.Errorf("orphan: %+v", o)
	}
	if f := issue(rep, "files:"); f == nil || !strings.Contains(f.Detail, "compose.override.yml") {
		t.Errorf("override files: %+v", f)
	}
	if v := issue(rep, "vars:"); v == nil || v.Level != levelConfirm || !strings.Contains(v.Detail, "DB_PASSWORD") {
		t.Errorf("unset variables: %+v", v)
	}
	if w := issue(rep, "warnings"); w == nil || w.Level != levelInfo {
		t.Errorf("other warnings: %+v", w)
	}
	if rep.Ack == "" || rep.Issues[0].Level != levelConfirm {
		t.Errorf("ack %q, first issue %+v", rep.Ack, rep.Issues[0])
	}

	// The ack changes when the findings do, so an old confirmation doesn't carry over.
	p.Mounts[2].Source = "app_db"
	if again := buildTakeoverReport(p, ""); again.Ack == rep.Ack || issue(again, "volume:") != nil {
		t.Errorf("ack after fixing the volume: %q vs %q", again.Ack, rep.Ack)
	}
}

func TestTakeoverReportCleanAndBlocked(t *testing.T) {
	clean := wire.StackPreview{
		Dir:      tdir,
		Services: []wire.StackService{{Name: "web"}, {Name: "db"}, {Name: "worker"}},
		Mounts: []wire.StackMount{
			{Service: "web", Type: "bind", Source: "/home/me/apps/app/html", Target: "/usr/share/nginx/html"},
			{Service: "web", Type: "bind", Source: "/srv/media", Target: "/media"},
			{Service: "db", Type: "volume", Source: "app_db", Target: "/var/lib/postgresql/data"},
		},
		Running: running(),
	}
	rep := buildTakeoverReport(clean, "")
	if rep.Blocked || rep.Ack != "" || len(rep.Issues) != 0 {
		t.Errorf("clean takeover: %+v", rep)
	}

	// Empty folder already there: the fix copies into it.
	moved := clean
	moved.Mounts = append([]wire.StackMount{{Service: "web", Type: "bind", Source: tdir + "/html", Target: "/usr/share/nginx/html"}}, clean.Mounts[1:]...)
	moved.Paths = map[string]int{tdir + "/html": 0}
	if b := issue(buildTakeoverReport(moved, ""), "bind:"); b == nil || b.Fix != "sudo cp -a /home/me/apps/app/html/. /opt/tainerhub/stacks/app/html/" {
		t.Errorf("empty target: %+v", b)
	}
	moved.Paths[tdir+"/html"] = 3
	if b := issue(buildTakeoverReport(moved, ""), "bind:"); b == nil || b.Fix != "" || !strings.Contains(b.Detail, "already has files") {
		t.Errorf("filled target: %+v", b)
	}

	broken := wire.StackPreview{Dir: tdir, ConfigError: "env file /opt/tainerhub/stacks/app/app.env not found", Running: running()}
	rep = buildTakeoverReport(broken, "")
	if !rep.Blocked || issue(rep, "config") == nil || issue(rep, "orphan:") != nil {
		t.Errorf("config error: %+v", rep)
	}
	build := clean
	build.Services = append(build.Services, wire.StackService{Name: "api", Build: true})
	// Builds work now, from the stack's files: confirm, don't block.
	if rep := buildTakeoverReport(build, ""); rep.Blocked || issue(rep, "build:api") == nil || issue(rep, "build:api").Level != levelConfirm {
		t.Errorf("build: %+v", rep)
	}
}

func TestShellQuote(t *testing.T) {
	if shellQuote("/srv/a-b_c.d") != "/srv/a-b_c.d" || shellQuote("/srv/my media") != "'/srv/my media'" || shellQuote("it's") != `'it'\''s'` {
		t.Errorf("%s %s %s", shellQuote("/srv/a-b_c.d"), shellQuote("/srv/my media"), shellQuote("it's"))
	}
}

func TestTakeoverReportMergedFile(t *testing.T) {
	files := []string{"/home/me/apps/app/compose.yaml", "/home/me/apps/app/compose.override.yml"}
	p := wire.StackPreview{Dir: tdir, ConfigFiles: files}
	if issue(buildTakeoverReport(p, "services: {}\n"), "files:") == nil {
		t.Error("a pasted file should be asked about the override file")
	}
	header := "# Taken over by TainerHub from /home/me/apps/app\n# Merged from: " + strings.Join(files, ", ") + "\n"
	if issue(buildTakeoverReport(p, header+"services: {}\n"), "files:") != nil {
		t.Error("a file TainerHub merged from them shouldn't be")
	}
}

func TestStartedElsewhere(t *testing.T) {
	hello := &wire.Hello{Stacks: &wire.StackSupport{Dir: "/opt/tainerhub/stacks"}}
	c := func(project, wd string) wire.Container {
		return wire.Container{Project: project, Labels: map[string]string{"com.docker.compose.project.working_dir": wd}}
	}
	for _, tc := range []struct {
		name  string
		cs    []wire.Container
		hello *wire.Hello
		want  bool
	}{
		{"nothing running", nil, hello, false},
		{"another project", []wire.Container{c("other", "/srv/other")}, hello, false},
		// a first deploy that created the containers and then failed
		{"TainerHub's own folder", []wire.Container{c("media", "/opt/tainerhub/stacks/media"), c("media", "/opt/tainerhub/stacks/media/")}, hello, false},
		{"started elsewhere", []wire.Container{c("media", "/home/me/media")}, hello, true},
		{"one container elsewhere", []wire.Container{c("media", "/opt/tainerhub/stacks/media"), c("media", "/home/me/media")}, hello, true},
		{"no label", []wire.Container{{Project: "media"}}, hello, true},
		{"stacks folder unknown", []wire.Container{c("media", "/opt/tainerhub/stacks/media")}, nil, true},
	} {
		if got := startedElsewhere(tc.cs, tc.hello, "media"); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}
