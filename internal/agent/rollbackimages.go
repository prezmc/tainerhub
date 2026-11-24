package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/prezmc/tainerhub/internal/wire"
)

// RollbackImages is the Docker side of stack rollbacks: finding what a
// compose project runs, and keeping (tagging) the images to go back to.
// SocketDocker implements it.
type RollbackImages interface {
	// ProjectImages is the image each service of a compose project runs.
	ProjectImages(ctx context.Context, project string) (map[string]wire.StackImage, error)
	// ImageExists fails with errImageGone if the image isn't on the host.
	ImageExists(ctx context.Context, id string) error
	// TagImage gives image src the name repo:tag (moving it if it exists).
	TagImage(ctx context.Context, src, repo, tag string) error
	// RepoTags lists the tags (repo:tag) the host has in repository repo.
	RepoTags(ctx context.Context, repo string) ([]string, error)
	// Untag removes one tag. An image left without tags and unused goes too.
	Untag(ctx context.Context, ref string) error
}

var errImageGone = errors.New("image not found")

func (d *SocketDocker) ProjectImages(ctx context.Context, project string) (map[string]wire.StackImage, error) {
	f, _ := json.Marshal(map[string][]string{"label": {"com.docker.compose.project=" + project}})
	var raw []struct {
		ID      string `json:"Id"`
		Names   []string
		Image   string
		ImageID string
		Labels  map[string]string
	}
	if err := d.getJSON(ctx, "/containers/json", url.Values{"all": {"1"}, "filters": {string(f)}}, &raw); err != nil {
		return nil, err
	}
	// The same service may have several containers (scale); they run the
	// same image, so the first by name stands for all.
	sort.Slice(raw, func(i, j int) bool { return strings.Join(raw[i].Names, ",") < strings.Join(raw[j].Names, ",") })
	out := map[string]wire.StackImage{}
	for _, c := range raw {
		svc := c.Labels["com.docker.compose.service"]
		if svc == "" || !wire.ValidServiceName(svc) || c.Labels["com.docker.compose.project"] != project {
			continue
		}
		if c.Labels["com.docker.compose.oneoff"] == "True" { // `compose run` leftovers
			continue
		}
		if _, ok := out[svc]; ok || c.ImageID == "" {
			continue
		}
		ref := c.Image
		if looksLikeImageID(ref) {
			// Docker lists the ID once the tag moved on: the name the
			// container was created with is better.
			if cfg := d.configImage(ctx, c.ID); cfg != "" {
				ref = cfg
			}
		}
		im := wire.StackImage{Ref: ref, ImageID: c.ImageID}
		var insp struct {
			RepoTags    []string
			RepoDigests []string
			Created     string
		}
		if err := d.getJSON(ctx, "/images/"+url.PathEscape(c.ImageID)+"/json", nil, &insp); err == nil {
			im.Created = insp.Created
			im.Ref = displayRef(ref, insp.RepoTags, insp.RepoDigests)
			im.Digest = pickDigest(im.Ref, insp.RepoDigests)
		}
		out[svc] = im
	}
	return out, nil
}

// displayRef is the name to show for an image a container runs. A
// rolled-back service runs a tainerhub-rollback/ tag; the image's own name
// (another tag, or the repository of its digest) says more.
func displayRef(ref string, tags, digests []string) string {
	if !wire.IsRollbackTag(ref) && !looksLikeImageID(ref) {
		return ref
	}
	for _, t := range tags {
		if !wire.IsRollbackTag(t) {
			return t
		}
	}
	for _, d := range digests {
		if !wire.IsRollbackTag(d) {
			return d
		}
	}
	return ref
}

// pickDigest is the repo digest for ref's repository, or the first one.
func pickDigest(ref string, digests []string) string {
	repo := ref
	if i := strings.LastIndex(repo, "@"); i >= 0 {
		repo = repo[:i]
	}
	if i := strings.LastIndex(repo, ":"); i > strings.LastIndex(repo, "/") {
		repo = repo[:i]
	}
	for _, d := range digests {
		if strings.HasPrefix(d, repo+"@") {
			return d
		}
	}
	if len(digests) > 0 {
		return digests[0]
	}
	return ""
}

func (d *SocketDocker) ImageExists(ctx context.Context, id string) error {
	resp, err := d.do(ctx, http.MethodGet, "/images/"+url.PathEscape(id)+"/json", nil)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "no such image") || strings.Contains(err.Error(), "404") {
			return errImageGone
		}
		return err
	}
	resp.Body.Close()
	return nil
}

func (d *SocketDocker) TagImage(ctx context.Context, src, repo, tag string) error {
	resp, err := d.do(ctx, http.MethodPost, "/images/"+url.PathEscape(src)+"/tag", url.Values{"repo": {repo}, "tag": {tag}})
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (d *SocketDocker) RepoTags(ctx context.Context, repo string) ([]string, error) {
	var raw []struct{ RepoTags []string }
	if err := d.getJSON(ctx, "/images/json", nil, &raw); err != nil {
		return nil, err
	}
	var out []string
	for _, im := range raw {
		for _, t := range im.RepoTags {
			if strings.HasPrefix(t, repo+":") {
				out = append(out, t)
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

func (d *SocketDocker) Untag(ctx context.Context, ref string) error {
	return d.remove(ctx, "/images/"+url.PathEscape(ref))
}

// keptFor lists the stacks an image's tags keep it for (rollback tags).
func keptFor(tags []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, t := range tags {
		if s := wire.RollbackStackOf(t); s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// removeImage answers OpImageRemove. An image kept for a stack's rollback
// is refused unless forced; forcing drops the rollback tags first (Docker
// won't remove an image by ID while it has several names).
func (d *SocketDocker) removeImage(ctx context.Context, id string, force bool) error {
	var insp struct{ RepoTags []string }
	if err := d.getJSON(ctx, "/images/"+url.PathEscape(id)+"/json", nil, &insp); err == nil {
		if stacks := keptFor(insp.RepoTags); len(stacks) > 0 {
			if !force {
				return fmt.Errorf("this image is kept so %s can be rolled back to it; an admin can remove it anyway (the rollback then fails)", strings.Join(stacks, ", "))
			}
			for _, t := range insp.RepoTags {
				if wire.IsRollbackTag(t) {
					if err := d.Untag(ctx, t); err != nil {
						return err
					}
				}
			}
			if len(insp.RepoTags) == len(rollbackTagsOf(insp.RepoTags)) {
				return nil // the last tag went, and with it the image (unless a container uses it)
			}
		}
	}
	return d.remove(ctx, "/images/"+url.PathEscape(id))
}

func rollbackTagsOf(tags []string) []string {
	var out []string
	for _, t := range tags {
		if wire.IsRollbackTag(t) {
			out = append(out, t)
		}
	}
	return out
}
