package agent

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/prezmc/tainerhub/internal/wire"
)

func deployFiles(t *testing.T, r *StackRunner, files []wire.StackFile, compose string, pull bool) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := r.Run(context.Background(), wire.StackCommand{Action: wire.StackDeploy, Name: "app", Compose: compose, Files: files, Pull: pull}, &out)
	return out.String(), err
}

func TestStackFilesWrittenAndStaleRemoved(t *testing.T) {
	r, _ := fakeCompose(t)
	dir := filepath.Join(r.Dir, "app")
	files := []wire.StackFile{
		{Path: "Dockerfile", Content: []byte("FROM alpine\n")},
		{Path: "bin/entrypoint.sh", Content: []byte("#!/bin/sh\nexec \"$@\"\n"), Executable: true},
		{Path: "conf/nginx/site.conf", Content: []byte("server {}\n")},
	}
	if out, err := deployFiles(t, r, files, goodCompose, false); err != nil {
		t.Fatalf("deploy: %v\n%s", err, out)
	}
	for _, f := range files {
		p := filepath.Join(dir, f.Path)
		b, err := os.ReadFile(p)
		if err != nil || !bytes.Equal(b, f.Content) {
			t.Fatalf("%s = %q, %v", f.Path, b, err)
		}
		st, _ := os.Stat(p)
		want := os.FileMode(stackFileMode)
		if f.Executable {
			want = stackExecMode
		}
		if st.Mode().Perm() != want {
			t.Errorf("%s mode %v, want %v", f.Path, st.Mode().Perm(), want)
		}
	}
	if got := readManifest(dir); !reflect.DeepEqual(got, []string{"Dockerfile", "bin/entrypoint.sh", "conf/nginx/site.conf"}) {
		t.Fatalf("manifest = %v", got)
	}

	// Files the user made by hand, including one next to a managed file.
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"data/db.sqlite", "conf/local.conf", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(dir, p), []byte("mine"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// Drop two files and change one.
	files = []wire.StackFile{{Path: "Dockerfile", Content: []byte("FROM alpine:3.20\n")}}
	if out, err := deployFiles(t, r, files, goodCompose, false); err != nil {
		t.Fatalf("redeploy: %v\n%s", err, out)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "Dockerfile")); string(b) != "FROM alpine:3.20\n" {
		t.Fatalf("Dockerfile not updated: %q", b)
	}
	for _, gone := range []string{"bin/entrypoint.sh", "bin", "conf/nginx/site.conf", "conf/nginx"} {
		if _, err := os.Lstat(filepath.Join(dir, gone)); !os.IsNotExist(err) {
			t.Errorf("%s should be gone: %v", gone, err)
		}
	}
	for _, kept := range []string{"data/db.sqlite", "conf/local.conf", "notes.txt"} {
		if _, err := os.Stat(filepath.Join(dir, kept)); err != nil {
			t.Errorf("unmanaged %s was removed: %v", kept, err)
		}
	}
	if got := readManifest(dir); !reflect.DeepEqual(got, []string{"Dockerfile"}) {
		t.Fatalf("manifest = %v", got)
	}

	// Remove takes the managed files and manifest, not the user's.
	var out bytes.Buffer
	if err := r.Run(context.Background(), wire.StackCommand{Action: wire.StackRemove, Name: "app"}, &out); err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"Dockerfile", manifestFile} {
		if _, err := os.Lstat(filepath.Join(dir, gone)); !os.IsNotExist(err) {
			t.Errorf("%s should be gone after remove", gone)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "notes.txt")); err != nil {
		t.Error("remove deleted an unmanaged file")
	}
}

func TestStackFilesInvalidComposeLeavesFilesAlone(t *testing.T) {
	r, _ := fakeCompose(t)
	dir := filepath.Join(r.Dir, "app")
	if _, err := deployFiles(t, r, []wire.StackFile{{Path: "Dockerfile", Content: []byte("one")}}, goodCompose, false); err != nil {
		t.Fatal(err)
	}
	if _, err := deployFiles(t, r, []wire.StackFile{{Path: "Dockerfile", Content: []byte("two")}}, "INVALID", false); err == nil {
		t.Fatal("invalid compose deployed")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "Dockerfile")); string(b) != "one" {
		t.Fatalf("a failed deploy replaced the Dockerfile: %q", b)
	}
}

