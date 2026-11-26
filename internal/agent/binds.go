package agent

import (
	"context"
	"net/url"
	"sort"
	"strings"

	"github.com/prezmc/tainerhub/internal/wire"
)

// systemBind says whether a bind source is the host's own plumbing (the
// Docker socket, clock, devices, kernel files) rather than an app's data.
func systemBind(src string) bool {
	if src == "/" || strings.HasSuffix(src, ".sock") {
		return true
	}
	for _, p := range []string{"/var/run", "/run", "/proc", "/sys", "/dev", "/etc", "/usr", "/lib", "/boot", "/var/lib/docker"} {
		if src == p || strings.HasPrefix(src, p+"/") {
			return true
		}
	}
	return false
}

// binds answers OpBindsList from Docker's container list (which carries
// each container's mounts), so it costs one request.
func (d *SocketDocker) binds(ctx context.Context) ([]wire.BindMount, error) {
	var cs []struct {
		Names  []string
		Image  string
		Mounts []struct {
			Type        string
			Source      string
			Destination string
		}
	}
	if err := d.getJSON(ctx, "/containers/json", url.Values{"all": {"1"}}, &cs); err != nil {
		return nil, err
	}
	out := []wire.BindMount{}
	for _, c := range cs {
		name := ""
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		for _, m := range c.Mounts {
			if m.Type != "bind" || systemBind(m.Source) {
				continue
			}
			out = append(out, wire.BindMount{Container: name, Image: c.Image, Source: m.Source, Target: m.Destination})
		}
		if len(out) >= 2000 {
			break
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Container != out[j].Container {
			return out[i].Container < out[j].Container
		}
		return out[i].Target < out[j].Target
	})
	return out, nil
}
