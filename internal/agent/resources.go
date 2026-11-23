package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

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
		if op.Arg == "all" {
			return d.pruneAllImages(ctx)
		}
		return d.prune(ctx, "/images/prune", map[string][]string{"dangling": {"true"}}, "ImagesDeleted")
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
	case wire.OpContainerRemoveVolumes:
		names, _ := wire.RemoveVolumeNames(op.Arg) // validated above
		if err := d.guardSelf(ctx, op.Target); err != nil {
			return nil, err
		}
		return d.removeWithVolumes(ctx, op.Target, names)
	case wire.OpSelfUpdate:
		return d.selfUpdate(ctx, op.Arg == wire.SelfUpdateAddCgroupMount)
	case wire.OpDockerPlan:
		return d.dockerPlan(ctx)
	case wire.OpDockerUpgrade:
		return d.dockerUpgrade(ctx)
	case wire.OpImagesCheckUpdates:
		r, err := d.checkUpdates(ctx)
		if err == nil {
			d.updates.set(r)
		}
		return r, err
	case wire.OpContainerRecreate:
		r, err := d.recreate(ctx, op.Target)
		if err == nil {
			_ = d.refreshUpdates(ctx) // the badge should go away now
		}
		return r, err
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

// removeWithVolumes removes a container with its anonymous volumes, then
// deletes the requested named volumes, but only ones this container
// actually mounted and that no other container uses once it is gone.
func (d *SocketDocker) removeWithVolumes(ctx context.Context, id string, names []string) (wire.RemoveVolumesResult, error) {
	res := wire.RemoveVolumesResult{Removed: []string{}, Kept: []wire.KeptVolume{}}
	var insp struct {
		Mounts []struct{ Type, Name string }
	}
	if err := d.getJSON(ctx, "/containers/"+url.PathEscape(id)+"/json", nil, &insp); err != nil {
		return res, err
	}
	mounted := map[string]bool{}
	for _, m := range insp.Mounts {
		if m.Type != "volume" || m.Name == "" {
			continue
		}
		if isAnonymousVolume(m.Name) {
			res.Anonymous++
		} else {
			mounted[m.Name] = true
		}
	}
	// v=1 makes Docker delete the container's anonymous volumes with it.
	if err := d.remove(ctx, "/containers/"+url.PathEscape(id)+"?force=1&v=1"); err != nil {
		return res, err
	}
	for _, n := range names {
		if !mounted[n] {
			res.Kept = append(res.Kept, wire.KeptVolume{Name: n, Reason: "not used by this container"})
			continue
		}
		users, err := d.volumeUsers(ctx, n)
		if err != nil {
			res.Kept = append(res.Kept, wire.KeptVolume{Name: n, Reason: err.Error()})
			continue
		}
		if len(users) > 0 {
			res.Kept = append(res.Kept, wire.KeptVolume{Name: n, Reason: "still used by " + strings.Join(users, ", ")})
			continue
		}
		if err := d.remove(ctx, "/volumes/"+url.PathEscape(n)); err != nil {
			res.Kept = append(res.Kept, wire.KeptVolume{Name: n, Reason: err.Error()})
			continue
		}
		res.Removed = append(res.Removed, n)
	}
	return res, nil
}

// volumeUsers lists the containers (running or not) that mount a volume.
func (d *SocketDocker) volumeUsers(ctx context.Context, name string) ([]string, error) {
	f, _ := json.Marshal(map[string][]string{"volume": {name}})
	var cs []struct{ Names []string }
	if err := d.getJSON(ctx, "/containers/json", url.Values{"all": {"1"}, "filters": {string(f)}}, &cs); err != nil {
		return nil, err
	}
	out := []string{}
	for _, c := range cs {
		if len(c.Names) > 0 {
			out = append(out, strings.TrimPrefix(c.Names[0], "/"))
		}
	}
	return out, nil
}

// isAnonymousVolume recognises the 64-hex-digit names Docker gives
// volumes nobody named.
func isAnonymousVolume(name string) bool {
	if len(name) != 64 {
		return false
	}
	for i := 0; i < len(name); i++ {
		if !(name[i] >= '0' && name[i] <= '9' || name[i] >= 'a' && name[i] <= 'f') {
			return false
		}
	}
	return true
}

