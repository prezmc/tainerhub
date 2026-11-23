package agent

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/prezmc/tainerhub/internal/wire"
)

// Taking over a project in place: rather than asking for the compose file,
// the agent reads the project's own files from the folder it was started
// in. The agent can't see that folder, so a helper container (its own
// image, which has docker compose) mounts it read-only at the same path and
// runs `docker compose config`, which merges override files and turns
// relative paths absolute. Bind mounts then keep pointing at the data where
// it is. The helper has no network and no Docker socket; `config` needs
// neither.

// envFileDir is where copies of a taken-over project's env_file files go,
// inside the stack's folder.
const envFileDir = "env"

// maxProjectFile bounds each file read from a project's folder.
const maxProjectFile = 1 << 20

// composeRun is the outcome of one helper run.
type composeRun struct {
	Stdout, Stderr string
	Exit           int
}

// ProjectReader runs docker compose against host folders the agent can't
// see. SocketDocker implements it.
type ProjectReader interface {
	// ComposeInFolders runs the agent's docker compose with args in a helper
	// container that has dirs mounted read-only at their own paths and
	// workDir as its working folder. read returns a file from those
	// folders; done removes the helper.
	ComposeInFolders(ctx context.Context, workDir string, dirs, args []string) (run composeRun, read func(context.Context, string) ([]byte, error), done func(), err error)
}

func (d *SocketDocker) ComposeInFolders(ctx context.Context, workDir string, dirs, args []string) (composeRun, func(context.Context, string) ([]byte, error), func(), error) {
	var run composeRun
	image, err := d.HelperImage(ctx)
	if err != nil {
		return run, nil, nil, err
	}
	mounts := make([]map[string]any, 0, len(dirs))
	for _, dir := range dirs {
		mounts = append(mounts, map[string]any{"Type": "bind", "Source": dir, "Target": dir, "ReadOnly": true})
	}
	body := map[string]any{
		"Image":           image,
		"Entrypoint":      []string{"/usr/local/bin/docker-compose"},
		"Cmd":             args,
		"WorkingDir":      workDir,
		"Env":             []string{"HOME=/tmp", "DOCKER_CONFIG=/tmp/.docker", "COMPOSE_ANSI=never"},
		"Labels":          map[string]string{helperLabel: "read-project"},
		"NetworkDisabled": true,
		"HostConfig": map[string]any{
			"Mounts":      mounts,
			"NetworkMode": "none",
			"SecurityOpt": []string{"no-new-privileges:true"},
		},
	}
	resp, err := d.doBody(ctx, http.MethodPost, "/containers/create", nil, body)
	if err != nil {
		return run, nil, nil, fmt.Errorf("create helper: %w", err)
	}
	var created struct{ Id string }
	if err := jsonDecode(resp, &created); err != nil || created.Id == "" {
		return run, nil, nil, fmt.Errorf("create helper: %v", err)
	}
	id := created.Id
	done := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = d.RemoveContainer(ctx, id)
	}
	fail := func(err error) (composeRun, func(context.Context, string) ([]byte, error), func(), error) {
		done()
		return run, nil, nil, err
	}
	if resp, err := d.do(ctx, http.MethodPost, "/containers/"+id+"/start", nil); err != nil {
		return fail(fmt.Errorf("start helper: %w", err))
	} else {
		resp.Body.Close()
	}
	wctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	resp, err = d.do(wctx, http.MethodPost, "/containers/"+id+"/wait", nil)
	if err != nil {
		return fail(fmt.Errorf("wait for helper: %w", err))
	}
	var waited struct{ StatusCode int }
	if err := jsonDecode(resp, &waited); err != nil {
		return fail(err)
	}
	run.Exit = waited.StatusCode
	for _, s := range []struct {
		q   string
		dst *string
	}{{"stdout", &run.Stdout}, {"stderr", &run.Stderr}} {
		resp, err := d.do(ctx, http.MethodGet, "/containers/"+id+"/logs", url.Values{s.q: {"1"}})
		if err != nil {
			return fail(fmt.Errorf("helper output: %w", err))
		}
		var buf bytes.Buffer
		err = demux(io.LimitReader(resp.Body, 4<<20), &buf)
		resp.Body.Close()
		if err != nil && !errors.Is(err, io.EOF) {
			return fail(fmt.Errorf("helper output: %w", err))
		}
		*s.dst = buf.String()
	}
	read := func(ctx context.Context, p string) ([]byte, error) {
		rc, err := d.ArchiveGet(ctx, id, p)
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		tr := tar.NewReader(rc)
		h, err := tr.Next()
		if err != nil {
			return nil, err
		}
		if h.Typeflag != tar.TypeReg {
			return nil, fmt.Errorf("%s isn't a regular file", p)
		}
		if h.Size > maxProjectFile {
			return nil, fmt.Errorf("%s is larger than 1 MB", p)
		}
		return io.ReadAll(io.LimitReader(tr, maxProjectFile))
	}
	return run, read, done, nil
}

