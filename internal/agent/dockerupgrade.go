package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/prezmc/tainerhub/internal/wire"
)

// Upgrading Docker itself. The agent only has the Docker socket, so it
// reaches the host through a short-lived privileged helper that enters the
// host's namespaces (nsenter into PID 1) and runs the package manager there.
//
// The upgrade restarts dockerd, which would kill a helper container halfway
// through a package install. So the helper only hands the job to the host's
// systemd (systemd-run) and exits; the upgrade runs outside Docker, logging to
// /var/log/tainerhub-docker-upgrade.log, and the agent reconnects afterwards.

const helperImage = "alpine:3" // has busybox nsenter and sh

// Packages upgraded together (only those already installed are touched).
const dockerPkgs = "docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin docker-ce-rootless-extras"

const planScript = `set +e
if command -v systemd-run >/dev/null 2>&1; then echo systemd=yes; else echo systemd=no; fi
if command -v snap >/dev/null 2>&1 && snap list docker >/dev/null 2>&1; then echo manager=snap; exit 0; fi
if command -v dpkg-query >/dev/null 2>&1; then
  inst=$(dpkg-query -W -f='${Version}' docker-ce 2>/dev/null)
  if [ -z "$inst" ]; then
    if dpkg-query -W -f='${Status}' docker.io 2>/dev/null | grep -q 'ok installed'; then
      echo manager=apt-docker.io; echo installed=$(dpkg-query -W -f='${Version}' docker.io); exit 0
    fi
    echo manager=unknown; exit 0
  fi
  echo manager=apt; echo installed=$inst
  apt-get update -qq >/dev/null 2>&1 || echo refresh_failed=yes
  echo candidate=$(apt-cache policy docker-ce 2>/dev/null | awk '/Candidate:/{print $2}')
  exit 0
fi
if command -v rpm >/dev/null 2>&1; then
  if rpm -q docker-ce >/dev/null 2>&1; then
    echo manager=dnf; echo installed=$(rpm -q --qf '%{VERSION}-%{RELEASE}' docker-ce)
    if command -v dnf >/dev/null 2>&1; then c=$(dnf -q --refresh list --upgrades docker-ce 2>/dev/null | awk '/^docker-ce/{print $2}'); else c=$(yum -q list updates docker-ce 2>/dev/null | awk '/^docker-ce/{print $2}'); fi
    echo candidate=$c
    exit 0
  fi
  if rpm -q moby-engine >/dev/null 2>&1 || rpm -q docker >/dev/null 2>&1; then echo manager=rpm-distro; exit 0; fi
fi
echo manager=unknown
`

// restartIfStale restarts Docker when the daemon still runs the old version
// after the packages were upgraded (dnf doesn't restart it; some systems
// block service restarts during package installs).
const restartIfStale = ` && { cv=$(docker version -f '{{.Client.Version}}' 2>/dev/null); sv=$(docker version -f '{{.Server.Version}}' 2>/dev/null); if [ -n "$cv" ] && [ "$sv" != "$cv" ]; then echo "restarting docker ($sv -> $cv)"; systemctl daemon-reload; systemctl restart docker; fi; }`

func upgradeCommand(manager string) string {
	switch manager {
	case "apt":
		return "export DEBIAN_FRONTEND=noninteractive; apt-get update -qq && apt-get install -y -q --only-upgrade -o Dpkg::Options::=--force-confold " + dockerPkgs + restartIfStale
	case "dnf":
		return "{ if command -v dnf >/dev/null 2>&1; then dnf -y upgrade " + dockerPkgs + "; else yum -y update " + dockerPkgs + "; fi; }" + restartIfStale
	}
	return ""
}

// parsePlan turns the plan script's key=value lines into a DockerPlan.
func parsePlan(out string) wire.DockerPlan {
	kv := map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(l), "="); ok {
			kv[k] = strings.TrimSpace(v)
		}
	}
	p := wire.DockerPlan{Manager: kv["manager"], Installed: kv["installed"], Candidate: kv["candidate"], Systemd: kv["systemd"] == "yes"}
	if p.Manager == "" {
		p.Manager = "unknown"
	}
	if p.Manager == "dnf" && p.Candidate == "" {
		p.Candidate = p.Installed // dnf lists nothing when it's up to date
	}
	p.Available = p.Candidate != "" && p.Candidate != "(none)" && p.Candidate != p.Installed
	switch p.Manager {
	case "apt", "dnf":
		p.Command = upgradeCommand(p.Manager)
		if !p.Systemd {
			p.Reason = "this server doesn't use systemd, so TainerHub can't run the upgrade safely; run the command below on the server"
		} else {
			p.Supported = true
		}
		if kv["refresh_failed"] == "yes" {
			p.Reason = "couldn't refresh the package lists (apt-get update failed); the available version may be out of date"
		}
	case "snap":
		p.Reason = "Docker was installed with Snap, which updates it on its own schedule"
		p.Command = "sudo snap refresh docker"
	case "apt-docker.io":
		p.Reason = "Docker here is your distribution's docker.io package, not Docker's own; it updates with your normal system updates"
		p.Command = "sudo apt-get update && sudo apt-get install --only-upgrade docker.io"
	case "rpm-distro":
		p.Reason = "Docker here is your distribution's package, not Docker's own; it updates with your normal system updates"
		p.Command = "sudo dnf upgrade"
	default:
		p.Reason = "couldn't tell how Docker was installed on this server (Docker Desktop, a static binary, or an unusual distribution)"
	}
	return p
}

