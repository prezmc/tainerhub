package agent

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prezmc/tainerhub/internal/regcred"
	"github.com/prezmc/tainerhub/internal/wire"
)

var (
	ghcrLogin = wire.RegistryAuth{Registry: "ghcr.io", Username: "octo", Password: "ghp_topsecret"}
	hubLogin  = wire.RegistryAuth{Registry: "docker.io", Username: "me", Password: "dckr_pat_secret"}
)

// authEngine is a fake Docker Engine that records the X-Registry-Auth
// header (or its absence) of every request, by path.
func authEngine(t *testing.T) (*SocketDocker, func() map[string]string) {
	t.Helper()
	// A short folder: Unix socket paths are limited to ~100 bytes.
	tmp, err := os.MkdirTemp("", "th")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(tmp) })
	sock := filepath.Join(tmp, "d.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	seen := map[string]string{}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		key := r.Method + " " + r.URL.Path
		if r.URL.Path == "/images/create" {
			key += "?" + r.URL.Query().Get("fromImage")
		}
		seen[key] = r.Header.Get("X-Registry-Auth")
		mu.Unlock()
		switch {
		case r.URL.Path == "/images/create":
			w.Write([]byte(`{"status":"Pulling"}` + "\n" + `{"status":"Status: Downloaded newer image"}` + "\n"))
		case strings.HasPrefix(r.URL.Path, "/images/"):
			w.Write([]byte(`{"RepoDigests":["ghcr.io/org/app@sha256:old"]}`))
		case strings.HasPrefix(r.URL.Path, "/distribution/"):
			w.Write([]byte(`{"Descriptor":{"Digest":"sha256:new"}}`))
		case r.URL.Path == "/auth":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["password"] != ghcrLogin.Password || body["serveraddress"] != "ghcr.io" {
				w.WriteHeader(401)
				w.Write([]byte(`{"message":"login attempt failed"}`))
				return
			}
			w.Write([]byte(`{"Status":"Login Succeeded"}`))
		default:
			w.WriteHeader(404)
		}
	})
	srv := &http.Server{Handler: h}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return NewSocketDocker(sock), func() map[string]string {
		mu.Lock()
		defer mu.Unlock()
		out := map[string]string{}
		for k, v := range seen {
			out[k] = v
		}
		return out
	}
}

