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

	"github.com/prezmc/tainerhub/internal/regcred"
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
// Docker's own registry access (/distribution), with the login in auths for
// each image's registry (Docker does the token handshake with it).
func (d *SocketDocker) checkUpdates(ctx context.Context, auths []wire.RegistryAuth) (wire.ImageUpdates, error) {
	res := wire.ImageUpdates{Checked: time.Now().Unix(), Images: map[string]bool{}, Errors: map[string]string{}, Remote: map[string]string{}}
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
			newer, remote, err := d.newerImage(ctx, ref, auths)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				res.Errors[ref] = regcred.Scrub(err.Error(), auths)
				return
			}
			res.Images[ref] = newer
			res.Remote[ref] = remote
		}(ref)
	}
	wg.Wait()
	// Then each container against the image it runs: one that runs an older
	// image than the tag on disk is behind too.
	res.Containers = map[string]bool{}
	behind := map[string]bool{} // image ID + digest -> image lacks the digest
	for _, c := range cs {
		digest, ok := res.Remote[c.Image]
		if !ok || c.ImageID == "" {
			continue
		}
		key := c.ImageID + "|" + digest
		b, known := behind[key]
		if !known {
			runs, err := d.imageHasDigest(ctx, c.ImageID, digest)
			if err != nil {
				continue // can't tell for this one: the tag's answer stands
			}
			b = !runs
			behind[key] = b
		}
		res.Containers[c.ID] = b
		if b {
			res.Images[c.Image] = true
		}
	}
	return res, nil
}

// imageHasDigest says whether image id is the registry image with digest.
func (d *SocketDocker) imageHasDigest(ctx context.Context, id, digest string) (bool, error) {
	var img struct{ RepoDigests []string }
	if err := d.getJSON(ctx, "/images/"+url.PathEscape(id)+"/json", nil, &img); err != nil {
		return false, err
	}
	return hasDigest(img.RepoDigests, digest), nil
}

// newerImage reports whether the registry's image for ref differs from the
// local one, and the registry's digest.
func (d *SocketDocker) newerImage(ctx context.Context, ref string, auths []wire.RegistryAuth) (bool, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var local struct{ RepoDigests []string }
	if err := d.getJSON(ctx, "/images/"+url.PathEscape(ref)+"/json", nil, &local); err != nil {
		return false, "", err
	}
	if len(local.RepoDigests) == 0 {
		return false, "", errors.New("built locally: no registry digest to compare")
	}
	var remote struct{ Descriptor struct{ Digest string } }
	if err := d.getJSON(withRegistryAuth(ctx, auths, ref), "/distribution/"+url.PathEscape(ref)+"/json", nil, &remote); err != nil {
		return false, "", err
	}
	digest := remote.Descriptor.Digest
	if digest == "" {
		return false, "", errors.New("registry returned no digest")
	}
	if hasDigest(local.RepoDigests, digest) {
		return false, digest, nil
	}
	return true, digest, nil
}

func hasDigest(repoDigests []string, digest string) bool {
	for _, rd := range repoDigests {
		if strings.HasSuffix(rd, "@"+digest) {
			return true
		}
	}
	return false
}

// updateCache keeps the last check's results across reconnects.
type updateCache struct {
	mu      sync.Mutex
	results map[string]bool   // image name -> some container or the tag is behind the registry
	remote  map[string]string // image name -> the registry's digest
	// runs remembers, per image ID and digest, whether that image is the
	// registry's newer one (the answer never changes for an image ID).
	runs map[string]bool
}

// set keeps the new results. An image whose check failed this time keeps
// its previous answer: the background checks run without registry logins,
// so for private images only checks the hub asks for (with logins) count.
func (u *updateCache) set(r wire.ImageUpdates) {
	u.mu.Lock()
	defer u.mu.Unlock()
	next := make(map[string]bool, len(r.Images))
	remote := make(map[string]string, len(r.Remote))
	for ref, newer := range r.Images {
		next[ref] = newer
		if r.Remote[ref] != "" {
			remote[ref] = r.Remote[ref]
		}
	}
	for ref := range r.Errors {
		if u.results[ref] {
			next[ref] = true
			if d := u.remote[ref]; d != "" {
				remote[ref] = d
			}
		}
	}
	u.results, u.remote, u.runs = next, remote, map[string]bool{}
}

func (u *updateCache) newer(ref string) (bool, string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.results[ref], u.remote[ref]
}

func (u *updateCache) has(ref string) bool {
	n, _ := u.newer(ref)
	return n
}

// updateFor says whether a container on image imageID (named ref) is behind
// the registry: whether the image it runs is the registry's image for ref,
// as of the last check. So the badge goes as soon as the container runs the
// new image (Pull & redeploy, a recreate, an automatic update), and stays on
// a container left on an old image even when the tag on disk is current.
func (d *SocketDocker) updateFor(ctx context.Context, ref, imageID string) bool {
	newer, digest := d.updates.newer(ref)
	if digest == "" || imageID == "" {
		return newer // no digest (the check failed for it): the last answer
	}
	key := imageID + "|" + digest
	d.updates.mu.Lock()
	runs, known := d.updates.runs[key]
	d.updates.mu.Unlock()
	if !known {
		var err error
		if runs, err = d.imageHasDigest(ctx, imageID, digest); err != nil {
			return newer // can't tell: keep the check's answer
		}
		d.updates.mu.Lock()
		if d.updates.runs != nil {
			d.updates.runs[key] = runs
		}
		d.updates.mu.Unlock()
	}
	return !runs
}