// selfUpdate updates the Docker Compose project this agent runs in. A
// container can't recreate itself, so it starts a short-lived helper (its own
// image, which includes docker compose) that runs `compose up -d --pull
// always` for the project and removes itself when done. On the hub's server
// the project is the hub's, so the hub, its database and this agent are all
// brought up to date.
//
// With addMount, it instead writes compose.override.yml (which docker
// compose also reads by default) adding the read-only cgroup mount to this
// agent's service, and recreates only what that changes: the agent.
func (d *SocketDocker) selfUpdate(ctx context.Context, addMount bool) (wire.SelfUpdateResult, error) {
	var res wire.SelfUpdateResult
	if addMount && d.cg != nil {
		return res, errors.New("this agent already reads stats from the kernel")
	}
	self, _ := os.Hostname() // Docker sets it to the container ID unless overridden
	if !wire.ValidContainerRef(self) {
		return res, errors.New("can't identify this agent's container")
	}
	var insp struct {
		Config struct {
			Image  string
			Labels map[string]string
		}
		Mounts []struct{ Source, Destination string }
	}
	if err := d.getJSON(ctx, "/containers/"+url.PathEscape(self)+"/json", nil, &insp); err != nil {
		return res, fmt.Errorf("can't find this agent's own container (was its hostname changed?): %w", err)
	}
	l := insp.Config.Labels
	project, dir, files := l["com.docker.compose.project"], l["com.docker.compose.project.working_dir"], l["com.docker.compose.project.config_files"]
	if project == "" || dir == "" || files == "" {
		return res, errors.New("this agent wasn't started with docker compose, so it can't update itself; on the server run: docker compose pull && docker compose up -d")
	}
	hostSocket := "/var/run/docker.sock"
	for _, m := range insp.Mounts {
		if m.Destination == d.socket {
			hostSocket = m.Source
		}
	}
	cmd := []string{"--project-name", project, "--project-directory", dir}
	dirMode := ":ro"
	entrypoint := []string{"/usr/local/bin/docker-compose"}
	override := filepath.Join(dir, "compose.override.yml")
	if addMount {
		service := l["com.docker.compose.service"]
		if !validServiceName(service) {
			return res, errors.New("can't tell this agent's compose service name")
		}
		// The helper (this image) writes the override, then runs compose.
		entrypoint = []string{"/tainerhub-agent", "compose-helper", "--override", override, "--service", service, "--"}
		dirMode = "" // it writes there
	}
	binds := []string{hostSocket + ":/var/run/docker.sock", dir + ":" + dir + dirMode}
	haveOverride := false
	for _, f := range strings.Split(files, ",") {
		cmd = append(cmd, "--file", f)
		haveOverride = haveOverride || f == override
		if fd := filepath.Dir(f); fd != dir && !strings.HasPrefix(fd, dir+"/") {
			binds = append(binds, fd+":"+fd+":ro")
		}
	}
	if addMount {
		if !haveOverride {
			cmd = append(cmd, "--file", override)
		}
		cmd = append(cmd, "up", "--detach", "--no-deps", l["com.docker.compose.service"])
	} else {
		cmd = append(cmd, "up", "--detach", "--pull", "always")
	}

	res.Project, res.Dir = project, dir
	res.Helper = fmt.Sprintf("tainerhub-updater-%d", time.Now().Unix())
	spec := map[string]any{
		"Image":      insp.Config.Image,
		"Entrypoint": entrypoint,
		"Cmd":        cmd,
		"Env":        []string{"DOCKER_HOST=unix:///var/run/docker.sock", "HOME=/tmp", "DOCKER_CONFIG=/tmp/docker", "COMPOSE_ANSI=never"},
		"WorkingDir": dir,
		"Labels":     map[string]string{"tainerhub.updater": project},
		"HostConfig": map[string]any{
			"Binds":       binds,
			"AutoRemove":  true,
			"Tmpfs":       map[string]string{"/tmp": ""},
			"SecurityOpt": []string{"no-new-privileges:true"},
		},
	}
	resp, err := d.doBody(ctx, http.MethodPost, "/containers/create", url.Values{"name": {res.Helper}}, spec)
	if err != nil {
		return res, fmt.Errorf("start the updater: %w", err)
	}
	var created struct {
		ID string `json:"Id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	resp, err = d.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(created.ID)+"/start", nil)
	if err != nil {
		return res, fmt.Errorf("start the updater: %w", err)
	}
	resp.Body.Close()
	return res, nil
}

// pruneAllImages removes every image no container uses. Docker's own prune
// goes first; then each image that's still there and unused (by the same
// rule the Images list shows "unused" by) is removed one tag at a time,
// without forcing. Some Docker setups prune nothing for reasons they don't
// report; this way the result matches the list, and an image Docker refuses
// to remove is reported with Docker's reason.
func (d *SocketDocker) pruneAllImages(ctx context.Context) (wire.PruneResult, error) {
	before := d.imageLayersSize(ctx)
	res, err := d.prune(ctx, "/images/prune", map[string][]string{"dangling": {"false"}}, "ImagesDeleted")
	if err != nil {
		return res, err
	}
	pruned := res.Deleted
	imgs, err := d.images(ctx)
	if err != nil {
		return res, err
	}
	swept := 0
	for _, im := range imgs {
		if len(im.UsedBy) > 0 || ctx.Err() != nil {
			continue
		}
		name := shortImageID(im.ID)
		refs := im.Tags
		if len(refs) == 0 {
			refs = []string{im.ID}
		} else {
			name = im.Tags[0]
		}
		var failed error
		for _, ref := range refs {
			// Removing the last tag removes the image; Docker refuses if a
			// container uses it, so nothing in use can go.
			if err := d.remove(ctx, "/images/"+url.PathEscape(ref)); err != nil {
				failed = err
				break
			}
		}
		if failed != nil {
			res.Kept = append(res.Kept, wire.KeptItem{Name: name, Reason: strings.TrimPrefix(failed.Error(), "docker: ")})
			continue
		}
		swept++
	}
	res.Deleted = pruned + swept
	if after := d.imageLayersSize(ctx); before > 0 && after >= 0 && before > after {
		res.Reclaimed = max(res.Reclaimed, before-after)
	}
	return res, nil
}

// imageLayersSize is the disk space all images take (shared layers counted
// once), or -1 if Docker doesn't say.
func (d *SocketDocker) imageLayersSize(ctx context.Context) int64 {
	var df struct{ LayersSize int64 }
	if err := d.getJSON(ctx, "/system/df", url.Values{"type": {"image"}}, &df); err != nil {
		return -1
	}
	return df.LayersSize
}

func shortImageID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		id = id[:12]
	}
	return id
}
