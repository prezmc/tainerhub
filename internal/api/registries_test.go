package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prezmc/tainerhub/internal/auth"
	"github.com/prezmc/tainerhub/internal/regcred"
	"github.com/prezmc/tainerhub/internal/registry"
	"github.com/prezmc/tainerhub/internal/store"
	"github.com/prezmc/tainerhub/internal/wire"
)

// fakeRegStore keeps registry logins in memory.
type fakeRegStore struct {
	mu      sync.Mutex
	rows    map[string]store.RegistryCredential
	n       int
	touched []string
}

func (f *fakeRegStore) all() []store.RegistryCredential {
	var out []store.RegistryCredential
	for _, c := range f.rows {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Registry < out[j].Registry })
	return out
}

func (f *fakeRegStore) ListRegistryCredentials(context.Context) ([]store.RegistryCredential, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.all(), nil
}

func (f *fakeRegStore) RegistryCredentialsFor(_ context.Context, hosts []string) ([]store.RegistryCredential, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.RegistryCredential
	for _, c := range f.all() {
		for _, h := range hosts {
			if c.Registry == h {
				out = append(out, c)
			}
		}
	}
	return out, nil
}

func (f *fakeRegStore) GetRegistryCredential(_ context.Context, id string) (store.RegistryCredential, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.rows[id]
	if !ok {
		return c, store.ErrNotFound
	}
	return c, nil
}

