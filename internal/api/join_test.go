package api

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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
