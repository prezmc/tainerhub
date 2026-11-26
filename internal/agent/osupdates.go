package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/prezmc/tainerhub/internal/wire"
)

// OS package updates: what the host's package manager would upgrade, read
// through the same privileged helper as Docker upgrades. The check installs
// nothing: apt-get only refreshes its package lists and simulates the same
// upgrade osUpgradeCommand runs. Installing (OpHostOSUpgrade) is handed to
// the host's systemd, like Docker upgrades, and leaves its exit status and
// time in osUpgradeResult for the next check to report.

const (
	osUpgradeUnit   = "tainerhub-os-upgrade"
	osUpgradeLog    = "/var/log/tainerhub-os-upgrade.log"
	osUpgradeResult = "/var/lib/tainerhub-os-upgrade.result"
)

const osStatusScript = `set +e
if [ -r /etc/os-release ]; then . /etc/os-release; echo "os=$PRETTY_NAME"; fi
echo "kernel=$(uname -r)"
if command -v systemd-run >/dev/null 2>&1; then echo systemd=yes; fi
if [ -f ` + osUpgradeResult + ` ]; then echo "last_upgrade=$(head -1 ` + osUpgradeResult + `)"; fi
if [ -f ` + osUpgradeLog + ` ]; then echo "last_upgrade_log=$(grep -E '^(E:|W:|Error|error:)' ` + osUpgradeLog + ` | tail -3 | tr '\n' ' ')"; fi
if systemctl is-active --quiet ` + osUpgradeUnit + ` 2>/dev/null; then echo upgrade_running=yes; exit 0; fi
if [ -f /var/run/reboot-required ]; then echo reboot=yes; fi
if [ -f /var/run/reboot-required.pkgs ]; then echo "reboot_pkgs=$(sort -u /var/run/reboot-required.pkgs | tr '\n' ' ')"; fi
if command -v apt-get >/dev/null 2>&1 && command -v dpkg-query >/dev/null 2>&1; then
  echo manager=apt
  err=$(apt-get update -q 2>&1 >/dev/null); rc=$?
  msg=$(printf '%s\n' "$err" | grep -E '^(E:|W: (Failed|Some index))' | head -3 | tr '\n' ' ')
  if [ $rc -ne 0 ] || [ -n "$msg" ]; then echo refresh_failed=yes; echo "refresh_error=$msg"; fi
  LC_ALL=C apt-get -s -q upgrade --with-new-pkgs 2>/dev/null | grep '^Inst ' | head -500 | sed 's/^Inst /inst=/'
  exit 0
fi
pm=
if command -v dnf >/dev/null 2>&1; then pm=dnf; elif command -v yum >/dev/null 2>&1; then pm=yum; fi
if [ -n "$pm" ]; then
  echo manager=$pm
  if [ $pm = dnf ]; then out=$(dnf -q --refresh check-update 2>/dev/null); rc=$?; else out=$(yum -q check-update 2>/dev/null); rc=$?; fi
  if [ $rc -ne 0 ] && [ $rc -ne 100 ]; then echo refresh_failed=yes; echo "refresh_error=$pm check-update failed (exit $rc)"; fi
  printf '%s\n' "$out" | awk 'NF==3 && $1 ~ /\./ {print "rpm=" $1 " " $2 " " $3}' | head -500
  $pm -q updateinfo list --security 2>/dev/null | awk 'NF>=3 {print "sec=" $NF}' | head -500
  if command -v needs-restarting >/dev/null 2>&1; then needs-restarting -r >/dev/null 2>&1; [ $? -eq 1 ] && echo reboot=yes; fi
  exit 0
fi
echo manager=unknown
`

func (d *SocketDocker) osStatus(ctx context.Context) (wire.HostOSStatus, error) {
	out, err := d.runOnHost(ctx, osStatusScript)
	if err != nil {
		return wire.HostOSStatus{}, err
	}
	return parseOSStatus(out), nil
}

// parseOSStatus reads the script's key=value lines.
func parseOSStatus(out string) wire.HostOSStatus {
	s := wire.HostOSStatus{Packages: []wire.OSPackage{}}
	secRPM := map[string]bool{}
	var lastLog string
	for _, l := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(l), "=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch k {
		case "os":
			s.OS = v
		case "kernel":
			s.Kernel = v
		case "manager":
			s.Manager = v
		case "reboot":
			s.RebootRequired = v == "yes"
		case "reboot_pkgs":
			s.RebootPackages = strings.Fields(v)
		case "refresh_failed":
			s.RefreshFailed = v == "yes"
		case "refresh_error":
			s.RefreshError = v
		case "inst":
			if p, ok := parseAptInst(v); ok {
				s.Packages = append(s.Packages, p)
			}
		case "rpm":
			if f := strings.Fields(v); len(f) == 3 {
				s.Packages = append(s.Packages, wire.OSPackage{Name: f[0], To: f[1], Origin: f[2]})
			}
		case "sec":
			secRPM[v] = true
		case "systemd":
			s.Systemd = v == "yes"
		case "upgrade_running":
			s.UpgradeRunning = v == "yes"
		case "last_upgrade":
			var code int
			var at int64
			if n, _ := fmt.Sscanf(v, "%d %d", &code, &at); n == 2 {
				if s.LastUpgrade == nil {
					s.LastUpgrade = &wire.OSUpgradeResult{}
				}
				s.LastUpgrade.Exit, s.LastUpgrade.At = code, at
			}
		case "last_upgrade_log":
			lastLog = v
		}
	}
	if s.Manager == "" {
		s.Manager = "unknown"
	}
	if s.LastUpgrade != nil && s.LastUpgrade.Exit != 0 {
		s.LastUpgrade.Error = lastLog
	}
	s.CanInstall = s.Systemd && osUpgradeCommand(s.Manager) != ""
	for _, p := range s.Packages {
		if restartsDocker(p.Name) {
			s.RestartsDocker = true
		}
	}
	for i, p := range s.Packages {
		if p.Security || s.Manager == "apt" {
			continue
		}
		// dnf names advisories by package-version-release.arch; check-update
		// by name.arch and version-release.
		name, arch, _ := strings.Cut(p.Name, ".")
		if secRPM[name+"-"+p.To+"."+arch] {
			s.Packages[i].Security = true
		}
	}
	for _, p := range s.Packages {
		if p.Security {
			s.Security++
		}
	}
	return s
}