func (f *fakeRegStore) CreateRegistryCredential(_ context.Context, host, user string, secret []byte, by string) (store.RegistryCredential, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.rows {
		if c.Registry == host {
			return store.RegistryCredential{}, store.ErrConflict
		}
	}
	f.n++
	id := "6f1c2f2e-6a8e-4a43-9a65-1d0f6ad7b0" + string(rune('a'+f.n/10)) + string(rune('0'+f.n%10))
	c := store.RegistryCredential{ID: id, Registry: host, Username: user, Secret: secret, CreatedBy: by, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if f.rows == nil {
		f.rows = map[string]store.RegistryCredential{}
	}
	f.rows[id] = c
	return c, nil
}

func (f *fakeRegStore) UpdateRegistryCredential(_ context.Context, id, user string, secret []byte) (store.RegistryCredential, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.rows[id]
	if !ok {
		return c, store.ErrNotFound
	}
	c.Username = user
	if secret != nil {
		c.Secret = secret
	}
	f.rows[id] = c
	return c, nil
}

func (f *fakeRegStore) DeleteRegistryCredential(_ context.Context, id string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.rows[id]
	if !ok {
		return "", store.ErrNotFound
	}
	delete(f.rows, id)
	return c.Registry, nil
}

func (f *fakeRegStore) TouchRegistryCredentials(_ context.Context, hosts []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.touched = append(f.touched, hosts...)
	return nil
}

type regHarness struct {
	s       *Server
	h       http.Handler
	fs      *fakeRegStore
	mu      sync.Mutex
	audit   []store.AuditEntry
	user    store.User
	token   *auth.TokenInfo
	freshOK bool
}

func newRegHarness(t *testing.T) *regHarness {
	t.Helper()
	x := &regHarness{fs: &fakeRegStore{}, user: store.User{ID: "u1", Username: "ann", Role: "admin"}, freshOK: true}
	x.s = &Server{
		Auth:     &auth.Service{},
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		DataDir:  t.TempDir(),
		Reg:      registry.New(),
		regCreds: x.fs,
		audited: func(e store.AuditEntry) {
			x.mu.Lock()
			x.audit = append(x.audit, e)
			x.mu.Unlock()
		},
		authn: func(*http.Request) (store.User, *auth.TokenInfo, error) { return x.user, x.token, nil },
		freshCheck: func(*http.Request) error {
			if x.freshOK {
				return nil
			}
			return auth.ErrReauth
		},
	}
	x.h = x.s.Handler()
	return x
}

func (x *regHarness) do(method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("X-TainerHub", "1")
	w := httptest.NewRecorder()
	x.h.ServeHTTP(w, r)
	return w
}

const ghcrPAT = "ghp_SuperSecretToken123"

func TestRegistryCRUDNeverReturnsSecrets(t *testing.T) {
	x := newRegHarness(t)
	w := x.do("POST", "/api/registries", `{"registry":"https://GHCR.io/org","username":"octo","password":"  `+ghcrPAT+`  "}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("add: %d %s", w.Code, w.Body)
	}
	var added registryView
	_ = json.Unmarshal(w.Body.Bytes(), &added)
	if added.Registry != "ghcr.io" || added.Username != "octo" || !added.PasswordSet {
		t.Fatalf("added = %+v", added)
	}
	// Stored encrypted with the key in the data folder.
	row := x.fs.rows[added.ID]
	if strings.Contains(string(row.Secret), ghcrPAT) {
		t.Fatal("the password is stored in the clear")
	}
	if st, err := os.Stat(filepath.Join(x.s.DataDir, regcred.KeyFile)); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v %v", st, err)
	}
	if w := x.do("POST", "/api/registries", `{"registry":"ghcr.io","username":"x","password":"y"}`); w.Code != http.StatusConflict {
		t.Fatalf("duplicate: %d", w.Code)
	}
	for _, bad := range []string{
		`{"registry":"not a host","username":"u","password":"p"}`,
		`{"registry":"quay.io","username":"","password":"p"}`,
		`{"registry":"quay.io","username":"u","password":"   "}`,
	} {
		if w := x.do("POST", "/api/registries", bad); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", bad, w.Code)
		}
	}

	// Edit: username only keeps the password; a new password replaces it.
	if w := x.do("PUT", "/api/registries/"+added.ID, `{"username":"octocat","password":""}`); w.Code != 200 {
		t.Fatalf("update: %d %s", w.Code, w.Body)
	}
	if !strings.EqualFold(x.fs.rows[added.ID].Username, "octocat") || string(x.fs.rows[added.ID].Secret) != string(row.Secret) {
		t.Fatal("empty password must keep the saved one")
	}
	if w := x.do("PUT", "/api/registries/"+added.ID, `{"password":"ghp_NewToken456"}`); w.Code != 200 {
		t.Fatalf("update: %d", w.Code)
	}
	if string(x.fs.rows[added.ID].Secret) == string(row.Secret) {
		t.Fatal("password not replaced")
	}

	w = x.do("GET", "/api/registries", "")
	if w.Code != 200 {
		t.Fatalf("list: %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "ghp_") || strings.Contains(body, "secret") || !strings.Contains(body, `"passwordSet":true`) {
		t.Fatalf("list leaks or lacks fields: %s", body)
	}

	if w := x.do("DELETE", "/api/registries/"+added.ID, ""); w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", w.Code)
	}
	if w := x.do("DELETE", "/api/registries/"+added.ID, ""); w.Code != http.StatusNotFound {
		t.Fatalf("delete again: %d", w.Code)
	}

	var actions []string
	for _, e := range x.audit {
		actions = append(actions, e.Action)
		if strings.Contains(string(e.Detail), "ghp_") {
			t.Fatalf("audit %s holds the password: %s", e.Action, e.Detail)
		}
	}
	if strings.Join(actions, ",") != "registry.added,registry.updated,registry.updated,registry.removed" {
		t.Fatalf("audit actions = %v", actions)
	}
}

func TestRegistryAccessChecks(t *testing.T) {
	x := newRegHarness(t)
	add := `{"registry":"ghcr.io","username":"octo","password":"p"}`

	// Below admin: no access at all.
	x.user.Role = "operator"
	if w := x.do("GET", "/api/registries", ""); w.Code != http.StatusForbidden {
		t.Fatalf("operator list: %d", w.Code)
	}
	if w := x.do("POST", "/api/registries", add); w.Code != http.StatusForbidden {
		t.Fatalf("operator add: %d", w.Code)
	}
	x.user.Role = "admin"

	// Stale sign-in: 403 with reauth, which the UI turns into "Sign in again".
	x.freshOK = false
	for _, c := range [][2]string{{"POST", "/api/registries"}, {"PUT", "/api/registries/6f1c2f2e-6a8e-4a43-9a65-1d0f6ad7b0a1"}, {"DELETE", "/api/registries/6f1c2f2e-6a8e-4a43-9a65-1d0f6ad7b0a1"}} {
		w := x.do(c[0], c[1], add)
		var b struct{ Reauth bool }
		_ = json.Unmarshal(w.Body.Bytes(), &b)
		if w.Code != http.StatusForbidden || !b.Reauth {
			t.Errorf("%s %s with a stale sign-in: %d %s", c[0], c[1], w.Code, w.Body)
		}
	}
	x.freshOK = true

	// API tokens can't change logins, even an admin's.
	x.token = &auth.TokenInfo{ID: "t1", Name: "ci"}
	for _, c := range [][2]string{{"POST", "/api/registries"}, {"POST", "/api/registries/6f1c2f2e-6a8e-4a43-9a65-1d0f6ad7b0a1/test"}} {
		if w := x.do(c[0], c[1], add); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "API token") {
			t.Errorf("%s %s by token: %d %s", c[0], c[1], w.Code, w.Body)
		}
	}
	if len(x.fs.rows) != 0 || len(x.audit) != 0 {
		t.Fatal("refused requests changed something")
	}
}

func TestImagesToPull(t *testing.T) {
	st := registry.Status{
		Hello: &wire.Hello{OwnProject: "tainerhub"},
		Containers: []wire.Container{
			{ID: "aaaaaaaaaaaa1111", Name: "web", Image: "ghcr.io/org/web:1"},
			{ID: "bbbbbbbbbbbb2222", Name: "db", Image: "postgres:16"},
			{ID: "cccccccccccc3333", Name: "tainerhub", Image: "ghcr.io/prezmc/tainerhub:latest", Project: "tainerhub"},
		},
	}
	op := func(o wire.Operation) *wire.Command { return &wire.Command{Op: &o} }
	for name, c := range map[string]struct {
		cmd  *wire.Command
		want []string
	}{
		"pull":             {op(wire.Operation{Kind: wire.OpImagePull, Arg: "myreg.lan:5000/app"}), []string{"myreg.lan:5000/app"}},
		"recreate by id":   {op(wire.Operation{Kind: wire.OpContainerRecreate, Target: "aaaaaaaaaaaa"}), []string{"ghcr.io/org/web:1"}},
		"recreate by name": {op(wire.Operation{Kind: wire.OpContainerRecreate, Target: "db"}), []string{"postgres:16"}},
		"recreate unknown": {op(wire.Operation{Kind: wire.OpContainerRecreate, Target: "zzz"}), nil},
		"check updates":    {op(wire.Operation{Kind: wire.OpImagesCheckUpdates}), []string{"ghcr.io/org/web:1", "postgres:16", "ghcr.io/prezmc/tainerhub:latest"}},
		"self update":      {op(wire.Operation{Kind: wire.OpSelfUpdate}), []string{"ghcr.io/prezmc/tainerhub:latest"}},
		"add cgroup mount": {op(wire.Operation{Kind: wire.OpSelfUpdate, Arg: wire.SelfUpdateAddCgroupMount}), nil},
		"list (no pull)":   {op(wire.Operation{Kind: wire.OpImagesList}), nil},
		"remove (no pull)": {op(wire.Operation{Kind: wire.OpImageRemove, Target: "sha256:abc"}), nil},
		"deploy": {&wire.Command{Stack: &wire.StackCommand{Action: wire.StackDeploy, Name: "s",
			Compose: "services:\n  a:\n    image: ${REG}/a\n  b:\n    image: nginx\n", Env: "REG=registry.gitlab.com/g"}},
			[]string{"registry.gitlab.com/g/a", "nginx"}},
		"stop (no pull)": {&wire.Command{Stack: &wire.StackCommand{Action: wire.StackStop, Name: "s"}}, nil},
	} {
		got := imagesToPull(st, c.cmd)
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("%s: %q, want %q", name, got, c.want)
		}
	}
}

// The hub attaches the logins for exactly the registries a command's
// images come from, and records that they were used.
func TestRegistryAuthSelection(t *testing.T) {
	x := newRegHarness(t)
	for _, b := range []string{
		`{"registry":"ghcr.io","username":"octo","password":"` + ghcrPAT + `"}`,
		`{"registry":"docker.io","username":"me","password":"dckr_pat_1"}`,
		`{"registry":"quay.io","username":"q","password":"quay_1"}`,
	} {
		if w := x.do("POST", "/api/registries", b); w.Code != http.StatusCreated {
			t.Fatalf("add: %d %s", w.Code, w.Body)
		}
	}
	a := x.s.Reg.Attach("h1", "host1", func() {})
	ctx := context.Background()

	cmd := &wire.Command{Op: &wire.Operation{Kind: wire.OpImagePull, Arg: "ghcr.io/org/app:1"}}
	x.s.AddRegistryAuth(ctx, a, cmd)
	if len(cmd.Op.Auth) != 1 || cmd.Op.Auth[0].Registry != "ghcr.io" || cmd.Op.Auth[0].Password != ghcrPAT {
		t.Fatalf("pull auth = %v", cmd.Op.Auth)
	}

	stack := &wire.Command{Stack: &wire.StackCommand{Action: wire.StackDeploy, Name: "s",
		Compose: "services:\n  a:\n    image: ghcr.io/org/a\n  b:\n    image: library/redis\n  c:\n    image: myreg.lan:5000/c\n"}}
	x.s.AddRegistryAuth(ctx, a, stack)
	var regs []string
	for _, au := range stack.Stack.Auth {
		regs = append(regs, au.Registry)
	}
	if strings.Join(regs, ",") != "docker.io,ghcr.io" {
		t.Fatalf("stack auth registries = %v (quay.io isn't used; myreg has no login)", regs)
	}
	if err := wire.ValidStackCommand(*stack.Stack); err != nil {
		t.Fatalf("the command with logins must still be valid: %v", err)
	}

	none := &wire.Command{Op: &wire.Operation{Kind: wire.OpImagesList}}
	x.s.AddRegistryAuth(ctx, a, none)
	if len(none.Op.Auth) != 0 {
		t.Fatal("a listing must carry no logins")
	}
	anon := &wire.Command{Op: &wire.Operation{Kind: wire.OpImagePull, Arg: "lscr.io/linuxserver/plex"}}
	x.s.AddRegistryAuth(ctx, a, anon)
	if len(anon.Op.Auth) != 0 {
		t.Fatal("no login saved for lscr.io: none sent")
	}
	if strings.Join(x.fs.touched, ",") != "ghcr.io,docker.io,ghcr.io" {
		t.Fatalf("last used recorded for %v", x.fs.touched)
	}

	// A login whose secret can't be decrypted (the key changed) is skipped.
	x.s.regKey.sealer = nil
	if err := os.WriteFile(filepath.Join(x.s.DataDir, regcred.KeyFile), []byte("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	again := &wire.Command{Op: &wire.Operation{Kind: wire.OpImagePull, Arg: "ghcr.io/org/app:1"}}
	x.s.AddRegistryAuth(ctx, a, again)
	if len(again.Op.Auth) != 0 {
		t.Fatal("an undecryptable login must not be sent")
	}
}

func TestRegistryTestOffline(t *testing.T) {
	x := newRegHarness(t)
	w := x.do("POST", "/api/registries", `{"registry":"ghcr.io","username":"octo","password":"`+ghcrPAT+`"}`)
	var added registryView
	_ = json.Unmarshal(w.Body.Bytes(), &added)
	if w := x.do("POST", "/api/registries/"+added.ID+"/test", `{}`); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("test with no host online: %d %s", w.Code, w.Body)
	}
}