func TestStackFilesRefuseSymlinks(t *testing.T) {
	r, _ := fakeCompose(t)
	dir := filepath.Join(r.Dir, "app")
	outside := t.TempDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// A folder on the way is a symlink out of the stack folder.
	if err := os.Symlink(outside, filepath.Join(dir, "conf")); err != nil {
		t.Fatal(err)
	}
	_, err := deployFiles(t, r, []wire.StackFile{{Path: "conf/x.conf", Content: []byte("x")}}, goodCompose, false)
	if err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("expected a symlink refusal, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "x.conf")); !os.IsNotExist(err) {
		t.Fatal("wrote through a symlink")
	}
	if fileExists(filepath.Join(dir, composeFile)) {
		t.Fatal("a refused deploy still put the compose file in place")
	}

	// The file itself is a symlink.
	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(secret, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(dir, "Dockerfile")); err != nil {
		t.Fatal(err)
	}
	if _, err := deployFiles(t, r, []wire.StackFile{{Path: "Dockerfile", Content: []byte("FROM x")}}, goodCompose, false); err == nil {
		t.Fatal("replaced a symlinked file")
	}
	if b, _ := os.ReadFile(secret); string(b) != "keep" {
		t.Fatal("wrote through a symlinked file")
	}

	// Removing a stale managed file never follows a symlink either.
	_ = os.Remove(filepath.Join(dir, "conf"))
	_ = os.Remove(filepath.Join(dir, "Dockerfile"))
	if _, err := deployFiles(t, r, []wire.StackFile{{Path: "conf/x.conf", Content: []byte("x")}}, goodCompose, false); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(dir, "conf")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "x.conf"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "conf")); err != nil {
		t.Fatal(err)
	}
	out, err := deployFiles(t, r, nil, goodCompose, false)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(outside, "x.conf")); string(b) != "keep" {
		t.Fatal("removed a file through a symlink")
	}
	if !strings.Contains(out, "symbolic link") {
		t.Fatalf("expected a note about the skipped file:\n%s", out)
	}

	// The stack folder itself is a symlink.
	if err := os.Symlink(outside, filepath.Join(r.Dir, "linked")); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	err = r.Run(context.Background(), wire.StackCommand{Action: wire.StackDeploy, Name: "linked", Compose: goodCompose}, &buf)
	if err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("deploy into a symlinked stack folder: %v", err)
	}
}

func TestManifestIgnoresBadPaths(t *testing.T) {
	dir := t.TempDir()
	content := "# comment\nDockerfile\n../../etc/passwd\n/etc/shadow\n.tainerhub-staging/x\nok/file.txt\n"
	if err := os.WriteFile(filepath.Join(dir, manifestFile), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readManifest(dir); !reflect.DeepEqual(got, []string{"Dockerfile", "ok/file.txt"}) {
		t.Fatalf("manifest = %v", got)
	}
}

func TestDeployCommands(t *testing.T) {
	for _, tc := range []struct {
		pull, build bool
		want        [][]string
	}{
		{false, false, [][]string{{"up", "--detach", "--remove-orphans"}}},
		{true, false, [][]string{{"pull"}, {"up", "--detach", "--remove-orphans"}}},
		{false, true, [][]string{{"up", "--detach", "--remove-orphans", "--build"}}},
		{true, true, [][]string{{"build", "--pull"}, {"pull", "--ignore-buildable"}, {"up", "--detach", "--remove-orphans"}}},
	} {
		if got := deployCommands(tc.pull, tc.build); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("pull=%v build=%v: %v, want %v", tc.pull, tc.build, got, tc.want)
		}
	}
}

func TestDeployBuildsWhenComposeBuilds(t *testing.T) {
	r, logs := fakeCompose(t)
	compose := "services:\n  web:\n    build: .\n"
	if out, err := deployFiles(t, r, []wire.StackFile{{Path: "Dockerfile", Content: []byte("FROM alpine\n")}}, compose, false); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	c := calls(t, logs)
	if !strings.HasSuffix(c[len(c)-1], "up --detach --remove-orphans --build") {
		t.Fatalf("deploy: %s", c[len(c)-1])
	}
	if _, err := deployFiles(t, r, []wire.StackFile{{Path: "Dockerfile", Content: []byte("FROM alpine\n")}}, compose, true); err != nil {
		t.Fatal(err)
	}
	c = calls(t, logs)
	last := strings.Join(c[len(c)-3:], "\n")
	for _, want := range []string{"build --pull", "pull --ignore-buildable", "up --detach --remove-orphans"} {
		if !strings.Contains(last, want) {
			t.Fatalf("pull & redeploy: missing %q in\n%s", want, last)
		}
	}
	if !composeBuilds([]byte(`{"services":{"a":{"image":"x"},"b":{"build":{"context":"."}}}}`)) || composeBuilds([]byte(`{"services":{"a":{"image":"x","build":null}}}`)) {
		t.Fatal("composeBuilds")
	}
}
