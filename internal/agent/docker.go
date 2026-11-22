package agent

import (
	"bufio"
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
	"time"

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
	c *http.Client
}

func NewSocketDocker(socketPath string) *SocketDocker {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socketPath)
		},
		MaxIdleConns:    4,
		IdleConnTimeout: 30 * time.Second,
	}
	return &SocketDocker{c: &http.Client{Transport: tr}}
}

func (d *SocketDocker) do(ctx context.Context, method, path string, q url.Values) (*http.Response, error) {
	u := "http://docker" + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return nil, err
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
	return wire.Hello{
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
	out := make([]wire.Container, 0, len(raw))
	for _, r := range raw {
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
			Ports:   ports,
			Labels:  r.Labels,
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

// Stats takes one sample. Without one-shot, the Engine waits about a second
// so that it can report a CPU delta.
func (d *SocketDocker) Stats(ctx context.Context, id string) (Usage, error) {
	if !wire.ValidContainerRef(id) {
		return Usage{}, errors.New("invalid container id")
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
	if err := d.getJSON(ctx, "/containers/"+url.PathEscape(id)+"/stats", url.Values{"stream": {"false"}}, &s); err != nil {
		return Usage{}, err
	}
	return computeUsage(
		s.CPUStats.CPUUsage.TotalUsage, s.PreCPUStats.CPUUsage.TotalUsage,
		s.CPUStats.SystemUsage, s.PreCPUStats.SystemUsage, s.CPUStats.OnlineCPUs,
		s.MemoryStats.Usage, s.MemoryStats.Limit, s.MemoryStats.Stats,
	), nil
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
