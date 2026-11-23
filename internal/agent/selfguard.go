package agent

import (
	"context"
	"fmt"
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

type selfCache struct {
	mu    sync.Mutex
	known bool
	info  selfInfo
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
	h, _ := selfHostname()
	if !wire.ValidContainerRef(h) {
		return selfInfo{}
	}
	var insp struct {
		ID     string `json:"Id"`
		Config struct{ Labels map[string]string }
	}
	if err := d.getJSON(ctx, "/containers/"+url.PathEscape(h)+"/json", nil, &insp); err != nil {
		// Still protect a container whose ID matches the hostname.
		if isShortID(h) {
			return selfInfo{id: h}
		}
		return selfInfo{}
	}
	d.selfc.known = true
	// Only trust the match when the ID starts with the hostname: outside a
	// container (or with a custom hostname) a container could merely be
	// named like this machine.
	if insp.ID != "" && strings.HasPrefix(insp.ID, h) {
		d.selfc.info = selfInfo{id: insp.ID, project: insp.Config.Labels["com.docker.compose.project"]}
	}
	return d.selfc.info
}

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
