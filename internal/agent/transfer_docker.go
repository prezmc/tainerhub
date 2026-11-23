package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"

	"github.com/prezmc/tainerhub/internal/wire"
)

// Transferer is what copying volumes and stack folders needs from Docker.
// SocketDocker implements it; an agent whose Docker doesn't can't transfer.
//
// The agent runs with no capabilities, so it can't read or write files that
// belong to other users (a database's data folder, say). Docker can: the
// agent creates a helper container that is never started, with the data
// mounted at /th-data, and uses Docker's copy-from/to-container API, which
// keeps owners and permissions.
type Transferer interface {
	// HelperImage is an image present on this host to create helper
	// containers from: the agent's own.
	HelperImage(ctx context.Context) (string, error)
	// CreateHelper creates (without starting) a container with the data
	// mounted at /th-data: a volume by name, or a bind mount of a host path.
	CreateHelper(ctx context.Context, image string, m HelperMount) (string, error)
	RemoveContainer(ctx context.Context, id string) error
	// ArchiveGet returns a tar stream of path inside the container.
	ArchiveGet(ctx context.Context, id, path string) (io.ReadCloser, error)
	// ArchivePut unpacks a tar stream at path inside the container, keeping
	// the owners recorded in it.
	ArchivePut(ctx context.Context, id, path string, tar io.Reader) error
	// VolumeInfo returns a volume's driver, options and labels; ok is false
	// when it doesn't exist.
	VolumeInfo(ctx context.Context, name string) (v VolumeInfo, ok bool, err error)
	CreateVolume(ctx context.Context, name string, labels map[string]string) error
	RemoveVolume(ctx context.Context, name string) error
}

// HelperMount is what a helper container mounts at /th-data.
type HelperMount struct {
	Volume string // a named volume, or
	Bind   string // a host path
}

type VolumeInfo struct {
	Driver  string
	Options map[string]string
	Labels  map[string]string
}

const helperLabel = "tainerhub.helper"

func (d *SocketDocker) HelperImage(ctx context.Context) (string, error) {
	self, _ := os.Hostname() // Docker sets it to the container ID unless overridden
	if !wire.ValidContainerRef(self) {
		return "", errors.New("can't identify this agent's container")
	}
	var insp struct{ Image string }
	if err := d.getJSON(ctx, "/containers/"+url.PathEscape(self)+"/json", nil, &insp); err != nil {
		return "", fmt.Errorf("can't find this agent's own container (was its hostname changed?): %w", err)
	}
	if insp.Image == "" {
		return "", errors.New("can't tell this agent's image")
	}
	return insp.Image, nil
}

func (d *SocketDocker) CreateHelper(ctx context.Context, image string, m HelperMount) (string, error) {
	type mount struct {
		Type          string
		Source        string
		Target        string
		VolumeOptions *struct{ NoCopy bool } `json:",omitempty"`
	}
	var mt mount
	switch {
	case m.Volume != "" && m.Bind == "":
		// NoCopy: never seed the volume with the image's own files.
		mt = mount{Type: "volume", Source: m.Volume, Target: "/" + wire.TransferRoot, VolumeOptions: &struct{ NoCopy bool }{true}}
	case m.Bind != "" && m.Volume == "":
		mt = mount{Type: "bind", Source: m.Bind, Target: "/" + wire.TransferRoot}
	default:
		return "", errors.New("helper needs exactly one mount")
	}
	body := map[string]any{
		"Image":           image,
		"Entrypoint":      []string{"/nonexistent"}, // never started
		"Labels":          map[string]string{helperLabel: "transfer"},
		"NetworkDisabled": true,
		"HostConfig":      map[string]any{"Mounts": []mount{mt}, "NetworkMode": "none"},
	}
	resp, err := d.doBody(ctx, http.MethodPost, "/containers/create", nil, body)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct{ Id string }
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.Id == "" {
		return "", fmt.Errorf("docker: no container id: %v", err)
	}
	return out.Id, nil
}

func (d *SocketDocker) RemoveContainer(ctx context.Context, id string) error {
	return d.remove(ctx, "/containers/"+url.PathEscape(id)+"?force=1")
}

func (d *SocketDocker) ArchiveGet(ctx context.Context, id, path string) (io.ReadCloser, error) {
	resp, err := d.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/archive", url.Values{"path": {path}})
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

func (d *SocketDocker) ArchivePut(ctx context.Context, id, path string, tar io.Reader) error {
	// No copyUIDGID: Docker then keeps each entry's owner from the archive.
	// With it, Docker chowns everything to the helper container's user,
	// which for the agent's image (USER 0) makes every file root's.
	q := url.Values{"path": {path}, "noOverwriteDirNonDir": {"true"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://docker/containers/"+url.PathEscape(id)+"/archive?"+q.Encode(), tar)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-tar")
	resp, err := d.c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return dockerError(resp)
	}
	return nil
}

func (d *SocketDocker) VolumeInfo(ctx context.Context, name string) (VolumeInfo, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/volumes/"+url.PathEscape(name), nil)
	if err != nil {
		return VolumeInfo{}, false, err
	}
	resp, err := d.c.Do(req)
	if err != nil {
		return VolumeInfo{}, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return VolumeInfo{}, false, nil
	}
	if resp.StatusCode >= 400 {
		return VolumeInfo{}, false, dockerError(resp)
	}
	var v VolumeInfo
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return VolumeInfo{}, false, err
	}
	return v, true, nil
}

func (d *SocketDocker) CreateVolume(ctx context.Context, name string, labels map[string]string) error {
	resp, err := d.doBody(ctx, http.MethodPost, "/volumes/create", nil, map[string]any{"Name": name, "Driver": "local", "Labels": labels})
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (d *SocketDocker) RemoveVolume(ctx context.Context, name string) error {
	return d.remove(ctx, "/volumes/"+url.PathEscape(name))
}

func dockerError(resp *http.Response) error {
	var e struct {
		Message string `json:"message"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&e)
	if e.Message == "" {
		e.Message = resp.Status
	}
	return fmt.Errorf("docker: %s", e.Message)
}
