package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/prezmc/tainerhub/internal/wire"
)

// Operate runs one whitelisted resource operation and returns its JSON result.
func (d *SocketDocker) Operate(ctx context.Context, op wire.Operation) (any, error) {
	if err := wire.ValidOperation(op); err != nil {
		return nil, err
	}
	switch op.Kind {
	case wire.OpContainerInspect:
		return d.inspect(ctx, op.Target)
	case wire.OpImagesList:
		return d.images(ctx)
	case wire.OpImagePull:
		return d.pull(ctx, op.Arg)
	case wire.OpImageRemove:
		return nil, d.remove(ctx, "/images/"+url.PathEscape(op.Target))
	case wire.OpImagesPrune:
		dangling := "true"
		if op.Arg == "all" {
			dangling = "false" // every image not used by a container
		}
		return d.prune(ctx, "/images/prune", map[string][]string{"dangling": {dangling}}, "ImagesDeleted")
	case wire.OpVolumesList:
		return d.volumes(ctx)
	case wire.OpVolumeRemove:
		return nil, d.remove(ctx, "/volumes/"+url.PathEscape(op.Target))
	case wire.OpVolumesPrune:
		var f map[string][]string
		if op.Arg == "all" {
			f = map[string][]string{"all": {"true"}} // named volumes too, not just anonymous ones
		}
		return d.prune(ctx, "/volumes/prune", f, "VolumesDeleted")
	case wire.OpNetworksList:
		return d.networks(ctx)
	case wire.OpNetworkRemove:
		return nil, d.remove(ctx, "/networks/"+url.PathEscape(op.Target))
	case wire.OpNetworksPrune:
		return d.prune(ctx, "/networks/prune", nil, "NetworksDeleted")
	}
	return nil, errors.New("unknown operation")
}

func (d *SocketDocker) remove(ctx context.Context, path string) error {
	resp, err := d.do(ctx, http.MethodDelete, path, nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (d *SocketDocker) prune(ctx context.Context, path string, filters map[string][]string, listKey string) (wire.PruneResult, error) {
	var q url.Values
	if len(filters) > 0 {
		f, _ := json.Marshal(filters)
		q = url.Values{"filters": {string(f)}}
	}
	resp, err := d.do(ctx, http.MethodPost, path, q)
	if err != nil {
		return wire.PruneResult{}, err
	}
	defer resp.Body.Close()
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return wire.PruneResult{}, err
	}
	var res wire.PruneResult
	var deleted []json.RawMessage
	_ = json.Unmarshal(raw[listKey], &deleted)
	res.Deleted = len(deleted)
	_ = json.Unmarshal(raw["SpaceReclaimed"], &res.Reclaimed)
	return res, nil
}

// pull streams the Engine's progress messages to the end and reports the
// final status, or the first error.
func (d *SocketDocker) pull(ctx context.Context, ref string) (wire.PullResult, error) {
	resp, err := d.do(ctx, http.MethodPost, "/images/create", url.Values{"fromImage": {ref}})
	if err != nil {
		return wire.PullResult{}, err
	}
	defer resp.Body.Close()
	var last string
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var m struct {
			Status string `json:"status"`
			Error  string `json:"error"`
		}
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		if m.Error != "" {
			return wire.PullResult{}, errors.New(m.Error)
		}
		if m.Status != "" && !strings.HasPrefix(m.Status, "Downloading") && !strings.HasPrefix(m.Status, "Extracting") {
			last = m.Status
		}
	}
	if err := sc.Err(); err != nil {
		return wire.PullResult{}, err
	}
	return wire.PullResult{Status: last}, nil
}

// usage maps image IDs, volume names and network names to the containers using them.
type usage struct{ images, volumes, networks map[string][]string }

func (d *SocketDocker) usage(ctx context.Context) (usage, error) {
	var raw []struct {
		Names   []string
		ImageID string
		Mounts  []struct {
			Type string
			Name string
		}
		NetworkSettings struct {
			Networks map[string]json.RawMessage
		}
	}
	u := usage{map[string][]string{}, map[string][]string{}, map[string][]string{}}
	if err := d.getJSON(ctx, "/containers/json", url.Values{"all": {"1"}}, &raw); err != nil {
		return u, err
	}
	for _, c := range raw {
		name := ""
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		u.images[c.ImageID] = append(u.images[c.ImageID], name)
		for _, m := range c.Mounts {
			if m.Type == "volume" && m.Name != "" {
				u.volumes[m.Name] = append(u.volumes[m.Name], name)
			}
		}
		for n := range c.NetworkSettings.Networks {
			u.networks[n] = append(u.networks[n], name)
		}
	}
	return u, nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	sort.Strings(s)
	return s
}

func (d *SocketDocker) images(ctx context.Context) ([]wire.Image, error) {
	u, err := d.usage(ctx)
	if err != nil {
		return nil, err
	}
	var raw []struct {
		ID       string `json:"Id"`
		RepoTags []string
		Size     int64
		Created  int64
	}
	if err := d.getJSON(ctx, "/images/json", nil, &raw); err != nil {
		return nil, err
	}
	out := make([]wire.Image, 0, len(raw))
	for _, im := range raw {
		tags := []string{}
		for _, t := range im.RepoTags {
			if t != "<none>:<none>" {
				tags = append(tags, t)
			}
		}
		out = append(out, wire.Image{ID: im.ID, Tags: tags, Size: im.Size, Created: im.Created, UsedBy: nonNil(u.images[im.ID])})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created > out[j].Created })
	return out, nil
}

