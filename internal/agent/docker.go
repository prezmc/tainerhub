package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prezmc/tainerhub/internal/regcred"
	"github.com/prezmc/tainerhub/internal/wire"
)

// Docker is the subset of the Docker Engine API the agent uses.
type Docker interface {
	Info(ctx context.Context) (wire.Hello, error)
	List(ctx context.Context) ([]wire.Container, error)
	// Events streams container events until ctx ends or the stream fails.
	Events(ctx context.Context, fn func(wire.DockerEvent)) error
	Action(ctx context.Context, containerID, action string) error
	// Logs writes demultiplexed stdout/stderr to w until EOF or ctx ends.
	Logs(ctx context.Context, containerID string, tail int, follow bool, w io.Writer) error
	// Stats returns CPU (% of one core) and memory use for a running container.
	Stats(ctx context.Context, containerID string) (Usage, error)
	// Operate runs a whitelisted resource operation (inspect, images, volumes, networks).
	Operate(ctx context.Context, op wire.Operation) (any, error)
}

type Usage struct {
	CPU      float64
	Mem      int64
	MemLimit int64
}

// SocketDocker talks to the Docker Engine API over its Unix socket using only
// the standard library (no Docker SDK dependency).
type SocketDocker struct {
	c       *http.Client
	socket  string      // path of the Docker socket inside this container
	updates updateCache // last image update check, shown on each container
	// hubChecks tracks update checks the hub asked for (with registry
	// logins), so the agent's own scheduled check can stand down.
	hubChecks hubCheckTimes

	cpuMu   sync.Mutex
	lastCPU map[string]cpuSample // container ID -> previous stats sample

	cfgMu     sync.Mutex
	cfgImages map[string]string // container ID -> image name it was created with (see configImage)

	cg        *cgroupStats // reads stats from kernel files when the host's cgroups are mounted
	useDocker atomic.Bool  // the hub asked for stats from Docker even so

	selfc selfCache // the agent's own container, found on first use
}

// SetStatsFromDocker switches between cgroup files and the Docker API for
// stats, and reports whether that changed anything. Each source's previous
// samples are dropped so the first reading after a switch isn't averaged
// over the time the other source was in use.
func (d *SocketDocker) SetStatsFromDocker(on bool) bool {
	if d.useDocker.Swap(on) == on {
		return false
	}
	d.cpuMu.Lock()
	d.lastCPU = nil
	d.cpuMu.Unlock()
	if d.cg != nil {
		d.cg.mu.Lock()
		d.cg.last = map[string]cgroupCPU{}
		d.cg.mu.Unlock()
	}
	return true
}

// UseCgroups reads container stats from the host's cgroup v2 tree mounted at
// dir instead of asking Docker, which is much cheaper for the server. It
// reports false (and changes nothing) when dir isn't a cgroup v2 tree.
func (d *SocketDocker) UseCgroups(dir string) bool {
	d.cg = newCgroupStats(dir)
	return d.cg != nil
}

// cpuSample is a container's CPU counters at one stats sample, so the next
// sample can work out CPU use without the Engine's one-second wait.
type cpuSample struct {
	cpu, sys uint64
	at       time.Time
}

func NewSocketDocker(socketPath string) *SocketDocker {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socketPath)
		},
		// Stats sampling makes up to 8 calls at once; keep those connections
		// open between rounds instead of redialing every 10 seconds.
		MaxIdleConns:        8,
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     30 * time.Second,
	}
	return &SocketDocker{c: &http.Client{Transport: tr}, socket: socketPath}
}

// registryAuthKey carries an X-Registry-Auth header value in a context. It
// is set only around the one pull or registry lookup that needs it, so no
// other Docker request carries the login.
type registryAuthKey struct{}

// withRegistryAuth returns ctx carrying the login for ref's registry, if
// auths has one.
func withRegistryAuth(ctx context.Context, auths []wire.RegistryAuth, ref string) context.Context {
	if a := regcred.ForImage(auths, ref); a != nil {
		return context.WithValue(ctx, registryAuthKey{}, regcred.XRegistryAuth(*a))
	}
	return ctx
}

func (d *SocketDocker) do(ctx context.Context, method, path string, q url.Values) (*http.Response, error) {
	return d.doBody(ctx, method, path, q, nil)
}

