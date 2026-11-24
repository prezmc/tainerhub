package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/prezmc/tainerhub/internal/wire"
)

// Stack rollbacks on the agent. A rollback writes an override file
// (wire.StackRollbackFile) next to compose.yaml that points each service at
// its old image, tagged tainerhub-rollback/<stack>/pinned:<service>. While
// that file is there the stack is "pinned": start, stop, restart and down
// use it too, so a later command never flips containers back to the newest
// images by surprise. The next deploy (Deploy or Pull & redeploy: "Return to
// latest" in the UI) runs without it and then removes it.

// rollbackOverridePath is where a stack's rollback override lives.
func rollbackOverridePath(dir string) string { return filepath.Join(dir, wire.StackRollbackFile) }

// pinned reports whether the stack in dir runs a rollback.
func pinned(dir string) bool { return fileExists(rollbackOverridePath(dir)) }

// rollbackOverride is the override file's content: only the services
// listed, each with its pinned tag and pull_policy: never (the image is
// local, and must not be replaced by a pull). Names are written as JSON
// strings, which YAML reads as double-quoted scalars, so no service name
// can break out of its place.
func rollbackOverride(stack string, svcs []wire.RollbackService) []byte {
	sorted := append([]wire.RollbackService(nil), svcs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Service < sorted[j].Service })
	var b bytes.Buffer
	b.WriteString("# Written by TainerHub when this stack was rolled back. Deploying the\n")
	b.WriteString("# stack again (Return to latest) removes it. Don't edit it by hand.\n")
	b.WriteString("services:\n")
	for _, s := range sorted {
		name, _ := json.Marshal(s.Service)
		image, _ := json.Marshal(wire.PinnedTag(stack, s.Service))
		fmt.Fprintf(&b, "  %s:\n    image: %s\n    pull_policy: never\n", name, image)
		if s.Ref != "" && !wire.IsRollbackTag(s.Ref) {
			ref, _ := json.Marshal(s.Ref)
			label, _ := json.Marshal(wire.RollbackFromLabel)
			fmt.Fprintf(&b, "    labels:\n      %s: %s\n", label, ref)
		}
	}
	return b.Bytes()
}

// splitTag splits repo:tag at the last ':' (repositories here have no port).
func splitTag(ref string) (string, string) {
	i := strings.LastIndex(ref, ":")
	return ref[:i], ref[i+1:]
}

// composeServices lists the services of a resolved compose file (`config
// --format json`).
func composeServices(resolved []byte) map[string]bool {
	var cfg struct {
		Services map[string]json.RawMessage `json:"services"`
	}
	out := map[string]bool{}
	if json.Unmarshal(resolved, &cfg) == nil {
		for name := range cfg.Services {
			out[name] = true
		}
	}
	return out
}

// protect tags the images the stack runs now as its rollback images
// (tainerhub-rollback/<stack>:<service>) and drops the stack's older
// rollback tags. With nothing running it changes nothing: the tags from the
// last time stay, as they are still the newest rollback point's.
func (r *StackRunner) protect(ctx context.Context, name string) (map[string]wire.StackImage, error) {
	imgs, err := r.Images.ProjectImages(ctx, name)
	if err != nil || len(imgs) == 0 {
		return imgs, err
	}
	keep := map[string]bool{}
	for svc, im := range imgs {
		ref := wire.RollbackTag(name, svc)
		repo, tag := splitTag(ref)
		if err := r.Images.TagImage(ctx, im.ImageID, repo, tag); err != nil {
			return imgs, fmt.Errorf("keep the image of %s: %w", svc, err)
		}
		keep[ref] = true
	}
	old, err := r.Images.RepoTags(ctx, wire.RollbackRepo(name))
	if err != nil {
		return imgs, nil // the new tags are in place; stale ones only cost disk space
	}
	for _, t := range old {
		if !keep[t] {
			_ = r.Images.Untag(ctx, t) // a service that's gone; Docker keeps an image a container still uses
		}
	}
	return imgs, nil
}

// StackImages answers OpStackImages.
func (r *StackRunner) StackImages(ctx context.Context, name string, protect bool) (wire.StackImages, error) {
	if r.Images == nil {
		return wire.StackImages{}, errors.New("this agent can't record rollback points; update it to the latest tainerhub-agent image")
	}
	var imgs map[string]wire.StackImage
	var err error
	if protect {
		imgs, err = r.protect(ctx, name)
	} else {
		imgs, err = r.Images.ProjectImages(ctx, name)
	}
	if imgs == nil {
		imgs = map[string]wire.StackImage{}
	}
	return wire.StackImages{Services: imgs}, err
}

