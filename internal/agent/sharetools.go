package agent

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/prezmc/tainerhub/internal/wire"
)

// Installing the host's tools for mounting network shares
// (OpHostInstallShareTools), through the same privileged host helper as OS
// updates. Unlike those it doesn't restart Docker, so it runs to the end
// here instead of being handed to systemd. Only the package names below
// ever reach the host's shell.

// shareToolPkgs are the package names per type: apt, then rpm and apk.
var shareToolPkgs = map[string][2]string{
	wire.ShareCIFS: {"cifs-utils", "cifs-utils"},
	wire.ShareNFS:  {"nfs-common", "nfs-utils"},
}

func shareToolsScript(apt, other string) string {
	return `set -e
if command -v apt-get >/dev/null 2>&1; then
  echo manager=apt; echo package=` + apt + `
  if dpkg-query -W -f='${Status}' ` + apt + ` 2>/dev/null | grep -q 'install ok installed'; then echo already=yes; exit 0; fi
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -q -o DPkg::Lock::Timeout=120 >/dev/null 2>&1 || true
  apt-get install -y -q -o DPkg::Lock::Timeout=120 ` + apt + `
  exit 0
fi
for pm in dnf yum; do
  if command -v $pm >/dev/null 2>&1; then
    echo manager=$pm; echo package=` + other + `
    if rpm -q ` + other + ` >/dev/null 2>&1; then echo already=yes; exit 0; fi
    $pm install -y -q ` + other + `
    exit 0
  fi
done
if command -v apk >/dev/null 2>&1; then
  echo manager=apk; echo package=` + other + `
  if apk info -e ` + other + ` >/dev/null 2>&1; then echo already=yes; exit 0; fi
  apk add -q ` + other + `
  exit 0
fi
echo "this host has no apt-get, dnf, yum or apk: install ` + apt + ` (or ` + other + `) yourself" >&2
exit 3
`
}

func (d *SocketDocker) installShareTools(ctx context.Context, typ string) (wire.ShareTools, error) {
	pkgs, ok := shareToolPkgs[typ]
	if !ok {
		return wire.ShareTools{}, errors.New("install tools for cifs or nfs")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	out, err := d.runOnHost(ctx, shareToolsScript(pkgs[0], pkgs[1]))
	res := parseShareTools(out)
	if err != nil {
		return res, err
	}
	return res, nil
}

func parseShareTools(out string) wire.ShareTools {
	var r wire.ShareTools
	for _, l := range strings.Split(out, "\n") {
		k, v, _ := strings.Cut(strings.TrimSpace(l), "=")
		switch k {
		case "manager":
			r.Manager = v
		case "package":
			r.Package = v
		case "already":
			r.Already = v == "yes"
		}
	}
	return r
}
