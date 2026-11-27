package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prezmc/tainerhub/internal/wire"
)

// Checking a new stack against this host before its first deploy
// (OpStackPreflight). Nothing here changes the host: compose only resolves
// the files, and Docker only answers questions about the host and the
// images' registries.

// preflightConfig is the part of `docker compose config --format json` the
// preflight reads.
type preflightConfig struct {
	Services map[string]struct {
		Image         string          `json:"image"`
		ContainerName string          `json:"container_name"`
		Build         json.RawMessage `json:"build"`
		NetworkMode   string          `json:"network_mode"`
		Runtime       string          `json:"runtime"`
		Ports         []struct {
			HostIP    string          `json:"host_ip"`
			Published json.RawMessage `json:"published"`
			Target    json.RawMessage `json:"target"`
			Protocol  string          `json:"protocol"`
		} `json:"ports"`
		CPUs     json.RawMessage   `json:"cpus"`
		MemLimit json.RawMessage   `json:"mem_limit"`
		Devices  []json.RawMessage `json:"devices"`
		GPUs     json.RawMessage   `json:"gpus"`
		Deploy   *struct {
			Resources struct {
				Limits struct {
					CPUs   json.RawMessage `json:"cpus"`
					Memory json.RawMessage `json:"memory"`
				} `json:"limits"`
				Reservations struct {
					Devices []struct {
						Driver       string   `json:"driver"`
						Capabilities []string `json:"capabilities"`
					} `json:"devices"`
				} `json:"reservations"`
			} `json:"resources"`
		} `json:"deploy"`
		Volumes []struct {
			Type   string `json:"type"`
			Source string `json:"source"`
			Target string `json:"target"`
		} `json:"volumes"`
		Environment map[string]*string `json:"environment"`
	} `json:"services"`
	Networks map[string]struct {
		Name     string `json:"name"`
		External bool   `json:"external"`
	} `json:"networks"`
	Volumes map[string]struct {
		Name     string `json:"name"`
		External bool   `json:"external"`
	} `json:"volumes"`
}

// rawString is a JSON string or number as text ("" for null or absent).
func rawString(b json.RawMessage) string {
	s := strings.TrimSpace(string(b))
	if s == "" || s == "null" {
		return ""
	}
	if strings.HasPrefix(s, `"`) {
		var v string
		if json.Unmarshal(b, &v) == nil {
			return v
		}
		return ""
	}
	return s
}

func rawFloat(b json.RawMessage) float64 {
	f, _ := strconv.ParseFloat(rawString(b), 64)
	return f
}

// rawBytes reads a size compose has normalised to bytes ("536870912").
func rawBytes(b json.RawMessage) int64 {
	n, _ := strconv.ParseInt(rawString(b), 10, 64)
	return n
}