// safeHostDir accepts the absolute, clean host folders a helper may mount:
// never the root, nor a system folder.
func safeHostDir(p string) bool {
	if !filepath.IsAbs(p) || filepath.Clean(p) != p || p == "/" {
		return false
	}
	for _, sys := range []string{"/proc", "/sys", "/dev", "/boot", "/etc", "/bin", "/sbin", "/usr", "/lib", "/lib64", "/var/run", "/run"} {
		if p == sys || strings.HasPrefix(p, sys+"/") {
			return false
		}
	}
	return true
}

// readProject answers OpStackRead.
func (s *session) readProject(ctx context.Context, name string) (wire.StackSource, error) {
	var out wire.StackSource
	r := s.a.cfg.Stacks
	if r == nil {
		return out, errors.New("stacks aren't set up on this host")
	}
	if sup := r.Supported(ctx); sup.Error != "" {
		return out, errors.New(sup.Error)
	}
	pr, ok := s.a.cfg.Docker.(ProjectReader)
	if !ok {
		return out, errors.New("this agent can't read project folders")
	}
	cs, err := s.a.cfg.Docker.List(ctx)
	if err != nil {
		return out, err
	}
	var labels map[string]string
	for _, c := range cs {
		if c.Project == name {
			labels = c.Labels
			break
		}
	}
	if labels == nil {
		return out, fmt.Errorf("no containers of a project called %s on this host", name)
	}
	out.WorkingDir = labels["com.docker.compose.project.working_dir"]
	if f := labels["com.docker.compose.project.config_files"]; f != "" {
		out.ConfigFiles = strings.Split(f, ",")
	}
	if out.WorkingDir == "" || len(out.ConfigFiles) == 0 {
		return out, errors.New("its containers don't say where it was started from (an old compose version?); paste its compose file instead")
	}
	out.EnvFile = labels["com.docker.compose.project.environment_file"]
	if strings.Contains(out.EnvFile, ",") {
		return out, errors.New("it was started with several --env-file files; paste its compose file and .env instead")
	}
	return readProjectFiles(ctx, pr, r.Dir, name, out)
}

