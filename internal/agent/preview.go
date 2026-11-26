package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/prezmc/tainerhub/internal/wire"
)

// Preview resolves a compose file and .env for stack name as if they were
// in the stack's folder (see config).
func (r *StackRunner) Preview(ctx context.Context, name string, f wire.StackFiles) (wire.StackPreview, error) {
	var out wire.StackPreview
	dir := filepath.Join(r.Dir, name)
	out.Dir = dir
	cfg, warnings, cfgErr, err := r.config(ctx, name, f)
	if err != nil {
		return out, err
	}
	out.DirFiles, _, _ = scanStackDir(dir)
	out.Warnings = warnings
	if cfgErr != "" {
		out.ConfigError = cfgErr
		return out, nil
	}
	var in wire.StackInspect
	if err := parseComposeConfig(cfg, dir, &in); err != nil {
		out.ConfigError = err.Error()
		return out, nil
	}
	out.Services = in.Services
	out.Mounts, err = parseMounts(cfg)
	if err != nil {
		out.ConfigError = err.Error()
		return out, nil
	}
	for _, m := range out.Mounts {
		if m.Type != "bind" || (m.Source != dir && !strings.HasPrefix(m.Source, dir+"/")) {
			continue
		}
		if out.Paths == nil {
			out.Paths = map[string]int{}
		}
		out.Paths[m.Source] = pathEntries(m.Source)
	}
	return out, nil
}

// config runs `compose config --format json` on files for stack name as if
// they were in the stack's folder, without writing anything there. The files
// go to a temporary folder; --project-directory points relative paths at the
// stack folder, as a deploy would. cfgErr is compose's complaint when it
// rejects the files; err is for not being able to ask at all.
func (r *StackRunner) config(ctx context.Context, name string, f wire.StackFiles) (cfg []byte, warnings []string, cfgErr string, err error) {
	if sup := r.Supported(ctx); sup.Error != "" {
		return nil, nil, "", errors.New(sup.Error)
	}
	dir := filepath.Join(r.Dir, name)
	tmpRoot := filepath.Join(r.WorkDir, "tmp")
	if err := os.MkdirAll(tmpRoot, 0o700); err != nil {
		return nil, nil, "", err
	}
	tmp, err := os.MkdirTemp(tmpRoot, "preview-")
	if err != nil {
		return nil, nil, "", err
	}
	defer os.RemoveAll(tmp)
	if err := os.WriteFile(filepath.Join(tmp, composeFile), []byte(f.Compose), 0o600); err != nil {
		return nil, nil, "", err
	}
	if err := os.WriteFile(filepath.Join(tmp, envFile), []byte(f.Env), 0o600); err != nil {
		return nil, nil, "", err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, r.Bin, "--project-name", name,
		"--file", filepath.Join(tmp, composeFile), "--project-directory", dir,
		"--env-file", filepath.Join(tmp, envFile), "config", "--format", "json")
	cmd.Env = r.env()
	cmd.Dir = r.Dir
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	warnings = composeWarnings(stderr.String())
	if runErr != nil {
		msg := strings.TrimSpace(composeErrors(stderr.String()))
		if msg == "" {
			msg = runErr.Error()
		}
		if len(msg) > 1000 {
			msg = msg[:1000]
		}
		return nil, warnings, msg, nil
	}
	return stdout.Bytes(), warnings, "", nil
}

// pathEntries is -1 if p doesn't exist, the number of entries for a folder,
// and 1 for anything else.
func pathEntries(p string) int {
	st, err := os.Stat(p)
	if err != nil {
		return -1
	}
	if !st.IsDir() {
		return 1
	}
	ents, err := os.ReadDir(p)
	if err != nil {
		return 1 // there, but unreadable to the agent: assume it holds something
	}
	return len(ents)
}

