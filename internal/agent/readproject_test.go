package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prezmc/tainerhub/internal/wire"
)

// fakeReader answers like the helper container would for a project in
// /home/me/apps/app with an override file and an env_file.
type fakeReader struct {
	files map[string]string
	args  [][]string
	dirs  []string
	exit  int
}

func (f *fakeReader) ComposeInFolders(_ context.Context, workDir string, dirs, args []string) (composeRun, func(context.Context, string) ([]byte, error), func(), error) {
	f.args = append(f.args, args)
	f.dirs = dirs
	run := composeRun{Exit: f.exit, Stderr: `time="x" level=warning msg="The \"TZ\" variable is not set. Defaulting to a blank string."` + "\n"}
	if f.exit != 0 {
		run.Stderr += "services.web.image must be a string\n"
	} else if args[len(args)-1] == "json" {
		run.Stdout = `{"services":{"web":{"env_file":[{"path":"/home/me/apps/app/app.env","required":true}]},"db":{"env_file":[{"path":"/home/me/shared/app.env","required":true}]}}}`
	} else {
		run.Stdout = "name: app\nservices:\n  db:\n    env_file:\n      - path: /home/me/shared/app.env\n        required: true\n  web:\n    env_file:\n      - path: /home/me/apps/app/app.env\n        required: true\n    volumes:\n      - source: /home/me/apps/app/html\n        target: /usr/share/nginx/html\n        type: bind\n"
	}
	mounted := slicesClone(dirs)
	read := func(_ context.Context, p string) ([]byte, error) {
		// Like Docker: only files in folders this helper has mounted.
		if !underAny(filepath.Dir(p), mounted) {
			return nil, errors.New("Could not find the file " + p + " in container")
		}
		if b, ok := f.files[p]; ok {
			return []byte(b), nil
		}
		return nil, errors.New("Could not find the file " + p)
	}
	return run, read, func() {}, nil
}

func TestReadProjectFiles(t *testing.T) {
	stacks := t.TempDir()
	f := &fakeReader{files: map[string]string{
		"/home/me/apps/app/app.env": "SECRET=abc\n",
		"/home/me/shared/app.env":   "OTHER=1\n",
		"/home/me/apps/app/.env":    "DB_PASSWORD=hunter2\n",
	}}
	in := wire.StackSource{WorkingDir: "/home/me/apps/app", ConfigFiles: []string{"/home/me/apps/app/compose.yaml", "/home/me/apps/app/compose.override.yml"}}
	out, err := readProjectFiles(context.Background(), f, stacks, "app", in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.Compose, "# Taken over by TainerHub from /home/me/apps/app\n# Merged from: /home/me/apps/app/compose.yaml, /home/me/apps/app/compose.override.yml\n") {
		t.Errorf("header:\n%s", out.Compose)
	}
	if !strings.Contains(out.Compose, "path: ./env/app.env\n") || !strings.Contains(out.Compose, "path: ./env/2-app.env\n") || strings.Contains(out.Compose, "/home/me/shared") {
		t.Errorf("env_file paths not pointed at the copies:\n%s", out.Compose)
	}
	if !strings.Contains(out.Compose, "source: /home/me/apps/app/html") {
		t.Error("bind mount should keep its absolute path")
	}
	for name, want := range map[string]string{"app.env": "SECRET=abc\n", "2-app.env": "OTHER=1\n"} {
		b, err := os.ReadFile(filepath.Join(stacks, "app", envFileDir, name))
		if err != nil || string(b) != want {
			t.Errorf("copy %s: %q %v", name, b, err)
		}
	}
	if out.Env != "DB_PASSWORD=hunter2\n" || out.EnvFile != "/home/me/apps/app/.env" {
		t.Errorf("env %q from %q", out.Env, out.EnvFile)
	}
	if len(out.Copied) != 2 || len(out.Warnings) != 1 || !strings.Contains(out.Warnings[0], `"TZ"`) {
		t.Errorf("copied %v warnings %q", out.Copied, out.Warnings)
	}
	// Both runs merge every file, with the variables left alone.
	for _, a := range f.args {
		joined := strings.Join(a, " ")
		if !strings.Contains(joined, "--file /home/me/apps/app/compose.override.yml") || !strings.Contains(joined, "config --no-interpolate") {
			t.Errorf("args %q", joined)
		}
	}
	if strings.Join(f.dirs, ",") != "/home/me/apps/app,/home/me/shared" {
		t.Errorf("mounted %v (the env file's folder should be added once the JSON names it)", f.dirs)
	}
	if len(f.args) != 3 {
		t.Errorf("%d helper runs, want 3 (YAML, JSON, JSON again with the env file's folder)", len(f.args))
	}
}

func TestReadProjectFilesErrors(t *testing.T) {
	in := wire.StackSource{WorkingDir: "/home/me/apps/app", ConfigFiles: []string{"/home/me/apps/app/compose.yaml"}}
	if _, err := readProjectFiles(context.Background(), &fakeReader{exit: 1}, t.TempDir(), "app", in); err == nil || !strings.Contains(err.Error(), "must be a string") || strings.Contains(err.Error(), "level=warning") {
		t.Errorf("compose error: %v", err)
	}
	if _, err := readProjectFiles(context.Background(), &fakeReader{files: map[string]string{}}, t.TempDir(), "app", in); err == nil || !strings.Contains(err.Error(), "read env file") {
		t.Errorf("missing env file: %v", err)
	}
	bad := in
	bad.WorkingDir = "/etc"
	if _, err := readProjectFiles(context.Background(), &fakeReader{}, t.TempDir(), "app", bad); err == nil {
		t.Error("read from /etc")
	}
}

func TestSafeHostDir(t *testing.T) {
	for _, ok := range []string{"/home/me/app", "/opt/stacks/x", "/srv/compose", "/root/app"} {
		if !safeHostDir(ok) {
			t.Errorf("%s refused", ok)
		}
	}
	for _, bad := range []string{"/", "relative/x", "/home/me/../etc", "/etc", "/proc/1", "/var/run", "/usr/local"} {
		if safeHostDir(bad) {
			t.Errorf("%s accepted", bad)
		}
	}
}
