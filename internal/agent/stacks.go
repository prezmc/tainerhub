package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/prezmc/tainerhub/internal/wire"
)

// Stack files inside each stack's folder. The folder itself may also hold the
// stack's own data (relative bind mounts such as ./data), so the agent only
// ever writes, replaces or deletes these two files.
const (
	composeFile = "compose.yaml"
	envFile     = ".env"
	stagingDir  = ".tainerhub-staging"
)

// StackRunner runs `docker compose` for stacks kept under Dir.
//
// Dir must be the same path inside the agent container and on the host
// (e.g. -v /opt/tainerhub/stacks:/opt/tainerhub/stacks). Compose resolves
// relative bind mounts against the stack folder, and the Docker daemon then
// looks those paths up on the host, so the two must agree.
type StackRunner struct {
	Dir     string // e.g. /opt/tainerhub/stacks
	Bin     string // docker compose binary, e.g. /usr/local/bin/docker-compose
	Socket  string // Docker socket path
	WorkDir string // writable folder for compose's own config and temp files

	// CheckMount, if set, verifies Dir is bind-mounted from the same host path.
	CheckMount func(ctx context.Context, dir string) error

	mu    sync.Mutex
	busy  map[string]bool
	probe *wire.StackSupport
}

// Probe checks the compose binary and the stacks folder. The result is sent
// to the hub in Hello so the UI can explain what is missing.
func (r *StackRunner) Probe(ctx context.Context) wire.StackSupport {
	sup := wire.StackSupport{Dir: r.Dir}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, r.Bin, "version", "--short")
	cmd.Env = r.env()
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		sup.Error = fmt.Sprintf("docker compose is not available (%s): %v", r.Bin, err)
		return sup
	}
	sup.ComposeVersion = strings.TrimSpace(out.String())
	if err := os.MkdirAll(r.Dir, 0o750); err != nil {
		sup.Error = "stacks folder is not writable: " + err.Error()
		return sup
	}
	probe := filepath.Join(r.Dir, ".tainerhub-write-test")
	if err := os.WriteFile(probe, nil, 0o600); err != nil {
		sup.Error = "stacks folder is not writable: " + err.Error()
		return sup
	}
	_ = os.Remove(probe)
	if r.CheckMount != nil {
		if err := r.CheckMount(ctx, r.Dir); err != nil {
			sup.Error = err.Error()
		}
	}
	return sup
}

// Supported returns the cached probe result, probing on first use.
func (r *StackRunner) Supported(ctx context.Context) wire.StackSupport {
	r.mu.Lock()
	p := r.probe
	r.mu.Unlock()
	if p != nil && p.Error == "" {
		return *p
	}
	sup := r.Probe(ctx) // re-probe failures: the admin may have fixed the mount
	r.mu.Lock()
	r.probe = &sup
	r.mu.Unlock()
	return sup
}

// env is the complete environment for compose. It is built from scratch so
// the agent's own variables (e.g. TAINERHUB_JOIN_TOKEN) can never be
// interpolated into a compose file.
func (r *StackRunner) env() []string {
	return []string{
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"HOME=" + r.WorkDir,
		"TMPDIR=" + filepath.Join(r.WorkDir, "tmp"),
		"DOCKER_HOST=unix://" + r.Socket,
		"DOCKER_CONFIG=" + filepath.Join(r.WorkDir, "docker"),
		"COMPOSE_ANSI=never",
	}
}

func (r *StackRunner) lock(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.busy == nil {
		r.busy = map[string]bool{}
	}
	if r.busy[name] {
		return false
	}
	r.busy[name] = true
	return true
}

func (r *StackRunner) unlock(name string) {
	r.mu.Lock()
	delete(r.busy, name)
	r.mu.Unlock()
}