// ---------------- recreate ----------------

// ownImageSettings are Config fields a container inherits from its image.
// When they still equal the old image's value, they are left out so the new
// image's defaults apply (a new version may change PATH, the command, ...).
var ownImageSettings = []string{"Cmd", "Entrypoint", "WorkingDir", "User", "ExposedPorts", "Healthcheck", "Volumes", "StopSignal", "Shell", "OnBuild"}

// recreate pulls the container's image and replaces the container with an
// identical one on the new image. The old container is kept (stopped and
// renamed) until the new one is running, and put back if it isn't.
func (d *SocketDocker) recreate(ctx context.Context, id string, auths []wire.RegistryAuth) (wire.RecreateResult, error) {
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
	if users, err := d.networkUsers(ctx, meta.ID, name); err != nil {
		return res, err
	} else if len(users) > 0 {
		verb, them := "use", "them"
		if len(users) == 1 {
			verb, them = "uses", "it"
		}
		return res, fmt.Errorf("%s %s this container's network, so replacing it would cut %s off for good. Put them in one compose stack and use the stack's Pull & redeploy, which recreates them together", strings.Join(users, ", "), verb, them)
	}
	ref := meta.Config.Image
	if !updateable(ref) {
		return res, errors.New("this container uses a pinned image (by digest or ID), so there's nothing newer to pull")
	}
	res.OldImage = meta.Image
	if _, err := d.pull(ctx, ref, auths); err != nil {
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
	var hc struct{ NetworkMode string }
	_ = json.Unmarshal(insp["HostConfig"], &hc)
	if strings.HasPrefix(hc.NetworkMode, "container:") {
		// It runs in another container's network: these come from that
		// container, and Docker refuses to create one that sets them.
		for _, k := range []string{"Hostname", "Domainname", "ExposedPorts", "MacAddress"} {
			delete(cfg, k)
		}
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

// networkUsers names the other containers (running or not) that run in the
// network of the container with this ID and name (network_mode
// container:<id or name>).
func (d *SocketDocker) networkUsers(ctx context.Context, id, name string) ([]string, error) {
	var raw []struct {
		ID         string `json:"Id"`
		Names      []string
		HostConfig struct{ NetworkMode string }
	}
	if err := d.getJSON(ctx, "/containers/json", url.Values{"all": {"1"}}, &raw); err != nil {
		return nil, err
	}
	gw := wire.Container{ID: id, Name: name}
	var cs []wire.Container
	for _, r := range raw {
		c := wire.Container{ID: r.ID, NetworkOf: wire.SharedNetworkTarget(r.HostConfig.NetworkMode)}
		if len(r.Names) > 0 {
			c.Name = strings.TrimPrefix(r.Names[0], "/")
		}
		cs = append(cs, c)
	}
	return wire.NetworkUsers(gw, cs), nil
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

// HubCheckCovers is how recent a check the hub asked for must be for the
// agent to skip its own scheduled one. A hub that schedules checks runs one
// every 6 hours (by default), so 5 hours means the agent stands down while
// the hub keeps checking, and takes over again when it stops (an older hub,
// or checks turned off there).
const HubCheckCovers = 5 * time.Hour

// hubCheckTimes records update checks the hub asked for.
type hubCheckTimes struct {
	mu      sync.Mutex
	running int
	last    time.Time // when the last one finished successfully
}

// begin records a hub check starting; call done with its outcome.
func (h *hubCheckTimes) begin() (done func(ok bool)) {
	h.mu.Lock()
	h.running++
	h.mu.Unlock()
	return func(ok bool) {
		h.mu.Lock()
		h.running--
		if ok {
			h.last = time.Now()
		}
		h.mu.Unlock()
	}
}

// covers reports whether a hub check is running or finished within window
// before now.
func (h *hubCheckTimes) covers(now time.Time, window time.Duration) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.running > 0 || (!h.last.IsZero() && now.Sub(h.last) < window)
}

// RunUpdateChecks checks for newer images shortly after start and then every
// interval, until ctx ends. Results show up on containers in the next
// snapshot. This is the fallback for hubs that don't schedule checks: it
// has no registry logins, so it's skipped while the hub's checks (which
// have them) are recent.
func (d *SocketDocker) RunUpdateChecks(ctx context.Context, first, every time.Duration) {
	runUpdateChecks(ctx, first, every, func() bool { return d.hubChecks.covers(time.Now(), HubCheckCovers) }, func(ctx context.Context) {
		_ = d.refreshUpdates(ctx, nil)
	})
}

func runUpdateChecks(ctx context.Context, first, every time.Duration, skip func() bool, check func(context.Context)) {
	t := time.NewTimer(first)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if !skip() {
			check(ctx)
		}
		t.Reset(every)
	}
}

func (d *SocketDocker) refreshUpdates(ctx context.Context, auths []wire.RegistryAuth) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	r, err := d.checkUpdates(ctx, auths)
	if err == nil {
		d.updates.set(r)
	}
	return err
}
