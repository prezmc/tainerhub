package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/prezmc/tainerhub/internal/wire"
)

// Moving a standalone container to another host: the source agent exports
// the container's settings (OpContainerExport), the hub copies its volumes
// with the usual transfers, and the target agent creates it again
// (OpContainerCreate).

// ContainerMover is implemented by Docker clients that can export and
// create containers for a move.
type ContainerMover interface {
	ExportContainer(ctx context.Context, id string) (wire.ContainerExport, error)
	CreateContainer(ctx context.Context, c *wire.ContainerCreate) (wire.ContainerCreateResult, error)
}

var builtinNetworks = map[string]bool{"bridge": true, "host": true, "none": true, "default": true}

// ExportContainer answers OpContainerExport.
func (d *SocketDocker) ExportContainer(ctx context.Context, id string) (wire.ContainerExport, error) {
	var out wire.ContainerExport
	var insp map[string]json.RawMessage
	if err := d.getJSON(ctx, "/containers/"+url.PathEscape(id)+"/json", nil, &insp); err != nil {
		return out, err
	}
	var meta containerMeta
	b, _ := json.Marshal(insp)
	if err := json.Unmarshal(b, &meta); err != nil {
		return out, err
	}
	var more struct {
		Mounts []exportMount
		Config struct {
			Image string
		}
		HostConfig struct {
			NetworkMode    string
			PidMode        string
			IpcMode        string
			Privileged     bool
			VolumesFrom    []string
			Links          []string
			Devices        []json.RawMessage
			DeviceRequests []json.RawMessage
			PortBindings   map[string][]struct {
				HostIP   string `json:"HostIp"`
				HostPort string
			}
		}
	}
	_ = json.Unmarshal(b, &more)
	out = wire.ContainerExport{
		ID: meta.ID, Name: strings.TrimPrefix(meta.Name, "/"), Image: meta.Config.Image, ImageID: meta.Image,
		Running: meta.State.Running, Project: meta.Config.Labels["com.docker.compose.project"],
		NetworkMode: more.HostConfig.NetworkMode, Privileged: more.HostConfig.Privileged,
		Devices: len(more.HostConfig.Devices) > 0 || len(more.HostConfig.DeviceRequests) > 0,
		Volumes: []wire.ExportVolume{}, Binds: []string{}, Networks: []wire.NetworkSpec{}, Ports: []string{},
	}
	problem := func(f string, a ...any) { out.Problems = append(out.Problems, fmt.Sprintf(f, a...)) }
	hc := more.HostConfig
	if strings.HasPrefix(hc.NetworkMode, "container:") {
		problem("It uses another container's network (--network container:…), which doesn't exist on the other host.")
	}
	if strings.HasPrefix(hc.PidMode, "container:") || strings.HasPrefix(hc.IpcMode, "container:") {
		problem("It shares another container's processes or memory (--pid or --ipc container:…).")
	}
	if len(hc.VolumesFrom) > 0 {
		problem("It mounts the volumes of %s (--volumes-from).", strings.Join(hc.VolumesFrom, ", "))
	}
	if len(hc.Links) > 0 {
		problem("It uses legacy links (--link) to other containers.")
	}
	if !wire.ValidImageRef(meta.Config.Image) || wire.ValidImageID(meta.Config.Image) || strings.HasPrefix(meta.Config.Image, "sha256:") {
		problem("Its image has no name (%s), so the other host can't pull it.", meta.Config.Image)
	}

	// Published ports.
	seen := map[string]bool{}
	for port, binds := range hc.PortBindings {
		_, proto, _ := strings.Cut(port, "/")
		if proto == "" {
			proto = "tcp"
		}
		for _, pb := range binds {
			if pb.HostPort == "" || pb.HostPort == "0" {
				continue // Docker picks a free port
			}
			p := pb.HostPort + "/" + proto
			if !seen[p] {
				seen[p] = true
				out.Ports = append(out.Ports, p)
			}
		}
	}
	sort.Strings(out.Ports)

	// Volumes and folders.
	var sizes map[string]VolumeUsage
	var users usage
	for _, m := range more.Mounts {
		switch m.Type {
		case "bind":
			out.Binds = append(out.Binds, m.Source)
		case "volume":
			if sizes == nil {
				if sizes, _ = d.Volumes(ctx); sizes == nil {
					sizes = map[string]VolumeUsage{}
				}
				users, _ = d.usage(ctx)
			}
			v := wire.ExportVolume{Name: m.Name, Destination: m.Destination, Driver: m.Driver, Size: -1,
				Anonymous: isAnonymousVolume(m.Name)}
			if u, ok := sizes[m.Name]; ok {
				v.Size, v.Labels, v.Options = u.Size, u.Labels, u.Opts
				if v.Driver == "" {
					v.Driver = u.Driver
				}
				if u.Labels["com.docker.volume.anonymous"] != "" {
					v.Anonymous = true
				}
			}
			for _, n := range users.volumes[m.Name] {
				if n != out.Name {
					v.UsedBy = append(v.UsedBy, n)
				}
			}
			out.Volumes = append(out.Volumes, v)
		}
	}
	sort.Strings(out.Binds)

	spec := d.createSpec(ctx, insp, meta, meta.Config.Image)
	named, err := nameVolumes(insp["HostConfig"], more.Mounts)
	if err != nil {
		return out, err
	}
	spec["HostConfig"] = named

	// Networks other than Docker's own, as needed to create them elsewhere.
	// A network keeps its addresses only if the container has a fixed one:
	// otherwise the other host picks a range that's free there.
	names := make([]string, 0, len(meta.NetworkSettings.Networks))
	for n := range meta.NetworkSettings.Networks {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		ns := wire.NetworkSpec{Name: n, Builtin: builtinNetworks[n]}
		if ns.Builtin {
			ns.Driver = n
			out.Networks = append(out.Networks, ns)
			continue
		}
		var raw struct {
			Driver     string
			Internal   bool
			Attachable bool
			EnableIPv6 bool
			Options    map[string]string
			Labels     map[string]string
			IPAM       json.RawMessage
		}
		if err := d.getJSON(ctx, "/networks/"+url.PathEscape(n), nil, &raw); err != nil {
			return out, fmt.Errorf("network %s: %w", n, err)
		}
		ns.Driver, ns.Internal, ns.Attachable, ns.IPv6, ns.Options, ns.Labels = raw.Driver, raw.Internal, raw.Attachable, raw.EnableIPv6, raw.Options, raw.Labels
		if ep := meta.NetworkSettings.Networks[n]; len(ep["IPAMConfig"]) > 0 && string(ep["IPAMConfig"]) != "null" && string(ep["IPAMConfig"]) != "{}" {
			ns.IPAM = raw.IPAM
		}
		out.Networks = append(out.Networks, ns)
	}

	out.Spec, err = json.Marshal(spec)
	if err != nil {
		return out, err
	}
	if len(out.Spec) > wire.MaxContainerSpec {
		problem("Its settings are too large to move (over 1 MB).")
	}
	return out, nil
}

