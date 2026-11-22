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
