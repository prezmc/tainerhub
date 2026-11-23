package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestComposeWarningsAndErrors(t *testing.T) {
	stderr := `time="2026-11-23T12:54:14-06:00" level=warning msg="The \"DB_PASSWORD\" variable is not set. Defaulting to a blank string."
WARN[0000] /x/compose.yaml: the attribute ` + "`version`" + ` is obsolete
env file /opt/tainerhub/stacks/app/app.env not found: stat /opt/tainerhub/stacks/app/app.env: no such file or directory
`
	w := composeWarnings(stderr)
	if len(w) != 2 || w[0] != `The "DB_PASSWORD" variable is not set. Defaulting to a blank string.` || w[1] != "/x/compose.yaml: the attribute `version` is obsolete" {
		t.Errorf("warnings %q", w)
	}
	if e := composeErrors(stderr); e != "env file /opt/tainerhub/stacks/app/app.env not found: stat /opt/tainerhub/stacks/app/app.env: no such file or directory\n" {
		t.Errorf("errors %q", e)
	}
}

func TestParseMounts(t *testing.T) {
	ms, err := parseMounts([]byte(composeJSON))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, m := range ms {
		got[m.Service+" "+m.Target] = m.Type + " " + m.Source
	}
	want := map[string]string{
		"web /usr/share/nginx/html":   "bind /opt/tainerhub/stacks/app/html",
		"web /media":                  "bind /srv/media",
		"web /cache":                  "volume app_db",
		"db /var/lib/postgresql/data": "volume app_db",
	}
	if len(got) != len(want) {
		t.Errorf("mounts %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

func TestPathEntries(t *testing.T) {
	dir := t.TempDir()
	if pathEntries(filepath.Join(dir, "missing")) != -1 || pathEntries(dir) != 0 {
		t.Error("missing or empty")
	}
	_ = os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o600)
	if pathEntries(dir) != 1 || pathEntries(filepath.Join(dir, "f")) != 1 {
		t.Error("folder with a file, or a file")
	}
}
