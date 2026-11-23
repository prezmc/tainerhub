package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/prezmc/tainerhub/internal/wire"
)

// ---------------- update checks ----------------

// updateable says whether a container's image reference can be checked:
// pinned digests ("@sha256:") and bare image IDs never change.
func updateable(ref string) bool {
	return ref != "" && !strings.Contains(ref, "@") && !strings.HasPrefix(ref, "sha256:")
}

// checkUpdates asks the registry for the current digest of every image
// reference the containers use and compares it with what's on disk. It uses
// Docker's own registry access (/distribution), so private registries the
// daemon is logged in to work too.
func (d *SocketDocker) checkUpdates(ctx context.Context) (wire.ImageUpdates, error) {
	res := wire.ImageUpdates{Checked: time.Now().Unix(), Images: map[string]bool{}, Errors: map[string]string{}}
	cs, err := d.List(ctx)
	if err != nil {
		return res, err
	}
	refs := map[string]bool{}
	for _, c := range cs {
		if updateable(c.Image) {
			refs[c.Image] = true
		}
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for ref := range refs {
		wg.Add(1)
		sem <- struct{}{}
		go func(ref string) {
			defer wg.Done()
			defer func() { <-sem }()
			newer, err := d.newerImage(ctx, ref)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				res.Errors[ref] = err.Error()
				return
			}
			res.Images[ref] = newer
		}(ref)
	}
	wg.Wait()
	return res, nil
}

func (d *SocketDocker) newerImage(ctx context.Context, ref string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var local struct{ RepoDigests []string }
	if err := d.getJSON(ctx, "/images/"+url.PathEscape(ref)+"/json", nil, &local); err != nil {
		return false, err
	}
	if len(local.RepoDigests) == 0 {
		return false, errors.New("built locally: no registry digest to compare")
	}
	var remote struct{ Descriptor struct{ Digest string } }
	if err := d.getJSON(ctx, "/distribution/"+url.PathEscape(ref)+"/json", nil, &remote); err != nil {
		return false, err
	}
	if remote.Descriptor.Digest == "" {
		return false, errors.New("registry returned no digest")
	}
	for _, rd := range local.RepoDigests {
		if strings.HasSuffix(rd, "@"+remote.Descriptor.Digest) {
			return false, nil
		}
	}
	return true, nil
}

// updateCache keeps the last check's results across reconnects.
type updateCache struct {
	mu      sync.Mutex
	results map[string]bool
}

func (u *updateCache) set(r wire.ImageUpdates) {
	u.mu.Lock()
	u.results = r.Images
	u.mu.Unlock()
}

func (u *updateCache) has(ref string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.results[ref]
}

// ---------------- recreate ----------------

// ownImageSettings are Config fields a container inherits from its image.
// When they still equal the old image's value, they are left out so the new
// image's defaults apply (a new version may change PATH, the command, ...).
var ownImageSettings = []string{"Cmd", "Entrypoint", "WorkingDir", "User", "ExposedPorts", "Healthcheck", "Volumes", "StopSignal", "Shell", "OnBuild"}