// doBody is do with a JSON request body (nil for none).
func (d *SocketDocker) doBody(ctx context.Context, method, path string, q url.Values, body any) (*http.Response, error) {
	u := "http://docker" + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if h, ok := ctx.Value(registryAuthKey{}).(string); ok && h != "" {
		req.Header.Set("X-Registry-Auth", h)
	}
	resp, err := d.c.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		var e struct {
			Message string `json:"message"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&e)
		if e.Message == "" {
			e.Message = resp.Status
		}
		return nil, fmt.Errorf("docker: %s", e.Message)
	}
	return resp, nil
}

func (d *SocketDocker) getJSON(ctx context.Context, path string, q url.Values, v any) error {
	resp, err := d.do(ctx, http.MethodGet, path, q)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(v)
}

func (d *SocketDocker) Info(ctx context.Context) (wire.Hello, error) {
	var info struct {
		Name            string
		NCPU            int
		MemTotal        int64
		ServerVersion   string
		OperatingSystem string
		Architecture    string
	}
	if err := d.getJSON(ctx, "/info", nil, &info); err != nil {
		return wire.Hello{}, err
	}
	cg := d.cg != nil
	return wire.Hello{
		CgroupStats:   &cg,
		OwnProject:    d.OwnProject(ctx),
		Hostname:      info.Name,
		DockerVersion: info.ServerVersion,
		OS:            info.OperatingSystem,
		Arch:          info.Architecture,
		NCPU:          info.NCPU,
		MemTotal:      info.MemTotal,
	}, nil
}

func (d *SocketDocker) List(ctx context.Context) ([]wire.Container, error) {
	var raw []struct {
		ID      string `json:"Id"`
		Names   []string
		Image   string
		ImageID string
		State   string
		Status  string
		Created int64
		Labels  map[string]string
		Ports   []struct {
			IP          string
			PrivatePort int
			PublicPort  int
			Type        string
		}
	}
	if err := d.getJSON(ctx, "/containers/json", url.Values{"all": {"1"}}, &raw); err != nil {
		return nil, err
	}
	live := make(map[string]bool, len(raw))
	defer func() { d.forgetConfigImages(live) }()
	out := make([]wire.Container, 0, len(raw))
	for _, r := range raw {
		live[r.ID] = true
		// Once a newer image takes over the tag (another container pulled
		// it), Docker lists this container's image as a bare ID. Use the name
		// it was created from instead, so it still shows as nginx:1.27 and
		// update checks and automatic updates still recognise it.
		if looksLikeImageID(r.Image) {
			if ref := d.configImage(ctx, r.ID); ref != "" {
				r.Image = ref
			}
		}
		// A rolled-back service runs a tainerhub-rollback/ tag; show the
		// image name it really is (recorded on the container by the rollback).
		if from := r.Labels[wire.RollbackFromLabel]; from != "" && wire.IsRollbackTag(r.Image) {
			r.Image = from
		}
		name := r.ID[:min(12, len(r.ID))]
		if len(r.Names) > 0 {
			name = strings.TrimPrefix(r.Names[0], "/")
		}
		seen := map[string]bool{}
		var ports []string
		for _, p := range r.Ports {
			s := fmt.Sprintf("%d/%s", p.PrivatePort, p.Type)
			if p.PublicPort > 0 {
				s = fmt.Sprintf("%d->%s", p.PublicPort, s)
			}
			if !seen[s] {
				seen[s] = true
				ports = append(ports, s)
			}
		}
		sort.Strings(ports)
		out = append(out, wire.Container{
			ID:      r.ID,
			Name:    name,
			Image:   r.Image,
			State:   r.State,
			Status:  r.Status,
			Created: r.Created,
			Project: r.Labels["com.docker.compose.project"],
			// from the last update check (RunUpdateChecks / OpImagesCheckUpdates),
			// against the image this container runs
			UpdateAvailable: d.updateFor(ctx, r.Image, r.ImageID),
			Ports:           ports,
			Labels:          r.Labels,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (d *SocketDocker) Events(ctx context.Context, fn func(wire.DockerEvent)) error {
	filters, _ := json.Marshal(map[string][]string{"type": {"container"}})
	resp, err := d.do(ctx, http.MethodGet, "/events", url.Values{"filters": {string(filters)}})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	dec := json.NewDecoder(resp.Body)
	for {
		var ev struct {
			Type   string
			Action string
			Actor  struct {
				ID         string
				Attributes map[string]string
			}
			Time int64 `json:"time"`
		}
		if err := dec.Decode(&ev); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		if strings.HasPrefix(ev.Action, "exec_") { // noisy and not useful in the fleet view
			continue
		}
		fn(wire.DockerEvent{
			Type:   ev.Type,
			Action: ev.Action,
			ID:     ev.Actor.ID,
			Name:   ev.Actor.Attributes["name"],
			Time:   ev.Time,
		})
	}
}

func (d *SocketDocker) Action(ctx context.Context, id, action string) error {
	if !wire.ValidAction(action) {
		return fmt.Errorf("unknown action %q", action)
	}
	if !wire.ValidContainerRef(id) {
		return errors.New("invalid container id")
	}
	if takesDown(action) {
		if err := d.guardSelf(ctx, id); err != nil {
			return err
		}
	}
	id = url.PathEscape(id)
	var resp *http.Response
	var err error
	if action == wire.ActionRemove {
		resp, err = d.do(ctx, http.MethodDelete, "/containers/"+id, url.Values{"force": {"1"}})
	} else {
		resp, err = d.do(ctx, http.MethodPost, "/containers/"+id+"/"+action, nil)
	}
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil // 204 done, 304 already in that state
}

func (d *SocketDocker) Logs(ctx context.Context, id string, tail int, follow bool, w io.Writer) error {
	if !wire.ValidContainerRef(id) {
		return errors.New("invalid container id")
	}
	var insp struct{ Config struct{ Tty bool } }
	if err := d.getJSON(ctx, "/containers/"+url.PathEscape(id)+"/json", nil, &insp); err != nil {
		return err
	}
	q := url.Values{"stdout": {"1"}, "stderr": {"1"}, "tail": {"all"}}
	if tail > 0 {
		q.Set("tail", strconv.Itoa(tail))
	}
	if follow {
		q.Set("follow", "1")
	}
	resp, err := d.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/logs", q)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if insp.Config.Tty {
		_, err = io.Copy(w, resp.Body)
	} else {
		err = demux(resp.Body, w)
	}
	if errors.Is(err, io.EOF) || ctx.Err() != nil {
		return nil
	}
	return err
}

// Stats takes one sample. Without one-shot, the Engine samples twice, a
// second apart, to report a CPU delta. That doubles its work per container
// every round, so after the first sample this asks for one-shot and takes the
// delta from the previous sample instead.
func (d *SocketDocker) Stats(ctx context.Context, id string) (Usage, error) {
	if !wire.ValidContainerRef(id) {
		return Usage{}, errors.New("invalid container id")
	}
	if d.cg != nil && !d.useDocker.Load() {
		if u, ok := d.cg.Stats(id); ok {
			return u, nil
		}
	}
	var s struct {
		CPUStats struct {
			CPUUsage struct {
				TotalUsage uint64 `json:"total_usage"`
			} `json:"cpu_usage"`
			SystemUsage uint64 `json:"system_cpu_usage"`
			OnlineCPUs  uint32 `json:"online_cpus"`
		} `json:"cpu_stats"`
		PreCPUStats struct {
			CPUUsage struct {
				TotalUsage uint64 `json:"total_usage"`
			} `json:"cpu_usage"`
			SystemUsage uint64 `json:"system_cpu_usage"`
		} `json:"precpu_stats"`
		MemoryStats struct {
			Usage uint64            `json:"usage"`
			Limit uint64            `json:"limit"`
			Stats map[string]uint64 `json:"stats"`
		} `json:"memory_stats"`
	}
	d.cpuMu.Lock()
	prev, havePrev := d.lastCPU[id]
	d.cpuMu.Unlock()
	q := url.Values{"stream": {"false"}}
	if havePrev {
		q.Set("one-shot", "true")
	}
	if err := d.getJSON(ctx, "/containers/"+url.PathEscape(id)+"/stats", q, &s); err != nil {
		return Usage{}, err
	}
	preCPU, preSys := s.PreCPUStats.CPUUsage.TotalUsage, s.PreCPUStats.SystemUsage
	if preSys == 0 && havePrev { // one-shot: no "pre" sample from the Engine
		preCPU, preSys = prev.cpu, prev.sys
	}
	d.rememberCPU(id, cpuSample{cpu: s.CPUStats.CPUUsage.TotalUsage, sys: s.CPUStats.SystemUsage, at: time.Now()})
	return computeUsage(
		s.CPUStats.CPUUsage.TotalUsage, preCPU,
		s.CPUStats.SystemUsage, preSys, s.CPUStats.OnlineCPUs,
		s.MemoryStats.Usage, s.MemoryStats.Limit, s.MemoryStats.Stats,
	), nil
}

func (d *SocketDocker) rememberCPU(id string, c cpuSample) {
	d.cpuMu.Lock()
	defer d.cpuMu.Unlock()
	if d.lastCPU == nil {
		d.lastCPU = map[string]cpuSample{}
	}
	d.lastCPU[id] = c
	for k, v := range d.lastCPU { // forget removed containers
		if time.Since(v.at) > 10*time.Minute {
			delete(d.lastCPU, k)
		}
	}
}

// computeUsage applies the same formulas as `docker stats`.
func computeUsage(cpu, preCPU, sys, preSys uint64, online uint32, memUsage, memLimit uint64, memStats map[string]uint64) Usage {
	var u Usage
	if online == 0 {
		online = 1
	}
	if cpu > preCPU && sys > preSys {
		u.CPU = float64(cpu-preCPU) / float64(sys-preSys) * float64(online) * 100
	}
	cache := memStats["inactive_file"]                // cgroup v2
	if v, ok := memStats["total_inactive_file"]; ok { // cgroup v1
		cache = v
	}
	if memUsage > cache {
		memUsage -= cache
	}
	u.Mem = int64(memUsage)
	u.MemLimit = int64(memLimit)
	return u
}

// demux decodes Docker's multiplexed log stream: an 8-byte header
// [stream, 0, 0, 0, size(4, big endian)] followed by size bytes of payload.
func demux(r io.Reader, w io.Writer) error {
	br := bufio.NewReader(r)
	var hdr [8]byte
	for {
		if _, err := io.ReadFull(br, hdr[:]); err != nil {
			if errors.Is(err, io.ErrUnexpectedEOF) {
				return io.EOF
			}
			return err
		}
		n := int64(binary.BigEndian.Uint32(hdr[4:8]))
		if _, err := io.CopyN(w, br, n); err != nil {
			return err
		}
	}
}

// CheckSameMount verifies that dir is bind-mounted into the agent's container
// from the same path on the host (see StackRunner). Outside a container, dir
// is already a host path and there is nothing to check.
func (d *SocketDocker) CheckSameMount(ctx context.Context, dir string) error {
	if _, err := os.Stat("/.dockerenv"); err != nil {
		return nil
	}
	self, _ := os.Hostname() // Docker sets it to the container ID unless overridden
	var insp struct {
		Mounts []struct{ Type, Source, Destination string }
	}
	if !wire.ValidContainerRef(self) || d.getJSON(ctx, "/containers/"+url.PathEscape(self)+"/json", nil, &insp) != nil {
		return nil // custom hostname: can't find our own container, so trust the setup
	}
	dir = filepath.Clean(dir)
	for _, m := range insp.Mounts {
		if filepath.Clean(m.Destination) != dir {
			continue
		}
		if m.Type != "bind" || !sameHostPath(m.Source, dir) {
			return fmt.Errorf("the stacks folder must be bind-mounted from the same path on the host: add \"%s:%s\" to the agent's volumes (it is currently a %s mount from %q)", dir, dir, m.Type, m.Source)
		}
		return nil
	}
	return fmt.Errorf("the stacks folder is not mounted: add \"%s:%s\" to the agent's volumes and recreate the agent", dir, dir)
}

// sameHostPath reports whether a bind mount's source (as Docker reports it)
// is the same folder as dir. Docker Desktop reports host paths with a
// /host_mnt prefix, and on macOS /tmp and /var live under /private.
func sameHostPath(source, dir string) bool {
	source, dir = filepath.Clean(source), filepath.Clean(dir)
	if source == dir {
		return true
	}
	s := strings.TrimPrefix(source, "/host_mnt")
	return s == dir || s == "/private"+dir || strings.TrimPrefix(s, "/private") == dir
}

// looksLikeImageID reports whether a container list's Image field is an image
// ID ("sha256:…" or bare hex) rather than the name it was created from.
func looksLikeImageID(s string) bool {
	h := strings.TrimPrefix(s, "sha256:")
	if len(h) != 12 && len(h) != 64 {
		return false
	}
	for _, c := range h {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// configImage is the image reference a container was created with
// (Config.Image), cached per container: it never changes for a container.
func (d *SocketDocker) configImage(ctx context.Context, id string) string {
	d.cfgMu.Lock()
	ref, ok := d.cfgImages[id]
	d.cfgMu.Unlock()
	if ok {
		return ref
	}
	var insp struct{ Config struct{ Image string } }
	if err := d.getJSON(ctx, "/containers/"+url.PathEscape(id)+"/json", nil, &insp); err != nil {
		return ""
	}
	ref = insp.Config.Image
	if looksLikeImageID(ref) {
		ref = "" // really created from an ID: nothing better to show
	}
	d.cfgMu.Lock()
	if d.cfgImages == nil {
		d.cfgImages = map[string]string{}
	}
	d.cfgImages[id] = ref
	d.cfgMu.Unlock()
	return ref
}

// forgetConfigImages drops cached names of containers that no longer exist.
func (d *SocketDocker) forgetConfigImages(live map[string]bool) {
	d.cfgMu.Lock()
	defer d.cfgMu.Unlock()
	for id := range d.cfgImages {
		if !live[id] {
			delete(d.cfgImages, id)
		}
	}
}
