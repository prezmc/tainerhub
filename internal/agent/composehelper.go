package agent

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"syscall"
)

var serviceNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

func validServiceName(s string) bool { return serviceNameRe.MatchString(s) }

// overrideMarker starts every compose.override.yml TainerHub writes, so it
// only ever replaces its own file.
const overrideMarker = "# Added by TainerHub"

// cgroupOverride adds the read-only cgroup mount to the agent's service.
// Compose merges a service's volumes from all files, so the rest of the
// user's compose file stays as it is.
func cgroupOverride(service string) string {
	return overrideMarker + ` (Hosts page > Lighter stats).
# Lets the agent read container CPU and memory from the kernel, read-only,
# which costs the server far less than asking Docker. Safe to delete: the
# agent then asks Docker again.
services:
  ` + service + `:
    volumes:
      - /sys/fs/cgroup:/host/sys/fs/cgroup:ro
`
}

// writeCgroupOverride writes the override unless a compose.override.yml the
// user wrote is already there.
func writeCgroupOverride(path, service string) error {
	if !validServiceName(service) {
		return errors.New("invalid service name")
	}
	if b, err := os.ReadFile(path); err == nil && !strings.HasPrefix(string(b), overrideMarker) {
		return fmt.Errorf("%s already exists and wasn't written by TainerHub; add this line under the agent's volumes yourself: - /sys/fs/cgroup:/host/sys/fs/cgroup:ro", path)
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.WriteFile(path, []byte(cgroupOverride(service)), 0o644)
}

// ComposeHelper runs as `tainerhub-agent compose-helper --override PATH
// --service NAME -- <compose args>` in the short-lived updater container:
// it writes the override, then becomes docker compose.
func ComposeHelper(args []string) error {
	if len(args) < 5 || args[0] != "--override" || args[2] != "--service" || args[4] != "--" {
		return errors.New("usage: compose-helper --override PATH --service NAME -- COMPOSE ARGS")
	}
	if err := writeCgroupOverride(args[1], args[3]); err != nil {
		return err
	}
	bin := "/usr/local/bin/docker-compose"
	return syscall.Exec(bin, append([]string{bin}, args[5:]...), os.Environ())
}
