package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/prezmc/tainerhub/internal/wire"
)

// VolumeUsage is a volume's size and labels as Docker reports them.
type VolumeUsage struct {
	Size   int64 // bytes; -1 if unknown
	Labels map[string]string
	Driver string
	Opts   bool // has driver options
}

// Volumes reports every volume's size, from Docker's disk usage summary.
// Docker measures sizes itself, so it can see files the agent can't.
func (d *SocketDocker) Volumes(ctx context.Context) (map[string]VolumeUsage, error) {
	var df struct {
		Volumes []struct {
			Name      string
			Driver    string
			Labels    map[string]string
			Options   map[string]string
			UsageData *struct{ Size int64 }
		}
	}
	if err := d.getJSON(ctx, "/system/df", url.Values{"type": {"volume"}}, &df); err != nil {
		return nil, err
	}
	out := make(map[string]VolumeUsage, len(df.Volumes))
	for _, v := range df.Volumes {
		u := VolumeUsage{Size: -1, Labels: v.Labels, Driver: v.Driver, Opts: len(v.Options) > 0}
		if v.UsageData != nil && v.UsageData.Size >= 0 {
			u.Size = v.UsageData.Size
		}
		out[v.Name] = u
	}
	return out, nil
}

// fillVolumes adds Docker's view of each volume to an inspection.
func fillVolumes(in *wire.StackInspect, vols map[string]VolumeUsage) {
	for i := range in.Volumes {
		v := &in.Volumes[i]
		u, ok := vols[v.Name]
		v.Exists = ok
		if !ok {
			continue
		}
		v.Size, v.Labels = u.Size, u.Labels
		if v.Driver == "" {
			v.Driver = u.Driver
		}
		v.Options = v.Options || u.Opts
	}
}

// RunCleanup deletes everything inside a host folder by running a helper
// container (the agent's image, `tainerhub-agent rm-helper`) with the folder
// mounted. Unlike the agent, the helper keeps Docker's default capabilities,
// so it can delete files that belong to other users.
func (d *SocketDocker) RunCleanup(ctx context.Context, hostPath string) error {
	image, err := d.HelperImage(ctx)
	if err != nil {
		return err
	}
	body := map[string]any{
		"Image":           image,
		"Entrypoint":      []string{"/tainerhub-agent", "rm-helper"},
		"Labels":          map[string]string{helperLabel: "cleanup"},
		"NetworkDisabled": true,
		"HostConfig": map[string]any{
			"Mounts":      []map[string]string{{"Type": "bind", "Source": hostPath, "Target": "/" + wire.TransferRoot}},
			"NetworkMode": "none",
			"SecurityOpt": []string{"no-new-privileges:true"},
		},
	}
	resp, err := d.doBody(ctx, http.MethodPost, "/containers/create", nil, body)
	if err != nil {
		return fmt.Errorf("create cleanup helper: %w", err)
	}
	var created struct{ Id string }
	err = jsonDecode(resp, &created)
	if err != nil || created.Id == "" {
		return fmt.Errorf("create cleanup helper: %v", err)
	}
	id := created.Id
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = d.RemoveContainer(ctx, id)
	}()
	if resp, err := d.do(ctx, http.MethodPost, "/containers/"+id+"/start", nil); err != nil {
		return fmt.Errorf("start cleanup helper: %w", err)
	} else {
		resp.Body.Close()
	}
	resp, err = d.do(ctx, http.MethodPost, "/containers/"+id+"/wait", nil)
	if err != nil {
		return fmt.Errorf("wait for cleanup helper: %w", err)
	}
	var waited struct {
		StatusCode int
		Error      *struct{ Message string }
	}
	if err := jsonDecode(resp, &waited); err != nil {
		return err
	}
	if waited.StatusCode != 0 {
		return fmt.Errorf("cleanup helper exited with code %d", waited.StatusCode)
	}
	return nil
}

// RmHelper is `tainerhub-agent rm-helper`, run inside a cleanup helper
// container: it deletes everything in /th-data, leaving the folder itself.
func RmHelper() error {
	root := "/" + wire.TransferRoot
	ents, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range ents {
		if err := os.RemoveAll(filepath.Join(root, e.Name())); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// removeStackDir deletes a stack's folder and everything in it, refusing
// while the stack is deployed here.
func (s *session) removeStackDir(ctx context.Context, name string) error {
	r := s.a.cfg.Stacks
	if r == nil {
		return errors.New("stacks aren't set up on this host")
	}
	if sup := r.Supported(ctx); sup.Error != "" {
		return errors.New(sup.Error)
	}
	dir := filepath.Join(r.Dir, name)
	if fileExists(filepath.Join(dir, composeFile)) {
		return errors.New("the stack is deployed on this host; remove it first")
	}
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err := os.RemoveAll(dir); err == nil {
		return nil // nothing belonged to other users
	}
	d, ok := s.a.cfg.Docker.(interface {
		RunCleanup(context.Context, string) error
	})
	if !ok {
		return errors.New("this agent can't remove other users' files")
	}
	if err := d.RunCleanup(ctx, dir); err != nil {
		return err
	}
	return os.Remove(dir)
}

// inspectStack answers OpStackInspect.
func (s *session) inspectStack(ctx context.Context, name string) (wire.StackInspect, error) {
	if s.a.cfg.Stacks == nil {
		return wire.StackInspect{}, errors.New("stacks aren't set up on this host")
	}
	out, err := s.a.cfg.Stacks.Inspect(ctx, name)
	if err != nil {
		return out, err
	}
	if d, ok := s.a.cfg.Docker.(interface {
		Volumes(context.Context) (map[string]VolumeUsage, error)
	}); ok && len(out.Volumes) > 0 {
		vols, err := d.Volumes(ctx)
		if err != nil {
			return out, fmt.Errorf("volume sizes: %w", err)
		}
		fillVolumes(&out, vols)
	}
	return out, nil
}

func jsonDecode(resp *http.Response, v any) error {
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(v)
}