type exportMount struct {
	Type        string
	Name        string
	Source      string
	Destination string
	Driver      string
	RW          bool
}

// nameVolumes returns a container's HostConfig with every volume it mounts
// given by name, so a container created from it on another host mounts the
// copies instead of fresh empty volumes. Volumes from the image's VOLUME
// lines or `-v /path` are otherwise made anew, empty, for each container.
func nameVolumes(hostConfig json.RawMessage, mounts []exportMount) (json.RawMessage, error) {
	var hc map[string]json.RawMessage
	if err := json.Unmarshal(hostConfig, &hc); err != nil || hc == nil {
		return nil, errors.New("unreadable host config")
	}
	var binds []string
	_ = json.Unmarshal(hc["Binds"], &binds)
	var ms []map[string]json.RawMessage
	_ = json.Unmarshal(hc["Mounts"], &ms)
	covered := map[string]bool{}
	for _, b := range binds {
		parts := strings.Split(b, ":")
		if len(parts) >= 2 {
			covered[parts[1]] = true
		}
	}
	byDest := map[string]exportMount{}
	for _, m := range mounts {
		if m.Type == "volume" && m.Name != "" {
			byDest[m.Destination] = m
		}
	}
	for _, m := range ms {
		var typ, src, target string
		_ = json.Unmarshal(m["Type"], &typ)
		_ = json.Unmarshal(m["Source"], &src)
		_ = json.Unmarshal(m["Target"], &target)
		covered[target] = true
		if v, ok := byDest[target]; ok && typ == "volume" && src == "" {
			m["Source"], _ = json.Marshal(v.Name) // --mount type=volume without a source
		}
	}
	dests := make([]string, 0, len(byDest))
	for d := range byDest {
		dests = append(dests, d)
	}
	sort.Strings(dests)
	for _, d := range dests {
		if covered[d] {
			continue
		}
		m := byDest[d]
		b := m.Name + ":" + d
		if !m.RW {
			b += ":ro"
		}
		binds = append(binds, b)
	}
	if len(binds) > 0 {
		hc["Binds"], _ = json.Marshal(binds)
	}
	if len(ms) > 0 {
		hc["Mounts"], _ = json.Marshal(ms)
	}
	return json.Marshal(hc)
}

// CreateContainer answers OpContainerCreate. Anything it made is removed
// again if a later step fails.
func (d *SocketDocker) CreateContainer(ctx context.Context, c *wire.ContainerCreate) (res wire.ContainerCreateResult, err error) {
	if err := wire.ValidContainerCreate(c); err != nil {
		return res, err
	}
	var existing []struct{ Name string }
	if err := d.getJSON(ctx, "/networks", nil, &existing); err != nil {
		return res, err
	}
	have := map[string]bool{}
	for _, n := range existing {
		have[n.Name] = true
	}
	defer func() {
		if err == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if res.ID != "" {
			_ = d.remove(ctx, "/containers/"+url.PathEscape(res.ID)+"?force=1")
		}
		for _, n := range res.Networks {
			_ = d.remove(ctx, "/networks/"+url.PathEscape(n))
		}
		res = wire.ContainerCreateResult{}
	}()
	for _, n := range c.Networks {
		if have[n.Name] {
			continue
		}
		body := map[string]any{
			"Name": n.Name, "Driver": n.Driver, "Internal": n.Internal, "Attachable": n.Attachable,
			"EnableIPv6": n.IPv6, "Options": n.Options, "Labels": n.Labels,
		}
		if len(n.IPAM) > 0 && string(n.IPAM) != "null" {
			body["IPAM"] = n.IPAM
		}
		resp, err := d.doBody(ctx, http.MethodPost, "/networks/create", nil, body)
		if err != nil {
			return res, fmt.Errorf("create network %s: %w", n.Name, err)
		}
		resp.Body.Close()
		res.Networks = append(res.Networks, n.Name)
	}
	resp, err := d.doBody(ctx, http.MethodPost, "/containers/create", url.Values{"name": {c.Name}}, c.Spec)
	if err != nil {
		return res, fmt.Errorf("create the container: %w", err)
	}
	var created struct {
		ID string `json:"Id"`
	}
	err = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if err != nil {
		return res, err
	}
	res.ID = created.ID
	if c.Start {
		if err := d.post(ctx, "/containers/"+url.PathEscape(created.ID)+"/start", nil); err != nil {
			return res, fmt.Errorf("start the container: %w", err)
		}
	}
	return res, nil
}