// runOnHost runs a shell script on the host itself (not in a container)
// through a privileged helper, and returns its output.
func (d *SocketDocker) runOnHost(ctx context.Context, script string) (string, error) {
	if err := d.ensureImage(ctx, helperImage); err != nil {
		return "", err
	}
	name := fmt.Sprintf("tainerhub-host-helper-%d", time.Now().UnixNano())
	resp, err := d.doBody(ctx, http.MethodPost, "/containers/create", url.Values{"name": {name}}, map[string]any{
		"Image":  helperImage,
		"Cmd":    []string{"nsenter", "-t", "1", "-m", "-u", "-n", "-i", "--", "/bin/sh", "-c", script},
		"Labels": map[string]string{"tainerhub.helper": "host"},
		"HostConfig": map[string]any{
			"Privileged": true,
			"PidMode":    "host",
		},
	})
	if err != nil {
		return "", fmt.Errorf("start the host helper: %w", err)
	}
	var created struct {
		ID string `json:"Id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = d.remove(cctx, "/containers/"+url.PathEscape(created.ID)+"?force=1")
	}()
	if err := d.post(ctx, "/containers/"+url.PathEscape(created.ID)+"/start", nil); err != nil {
		return "", fmt.Errorf("start the host helper: %w", err)
	}
	wresp, err := d.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(created.ID)+"/wait", nil)
	if err != nil {
		return "", err
	}
	var w struct{ StatusCode int }
	_ = json.NewDecoder(wresp.Body).Decode(&w)
	wresp.Body.Close()
	lresp, err := d.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(created.ID)+"/logs", url.Values{"stdout": {"1"}, "stderr": {"1"}})
	if err != nil {
		return "", err
	}
	defer lresp.Body.Close()
	var out bytes.Buffer
	_ = demux(lresp.Body, &out)
	if w.StatusCode != 0 {
		return out.String(), fmt.Errorf("host command failed (exit %d): %s", w.StatusCode, lastLine(out.String()))
	}
	return out.String(), nil
}

func (d *SocketDocker) ensureImage(ctx context.Context, ref string) error {
	var img struct {
		ID string `json:"Id"`
	}
	if err := d.getJSON(ctx, "/images/"+url.PathEscape(ref)+"/json", nil, &img); err == nil {
		return nil
	}
	_, err := d.pull(ctx, ref)
	return err
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

func (d *SocketDocker) dockerPlan(ctx context.Context) (wire.DockerPlan, error) {
	out, err := d.runOnHost(ctx, planScript)
	if err != nil {
		return wire.DockerPlan{}, err
	}
	return parsePlan(out), nil
}

// dockerUpgrade checks the plan again and, if supported, starts the upgrade
// as a transient systemd unit on the host.
func (d *SocketDocker) dockerUpgrade(ctx context.Context) (wire.DockerPlan, error) {
	p, err := d.dockerPlan(ctx)
	if err != nil {
		return p, err
	}
	if !p.Supported {
		return p, errors.New(p.Reason)
	}
	if !p.Available {
		return p, errors.New("Docker is already the newest version this server's repository offers")
	}
	// The command is fixed per manager (no user input reaches the host).
	launch := "systemd-run --unit=tainerhub-docker-upgrade --collect --description='TainerHub: upgrade Docker' /bin/sh -c '" +
		strings.ReplaceAll(p.Command, "'", `'"'"'`) + " >/var/log/tainerhub-docker-upgrade.log 2>&1'"
	if _, err := d.runOnHost(ctx, launch); err != nil {
		if strings.Contains(err.Error(), "already") {
			return p, errors.New("a Docker upgrade is already running on this server")
		}
		return p, err
	}
	return p, nil
}
