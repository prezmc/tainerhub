package agent

import (
	"context"
	"strings"

	"github.com/prezmc/tainerhub/internal/wire"
)

// OS package updates: what the host's package manager would upgrade, read
// through the same privileged helper as Docker upgrades. Nothing is
// installed: apt-get only refreshes its package lists and simulates.

const osStatusScript = `set +e
if [ -r /etc/os-release ]; then . /etc/os-release; echo "os=$PRETTY_NAME"; fi
echo "kernel=$(uname -r)"
if [ -f /var/run/reboot-required ]; then echo reboot=yes; fi
if [ -f /var/run/reboot-required.pkgs ]; then echo "reboot_pkgs=$(sort -u /var/run/reboot-required.pkgs | tr '\n' ' ')"; fi
if command -v apt-get >/dev/null 2>&1 && command -v dpkg-query >/dev/null 2>&1; then
  echo manager=apt
  err=$(apt-get update -q 2>&1 >/dev/null); rc=$?
  msg=$(printf '%s\n' "$err" | grep -E '^(E:|W: (Failed|Some index))' | head -3 | tr '\n' ' ')
  if [ $rc -ne 0 ] || [ -n "$msg" ]; then echo refresh_failed=yes; echo "refresh_error=$msg"; fi
  LC_ALL=C apt-get -s -q dist-upgrade 2>/dev/null | grep '^Inst ' | head -500 | sed 's/^Inst /inst=/'
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
		}
	}
	if s.Manager == "" {
		s.Manager = "unknown"
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