// parsePreflight reads a resolved compose config. dir is the stack's folder.
func parsePreflight(b []byte, dir string, out *wire.StackPreflight) error {
	var cfg preflightConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		return fmt.Errorf("unreadable compose config: %w", err)
	}
	names := make([]string, 0, len(cfg.Services))
	for n := range cfg.Services {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		s := cfg.Services[n]
		ps := wire.PreflightService{
			Name: n, ContainerName: s.ContainerName, Image: s.Image,
			Build:       len(s.Build) > 0 && string(s.Build) != "null",
			HostNetwork: s.NetworkMode == "host",
			NetworkOf:   wire.SharedNetworkTarget(s.NetworkMode),
			CPUs:        rawFloat(s.CPUs),
			Memory:      rawBytes(s.MemLimit),
		}
		if s.Deploy != nil {
			if ps.CPUs == 0 {
				ps.CPUs = rawFloat(s.Deploy.Resources.Limits.CPUs)
			}
			if ps.Memory == 0 {
				ps.Memory = rawBytes(s.Deploy.Resources.Limits.Memory)
			}
			for _, d := range s.Deploy.Resources.Reservations.Devices {
				if d.Driver == "nvidia" || contains(d.Capabilities, "gpu") {
					ps.GPU = true
				}
			}
		}
		if g := strings.TrimSpace(string(s.GPUs)); g != "" && g != "null" && g != "[]" {
			ps.GPU = true
		}
		if s.Runtime == "nvidia" {
			ps.GPU = true
		}
		for _, p := range s.Ports {
			proto := p.Protocol
			if proto == "" {
				proto = "tcp"
			}
			ps.Ports = append(ps.Ports, wire.PreflightPort{HostIP: p.HostIP, Published: rawString(p.Published), Target: rawString(p.Target), Protocol: proto})
		}
		for _, d := range s.Devices {
			var obj struct{ Source string }
			var str string
			switch {
			case json.Unmarshal(d, &obj) == nil && obj.Source != "":
				ps.Devices = append(ps.Devices, obj.Source)
			case json.Unmarshal(d, &str) == nil && str != "":
				src, _, _ := strings.Cut(str, ":")
				ps.Devices = append(ps.Devices, src)
			}
		}
		for _, v := range s.Volumes {
			if v.Type != "bind" {
				continue
			}
			in := v.Source == dir || strings.HasPrefix(v.Source, dir+"/")
			ps.Binds = append(ps.Binds, wire.StackBind{Service: n, Source: v.Source, Target: v.Target, InStackDir: in})
		}
		for _, k := range sortedKeys(s.Environment) {
			if v := s.Environment[k]; v != nil && wire.LooksLikePlaceholder(*v) {
				ps.PlaceholderEnv = append(ps.PlaceholderEnv, k)
			}
		}
		out.Services = append(out.Services, ps)
	}
	for _, k := range sortedKeys(cfg.Networks) {
		if n := cfg.Networks[k]; n.External {
			out.Networks = append(out.Networks, wire.PreflightResource{Name: firstNonEmpty(n.Name, k)})
		}
	}
	for _, k := range sortedKeys(cfg.Volumes) {
		if v := cfg.Volumes[k]; v.External {
			out.Volumes = append(out.Volumes, wire.PreflightResource{Name: firstNonEmpty(v.Name, k)})
		}
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// preflightDocker is what the preflight asks Docker; SocketDocker has it.
type preflightDocker interface {
	preflightHost(ctx context.Context) (hostFacts, error)
	checkImage(ctx context.Context, ref string, auths []wire.RegistryAuth) wire.ImageCheck
}

type hostFacts struct {
	Arch     string
	GPU      bool
	Networks map[string]bool
	Volumes  map[string]bool
}

// dockerArch turns `docker info`'s architecture (uname -m) into the name
// images use for platforms.
func dockerArch(a string) string {
	switch a {
	case "x86_64", "amd64":
		return "amd64"
	case "aarch64", "arm64":
		return "arm64"
	case "armv7l", "armhf", "arm":
		return "arm/v7"
	case "armv6l":
		return "arm/v6"
	case "i386", "i686":
		return "386"
	}
	return a
}

func (d *SocketDocker) preflightHost(ctx context.Context) (hostFacts, error) {
	var info struct {
		Architecture      string
		Runtimes          map[string]json.RawMessage
		DiscoveredDevices []struct{ Source, ID string }
	}
	if err := d.getJSON(ctx, "/info", nil, &info); err != nil {
		return hostFacts{}, err
	}
	f := hostFacts{Arch: dockerArch(info.Architecture), Networks: map[string]bool{}, Volumes: map[string]bool{}}
	_, f.GPU = info.Runtimes["nvidia"]
	for _, dev := range info.DiscoveredDevices {
		if strings.HasPrefix(dev.ID, "nvidia.com/gpu") {
			f.GPU = true
		}
	}
	var nets []struct{ Name string }
	if err := d.getJSON(ctx, "/networks", nil, &nets); err != nil {
		return f, err
	}
	for _, n := range nets {
		f.Networks[n.Name] = true
	}
	var vols struct{ Volumes []struct{ Name string } }
	if err := d.getJSON(ctx, "/volumes", nil, &vols); err != nil {
		return f, err
	}
	for _, v := range vols.Volumes {
		f.Volumes[v.Name] = true
	}
	return f, nil
}

// imageChecks remembers registry answers for a few minutes: the New
// container page checks as you type, and registries limit lookups.
var imageChecks = struct {
	sync.Mutex
	m map[string]cachedImageCheck
}{m: map[string]cachedImageCheck{}}

type cachedImageCheck struct {
	c  wire.ImageCheck
	at time.Time
}

const imageCheckTTL = 5 * time.Minute

func (d *SocketDocker) checkImage(ctx context.Context, ref string, auths []wire.RegistryAuth) wire.ImageCheck {
	var out wire.ImageCheck
	var local struct{ ID string }
	out.Local = d.getJSON(ctx, "/images/"+url.PathEscape(ref)+"/json", nil, &local) == nil && local.ID != ""

	ctx = withRegistryAuth(ctx, auths, ref)
	key := ref
	if _, ok := ctx.Value(registryAuthKey{}).(string); ok {
		key += "\x00auth"
	}
	imageChecks.Lock()
	c, ok := imageChecks.m[key]
	imageChecks.Unlock()
	if ok && time.Since(c.at) < imageCheckTTL {
		c.c.Local = out.Local
		return c.c
	}

	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var remote struct {
		Platforms []struct {
			Architecture string `json:"architecture"`
			OS           string `json:"os"`
			Variant      string `json:"variant"`
		}
	}
	err := d.getJSON(ctx, "/distribution/"+url.PathEscape(ref)+"/json", nil, &remote)
	out.Remote, out.Error = classifyRegistryError(err)
	for _, p := range remote.Platforms {
		if p.OS == "" || p.Architecture == "" || p.Architecture == "unknown" {
			continue // attestation manifests
		}
		s := p.OS + "/" + p.Architecture
		if p.Variant != "" {
			s += "/" + p.Variant
		}
		if !contains(out.Platforms, s) {
			out.Platforms = append(out.Platforms, s)
		}
	}
	if out.Remote != "error" { // don't remember a registry that was down
		imageChecks.Lock()
		if len(imageChecks.m) > 500 {
			imageChecks.m = map[string]cachedImageCheck{}
		}
		imageChecks.m[key] = cachedImageCheck{c: out, at: time.Now()}
		imageChecks.Unlock()
	}
	return out
}

// classifyRegistryError sorts Docker's answer to a registry lookup.
func classifyRegistryError(err error) (remote, msg string) {
	if err == nil {
		return "found", ""
	}
	msg = strings.TrimPrefix(err.Error(), "docker: ")
	if len(msg) > 300 {
		msg = msg[:300]
	}
	low := strings.ToLower(msg)
	switch {
	case strings.Contains(low, "manifest unknown"), strings.Contains(low, "not found"), strings.Contains(low, "no such"), strings.Contains(low, "does not exist"):
		return "not_found", msg
	case strings.Contains(low, "denied"), strings.Contains(low, "unauthorized"), strings.Contains(low, "authentication required"):
		// Docker Hub answers this for repositories that don't exist, too.
		return "denied", msg
	}
	return "error", msg
}

// Preflight resolves files for new stack name and checks them against
// this host.
func (r *StackRunner) Preflight(ctx context.Context, name string, f wire.StackFiles) (wire.StackPreflight, error) {
	out := wire.StackPreflight{Dir: filepath.Join(r.Dir, name)}
	cfg, warnings, cfgErr, err := r.config(ctx, name, f)
	if err != nil {
		return out, err
	}
	out.Warnings = warnings
	if cfgErr != "" {
		out.ConfigError = cfgErr
		return out, nil
	}
	if err := parsePreflight(cfg, out.Dir, &out); err != nil {
		out.ConfigError = err.Error()
	}
	return out, nil
}

// preflightStack answers OpStackPreflight.
func (s *session) preflightStack(ctx context.Context, name string, f wire.StackFiles, auths []wire.RegistryAuth) (wire.StackPreflight, error) {
	if s.a.cfg.Stacks == nil {
		return wire.StackPreflight{}, errors.New("stacks aren't set up on this host")
	}
	out, err := s.a.cfg.Stacks.Preflight(ctx, name, f)
	if err != nil || out.ConfigError != "" {
		return out, err
	}
	d, ok := s.a.cfg.Docker.(preflightDocker)
	if !ok {
		return out, nil
	}
	facts, err := d.preflightHost(ctx)
	if err != nil {
		return out, fmt.Errorf("ask Docker about the host: %w", err)
	}
	fillPreflight(ctx, &out, facts, d, auths)
	return out, nil
}

// fillPreflight adds what the host and registries say to a parsed preflight.
func fillPreflight(ctx context.Context, out *wire.StackPreflight, facts hostFacts, d preflightDocker, auths []wire.RegistryAuth) {
	out.Arch, out.GPURuntime = facts.Arch, facts.GPU
	for i := range out.Networks {
		out.Networks[i].Exists = facts.Networks[out.Networks[i].Name]
	}
	for i := range out.Volumes {
		out.Volumes[i].Exists = facts.Volumes[out.Volumes[i].Name]
	}
	checked := map[string]*wire.ImageCheck{}
	for i := range out.Services {
		s := &out.Services[i]
		if s.Image == "" || s.Build { // built here: nothing to look up
			continue
		}
		if !wire.ValidImageRef(s.Image) {
			s.ImageCheck = &wire.ImageCheck{Remote: "error", Error: "not a valid image reference"}
			continue
		}
		if c, ok := checked[s.Image]; ok {
			s.ImageCheck = c
			continue
		}
		c := d.checkImage(ctx, s.Image, auths)
		s.ImageCheck = &c
		checked[s.Image] = &c
	}
}