func (d *SocketDocker) volumes(ctx context.Context) ([]wire.Volume, error) {
	u, err := d.usage(ctx)
	if err != nil {
		return nil, err
	}
	var raw struct {
		Volumes []struct {
			Name       string
			Driver     string
			Mountpoint string
			CreatedAt  string
			Labels     map[string]string
		}
	}
	if err := d.getJSON(ctx, "/volumes", nil, &raw); err != nil {
		return nil, err
	}
	out := make([]wire.Volume, 0, len(raw.Volumes))
	for _, v := range raw.Volumes {
		out = append(out, wire.Volume{Name: v.Name, Driver: v.Driver, Mountpoint: v.Mountpoint, Created: v.CreatedAt,
			Project: v.Labels["com.docker.compose.project"], UsedBy: nonNil(u.volumes[v.Name])})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (d *SocketDocker) networks(ctx context.Context) ([]wire.Network, error) {
	u, err := d.usage(ctx)
	if err != nil {
		return nil, err
	}
	var raw []struct {
		ID       string `json:"Id"`
		Name     string
		Driver   string
		Scope    string
		Internal bool
		Labels   map[string]string
	}
	if err := d.getJSON(ctx, "/networks", nil, &raw); err != nil {
		return nil, err
	}
	out := make([]wire.Network, 0, len(raw))
	for _, n := range raw {
		builtin := n.Name == "bridge" || n.Name == "host" || n.Name == "none"
		out = append(out, wire.Network{ID: n.ID, Name: n.Name, Driver: n.Driver, Scope: n.Scope, Internal: n.Internal, Builtin: builtin,
			Project: n.Labels["com.docker.compose.project"], UsedBy: nonNil(u.networks[n.Name])})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (d *SocketDocker) inspect(ctx context.Context, id string) (wire.ContainerDetail, error) {
	var r struct {
		ID      string `json:"Id"`
		Name    string
		Created string
		Image   string
		State   struct {
			Status     string
			StartedAt  string
			FinishedAt string
			ExitCode   int
			OOMKilled  bool
			Error      string
			Health     *struct {
				Status        string
				FailingStreak int
				Log           []struct {
					Start    string
					End      string
					ExitCode int
					Output   string
				}
			}
		}
		Config struct {
			User       string
			Env        []string
			Cmd        []string
			Entrypoint []string
			WorkingDir string
			Image      string
			Labels     map[string]string
		}
		HostConfig struct {
			RestartPolicy struct {
				Name              string
				MaximumRetryCount int
			}
			Privileged  bool
			NetworkMode string
			Memory      int64
			NanoCpus    int64
		}
		Mounts []struct {
			Type        string
			Name        string
			Source      string
			Destination string
			RW          bool
		}
		NetworkSettings struct {
			Ports map[string][]struct {
				HostIP   string `json:"HostIp"`
				HostPort string
			}
			Networks map[string]struct {
				IPAddress string
				Gateway   string
			}
		}
	}
	if err := d.getJSON(ctx, "/containers/"+url.PathEscape(id)+"/json", nil, &r); err != nil {
		return wire.ContainerDetail{}, err
	}
	out := wire.ContainerDetail{
		ID: r.ID, Name: strings.TrimPrefix(r.Name, "/"), Image: r.Config.Image, ImageID: r.Image, Created: r.Created,
		State: wire.DetailState{Status: r.State.Status, StartedAt: r.State.StartedAt, FinishedAt: r.State.FinishedAt,
			ExitCode: r.State.ExitCode, OOMKilled: r.State.OOMKilled, Error: r.State.Error},
		Cmd: r.Config.Cmd, Entrypoint: r.Config.Entrypoint, WorkingDir: r.Config.WorkingDir, User: r.Config.User,
		Env: r.Config.Env, Labels: r.Config.Labels,
		RestartPolicy: r.HostConfig.RestartPolicy.Name, MaxRetries: r.HostConfig.RestartPolicy.MaximumRetryCount,
		Privileged: r.HostConfig.Privileged, NetworkMode: r.HostConfig.NetworkMode,
		MemoryLimit: r.HostConfig.Memory, NanoCPUs: r.HostConfig.NanoCpus,
		Mounts: []wire.DetailMount{}, Networks: []wire.DetailNetwork{}, Ports: []wire.DetailPort{},
	}
	if h := r.State.Health; h != nil {
		dh := &wire.DetailHealth{Status: h.Status, FailingStreak: h.FailingStreak}
		for _, l := range h.Log {
			out := l.Output
			if len(out) > 2000 {
				out = out[len(out)-2000:]
			}
			dh.Log = append(dh.Log, wire.HealthProbe{Start: l.Start, End: l.End, ExitCode: l.ExitCode, Output: out})
		}
		out.State.Health = dh
	}
	for _, m := range r.Mounts {
		out.Mounts = append(out.Mounts, wire.DetailMount{Type: m.Type, Name: m.Name, Source: m.Source, Destination: m.Destination, RW: m.RW})
	}
	for name, n := range r.NetworkSettings.Networks {
		out.Networks = append(out.Networks, wire.DetailNetwork{Name: name, IP: n.IPAddress, Gateway: n.Gateway})
	}
	sort.Slice(out.Networks, func(i, j int) bool { return out.Networks[i].Name < out.Networks[j].Name })
	for port, binds := range r.NetworkSettings.Ports {
		if len(binds) == 0 {
			out.Ports = append(out.Ports, wire.DetailPort{Container: port})
		}
		seen := map[string]bool{}
		for _, b := range binds {
			h := b.HostPort
			if b.HostIP != "" && b.HostIP != "0.0.0.0" && b.HostIP != "::" {
				h = b.HostIP + ":" + h
			}
			if !seen[h] {
				seen[h] = true
				out.Ports = append(out.Ports, wire.DetailPort{Container: port, Host: h})
			}
		}
	}
	sort.Slice(out.Ports, func(i, j int) bool { return out.Ports[i].Container < out.Ports[j].Container })
	return out, nil
}
