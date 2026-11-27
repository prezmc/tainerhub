package agent

import (
	"strings"
	"testing"

	"github.com/prezmc/tainerhub/internal/wire"
)

func TestShareToolsScript(t *testing.T) {
	s := shareToolsScript(shareToolPkgs[wire.ShareNFS][0], shareToolPkgs[wire.ShareNFS][1])
	for _, want := range []string{"apt-get install -y -q -o DPkg::Lock::Timeout=120 nfs-common", "$pm install -y -q nfs-utils", "apk add -q nfs-utils"} {
		if !strings.Contains(s, want) {
			t.Errorf("script lacks %q", want)
		}
	}
	r := parseShareTools("manager=apt\npackage=cifs-utils\nalready=yes\nReading package lists...\n")
	if r != (wire.ShareTools{Manager: "apt", Package: "cifs-utils", Already: true}) {
		t.Fatalf("%+v", r)
	}
	if err := wire.ValidOperation(wire.Operation{Kind: wire.OpHostInstallShareTools, Arg: "cifs; rm -rf /"}); err == nil {
		t.Fatal("bad arg accepted")
	}
}