func decodeAuthHeader(t *testing.T, h string) map[string]string {
	t.Helper()
	b, err := base64.URLEncoding.DecodeString(h)
	if err != nil {
		t.Fatalf("X-Registry-Auth %q is not base64url: %v", h, err)
	}
	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// A pull sends the login for its image's registry, and only that one; an
// image from a registry without a login is pulled anonymously.
func TestPullSendsOnlyItsRegistrysLogin(t *testing.T) {
	d, seen := authEngine(t)
	ctx := context.Background()
	auths := []wire.RegistryAuth{ghcrLogin, hubLogin}
	if _, err := d.Operate(ctx, wire.Operation{Kind: wire.OpImagePull, Arg: "ghcr.io/org/app:1", Auth: auths}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Operate(ctx, wire.Operation{Kind: wire.OpImagePull, Arg: "nginx:1.27", Auth: auths}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Operate(ctx, wire.Operation{Kind: wire.OpImagePull, Arg: "quay.io/x/y", Auth: auths}); err != nil {
		t.Fatal(err)
	}
	s := seen()
	got := decodeAuthHeader(t, s["POST /images/create?ghcr.io/org/app:1"])
	if got["username"] != "octo" || got["password"] != "ghp_topsecret" || got["serveraddress"] != "ghcr.io" {
		t.Fatalf("ghcr pull auth = %v", got)
	}
	got = decodeAuthHeader(t, s["POST /images/create?nginx:1.27"])
	if got["username"] != "me" || got["serveraddress"] != "https://index.docker.io/v1/" {
		t.Fatalf("Docker Hub pull auth = %v", got)
	}
	if h := s["POST /images/create?quay.io/x/y"]; h != "" {
		t.Fatalf("quay.io pull carried a login: %q", h)
	}
}

// Update checks use the login for the registry lookup only, not for the
// local image inspection.
func TestUpdateCheckUsesLoginForRegistryOnly(t *testing.T) {
	d, seen := authEngine(t)
	newer, _, err := d.newerImage(context.Background(), "ghcr.io/org/app:1", []wire.RegistryAuth{ghcrLogin})
	if err != nil || !newer {
		t.Fatalf("newerImage = %v, %v", newer, err)
	}
	for k, v := range seen() {
		switch {
		case strings.HasPrefix(k, "GET /distribution/"):
			if decodeAuthHeader(t, v)["password"] != ghcrLogin.Password {
				t.Fatalf("%s: wrong login", k)
			}
		case v != "":
			t.Fatalf("%s carried a login it doesn't need", k)
		}
	}
}

func TestUpdateCacheKeepsAnswerOnError(t *testing.T) {
	var u updateCache
	u.set(wire.ImageUpdates{Images: map[string]bool{"ghcr.io/a": true, "nginx": true}})
	// The background check has no logins: the private image errors.
	u.set(wire.ImageUpdates{Images: map[string]bool{"nginx": false}, Errors: map[string]string{"ghcr.io/a": "unauthorized"}})
	if !u.has("ghcr.io/a") || u.has("nginx") {
		t.Fatal("a failed check must keep the previous answer; a successful one replaces it")
	}
}

func TestRegistryLoginOp(t *testing.T) {
	d, seen := authEngine(t)
	ctx := context.Background()
	res, err := d.Operate(ctx, wire.Operation{Kind: wire.OpRegistryLogin, Auth: []wire.RegistryAuth{ghcrLogin}})
	if err != nil || res.(wire.RegistryLoginResult).Status != "Login Succeeded" {
		t.Fatalf("login = %v, %v", res, err)
	}
	if _, ok := seen()["POST /auth"]; !ok {
		t.Fatal("the engine's /auth wasn't asked")
	}
	bad := ghcrLogin
	bad.Password = "wrong-password"
	if _, err := d.Operate(ctx, wire.Operation{Kind: wire.OpRegistryLogin, Auth: []wire.RegistryAuth{bad}}); err == nil || strings.Contains(err.Error(), "wrong-password") {
		t.Fatalf("bad login: err = %v", err)
	}
	if _, err := d.Operate(ctx, wire.Operation{Kind: wire.OpRegistryLogin}); err == nil {
		t.Fatal("a login check without a login must be refused")
	}
}

// Logins are refused on operations that don't pull.
func TestLoginsOnlyOnPullingOps(t *testing.T) {
	d, _ := authEngine(t)
	_, err := d.Operate(context.Background(), wire.Operation{Kind: wire.OpImagesList, Auth: []wire.RegistryAuth{ghcrLogin}})
	if err == nil {
		t.Fatal("images.list with a login must be refused")
	}
	if err := wire.ValidStackCommand(wire.StackCommand{Action: wire.StackStop, Name: "web", Auth: []wire.RegistryAuth{ghcrLogin}}); err == nil {
		t.Fatal("stop with a login must be refused")
	}
}

func TestAuthArchive(t *testing.T) {
	tr := tar.NewReader(bytes.NewReader(authArchive([]wire.RegistryAuth{ghcrLogin})))
	var names []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
		switch h.Name {
		case helperAuthDir + "/":
			if h.Mode != 0o700 {
				t.Fatalf("dir mode %o", h.Mode)
			}
		case helperAuthDir + "/config.json":
			if h.Mode != 0o600 {
				t.Fatalf("file mode %o", h.Mode)
			}
			b, _ := io.ReadAll(tr)
			if !strings.Contains(string(b), `"ghcr.io"`) || strings.Contains(string(b), "docker.io") {
				t.Fatalf("config = %s", b)
			}
		}
	}
	if len(names) != 2 {
		t.Fatalf("entries %v", names)
	}
}

// A deploy with logins runs compose with its own DOCKER_CONFIG (0700,
// holding only those logins), removed when it's done; commands without
// logins use the shared one, which never holds a password.
func TestStackDeployTempDockerConfig(t *testing.T) {
	r, logs := fakeCompose(t)
	// Make the fake compose copy what it sees in its DOCKER_CONFIG.
	script, _ := os.ReadFile(r.Bin)
	snap := filepath.Join(logs, "seen")
	extra := strings.Replace(string(script), "#!/bin/sh\n", "#!/bin/sh\n"+
		`case "$*" in *" pull"*) stat -c %a "$DOCKER_CONFIG" > "`+snap+`.mode" 2>/dev/null || stat -f %Lp "$DOCKER_CONFIG" > "`+snap+`.mode"; echo "$DOCKER_CONFIG" > "`+snap+`.dir"; cat "$DOCKER_CONFIG/config.json" > "`+snap+`.json"; echo "auth for me: ghp_topsecret";; esac`+"\n", 1)
	if err := os.WriteFile(r.Bin, []byte(extra), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var out bytes.Buffer
	c := wire.StackCommand{Action: wire.StackDeploy, Name: "web", Compose: goodCompose, Pull: true, Auth: []wire.RegistryAuth{ghcrLogin}}
	sw := regcred.NewScrubWriter(&out, c.Auth)
	if err := r.Run(ctx, c, sw); err != nil {
		t.Fatalf("deploy: %v\n%s", err, out.String())
	}
	sw.Flush()
	if strings.Contains(out.String(), "ghp_topsecret") || !strings.Contains(out.String(), regcred.Redacted) {
		t.Fatalf("compose output not scrubbed:\n%s", out.String())
	}
	mode, _ := os.ReadFile(snap + ".mode")
	if strings.TrimSpace(string(mode)) != "700" {
		t.Fatalf("DOCKER_CONFIG mode = %q, want 700", mode)
	}
	dir, _ := os.ReadFile(snap + ".dir")
	cfgDir := strings.TrimSpace(string(dir))
	if !strings.HasPrefix(cfgDir, filepath.Join(r.WorkDir, "auth")+"/") {
		t.Fatalf("DOCKER_CONFIG = %q, want a folder of its own", cfgDir)
	}
	var cfg struct{ Auths map[string]json.RawMessage }
	b, _ := os.ReadFile(snap + ".json")
	if err := json.Unmarshal(b, &cfg); err != nil || len(cfg.Auths) != 1 || cfg.Auths["ghcr.io"] == nil {
		t.Fatalf("config.json = %s (%v)", b, err)
	}
	if _, err := os.Stat(cfgDir); !os.IsNotExist(err) {
		t.Fatal("the temporary DOCKER_CONFIG must be removed after the command")
	}
	// The shared config folder never got the login.
	shared := filepath.Join(r.WorkDir, "docker")
	filepath.Walk(shared, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			if b, _ := os.ReadFile(p); bytes.Contains(b, []byte("octo")) {
				t.Fatalf("%s holds the login", p)
			}
		}
		return nil
	})
	// Without logins: the shared folder, as before.
	os.Remove(snap + ".dir")
	if err := r.Run(ctx, wire.StackCommand{Action: wire.StackDeploy, Name: "web", Compose: goodCompose, Pull: true}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if dir, _ := os.ReadFile(snap + ".dir"); strings.TrimSpace(string(dir)) != shared {
		t.Fatalf("DOCKER_CONFIG = %q, want %q", dir, shared)
	}
}

// opDocker fails every operation with a message that quotes the login,
// the way a careless error could.
type opDocker struct{ Docker }

func (opDocker) Operate(ctx context.Context, op wire.Operation) (any, error) {
	return nil, errors.New("pull failed for " + op.Auth[0].Username + " with password " + op.Auth[0].Password)
}

// The agent scrubs logins from operation errors before they reach the hub
// (and its own log).
func TestOperationErrorsAreScrubbed(t *testing.T) {
	var logBuf bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &session{
		a:   &Agent{cfg: Config{Docker: opDocker{}}, log: slog.New(slog.NewTextHandler(&logBuf, nil))},
		ctx: ctx, cancel: cancel,
		out:  make(chan wire.AgentMessage, 4),
		snap: make(chan struct{}, 1),
		logs: map[string]context.CancelFunc{},
	}
	s.handle(wire.Command{ID: "c1", Op: &wire.Operation{Kind: wire.OpImagePull, Arg: "ghcr.io/org/app", Auth: []wire.RegistryAuth{ghcrLogin}}})
	select {
	case m := <-s.out:
		if m.Result == nil || m.Result.OK || strings.Contains(m.Result.Error, ghcrLogin.Password) || !strings.Contains(m.Result.Error, "octo") {
			t.Fatalf("result = %+v", m.Result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no result")
	}
	if strings.Contains(logBuf.String(), ghcrLogin.Password) {
		t.Fatalf("agent log holds the password:\n%s", logBuf.String())
	}
}
