package regcred

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/prezmc/tainerhub/internal/wire"
)

// dockerHubServer is the key Docker itself files Docker Hub logins under.
const dockerHubServer = "https://index.docker.io/v1/"

// ServerAddress is the name Docker expects for a registry in logins.
func ServerAddress(registry string) string {
	if registry == DockerHub {
		return dockerHubServer
	}
	return registry
}

// ForImage returns the login for the registry ref pulls from, if any.
func ForImage(auths []wire.RegistryAuth, ref string) *wire.RegistryAuth {
	h := ImageHost(ref)
	for i := range auths {
		if auths[i].Registry == h {
			return &auths[i]
		}
	}
	return nil
}

// XRegistryAuth is the X-Registry-Auth header value the Docker Engine API
// takes for pulls and registry lookups: base64url-encoded JSON.
func XRegistryAuth(a wire.RegistryAuth) string {
	b, _ := json.Marshal(map[string]string{
		"username":      a.Username,
		"password":      a.Password,
		"serveraddress": ServerAddress(a.Registry),
	})
	return base64.URLEncoding.EncodeToString(b)
}

func basicAuth(a wire.RegistryAuth) string {
	return base64.StdEncoding.EncodeToString([]byte(a.Username + ":" + a.Password))
}

// DockerConfigJSON is a Docker client config.json holding just these logins.
func DockerConfigJSON(auths []wire.RegistryAuth) []byte {
	m := map[string]map[string]string{}
	for _, a := range auths {
		m[ServerAddress(a.Registry)] = map[string]string{"auth": basicAuth(a)}
	}
	b, _ := json.Marshal(map[string]any{"auths": m})
	return b
}

// TempDockerConfig writes a fresh DOCKER_CONFIG folder (0700) under parent
// holding a config.json (0600) with only these logins. Call cleanup as soon
// as the one command that needs it has finished.
func TempDockerConfig(parent string, auths []wire.RegistryAuth) (dir string, cleanup func(), err error) {
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return "", nil, err
	}
	dir, err = os.MkdirTemp(parent, "auth-")
	if err != nil {
		return "", nil, err
	}
	cleanup = func() { _ = os.RemoveAll(dir) }
	if err := os.Chmod(dir, 0o700); err != nil {
		cleanup()
		return "", nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), DockerConfigJSON(auths), 0o600); err != nil {
		cleanup()
		return "", nil, err
	}
	return dir, cleanup, nil
}

// Redacted replaces secrets in messages.
const Redacted = "[redacted]"

// secretsOf lists every form a login's secret could show up in.
func secretsOf(auths []wire.RegistryAuth) []string {
	var out []string
	for _, a := range auths {
		if a.Password == "" {
			continue
		}
		out = append(out, a.Password, basicAuth(a), XRegistryAuth(a))
		if b, err := json.Marshal(a.Password); err == nil {
			out = append(out, strings.Trim(string(b), `"`)) // as escaped inside JSON
		}
	}
	return out
}

// Scrub removes the logins' passwords (and their encoded forms) from s, for
// error messages and output that end up in logs, job output or the audit log.
func Scrub(s string, auths []wire.RegistryAuth) string {
	for _, sec := range secretsOf(auths) {
		if sec != "" {
			s = strings.ReplaceAll(s, sec, Redacted)
		}
	}
	return s
}

// ScrubWriter scrubs secrets from output on its way to w. It holds back a
// line until it's complete, so a secret split across two writes is still
// caught (output without newlines is passed on at Flush).
type ScrubWriter struct {
	mu    sync.Mutex
	w     io.Writer
	auths []wire.RegistryAuth
	buf   []byte
}

func NewScrubWriter(w io.Writer, auths []wire.RegistryAuth) *ScrubWriter {
	return &ScrubWriter{w: w, auths: auths}
}

func (s *ScrubWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf = append(s.buf, p...)
	i := strings.LastIndexByte(string(s.buf), '\n')
	if i < 0 && len(s.buf) < 64<<10 {
		return len(p), nil
	}
	if i < 0 {
		i = len(s.buf) - 1
	}
	line := s.buf[:i+1]
	_, err := io.WriteString(s.w, Scrub(string(line), s.auths))
	s.buf = append(s.buf[:0], s.buf[i+1:]...)
	return len(p), err
}

// Flush writes out whatever is held back.
func (s *ScrubWriter) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.buf) == 0 {
		return nil
	}
	_, err := io.WriteString(s.w, Scrub(string(s.buf), s.auths))
	s.buf = s.buf[:0]
	return err
}
