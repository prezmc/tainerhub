package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/prezmc/tainerhub/internal/wire"
)

// Inspect describes stack name on this host: what its folder holds and,
// when it's deployed here, what `compose config` says it uses. Volume sizes
// and labels are filled in by the caller from Docker.
func (r *StackRunner) Inspect(ctx context.Context, name string) (wire.StackInspect, error) {
	var out wire.StackInspect
	if !wire.ValidStackName(name) {
		return out, errors.New("invalid stack name")
	}
	if sup := r.Supported(ctx); sup.Error != "" {
		return out, errors.New(sup.Error)
	}
	dir := filepath.Join(r.Dir, name)
	out.Dir = dir
	out.Deployed = fileExists(filepath.Join(dir, composeFile))
	out.DirFiles, out.DirSize, out.DirSizePartial = scanStackDir(dir)
	if !out.Deployed {
		return out, nil
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	var stdout, stderr bytes.Buffer
	args := []string{"--project-name", name, "--file", filepath.Join(dir, composeFile), "--project-directory", dir}
	if fileExists(filepath.Join(dir, envFile)) {
		args = append(args, "--env-file", filepath.Join(dir, envFile))
	}
	cmd := exec.CommandContext(ctx, r.Bin, append(args, "config", "--format", "json")...)
	cmd.Env = r.env()
	cmd.Dir = r.Dir
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 500 {
			msg = msg[:500]
		}
		return out, fmt.Errorf("docker compose config: %v: %s", err, msg)
	}
	if err := parseComposeConfig(stdout.Bytes(), dir, &out); err != nil {
		return out, err
	}
	return out, nil
}

// scanStackDir counts and sizes what's in a stack folder besides
// TainerHub's own files. partial is true when some of it couldn't be read.
func scanStackDir(dir string) (files int, size int64, partial bool) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return 0, 0, !errors.Is(err, fs.ErrNotExist)
	}
	for _, e := range ents {
		n := e.Name()
		if n == composeFile || n == envFile || n == stagingDir || n == manifestFile || strings.HasPrefix(n, incomingPrefix) {
			continue
		}
		files++
	}
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			partial = true
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				size += info.Size()
			}
		}
		return nil
	})
	return files, size, partial
}

// composeConfig is the part of `docker compose config --format json` used here.
type composeConfig struct {
	Services map[string]struct {
		Image       string          `json:"image"`
		Build       json.RawMessage `json:"build"`
		NetworkMode string          `json:"network_mode"`
		Devices     json.RawMessage `json:"devices"`
		Ports       []struct {
			Published json.RawMessage `json:"published"` // a string, or a number in older compose versions
			Protocol  string          `json:"protocol"`
		} `json:"ports"`
		Volumes []struct {
			Type   string `json:"type"`
			Source string `json:"source"`
			Target string `json:"target"`
		} `json:"volumes"`
	} `json:"services"`
	Volumes map[string]struct {
		Name       string            `json:"name"`
		External   bool              `json:"external"`
		Driver     string            `json:"driver"`
		DriverOpts map[string]string `json:"driver_opts"`
	} `json:"volumes"`
}

func parseComposeConfig(b []byte, dir string, out *wire.StackInspect) error {
	var cfg composeConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		return fmt.Errorf("unreadable compose config: %w", err)
	}
	names := make([]string, 0, len(cfg.Services))
	for n := range cfg.Services {
		names = append(names, n)
	}
	sort.Strings(names)
	ports := map[string]bool{}
	for _, n := range names {
		svc := cfg.Services[n]
		devices := len(svc.Devices) > 0 && string(svc.Devices) != "null" && string(svc.Devices) != "[]"
		out.Services = append(out.Services, wire.StackService{
			Name: n, Image: svc.Image, Build: len(svc.Build) > 0 && string(svc.Build) != "null",
			HostNetwork: svc.NetworkMode == "host", Devices: devices,
		})
		for _, p := range svc.Ports {
			pub := strings.Trim(string(p.Published), `"`)
			if pub == "" || pub == "null" || pub == "0" {
				continue // not published on the host
			}
			proto := p.Protocol
			if proto == "" {
				proto = "tcp"
			}
			ports[pub+"/"+proto] = true
		}
		for _, v := range svc.Volumes {
			if v.Type != "bind" {
				continue
			}
			in := v.Source == dir || strings.HasPrefix(v.Source, dir+"/")
			out.Binds = append(out.Binds, wire.StackBind{Service: n, Source: v.Source, Target: v.Target, InStackDir: in})
		}
	}
	for p := range ports {
		out.Ports = append(out.Ports, p)
	}
	sort.Strings(out.Ports)
	keys := make([]string, 0, len(cfg.Volumes))
	for k := range cfg.Volumes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := cfg.Volumes[k]
		out.Volumes = append(out.Volumes, wire.StackVolume{
			Key: k, Name: v.Name, External: v.External, Driver: v.Driver, Options: len(v.DriverOpts) > 0, Size: -1,
		})
	}
	return nil
}