// parseAptInst reads a simulated apt line:
//
//	docker-ce [5:29.10.0-1~debian.12~bookworm] (5:29.10.1-1~debian.12~bookworm Docker CE:bookworm [amd64])
//
// A package that isn't installed yet (a new kernel) has no [from].
func parseAptInst(v string) (wire.OSPackage, bool) {
	name, rest, _ := strings.Cut(v, " ")
	if name == "" {
		return wire.OSPackage{}, false
	}
	p := wire.OSPackage{Name: name}
	rest = strings.TrimSpace(rest)
	if strings.HasPrefix(rest, "[") {
		if from, after, ok := strings.Cut(rest[1:], "]"); ok {
			p.From, rest = from, strings.TrimSpace(after)
		}
	}
	if strings.HasPrefix(rest, "(") {
		inner := strings.TrimSuffix(strings.TrimPrefix(rest, "("), ")")
		to, origin, _ := strings.Cut(inner, " ")
		p.To = to
		if i := strings.LastIndex(origin, " ["); i >= 0 {
			origin = origin[:i]
		}
		p.Origin = strings.TrimSpace(origin)
	}
	p.Security = strings.Contains(strings.ToLower(p.Origin), "security")
	return p, true
}

// osUpgradeCommand installs what the check listed. Config files the admin
// changed are kept (confold); needrestart only lists services instead of
// restarting them, so Docker isn't restarted for a library update. If the
// updates include Docker itself, its packages restart it as usual.
func osUpgradeCommand(manager string) string {
	switch manager {
	case "apt":
		return "export DEBIAN_FRONTEND=noninteractive NEEDRESTART_MODE=l; apt-get update -q && apt-get -y -q -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold upgrade --with-new-pkgs" + restartIfStale
	case "dnf":
		return "dnf -y upgrade" + restartIfStale
	case "yum":
		return "yum -y update" + restartIfStale
	}
	return ""
}

// restartsDocker: upgrading this package restarts the Docker daemon, and
// with it every container (apt "docker-ce", rpm "containerd.io.x86_64").
func restartsDocker(pkg string) bool {
	name := pkg
	for _, arch := range []string{".x86_64", ".aarch64", ".noarch", ".armv7hl", ".ppc64le", ".s390x"} {
		name = strings.TrimSuffix(name, arch)
	}
	name, _, _ = strings.Cut(name, ":") // apt multi-arch: containerd.io:amd64
	switch name {
	case "docker-ce", "docker.io", "docker-engine", "moby-engine", "containerd.io", "containerd", "moby-containerd", "runc":
		return true
	}
	return false
}

// osUpgrade checks again and starts installing every pending update as a
// transient systemd unit on the host, returning what it started. arg is
// wire.OSUpgradeRestartAll when the admin accepted that Docker restarts.
func (d *SocketDocker) osUpgrade(ctx context.Context, arg string) (wire.HostOSStatus, error) {
	s, err := d.osStatus(ctx)
	if err != nil {
		return s, err
	}
	switch {
	case s.UpgradeRunning:
		return s, errors.New("updates are already being installed on this server")
	case !s.CanInstall:
		if !s.Systemd {
			return s, errors.New("this server doesn't use systemd, so TainerHub can't install updates safely; run your package manager on the server")
		}
		return s, errors.New("TainerHub can't tell how to install updates on this server (no apt, dnf or yum)")
	case s.RefreshFailed:
		return s, errors.New("the server couldn't refresh its package lists: " + s.RefreshError)
	case len(s.Packages) == 0:
		return s, errors.New("there's nothing to install: the server is up to date")
	case s.RestartsDocker && arg != wire.OSUpgradeRestartAll:
		return s, errors.New("these updates include Docker, which restarts every container: confirm that to go ahead")
	}
	// The command is fixed per manager (no user input reaches the host).
	script := "{ " + osUpgradeCommand(s.Manager) + "; } >" + osUpgradeLog + " 2>&1; echo \"$? $(date +%s)\" >" + osUpgradeResult
	launch := "systemd-run --unit=" + osUpgradeUnit + " --collect --description='TainerHub: install OS updates' /bin/sh -c '" +
		strings.ReplaceAll(script, "'", `'"'"'`) + "'"
	if _, err := d.runOnHost(ctx, launch); err != nil {
		if strings.Contains(err.Error(), "already") {
			return s, errors.New("updates are already being installed on this server")
		}
		return s, err
	}
	s.UpgradeRunning = true
	return s, nil
}