// readProjectFiles merges the project's files with compose in a helper,
// copies its env_file files into the stack folder, and reads its .env.
func readProjectFiles(ctx context.Context, pr ProjectReader, stacksDir, name string, out wire.StackSource) (wire.StackSource, error) {
	dirs := map[string]bool{out.WorkingDir: true}
	for _, f := range out.ConfigFiles {
		dirs[filepath.Dir(f)] = true
	}
	if out.EnvFile != "" {
		dirs[filepath.Dir(out.EnvFile)] = true
	}
	for d := range dirs {
		if !safeHostDir(d) {
			return out, fmt.Errorf("won't read from %s", d)
		}
	}

	args := []string{"--project-name", name, "--project-directory", out.WorkingDir}
	for _, f := range out.ConfigFiles {
		args = append(args, "--file", f)
	}
	if out.EnvFile != "" {
		args = append(args, "--env-file", out.EnvFile)
	}
	// env_file files can live outside these folders; compose says which one
	// it couldn't find, and that folder is mounted too on the next try.
	var yamlRun composeRun
	for try := 0; ; try++ {
		run, _, done, err := pr.ComposeInFolders(ctx, out.WorkingDir, topDirs(dirs), append(slicesClone(args), "config", "--no-interpolate"))
		if err != nil {
			return out, err
		}
		done()
		yamlRun = run
		if run.Exit == 0 {
			break
		}
		missing := missingEnvFile(run.Stderr)
		if missing == "" || try >= 8 || dirs[filepath.Dir(missing)] {
			return out, fmt.Errorf("docker compose config in %s: %s", out.WorkingDir, strings.TrimSpace(composeErrors(run.Stderr)))
		}
		if !safeHostDir(filepath.Dir(missing)) {
			return out, fmt.Errorf("won't read the env file %s", missing)
		}
		dirs[filepath.Dir(missing)] = true
	}
	top := topDirs(dirs)
	out.Warnings = composeWarnings(yamlRun.Stderr)

	// The same in JSON, to find env_file paths; its helper also reads the
	// files. With --no-interpolate compose doesn't open env files, so one
	// outside the folders mounted so far only shows up here: then the
	// helper runs again with its folder mounted, so it can be read.
	var (
		cfg struct {
			Services map[string]struct {
				EnvFile []struct {
					Path string `json:"path"`
				} `json:"env_file"`
			} `json:"services"`
		}
		read     func(context.Context, string) ([]byte, error)
		envPaths map[string]bool
	)
	for try := 0; ; try++ {
		jsonRun, rd, done, err := pr.ComposeInFolders(ctx, out.WorkingDir, top, append(slicesClone(args), "config", "--no-interpolate", "--format", "json"))
		if err != nil {
			return out, err
		}
		if jsonRun.Exit != 0 {
			done()
			return out, fmt.Errorf("docker compose config in %s: %s", out.WorkingDir, strings.TrimSpace(composeErrors(jsonRun.Stderr)))
		}
		if err := json.Unmarshal([]byte(jsonRun.Stdout), &cfg); err != nil {
			done()
			return out, fmt.Errorf("unreadable compose config: %w", err)
		}
		envPaths = map[string]bool{}
		added := false
		for _, svc := range cfg.Services {
			for _, e := range svc.EnvFile {
				envPaths[e.Path] = true
				d := filepath.Dir(e.Path)
				if !underAny(d, top) {
					if !safeHostDir(d) {
						done()
						return out, fmt.Errorf("won't read the env file %s", e.Path)
					}
					dirs[d], added = true, true
				}
			}
		}
		if !added || try >= 2 {
			read = rd
			defer done()
			break
		}
		done()
		top = topDirs(dirs)
	}

	compose := yamlRun.Stdout
	if len(envPaths) > 0 {
		// Read them all first, so a failure leaves nothing in the stack folder.
		var paths []string
		for p := range envPaths {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		contents := make([][]byte, len(paths))
		for i, p := range paths {
			b, err := read(ctx, p)
			if err != nil {
				return out, fmt.Errorf("read env file %s: %w", p, err)
			}
			contents[i] = b
		}
		dst := filepath.Join(stacksDir, name, envFileDir)
		if err := os.MkdirAll(dst, 0o700); err != nil {
			return out, err
		}
		used := map[string]bool{}
		for i, p := range paths {
			base := path.Base(p)
			for n := 2; used[base]; n++ {
				base = fmt.Sprintf("%d-%s", n, path.Base(p))
			}
			used[base] = true
			rel := "./" + envFileDir + "/" + base
			replaced := false
			for _, form := range []string{"path: " + p, `path: "` + p + `"`, "path: '" + p + "'"} {
				if strings.Contains(compose, form+"\n") {
					compose = strings.ReplaceAll(compose, form+"\n", "path: "+rel+"\n")
					replaced = true
				}
			}
			if !replaced {
				return out, fmt.Errorf("couldn't point the compose file at the copy of %s", p)
			}
			if err := os.WriteFile(filepath.Join(dst, base), contents[i], 0o600); err != nil {
				return out, err
			}
			out.Copied = append(out.Copied, p)
		}
	}

	envPath := out.EnvFile
	if envPath == "" {
		envPath = filepath.Join(out.WorkingDir, ".env")
	}
	if b, err := read(ctx, envPath); err == nil {
		out.Env = string(b)
		out.EnvFile = envPath
	} else if out.EnvFile != "" {
		return out, fmt.Errorf("read %s: %w", out.EnvFile, err)
	}

	out.Compose = takeoverHeader(out.WorkingDir, out.ConfigFiles) + compose
	return out, nil
}

// underAny reports whether dir is one of tops or inside one.
func underAny(dir string, tops []string) bool {
	for _, t := range tops {
		if dir == t || strings.HasPrefix(dir, t+"/") {
			return true
		}
	}
	return false
}

// topDirs lists folders to mount, leaving out any inside another one.
func topDirs(dirs map[string]bool) []string {
	var all []string
	for d := range dirs {
		all = append(all, d)
	}
	sort.Strings(all)
	var top []string
	for _, d := range all {
		if len(top) > 0 && strings.HasPrefix(d, top[len(top)-1]+"/") {
			continue
		}
		top = append(top, d)
	}
	return top
}

// missingEnvFile finds "env file /x/y not found" in compose's errors.
func missingEnvFile(stderr string) string {
	const prefix = "env file "
	for _, line := range strings.Split(stderr, "\n") {
		i := strings.Index(line, prefix)
		if i < 0 {
			continue
		}
		rest := line[i+len(prefix):]
		if j := strings.Index(rest, " not found"); j > 0 && filepath.IsAbs(rest[:j]) {
			return filepath.Clean(rest[:j])
		}
	}
	return ""
}

// takeoverHeader opens a compose file read from a project, saying where it
// came from. The takeover check looks for it to know the files were merged.
func takeoverHeader(dir string, files []string) string {
	return "# Taken over by TainerHub from " + dir + "\n# Merged from: " + strings.Join(files, ", ") + "\n"
}

func slicesClone(s []string) []string { return append([]string(nil), s...) }
