package main

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prezmc/tainerhub/internal/store"
)

// fakePG accepts only its current password, like Postgres.
type fakePG struct {
	password string
	failSet  bool
}

type fakeConn struct{ pg *fakePG }

func (c *fakeConn) SetOwnPassword(_ context.Context, pw string) error {
	if c.pg.failSet {
		return os.ErrPermission
	}
	c.pg.password = pw
	return nil
}
func (c *fakeConn) Close() {}

func (pg *fakePG) open(_ context.Context, pw string) (*fakeConn, error) {
	if pw != pg.password {
		return nil, store.ErrAuth
	}
	return &fakeConn{pg}, nil
}

func newLog() (*slog.Logger, *bytes.Buffer) {
	var b bytes.Buffer
	return slog.New(slog.NewTextHandler(&b, nil)), &b
}

func TestFirstStartReplacesDefaultPassword(t *testing.T) {
	dir := t.TempDir()
	pg := &fakePG{password: store.DefaultPassword}
	log, out := newLog()
	if _, err := openManaged(context.Background(), pg.open, store.DefaultPassword, dir, log); err != nil {
		t.Fatal(err)
	}
	if pg.password == store.DefaultPassword || len(pg.password) < 32 {
		t.Fatalf("password not replaced: %q", pg.password)
	}
	saved, _ := os.ReadFile(filepath.Join(dir, dbPasswordFile))
	if strings.TrimSpace(string(saved)) != pg.password {
		t.Fatal("saved password doesn't match the database")
	}
	if st, _ := os.Stat(filepath.Join(dir, dbPasswordFile)); st.Mode().Perm() != 0o600 {
		t.Fatalf("password file mode %v", st.Mode().Perm())
	}
	if !strings.Contains(out.String(), "SAVE THIS") || !strings.Contains(out.String(), "db-password") {
		t.Fatalf("admin not told how to see the password:\n%s", out.String())
	}
	if strings.Contains(out.String(), pg.password) {
		t.Fatal("the database password went into the logs")
	}

	// Second start: uses the saved password quietly.
	first := pg.password
	log2, out2 := newLog()
	if _, err := openManaged(context.Background(), pg.open, store.DefaultPassword, dir, log2); err != nil {
		t.Fatal(err)
	}
	if pg.password != first || strings.Contains(out2.String(), first) {
		t.Fatal("restart must reuse the saved password without printing it again")
	}
}

func TestOwnPasswordIsLeftAlone(t *testing.T) {
	dir := t.TempDir()
	pg := &fakePG{password: "chosen-by-admin"}
	log, _ := newLog()
	if _, err := openManaged(context.Background(), pg.open, "chosen-by-admin", dir, log); err != nil {
		t.Fatal(err)
	}
	if pg.password != "chosen-by-admin" {
		t.Fatal("a non-default password must not be changed")
	}
	if _, err := os.Stat(filepath.Join(dir, dbPasswordFile)); !os.IsNotExist(err) {
		t.Fatal("nothing should be saved for an admin-chosen password")
	}
}

func TestRecreatedDatabaseGetsSavedPasswordBack(t *testing.T) {
	dir := t.TempDir()
	pg := &fakePG{password: store.DefaultPassword}
	log, _ := newLog()
	if _, err := openManaged(context.Background(), pg.open, store.DefaultPassword, dir, log); err != nil {
		t.Fatal(err)
	}
	saved := pg.password
	pg.password = store.DefaultPassword // database volume deleted and re-initialised
	if _, err := openManaged(context.Background(), pg.open, store.DefaultPassword, dir, log); err != nil {
		t.Fatal(err)
	}
	if pg.password != saved {
		t.Fatal("saved password not re-applied to the recreated database")
	}
}

func TestCrashBetweenChangeAndSaveRecovers(t *testing.T) {
	dir := t.TempDir()
	// The password was changed and written to the pending file, but the
	// rename never happened.
	pg := &fakePG{password: "abc123abc123abc123abc123"}
	if err := os.WriteFile(filepath.Join(dir, dbPasswordFile+".new"), []byte(pg.password+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	log, _ := newLog()
	if _, err := openManaged(context.Background(), pg.open, store.DefaultPassword, dir, log); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, dbPasswordFile)); strings.TrimSpace(string(b)) != pg.password {
		t.Fatal("pending password not promoted")
	}
}

func TestFailedChangeKeepsDefault(t *testing.T) {
	dir := t.TempDir()
	pg := &fakePG{password: store.DefaultPassword, failSet: true}
	log, _ := newLog()
	if _, err := openManaged(context.Background(), pg.open, store.DefaultPassword, dir, log); err == nil {
		t.Fatal("expected an error")
	}
	if _, err := os.Stat(filepath.Join(dir, dbPasswordFile+".new")); !os.IsNotExist(err) {
		t.Fatal("pending file left behind after a failed change")
	}
	pg.failSet = false
	if _, err := openManaged(context.Background(), pg.open, store.DefaultPassword, dir, log); err != nil || pg.password == store.DefaultPassword {
		t.Fatalf("next start should succeed and rotate: err=%v", err)
	}
}
