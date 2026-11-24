package regcred

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/prezmc/tainerhub/internal/wire"
)

func TestNormalizeHost(t *testing.T) {
	for in, want := range map[string]string{
		"ghcr.io":                                   "ghcr.io",
		"  GHCR.IO  ":                               "ghcr.io",
		"https://ghcr.io/":                          "ghcr.io",
		"https://ghcr.io/v2/org/app":                "ghcr.io",
		"registry.gitlab.com/group/proj":            "registry.gitlab.com",
		"docker.io":                                 "docker.io",
		"index.docker.io":                           "docker.io",
		"https://index.docker.io/v1/":               "docker.io",
		"registry-1.docker.io":                      "docker.io",
		"registry.hub.docker.com":                   "docker.io",
		"myreg.lan:5000":                            "myreg.lan:5000",
		"http://MyReg.lan:5000/":                    "myreg.lan:5000",
		"registry.example.com:443":                  "registry.example.com",
		"localhost:5000":                            "localhost:5000",
		"192.168.1.20:5000":                         "192.168.1.20:5000",
		"123456789.dkr.ecr.eu-west-1.amazonaws.com": "123456789.dkr.ecr.eu-west-1.amazonaws.com",
	} {
		got, err := NormalizeHost(in)
		if err != nil || got != want {
			t.Errorf("NormalizeHost(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "   ", "https://", "user@ghcr.io", "my reg", "reg.lan:0", "reg.lan:70000", "reg.lan:abc", "-reg.lan", "reg..lan", "reg_lan", "reg.lan:05000"} {
		if got, err := NormalizeHost(bad); err == nil {
			t.Errorf("NormalizeHost(%q) = %q, want an error", bad, got)
		}
	}
}

func TestImageHost(t *testing.T) {
	for ref, want := range map[string]string{
		"nginx":                                                "docker.io",
		"nginx:1.27":                                           "docker.io",
		"library/nginx":                                        "docker.io",
		"library/nginx:latest":                                 "docker.io",
		"bitnami/redis:7":                                      "docker.io",
		"docker.io/library/nginx":                              "docker.io",
		"index.docker.io/me/app:1":                             "docker.io",
		"registry-1.docker.io/me/app":                          "docker.io",
		"ghcr.io/org/app:v1":                                   "ghcr.io",
		"GHCR.io/Org/App":                                      "ghcr.io",
		"registry.gitlab.com/group/sub/app:2":                  "registry.gitlab.com",
		"myreg.lan:5000/app":                                   "myreg.lan:5000",
		"myreg.lan:5000/team/app:1.0":                          "myreg.lan:5000",
		"localhost/app":                                        "localhost",
		"localhost:5000/app":                                   "localhost:5000",
		"nginx@sha256:" + strings.Repeat("a", 64):              "docker.io",
		"ghcr.io/org/app@sha256:" + strings.Repeat("b", 64):    "ghcr.io",
		"ghcr.io/org/app:v1@sha256:" + strings.Repeat("c", 64): "ghcr.io",
		"myreg.lan:5000/app@sha256:" + strings.Repeat("d", 64): "myreg.lan:5000",
		"me/app.with.dots":                                     "docker.io", // dots after the first part don't count
	} {
		if got := ImageHost(ref); got != want {
			t.Errorf("ImageHost(%q) = %q, want %q", ref, got, want)
		}
	}
}

func TestComposeImages(t *testing.T) {
	compose := `services:
  web:
    image: ghcr.io/org/web:1.2   # the site
  db:
    image: "postgres:16"
  cache:
    image: '${REGISTRY}/cache:${TAG:-latest}'
  worker:
    image: ${UNSET_REGISTRY}/worker
  fallback:
    image: ${MISSING:-registry.gitlab.com/g/fallback}:1
  built:
    build: .
`
	env := "# comment\nREGISTRY=myreg.lan:5000\nexport TAG=\"3\"\n"
	got := ComposeImages(compose, env)
	want := []string{"ghcr.io/org/web:1.2", "postgres:16", "myreg.lan:5000/cache:3", "registry.gitlab.com/g/fallback:1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ComposeImages = %q, want %q", got, want)
	}
	if hs := Hosts(got); !reflect.DeepEqual(hs, []string{"docker.io", "ghcr.io", "myreg.lan:5000", "registry.gitlab.com"}) {
		t.Fatalf("Hosts = %q", hs)
	}
	if got := ComposeImages("not: [valid yaml", ""); len(got) != 0 {
		t.Fatalf("no image lines should give nothing, got %q", got)
	}
	if hs := Hosts([]string{"sha256:" + strings.Repeat("a", 64), ""}); len(hs) != 0 {
		t.Fatalf("image IDs pull nothing, got %q", hs)
	}
}

func TestSealRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", KeyFile)
	key, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode = %v, %v; want 0600", st.Mode().Perm(), err)
	}
	again, err := LoadOrCreateKey(path)
	if err != nil || !bytes.Equal(key, again) {
		t.Fatal("loading the key again must return the same key")
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".*")); len(left) != 0 {
		t.Fatalf("temporary files left behind: %v", left)
	}

	s, _ := NewSealer(key)
	sealed, err := s.Seal([]byte("hunter2-token"), "ghcr.io")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("hunter2")) {
		t.Fatal("sealed value contains the plaintext")
	}
	if plain, err := s.Open(sealed, "ghcr.io"); err != nil || string(plain) != "hunter2-token" {
		t.Fatalf("Open = %q, %v", plain, err)
	}
	if _, err := s.Open(sealed, "docker.io"); err == nil {
		t.Fatal("a secret moved to another registry must not decrypt")
	}
	other, _ := NewSealer(bytes.Repeat([]byte{7}, 32))
	if _, err := other.Open(sealed, "ghcr.io"); err != ErrUnseal {
		t.Fatalf("wrong key: err = %v, want ErrUnseal", err)
	}
	sealed[len(sealed)-1] ^= 1
	if _, err := s.Open(sealed, "ghcr.io"); err != ErrUnseal {
		t.Fatal("a damaged value must not decrypt")
	}
	if _, err := s.Open(nil, "ghcr.io"); err != ErrUnseal {
		t.Fatal("an empty value must not decrypt")
	}
	if _, err := NewSealer([]byte("short")); err == nil {
		t.Fatal("short keys must be refused")
	}
}