// Run performs one stack command, writing compose's output to out.
func (r *StackRunner) Run(ctx context.Context, c wire.StackCommand, out io.Writer) error {
	if err := wire.ValidStackCommand(c); err != nil {
		return err
	}
	if sup := r.Supported(ctx); sup.Error != "" {
		return errors.New(sup.Error)
	}
	if !r.lock(c.Name) {
		return errors.New("another operation on this stack is still running")
	}
	defer r.unlock(c.Name)
	for _, d := range []string{filepath.Join(r.WorkDir, "tmp"), filepath.Join(r.WorkDir, "docker")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}

	dir := filepath.Join(r.Dir, c.Name)
	deployed := fileExists(filepath.Join(dir, composeFile))
	switch c.Action {
	case wire.StackDeploy:
		if err := r.writeFiles(ctx, c, dir, out); err != nil {
			return err
		}
		if c.Pull {
			if err := r.compose(ctx, c.Name, dir, out, "pull"); err != nil {
				return err
			}
		}
		return r.compose(ctx, c.Name, dir, out, "up", "--detach", "--remove-orphans")
	case wire.StackStart, wire.StackStop, wire.StackRestart:
		if !deployed {
			return errors.New("this stack has not been deployed on this host yet")
		}
		return r.compose(ctx, c.Name, dir, out, c.Action)
	case wire.StackDown:
		if !deployed {
			return errors.New("this stack has not been deployed on this host yet")
		}
		return r.compose(ctx, c.Name, dir, out, downArgs(c)...)
	case wire.StackRemove:
		if deployed {
			if err := r.compose(ctx, c.Name, dir, out, downArgs(c)...); err != nil {
				return err
			}
		}
		for _, f := range []string{composeFile, envFile} {
			if err := os.Remove(filepath.Join(dir, f)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		_ = os.RemoveAll(filepath.Join(dir, stagingDir))
		// Only removes the folder when empty: anything left is the stack's own data.
		if err := os.Remove(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(out, "Kept %s on the host because it still holds files (for example bind-mounted data).\n", dir)
		}
		return nil
	}
	return errors.New("unknown stack action")
}

func downArgs(c wire.StackCommand) []string {
	args := []string{"down", "--remove-orphans"}
	if c.Volumes {
		args = append(args, "--volumes")
	}
	return args
}

// writeFiles validates the new compose file and .env with `compose config`
// in a staging folder, then moves them into place. A file that fails
// validation never replaces a working one.
func (r *StackRunner) writeFiles(ctx context.Context, c wire.StackCommand, dir string, out io.Writer) error {
	staging := filepath.Join(dir, stagingDir)
	if err := os.MkdirAll(staging, 0o700); err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	if err := os.WriteFile(filepath.Join(staging, composeFile), []byte(c.Compose), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(staging, envFile), []byte(c.Env), 0o600); err != nil {
		return err
	}
	fmt.Fprintln(out, "Checking the compose file…")
	err := r.run(ctx, out, "--project-name", c.Name,
		"--file", filepath.Join(staging, composeFile),
		"--project-directory", dir,
		"--env-file", filepath.Join(staging, envFile),
		"config", "--quiet")
	if err != nil {
		return fmt.Errorf("compose file is not valid: %w", err)
	}
	if err := os.Rename(filepath.Join(staging, composeFile), filepath.Join(dir, composeFile)); err != nil {
		return err
	}
	return os.Rename(filepath.Join(staging, envFile), filepath.Join(dir, envFile))
}

func (r *StackRunner) compose(ctx context.Context, name, dir string, out io.Writer, args ...string) error {
	base := []string{"--project-name", name, "--file", filepath.Join(dir, composeFile), "--project-directory", dir, "--progress", "plain"}
	return r.run(ctx, out, append(base, args...)...)
}

func (r *StackRunner) run(ctx context.Context, out io.Writer, args ...string) error {
	fmt.Fprintf(out, "$ docker compose %s\n", strings.Join(redactArgs(args, r.Dir), " "))
	cmd := exec.CommandContext(ctx, r.Bin, args...)
	cmd.Env = r.env()
	cmd.Dir = r.Dir
	cmd.Stdout, cmd.Stderr = out, out
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 20 * time.Second
	err := cmd.Run()
	if ctx.Err() != nil {
		return fmt.Errorf("stopped: %w", ctx.Err())
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return fmt.Errorf("docker compose exited with code %d", ee.ExitCode())
	}
	return err
}

// redactArgs shortens the long --file / --project-directory paths for display.
func redactArgs(args []string, root string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		if rel, err := filepath.Rel(root, a); err == nil && filepath.IsAbs(a) && !strings.HasPrefix(rel, "..") {
			a = rel
		}
		out[i] = a
	}
	return out
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

// safeWriter forwards to w until a write fails, then silently discards. It
// keeps a stack operation running to completion when the hub connection
// drops: compose would otherwise get a broken pipe halfway through `up`.
type safeWriter struct {
	mu     sync.Mutex
	w      io.Writer
	failed bool
}

func (s *safeWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.failed {
		if _, err := s.w.Write(p); err != nil {
			s.failed = true
		}
	}
	return len(p), nil
}
