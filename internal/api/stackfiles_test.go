package api

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/prezmc/tainerhub/internal/auth"
	"github.com/prezmc/tainerhub/internal/store"
	"github.com/prezmc/tainerhub/internal/wire"
)

func TestStackHashUnchangedWithoutFiles(t *testing.T) {
	// Stacks deployed before extra files existed must not show as changed.
	old := sha256.Sum256([]byte("services: {}\x00A=1"))
	st := store.Stack{Compose: "services: {}", Env: "A=1"}
	if stackHash(st) != hex.EncodeToString(old[:]) {
		t.Fatal("hash of a stack without files changed")
	}
	with := st
	with.Files = []store.StackFile{{Path: "Dockerfile", Content: "FROM a"}}
	exec := st
	exec.Files = []store.StackFile{{Path: "Dockerfile", Content: "FROM a", Executable: true}}
	reordered := st
	reordered.Files = []store.StackFile{{Path: "b", Content: "2"}, {Path: "a", Content: "1"}}
	ordered := st
	ordered.Files = []store.StackFile{{Path: "a", Content: "1"}, {Path: "b", Content: "2"}}
	if stackHash(with) == stackHash(st) || stackHash(with) == stackHash(exec) {
		t.Error("file content or executable bit not in the hash")
	}
	if stackHash(reordered) != stackHash(ordered) {
		t.Error("file order changed the hash")
	}
}

func TestStackInputFiles(t *testing.T) {
	files := func(fs ...store.StackFile) *[]store.StackFile { return &fs }
	ok := stackInput{Name: "web", Compose: "services: {}", Files: files(store.StackFile{Path: "Dockerfile", Content: "FROM alpine"})}
	if err := ok.validate(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		in   stackInput
		want string
	}{
		{stackInput{Name: "web", Compose: "x", Files: files(store.StackFile{Path: "../evil", Content: "x"})}, `file "../evil"`},
		{stackInput{Name: "web", Compose: "x", Files: files(store.StackFile{Path: "bin", Content: "a\x00b"})}, `"bin" isn't text`},
		{stackInput{Name: "web", Compose: "x", Files: files(store.StackFile{Path: ".env", Content: ""})}, "own tab"},
		{stackInput{Name: "web", Compose: "x", Files: files(store.StackFile{Path: "big", Content: strings.Repeat("x", wire.MaxStackFileBytes+1)})}, "too large"},
	} {
		if err := tc.in.validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("err=%v, want %q", err, tc.want)
		}
	}
	// No files in an update keeps the saved ones; an empty list clears them.
	saved := []store.StackFile{{Path: "Dockerfile"}}
	if got := (&stackInput{}).files(saved); len(got) != 1 {
		t.Error("missing files dropped the saved ones")
	}
	if got := (&stackInput{Files: files()}).files(saved); len(got) != 0 {
		t.Error("an empty list should clear the files")
	}
}

func TestChangedFilesAndHiding(t *testing.T) {
	before := []store.StackFile{{Path: "Dockerfile", Content: "a"}, {Path: "run.sh", Content: "x"}, {Path: "same", Content: "s"}}
	after := []store.StackFile{{Path: "Dockerfile", Content: "b"}, {Path: "new.conf"}, {Path: "same", Content: "s"}}
	if got := changedFiles(before, after); !reflect.DeepEqual(got, []string{"Dockerfile", "new.conf", "run.sh"}) {
		t.Errorf("changed: %v", got)
	}
	if got := changedFiles(after, after); len(got) != 0 {
		t.Errorf("no changes: %v", got)
	}
	st := store.Stack{Compose: "c", Env: "e", Files: before}
	hideStackFiles(&st)
	if st.Compose != "" || st.Env != "" || st.Files == nil || len(st.Files) != 0 {
		t.Errorf("not hidden: %+v", st)
	}
}

// Writing stack files is root on the host: only admins can create or
// change stacks, so an operator's files never reach the store.
func TestStackWritesAdminOnly(t *testing.T) {
	user := store.User{ID: "u1", Username: "op", Role: "operator"}
	s := &Server{
		Auth:  &auth.Service{},
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		authn: func(*http.Request) (store.User, *auth.TokenInfo, error) { return user, nil, nil },
	}
	h := s.Handler()
	body := `{"hostId":"0b6b7a4e-55a4-4c2e-9d64-1c1f0f2a9b10","name":"web","compose":"services: {}","files":[{"path":"Dockerfile","content":"FROM alpine"}]}`
	for _, c := range [][2]string{{"POST", "/api/stacks"}, {"PUT", "/api/stacks/0b6b7a4e-55a4-4c2e-9d64-1c1f0f2a9b10"}} {
		r := httptest.NewRequest(c[0], c[1], strings.NewReader(body))
		r.Header.Set("X-TainerHub", "1")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s %s by an operator: %d %s", c[0], c[1], w.Code, w.Body)
		}
	}
}