func TestKeyFilePermsAndDamage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, KeyFile)
	if _, err := LoadOrCreateKey(path); err != nil {
		t.Fatal(err)
	}
	// A loosened key file is tightened again on load.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateKey(path); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", st.Mode().Perm())
	}
	// A damaged key is an error, never silently replaced.
	if err := os.WriteFile(path, []byte("garbage\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateKey(path); err == nil {
		t.Fatal("a damaged key file must be an error")
	}
	if b, _ := os.ReadFile(path); string(b) != "garbage\n" {
		t.Fatal("a damaged key file must not be overwritten")
	}
}

// Concurrent first uses all end up with the same key.
func TestKeyCreationRace(t *testing.T) {
	path := filepath.Join(t.TempDir(), KeyFile)
	keys := make([][]byte, 8)
	var wg sync.WaitGroup
	for i := range keys {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			k, err := LoadOrCreateKey(path)
			if err != nil {
				t.Error(err)
			}
			keys[i] = k
		}(i)
	}
	wg.Wait()
	for _, k := range keys[1:] {
		if !bytes.Equal(k, keys[0]) {
			t.Fatal("racing creators got different keys")
		}
	}
}

var testAuths = []wire.RegistryAuth{
	{Registry: "ghcr.io", Username: "octo", Password: "ghp_s3cr3t\"token"},
	{Registry: "docker.io", Username: "me", Password: "dckr_pat_xyz"},
}

func TestXRegistryAuth(t *testing.T) {
	h := XRegistryAuth(testAuths[0])
	if strings.ContainsAny(h, "+/") {
		t.Fatalf("header must be base64url: %q", h)
	}
	b, err := base64.URLEncoding.DecodeString(h)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"username": "octo", "password": "ghp_s3cr3t\"token", "serveraddress": "ghcr.io"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded %v, want %v", got, want)
	}
	if a := ForImage(testAuths, "nginx"); a == nil || ServerAddress(a.Registry) != "https://index.docker.io/v1/" {
		t.Fatalf("Docker Hub images use Docker's own server name, got %+v", a)
	}
	if a := ForImage(testAuths, "quay.io/x/y"); a != nil {
		t.Fatal("no login for quay.io")
	}
}

func TestTempDockerConfig(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "auth")
	dir, cleanup, err := TempDockerConfig(parent, testAuths[:1])
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(dir); st.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v, want 0700", st.Mode().Perm())
	}
	st, _ := os.Stat(filepath.Join(dir, "config.json"))
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("config.json mode %v, want 0600", st.Mode().Perm())
	}
	b, _ := os.ReadFile(filepath.Join(dir, "config.json"))
	var cfg struct {
		Auths map[string]struct{ Auth string }
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Auths) != 1 {
		t.Fatalf("auths = %v, want only ghcr.io", cfg.Auths)
	}
	dec, _ := base64.StdEncoding.DecodeString(cfg.Auths["ghcr.io"].Auth)
	if string(dec) != "octo:ghp_s3cr3t\"token" {
		t.Fatalf("auth = %q", dec)
	}
	cleanup()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("cleanup must remove the folder")
	}
}

func TestScrub(t *testing.T) {
	a := testAuths[0]
	msg := "pull failed: password " + a.Password + " basic " + basicAuth(a) + " hdr " + XRegistryAuth(a) + ` json "ghp_s3cr3t\"token"`
	got := Scrub(msg, testAuths)
	for _, sec := range []string{"ghp_s3cr3t", basicAuth(a), XRegistryAuth(a)} {
		if strings.Contains(got, sec) {
			t.Fatalf("scrubbed message still holds %q: %s", sec, got)
		}
	}
	if !strings.Contains(got, "pull failed") || !strings.Contains(got, Redacted) {
		t.Fatalf("scrubbed = %q", got)
	}
	if Scrub("nothing here", nil) != "nothing here" {
		t.Fatal("no logins: unchanged")
	}

	var out bytes.Buffer
	w := NewScrubWriter(&out, testAuths)
	// The secret is split across writes.
	w.Write([]byte("line one\nlogin with dckr_pa"))
	w.Write([]byte("t_xyz done\npartial"))
	w.Flush()
	if strings.Contains(out.String(), "dckr_pat_xyz") || out.String() != "line one\nlogin with "+Redacted+" done\npartial" {
		t.Fatalf("writer output %q", out.String())
	}
}

// Printing or logging a login never shows its password.
func TestRegistryAuthStringHidesPassword(t *testing.T) {
	a := testAuths[1]
	for _, s := range []string{a.String(), a.GoString(), a.LogValue().String()} {
		if strings.Contains(s, a.Password) {
			t.Fatalf("%q shows the password", s)
		}
	}
}