// recreate pulls the container's image and replaces the container with an
// identical one on the new image. The old container is kept (stopped and
// renamed) until the new one is running, and put back if it isn't.
func (d *SocketDocker) recreate(ctx context.Context, id string) (wire.RecreateResult, error) {
	var res wire.RecreateResult
	var insp map[string]json.RawMessage
	if err := d.getJSON(ctx, "/containers/"+url.PathEscape(id)+"/json", nil, &insp); err != nil {
		return res, err
	}
	var meta containerMeta
	b, _ := json.Marshal(insp)
	if err := json.Unmarshal(b, &meta); err != nil {
		return res, err
	}
	name := strings.TrimPrefix(meta.Name, "/")
	if p := meta.Config.Labels["com.docker.compose.project"]; p != "" {
		return res, fmt.Errorf("%s belongs to the %s compose stack: update it with the stack's Pull & redeploy", name, p)
	}
	if strings.HasPrefix(name, "tainerhub") {
		return res, errors.New("TainerHub's own containers are updated from Hosts → Updates")
	}
	ref := meta.Config.Image
	if !updateable(ref) {
		return res, errors.New("this container uses a pinned image (by digest or ID), so there's nothing newer to pull")
	}
	res.OldImage = meta.Image
	if _, err := d.pull(ctx, ref); err != nil {
		return res, fmt.Errorf("pull %s: %w", ref, err)
	}
	var img struct {
		ID     string `json:"Id"`
		Config map[string]json.RawMessage
	}
	if err := d.getJSON(ctx, "/images/"+url.PathEscape(ref)+"/json", nil, &img); err != nil {
		return res, err
	}
	res.NewImage = img.ID
	if img.ID == meta.Image {
		res.Status = "current"
		return res, nil
	}

	body := d.createSpec(ctx, insp, meta, ref)

	// Swap: stop and rename the old one, create and start the new one.
	backup := fmt.Sprintf("%s-tainerhub-old-%d", name, time.Now().Unix())
	wasRunning := meta.State.Running
	if wasRunning {
		if err := d.post(ctx, "/containers/"+url.PathEscape(meta.ID)+"/stop", url.Values{"t": {"30"}}); err != nil {
			return res, fmt.Errorf("stop: %w", err)
		}
	}
	if err := d.post(ctx, "/containers/"+url.PathEscape(meta.ID)+"/rename", url.Values{"name": {backup}}); err != nil {
		d.restore(meta.ID, name, wasRunning)
		return res, fmt.Errorf("rename the old container: %w", err)
	}
	rollback := func(newID string, cause error) (wire.RecreateResult, error) {
		if newID != "" {
			_ = d.remove(context.Background(), "/containers/"+url.PathEscape(newID)+"?force=1")
		}
		d.restore(meta.ID, name, wasRunning)
		return res, fmt.Errorf("%w (the old container was put back)", cause)
	}
	resp, err := d.doBody(ctx, http.MethodPost, "/containers/create", url.Values{"name": {name}}, body)
	if err != nil {
		return rollback("", fmt.Errorf("create the new container: %w", err))
	}
	var created struct {
		ID string `json:"Id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if wasRunning {
		if err := d.post(ctx, "/containers/"+url.PathEscape(created.ID)+"/start", nil); err != nil {
			return rollback(created.ID, fmt.Errorf("start the new container: %w", err))
		}
		// Give it a moment: a container that crashes straight away is a failed update.
		select {
		case <-ctx.Done():
		case <-time.After(3 * time.Second):
		}
		var st struct {
			State struct{ Running, Restarting bool }
		}
		if err := d.getJSON(ctx, "/containers/"+url.PathEscape(created.ID)+"/json", nil, &st); err != nil || (!st.State.Running && !st.State.Restarting) {
			return rollback(created.ID, errors.New("the new container stopped right after starting; check its logs"))
		}
	}
	_ = d.remove(ctx, "/containers/"+url.PathEscape(meta.ID)+"?force=1")
	res.Status = "updated"
	return res, nil
}

// containerMeta is the part of a container's inspection that recreating
// it needs.
type containerMeta struct {
	ID     string `json:"Id"`
	Name   string
	Image  string
	State  struct{ Running bool }
	Config struct {
		Image    string
		Hostname string
		Labels   map[string]string
	}
	NetworkSettings struct {
		Networks map[string]map[string]json.RawMessage
	}
}

// createSpec is the body for POST /containers/create that makes a
// container like the inspected one, on image ref: the old container's
// settings minus what came from its image, so the new image's defaults
// apply (a new version may change PATH, the command, ...).
func (d *SocketDocker) createSpec(ctx context.Context, insp map[string]json.RawMessage, meta containerMeta, ref string) map[string]any {
	var cfg map[string]json.RawMessage
	_ = json.Unmarshal(insp["Config"], &cfg)
	if cfg == nil {
		cfg = map[string]json.RawMessage{}
	}
	var oldImg struct{ Config map[string]json.RawMessage }
	if err := d.getJSON(ctx, "/images/"+url.PathEscape(meta.Image)+"/json", nil, &oldImg); err == nil {
		for _, k := range ownImageSettings {
			if sameJSON(cfg[k], oldImg.Config[k]) {
				delete(cfg, k)
			}
		}
		cfg["Env"] = withoutInherited(cfg["Env"], oldImg.Config["Env"])
		cfg["Labels"] = labelsWithoutInherited(cfg["Labels"], oldImg.Config["Labels"])
	}
	if len(meta.ID) >= 12 && meta.Config.Hostname == meta.ID[:12] {
		delete(cfg, "Hostname") // Docker's default (the old ID): let it pick the new one
	}
	cfg["Image"], _ = json.Marshal(ref)
	endpoints := map[string]any{}
	for net, ep := range meta.NetworkSettings.Networks {
		e := map[string]json.RawMessage{}
		for _, k := range []string{"IPAMConfig", "Links", "DriverOpts", "MacAddress"} {
			if v, ok := ep[k]; ok && string(v) != "null" && string(v) != `""` {
				e[k] = v
			}
		}
		var aliases []string
		_ = json.Unmarshal(ep["Aliases"], &aliases)
		keep := []string{}
		for _, a := range aliases {
			if len(meta.ID) < 12 || a != meta.ID[:12] {
				keep = append(keep, a)
			}
		}
		if len(keep) > 0 {
			e["Aliases"], _ = json.Marshal(keep)
		}
		endpoints[net] = e
	}
	body := map[string]any{}
	for k, v := range cfg {
		body[k] = v
	}
	body["HostConfig"] = insp["HostConfig"]
	body["NetworkingConfig"] = map[string]any{"EndpointsConfig": endpoints}
	return body
}

// restore undoes a half-done recreate: give the old container its name back
// and start it again. Best effort: it runs after something already failed.
func (d *SocketDocker) restore(id, name string, start bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = d.post(ctx, "/containers/"+url.PathEscape(id)+"/rename", url.Values{"name": {name}})
	if start {
		_ = d.post(ctx, "/containers/"+url.PathEscape(id)+"/start", nil)
	}
}

func (d *SocketDocker) post(ctx context.Context, path string, q url.Values) error {
	resp, err := d.do(ctx, http.MethodPost, path, q)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func sameJSON(a, b json.RawMessage) bool {
	if len(a) == 0 || len(b) == 0 {
		return len(a) == 0 && len(b) == 0
	}
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}

// withoutInherited drops env entries that are exactly the old image's.
func withoutInherited(container, image json.RawMessage) json.RawMessage {
	var c, i []string
	_ = json.Unmarshal(container, &c)
	_ = json.Unmarshal(image, &i)
	from := map[string]bool{}
	for _, e := range i {
		from[e] = true
	}
	out := []string{}
	for _, e := range c {
		if !from[e] {
			out = append(out, e)
		}
	}
	b, _ := json.Marshal(out)
	return b
}

// labelsWithoutInherited drops labels with exactly the old image's value.
func labelsWithoutInherited(container, image json.RawMessage) json.RawMessage {
	var c, i map[string]string
	_ = json.Unmarshal(container, &c)
	_ = json.Unmarshal(image, &i)
	out := map[string]string{}
	for k, v := range c {
		if iv, ok := i[k]; !ok || iv != v {
			out[k] = v
		}
	}
	b, _ := json.Marshal(out)
	return b
}

// RunUpdateChecks checks for newer images shortly after start and then every
// interval, until ctx ends. Results show up on containers in the next snapshot.
func (d *SocketDocker) RunUpdateChecks(ctx context.Context, first, every time.Duration) {
	t := time.NewTimer(first)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		_ = d.refreshUpdates(ctx)
		t.Reset(every)
	}
}

func (d *SocketDocker) refreshUpdates(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	r, err := d.checkUpdates(ctx)
	if err == nil {
		d.updates.set(r)
	}
	return err
}