// nothingChanged ends rollback errors found before anything on the host
// changed; the hub then knows the stack isn't pinned.
const nothingChanged = "; " + wire.RollbackUnchanged

// rollback answers StackRollback: check every image is still here, pin
// them, keep the current images for a later rollback, write the override
// and run `up` with it. Nothing is built or pulled.
func (cr composeRunner) rollback(ctx context.Context, c wire.StackCommand, dir string, out io.Writer) error {
	r := cr.r
	if r.Images == nil {
		return errors.New("this agent can't roll back stacks; update it to the latest tainerhub-agent image" + nothingChanged)
	}
	if st, err := os.Lstat(dir); err == nil && st.Mode()&os.ModeSymlink != 0 {
		return errSymlink{dir}
	}
	// Only services the compose file still has: an override naming a
	// service that's gone would start it again as a new one.
	var resolved bytes.Buffer
	if err := cr.runSplit(ctx, &resolved, out, "--project-name", c.Name,
		"--file", filepath.Join(dir, composeFile), "--project-directory", dir, "config", "--format", "json"); err != nil {
		return fmt.Errorf("compose file is not valid: %w%s", err, nothingChanged)
	}
	have := composeServices(resolved.Bytes())
	var use []wire.RollbackService
	for _, s := range c.Rollback {
		if have[s.Service] {
			use = append(use, s)
		} else {
			fmt.Fprintf(out, "Skipping %s: the compose file no longer has this service.\n", s.Service)
		}
	}
	if len(use) == 0 {
		return errors.New("none of the recorded services are in the compose file any more" + nothingChanged)
	}
	fmt.Fprintln(out, "Checking the previous images are still on this host…")
	var gone []string
	for _, s := range use {
		err := r.Images.ImageExists(ctx, s.ImageID)
		switch {
		case errors.Is(err, errImageGone):
			what := shortImageID(s.ImageID)
			if s.Ref != "" {
				what = s.Ref + ", " + what
			}
			gone = append(gone, s.Service+" ("+what+")")
		case err != nil:
			return fmt.Errorf("check the image of %s: %w%s", s.Service, err, nothingChanged)
		}
	}
	if len(gone) > 0 {
		return fmt.Errorf("the previous image is gone from this host for %s: it was removed outside TainerHub (for example by docker image prune -a)%s",
			strings.Join(gone, ", "), nothingChanged)
	}
	for _, s := range use {
		repo, tag := splitTag(wire.PinnedTag(c.Name, s.Service))
		if err := r.Images.TagImage(ctx, s.ImageID, repo, tag); err != nil {
			return fmt.Errorf("pin the image of %s: %w", s.Service, err)
		}
		ref := s.Ref
		if ref == "" {
			ref = shortImageID(s.ImageID)
		}
		fmt.Fprintf(out, "%s → %s (%s)\n", s.Service, ref, shortImageID(s.ImageID))
	}
	// The images running now become the next rollback point's (the hub
	// recorded which they are), so the rollback itself can be undone.
	if _, err := r.protect(ctx, c.Name); err != nil {
		fmt.Fprintf(out, "Warning: couldn't keep the current images for a later rollback: %v\n", err)
	}
	tmp := filepath.Join(dir, wire.StackRollbackFile+".tmp")
	if err := writeNoFollow(tmp, rollbackOverride(c.Name, use), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, rollbackOverridePath(dir)); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	cr.pinned = true
	// Kept on failure too: some containers may already run the old images,
	// and later commands should keep to them until a deploy.
	return cr.compose(ctx, c.Name, dir, out, "up", "--detach", "--remove-orphans")
}

// unpin removes a stack's rollback override and its pinned tags, after a
// deploy brought it back to the newest images or when it's removed.
func (r *StackRunner) unpin(ctx context.Context, name, dir string, out io.Writer) {
	if err := os.Remove(rollbackOverridePath(dir)); err == nil && out != nil {
		fmt.Fprintln(out, "Returned to the latest images: the rollback pin is removed.")
	}
	if r.Images == nil {
		return
	}
	tags, err := r.Images.RepoTags(ctx, wire.PinnedRepo(name))
	if err != nil {
		return
	}
	for _, t := range tags {
		_ = r.Images.Untag(ctx, t)
	}
}

// forgetRollbackImages drops every rollback tag of a removed stack, so its
// old images can be pruned.
func (r *StackRunner) forgetRollbackImages(ctx context.Context, name string) {
	if r.Images == nil {
		return
	}
	tags, _ := r.Images.RepoTags(ctx, wire.RollbackRepo(name))
	for _, t := range tags {
		_ = r.Images.Untag(ctx, t)
	}
}
