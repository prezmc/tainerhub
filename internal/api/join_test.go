package api

import (
	"log/slog"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prezmc/tainerhub/internal/auth"
)

func TestJoinScriptIsValidShell(t *testing.T) {
	compose := agentCompose("192.168.1.10:7443", "thj1_abc")
	script := renderJoinScript(`Server "two" $(rm -rf /)`, "192.168.1.10:7443", "192.168.1.10", compose)
	if strings.Contains(script, "$(rm") || strings.Contains(script, `"two"`) {
		t.Fatal("host name not sanitised in the script")
	}
	if !strings.Contains(script, compose+"TAINERHUB_COMPOSE_EOF") {
		t.Fatal("compose file not embedded verbatim before the heredoc end")
	}
	f := filepath.Join(t.TempDir(), "join.sh")
	if err := os.WriteFile(f, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("sh", "-n", f).CombinedOutput(); err != nil {
		t.Fatalf("sh -n: %v %s", err, out)
	}
}

func TestDirectHubURL(t *testing.T) {
	for _, c := range []struct{ addr, port, want string }{
		{"192.168.1.10:7443", "8585", "http://192.168.1.10:8585"},
		{"hub.lan:7443", "", "http://hub.lan:8585"},
		{"[fd00::10]:7443", "9000", "http://[fd00::10]:9000"},
		{"garbage", "8585", ""},
	} {
		if got := directHubURL(c.addr, c.port); got != c.want {
			t.Errorf("directHubURL(%q, %q) = %q, want %q", c.addr, c.port, got, c.want)
		}
	}
}

func TestAgentAddrNeverTheProxy(t *testing.T) {
	pub, _ := url.Parse("https://tainerhub.example.com")
	a, err := auth.New(nil, pub, []netip.Prefix{netip.MustParsePrefix("192.168.1.252/32"), netip.MustParsePrefix("10.0.0.0/8")}, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Auth: a}
	for _, c := range []struct{ host, bad string }{
		{"tainerhub.example.com", "reverse proxy serves"},
		{"TainerHub.Example.com", "reverse proxy serves"},
		{"192.168.1.252", "trusted proxies"},
		{"192.168.1.10", ""},
		{"10.1.2.3", ""}, // a broad trusted range isn't "the proxy"
		{"hub.lan", ""},
	} {
		got := s.proxyAddrProblem(c.host)
		if (c.bad == "") != (got == "") || !strings.Contains(got, c.bad) {
			t.Errorf("proxyAddrProblem(%q) = %q, want mention of %q", c.host, got, c.bad)
		}
	}
	// Opened through the proxy: no guess, unless an earlier address was kept.
	if g := s.guessAgentAddr("tainerhub.example.com", ""); g != "" {
		t.Errorf("guess through proxy = %q, want empty", g)
	}
	if g := s.guessAgentAddr("tainerhub.example.com", "192.168.1.10:7443"); g != "192.168.1.10:7443" {
		t.Errorf("guess with stored = %q", g)
	}
	if g := s.guessAgentAddr("tainerhub.example.com", "tainerhub.example.com:7443"); g != "" {
		t.Errorf("a stored proxy name must not be suggested, got %q", g)
	}
	// Opened by LAN IP: that's the guess.
	if g := s.guessAgentAddr("192.168.1.10:8585", ""); g != "192.168.1.10:7443" {
		t.Errorf("guess by LAN IP = %q", g)
	}
	s.AgentAddr = "hub.lan:7443"
	if g := s.guessAgentAddr("tainerhub.example.com", ""); g != "hub.lan:7443" {
		t.Errorf("TAINERHUB_AGENT_ADDR must win, got %q", g)
	}
	r := httptest.NewRequest("POST", "https://tainerhub.example.com/api/hosts", nil)
	if _, err := s.agentAddrFor(r, "tainerhub.example.com:7443"); err == nil || !strings.Contains(err.Error(), "LAN IP") {
		t.Errorf("agentAddrFor accepted the proxy name: %v", err)
	}
	if addr, err := s.agentAddrFor(r, "192.168.1.10:7443"); err != nil || addr != "192.168.1.10:7443" {
		t.Errorf("agentAddrFor(LAN IP) = %q, %v", addr, err)
	}
}