// composeWarnings picks compose's warning lines out of its stderr.
func composeWarnings(stderr string) []string {
	var out []string
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		low := strings.ToLower(line)
		if !strings.Contains(low, "warn") {
			continue
		}
		// `WARN[0000] The "X" variable is not set...` or `level=warning msg="..."`
		if i := strings.Index(line, "] "); i >= 0 && strings.HasPrefix(line, "WARN") {
			line = line[i+2:]
		}
		if i := strings.Index(line, `msg="`); i >= 0 {
			line = strings.ReplaceAll(strings.TrimSuffix(line[i+5:], `"`), `\"`, `"`)
		}
		out = append(out, line)
	}
	return out
}

// composeErrors is compose's stderr without its warning lines.
func composeErrors(stderr string) string {
	var keep []string
	for _, line := range strings.Split(stderr, "\n") {
		low := strings.ToLower(line)
		if strings.Contains(low, "level=warning") || strings.HasPrefix(strings.TrimSpace(line), "WARN") {
			continue
		}
		keep = append(keep, line)
	}
	return strings.Join(keep, "\n")
}

// parseMounts lists each service's bind and volume mounts from
// `compose config --format json`, with volumes as their Docker names.
func parseMounts(b []byte) ([]wire.StackMount, error) {
	var cfg struct {
		Services map[string]struct {
			Volumes []struct {
				Type   string `json:"type"`
				Source string `json:"source"`
				Target string `json:"target"`
			} `json:"volumes"`
		} `json:"services"`
		Volumes map[string]struct {
			Name string `json:"name"`
		} `json:"volumes"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("unreadable compose config: %w", err)
	}
	var out []wire.StackMount
	for svc, s := range cfg.Services {
		for _, v := range s.Volumes {
			switch v.Type {
			case "bind":
				out = append(out, wire.StackMount{Service: svc, Type: "bind", Source: v.Source, Target: v.Target})
			case "volume":
				src := v.Source
				if tv, ok := cfg.Volumes[v.Source]; ok && tv.Name != "" {
					src = tv.Name
				}
				if src == "" {
					continue // anonymous volume
				}
				out = append(out, wire.StackMount{Service: svc, Type: "volume", Source: src, Target: v.Target})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Service != out[j].Service {
			return out[i].Service < out[j].Service
		}
		return out[i].Target < out[j].Target
	})
	return out, nil
}

// previewStack answers OpStackPreview: the pasted files, resolved, and the
// project as it runs now.
func (s *session) previewStack(ctx context.Context, name string, f wire.StackFiles) (wire.StackPreview, error) {
	if s.a.cfg.Stacks == nil {
		return wire.StackPreview{}, errors.New("stacks aren't set up on this host")
	}
	out, err := s.a.cfg.Stacks.Preview(ctx, name, f)
	if err != nil {
		return out, err
	}
	cs, err := s.a.cfg.Docker.List(ctx)
	if err != nil {
		return out, err
	}
	for _, c := range cs {
		if c.Project != name {
			continue
		}
		rc := wire.RunningContainer{Name: c.Name, Service: c.Labels["com.docker.compose.service"], Mounts: []wire.DetailMount{}}
		if out.WorkingDir == "" {
			out.WorkingDir = c.Labels["com.docker.compose.project.working_dir"]
		}
		if out.ConfigFiles == nil {
			if files := c.Labels["com.docker.compose.project.config_files"]; files != "" {
				out.ConfigFiles = strings.Split(files, ",")
			}
		}
		raw, err := s.a.cfg.Docker.Operate(ctx, wire.Operation{Kind: wire.OpContainerInspect, Target: c.ID})
		if err != nil {
			return out, fmt.Errorf("inspect %s: %w", c.Name, err)
		}
		if d, ok := raw.(wire.ContainerDetail); ok {
			rc.Mounts = append(rc.Mounts, d.Mounts...)
		}
		out.Running = append(out.Running, rc)
	}
	sort.Slice(out.Running, func(i, j int) bool { return out.Running[i].Name < out.Running[j].Name })
	return out, nil
}
