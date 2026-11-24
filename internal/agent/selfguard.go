package agent

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"sync"

	"github.com/prezmc/tainerhub/internal/wire"
)

// selfHostname is os.Hostname, swapped out in tests. Docker sets a
// container's hostname to the first 12 characters of its ID.
var selfHostname = os.Hostname

// selfInfo is what the agent knows about its own container.
type selfInfo struct {
	id      string // full container ID, or the 12-character prefix if inspect failed
	project string // its compose project ("" if not started by compose)
}

// selfProcFiles are read for the agent's own container ID when its hostname
// isn't one (a custom hostname). Under Docker, /proc/self/mountinfo shows
// the files Docker mounts from /var/lib/docker/containers/<id>/, and with
// cgroup v1 /proc/self/cgroup names the container's cgroup. Swapped in tests.
var selfProcFiles = []string{"/proc/self/mountinfo", "/proc/self/cgroup"}

type selfCache struct {
	mu     sync.Mutex
	known  bool
	info   selfInfo
	warned bool // logged that the agent's container could not be found
}

// self finds the agent's own container. A successful lookup is cached for
// the agent's lifetime; a failed one is retried next time, since Docker may
// only have been briefly unreachable.
func (d *SocketDocker) self(ctx context.Context) selfInfo {
	d.selfc.mu.Lock()
	defer d.selfc.mu.Unlock()
	if d.selfc.known {
		return d.selfc.info
	}
	info, sure := d.findSelf(ctx)
	if sure {
		d.selfc.known, d.selfc.info = true, info
		return info
	}
	if info.id == "" && !d.selfc.warned {
		d.selfc.warned = true
		slog.Warn("could not tell which container this agent runs in, so it can't refuse to stop itself or the hub; it will keep trying")
	}
	return info
}

// findSelf looks up the agent's own container. sure means Docker confirmed
// it; otherwise info may still hold a likely ID worth protecting.
func (d *SocketDocker) findSelf(ctx context.Context) (info selfInfo, sure bool) {
	type inspect struct {
		ID     string `json:"Id"`
		Config struct{ Labels map[string]string }
	}
	lookup := func(ref string) (selfInfo, bool) {
		var insp inspect
		if err := d.getJSON(ctx, "/containers/"+url.PathEscape(ref)+"/json", nil, &insp); err != nil {
			return selfInfo{}, false
		}
		// Only trust the match when the ID starts with what we looked up:
		// a container could merely be named like this machine.
		if insp.ID == "" || !strings.HasPrefix(insp.ID, ref) {
			return selfInfo{}, false
		}
		return selfInfo{id: insp.ID, project: insp.Config.Labels["com.docker.compose.project"]}, true
	}
	h, _ := selfHostname()
	if isShortID(h) {
		if info, ok := lookup(h); ok {
			return info, true
		}
	}
	if id := ownContainerID(selfProcFiles); id != "" {
		if info, ok := lookup(id); ok {
			return info, true
		}
		// Still protect the container with this ID.
		return selfInfo{id: id}, false
	}
	if isShortID(h) {
		return selfInfo{id: h}, false
	}
	return selfInfo{}, false
}

// ownContainerID finds a 64-character container ID in the given /proc
// files, or "". Only IDs in places Docker (or Podman) puts container IDs
// count: image layer IDs look just the same.
func ownContainerID(files []string) string {
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil || len(b) > 1<<20 {
			continue
		}
		for _, line := range strings.Split(string(b), "\n") {
			if id := containerIDIn(line); id != "" {
				return id
			}
		}
	}
	return ""
}

// containerIDMarkers precede a container ID in mountinfo and cgroup lines:
// /var/lib/docker/containers/<id>/hostname, overlay-containers/<id>/ for
// Podman, and cgroup v1 paths /docker/<id> or docker-<id>.scope.
var containerIDMarkers = []string{"/containers/", "/docker/", "docker-", "libpod-"}

func containerIDIn(line string) string {
	for _, m := range containerIDMarkers {
		rest := line
		for {
			i := strings.Index(rest, m)
			if i < 0 {
				break
			}
			rest = rest[i+len(m):]
			if len(rest) >= 64 && isHex(rest[:64]) && (len(rest) == 64 || !isHexByte(rest[64])) {
				return rest[:64]
			}
		}
	}
	return ""
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		if !isHexByte(s[i]) {
			return false
		}
	}
	return true
}

func isHexByte(c byte) bool { return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' }

// OwnProject is the compose project the agent runs in ("" if unknown). On
// the hub's server that is also the hub's and its database's project.
func (d *SocketDocker) OwnProject(ctx context.Context) string {
	return d.self(ctx).project
}

func isShortID(s string) bool {
	if len(s) != 12 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// takesDown reports whether a container action would leave the container
// stopped. Restart brings it back, so it is allowed.
func takesDown(action string) bool {
	switch action {
	case wire.ActionStop, wire.ActionKill, wire.ActionRemove, wire.ActionPause:
		return true
	}
	return false
}

// guardSelf refuses to stop, kill, remove or pause the agent's own
// container or anything in its compose project (on the hub's server: the
// hub and its database). Doing so locks everyone out of this host, and
// "restart: unless-stopped" won't bring a stopped container back.
func (d *SocketDocker) guardSelf(ctx context.Context, id string) error {
	me := d.self(ctx)
	if me.id == "" && me.project == "" {
		return nil
	}
	var insp struct {
		ID     string `json:"Id"`
		Name   string
		Config struct{ Labels map[string]string }
	}
	if err := d.getJSON(ctx, "/containers/"+url.PathEscape(id)+"/json", nil, &insp); err != nil {
		return err
	}
	project := insp.Config.Labels["com.docker.compose.project"]
	isMe := me.id != "" && insp.ID != "" && (strings.HasPrefix(insp.ID, me.id) || strings.HasPrefix(me.id, insp.ID))
	if !isMe && (me.project == "" || project != me.project) {
		return nil
	}
	name := strings.TrimPrefix(insp.Name, "/")
	if name == "" {
		name = id
	}
	how := "docker compose"
	if me.project != "" {
		how = "docker compose -p " + me.project
	}
	return fmt.Errorf("%s is part of TainerHub, so that would stop TainerHub itself; manage it from the server (%s ...) or use Hosts → Updates", name, how)
}

// errOwnProject is returned for stack operations on the agent's own project.
func errOwnProject(name string) error {
	return fmt.Errorf("%q is the compose project TainerHub itself runs in on this host; a stack with that name would treat the hub, its database and this agent as its own and could remove them. Choose another name", name)
}
