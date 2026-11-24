package agent

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/prezmc/tainerhub/internal/wire"
)

// fakeImages is RollbackImages over an in-memory set of images and tags.
type fakeImages struct {
	mu      sync.Mutex
	running map[string]wire.StackImage // what ProjectImages returns
	images  map[string]bool            // image IDs on the host
	tags    map[string]string          // repo:tag -> image ID
	calls   []string
}

func newFakeImages() *fakeImages {
	return &fakeImages{running: map[string]wire.StackImage{}, images: map[string]bool{}, tags: map[string]string{}}
}

func (f *fakeImages) ProjectImages(_ context.Context, _ string) (map[string]wire.StackImage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]wire.StackImage{}
	for k, v := range f.running {
		out[k] = v
	}
	return out, nil
}

func (f *fakeImages) ImageExists(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.images[id] {
		return errImageGone
	}
	return nil
}

func (f *fakeImages) TagImage(_ context.Context, src, repo, tag string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "tag "+repo+":"+tag)
	f.tags[repo+":"+tag] = src
	return nil
}

func (f *fakeImages) RepoTags(_ context.Context, repo string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for t := range f.tags {
		if strings.HasPrefix(t, repo+":") {
			out = append(out, t)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (f *fakeImages) Untag(_ context.Context, ref string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "untag "+ref)
	delete(f.tags, ref)
	return nil
}

func imgID(c byte) string { return "sha256:" + strings.Repeat(string(c), 64) }

// deployedRunner is a fake-compose runner with stack "app" deployed and
// fake images: web ran image a before, runs b now.
func deployedRunner(t *testing.T) (*StackRunner, *fakeImages, string, string) {
	t.Helper()
	r, logs := fakeCompose(t)
	imgs := newFakeImages()
	r.Images = imgs
	imgs.images[imgID('a')], imgs.images[imgID('b')] = true, true
	imgs.running["web"] = wire.StackImage{Ref: "nginx:1", ImageID: imgID('b')}
	var out bytes.Buffer
	if err := r.Run(context.Background(), wire.StackCommand{Action: wire.StackDeploy, Name: "app", Compose: goodCompose}, &out); err != nil {
		t.Fatalf("deploy: %v\n%s", err, out.String())
	}
	return r, imgs, logs, filepath.Join(r.Dir, "app")
}

func TestRollbackOverrideFile(t *testing.T) {
	got := string(rollbackOverride("app", []wire.RollbackService{
		{Service: "web", ImageID: imgID('a')},
		{Service: ".hidden", ImageID: imgID('b')},
	}))
	want := `services:
  ".hidden":
    image: "tainerhub-rollback/app/pinned:_.hidden"
    pull_policy: never
  "web":
    image: "tainerhub-rollback/app/pinned:web"
    pull_policy: never
`
	if !strings.HasSuffix(got, want) {
		t.Fatalf("override:\n%s\nwant it to end with:\n%s", got, want)
	}
	// Names that could break YAML never reach the file: the command is refused.
	for _, bad := range []string{"web\n    privileged: true", "a: b", `x"y`, "", strings.Repeat("s", 121)} {
		if wire.ValidRollback([]wire.RollbackService{{Service: bad, ImageID: imgID('a')}}) == nil {
			t.Errorf("service name %q accepted", bad)
		}
	}
}

func TestRollbackRunsOnlyRecordedServicesTheFileHas(t *testing.T) {
	r, imgs, logs, dir := deployedRunner(t)
	var out bytes.Buffer
	err := r.Run(context.Background(), wire.StackCommand{Action: wire.StackRollback, Name: "app", Rollback: []wire.RollbackService{
		{Service: "web", ImageID: imgID('a'), Ref: "nginx:1"},
		{Service: "gone", ImageID: imgID('a')}, // no longer in the compose file
	}}, &out)
	if err != nil {
		t.Fatalf("rollback: %v\n%s", err, out.String())
	}
	b, err := os.ReadFile(filepath.Join(dir, wire.StackRollbackFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"web"`) || strings.Contains(string(b), "gone") {
		t.Fatalf("override:\n%s", b)
	}
	if !strings.Contains(out.String(), "Skipping gone") {
		t.Fatalf("output:\n%s", out.String())
	}
	// Pinned to the old image; the current one kept for undoing the rollback.
	if imgs.tags["tainerhub-rollback/app/pinned:web"] != imgID('a') || imgs.tags["tainerhub-rollback/app:web"] != imgID('b') {
		t.Fatalf("tags: %v", imgs.tags)
	}
	c := calls(t, logs)
	last := c[len(c)-1]
	if !strings.Contains(last, "/app/compose.yaml --file "+filepath.Join(dir, wire.StackRollbackFile)) || !strings.HasSuffix(last, "up --detach --remove-orphans") ||
		strings.Contains(last, "--build") || strings.Contains(last, "pull") {
		t.Fatalf("up call: %s", last)
	}
}

func TestRollbackFailsBeforeChangesWhenAnImageIsGone(t *testing.T) {
	r, imgs, logs, dir := deployedRunner(t)
	before := len(calls(t, logs))
	delete(imgs.images, imgID('a')) // someone ran docker image prune -a
	var out bytes.Buffer
	err := r.Run(context.Background(), wire.StackCommand{Action: wire.StackRollback, Name: "app", Rollback: []wire.RollbackService{{Service: "web", ImageID: imgID('a'), Ref: "nginx:1"}}}, &out)
	if err == nil || !strings.Contains(err.Error(), "the previous image is gone from this host for web (nginx:1, aaaaaaaaaaaa)") || !strings.Contains(err.Error(), wire.RollbackUnchanged) {
		t.Fatalf("err = %v", err)
	}
	if fileExists(filepath.Join(dir, wire.StackRollbackFile)) || len(imgs.calls) != 0 {
		t.Fatalf("changed things: override=%v calls=%v", fileExists(filepath.Join(dir, wire.StackRollbackFile)), imgs.calls)
	}
	for _, c := range calls(t, logs)[before:] {
		if strings.Contains(c, " up ") || strings.HasSuffix(c, "up --detach --remove-orphans") {
			t.Fatalf("ran up: %s", c)
		}
	}
}

func TestPinnedStackKeepsOverrideUntilDeploy(t *testing.T) {
	r, imgs, logs, dir := deployedRunner(t)
	ctx := context.Background()
	var out bytes.Buffer
	if err := r.Run(ctx, wire.StackCommand{Action: wire.StackRollback, Name: "app", Rollback: []wire.RollbackService{{Service: "web", ImageID: imgID('a')}}}, &out); err != nil {
		t.Fatal(err)
	}
	override := "--file " + filepath.Join(dir, wire.StackRollbackFile)
	for _, a := range []string{wire.StackRestart, wire.StackStop, wire.StackStart, wire.StackDown} {
		if err := r.Run(ctx, wire.StackCommand{Action: a, Name: "app"}, &out); err != nil {
			t.Fatalf("%s: %v", a, err)
		}
		if c := calls(t, logs); !strings.Contains(c[len(c)-1], override) {
			t.Fatalf("%s without the override: %s", a, c[len(c)-1])
		}
	}

	// A deploy that fails keeps the stack pinned.
	if err := r.Run(ctx, wire.StackCommand{Action: wire.StackDeploy, Name: "app", Compose: "INVALID"}, &out); err == nil {
		t.Fatal("invalid deploy accepted")
	}
	if !fileExists(filepath.Join(dir, wire.StackRollbackFile)) {
		t.Fatal("a failed deploy removed the rollback pin")
	}

	// Return to latest: a deploy runs without the override, then removes it
	// and the pinned tags (the rollback point's tags stay).
	out.Reset()
	if err := r.Run(ctx, wire.StackCommand{Action: wire.StackDeploy, Name: "app", Compose: goodCompose, Pull: true}, &out); err != nil {
		t.Fatal(err)
	}
	for _, c := range calls(t, logs)[len(calls(t, logs))-3:] {
		if strings.Contains(c, override) {
			t.Fatalf("deploy used the override: %s", c)
		}
	}
	if fileExists(filepath.Join(dir, wire.StackRollbackFile)) || !strings.Contains(out.String(), "Returned to the latest images") {
		t.Fatalf("override still there:\n%s", out.String())
	}
	if _, ok := imgs.tags["tainerhub-rollback/app/pinned:web"]; ok {
		t.Fatalf("pinned tag left: %v", imgs.tags)
	}
	if _, ok := imgs.tags["tainerhub-rollback/app:web"]; !ok {
		t.Fatalf("rollback tag removed: %v", imgs.tags)
	}

	// Removing the stack drops all its rollback tags.
	if err := r.Run(ctx, wire.StackCommand{Action: wire.StackRemove, Name: "app"}, &out); err != nil {
		t.Fatal(err)
	}
	if len(imgs.tags) != 0 {
		t.Fatalf("tags left after remove: %v", imgs.tags)
	}
}

func TestProtectRetagsAndDropsStaleTags(t *testing.T) {
	r, imgs, _, _ := deployedRunner(t)
	ctx := context.Background()
	imgs.tags["tainerhub-rollback/app:old"] = imgID('c')
	imgs.tags["tainerhub-rollback/app/pinned:web"] = imgID('a') // not this repo: left alone
	res, err := r.StackImages(ctx, "app", true)
	if err != nil || res.Services["web"].ImageID != imgID('b') {
		t.Fatalf("%+v %v", res, err)
	}
	if imgs.tags["tainerhub-rollback/app:web"] != imgID('b') {
		t.Fatalf("not tagged: %v", imgs.tags)
	}
	if _, ok := imgs.tags["tainerhub-rollback/app:old"]; ok {
		t.Fatal("stale tag kept")
	}
	if _, ok := imgs.tags["tainerhub-rollback/app/pinned:web"]; !ok {
		t.Fatal("pinned tag removed")
	}
	// Nothing running: the tags of the last point stay.
	imgs.running = map[string]wire.StackImage{}
	if res, err := r.StackImages(ctx, "app", true); err != nil || len(res.Services) != 0 || res.Services == nil {
		t.Fatalf("%+v %v", res, err)
	}
	if _, ok := imgs.tags["tainerhub-rollback/app:web"]; !ok {
		t.Fatal("tag dropped while nothing ran")
	}
	// Without Images (an e2e or older setup) recording is refused, not faked.
	r.Images = nil
	if _, err := r.StackImages(ctx, "app", false); err == nil {
		t.Fatal("no error without Images")
	}
}

// ---- prune and remove paths (real HTTP against a fake Docker) ----

// (Short test names: the fake socket path must stay under the OS limit.)
func TestPruneKeepsRollback(t *testing.T) {
	var mu sync.Mutex
	var deleted []string
	pruned := false
	mux := http.NewServeMux()
	mux.HandleFunc("GET /containers/json", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `[]`) })
	mux.HandleFunc("GET /images/json", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `[
			{"Id":"sha256:aaa","RepoTags":["tainerhub-rollback/app:web"],"Size":1,"Created":3},
			{"Id":"sha256:bbb","RepoTags":["nginx:1","tainerhub-rollback/shop/pinned:db"],"Size":1,"Created":2},
			{"Id":"sha256:ccc","RepoTags":["old:1"],"Size":1,"Created":1}]`)
	})
	mux.HandleFunc("POST /images/prune", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		pruned = true
		mu.Unlock()
		io.WriteString(w, `{"ImagesDeleted":null,"SpaceReclaimed":0}`)
	})
	mux.HandleFunc("DELETE /images/{ref...}", func(w http.ResponseWriter, r *http.Request) {
		ref, _ := url.PathUnescape(r.PathValue("ref"))
		mu.Lock()
		deleted = append(deleted, ref)
		mu.Unlock()
		io.WriteString(w, `[]`)
	})
	mux.HandleFunc("GET /system/df", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"LayersSize":10}`) })
	d := fakeDocker(t, mux)

	res, err := d.Operate(t.Context(), wire.Operation{Kind: wire.OpImagesPrune, Arg: "all"})
	if err != nil {
		t.Fatal(err)
	}
	pr := res.(wire.PruneResult)
	mu.Lock()
	defer mu.Unlock()
	if pruned {
		t.Fatal("Docker's own prune -a ran while images were kept for rollbacks")
	}
	if strings.Join(deleted, ",") != "old:1" || pr.Deleted != 1 {
		t.Fatalf("deleted %v (%d)", deleted, pr.Deleted)
	}
	if len(pr.Kept) != 2 || !strings.Contains(pr.Kept[0].Reason, "app can be rolled back") || !strings.Contains(pr.Kept[1].Reason, "shop") {
		t.Fatalf("kept: %+v", pr.Kept)
	}
	// The dangling-only prune is Docker's own: tagged images are never dangling.
	deleted, pruned = nil, false
	mu.Unlock()
	if _, err := d.Operate(t.Context(), wire.Operation{Kind: wire.OpImagesPrune}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if !pruned || len(deleted) != 0 {
		t.Fatalf("dangling prune: pruned=%v deleted=%v", pruned, deleted)
	}

	imgs, err := d.images(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(imgs[0].KeptFor, imgs[1].KeptFor, imgs[2].KeptFor) != "[app] [shop] []" {
		t.Fatalf("keptFor: %v %v %v", imgs[0].KeptFor, imgs[1].KeptFor, imgs[2].KeptFor)
	}
}

func TestRemoveKeptImage(t *testing.T) {
	var mu sync.Mutex
	var deleted []string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /images/{id}/json", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"RepoTags":["nginx:1","tainerhub-rollback/app:web"]}`)
	})
	mux.HandleFunc("DELETE /images/{ref...}", func(w http.ResponseWriter, r *http.Request) {
		ref, _ := url.PathUnescape(r.PathValue("ref"))
		mu.Lock()
		deleted = append(deleted, ref)
		mu.Unlock()
		io.WriteString(w, `[]`)
	})
	d := fakeDocker(t, mux)
	id := strings.Repeat("ab", 32)
	_, err := d.Operate(t.Context(), wire.Operation{Kind: wire.OpImageRemove, Target: id})
	if err == nil || !strings.Contains(err.Error(), "kept so app can be rolled back") {
		t.Fatalf("err = %v", err)
	}
	if len(deleted) != 0 {
		t.Fatalf("deleted %v", deleted)
	}
	if _, err := d.Operate(t.Context(), wire.Operation{Kind: wire.OpImageRemove, Target: id, Arg: wire.ImageRemoveForce}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(deleted, ",") != "tainerhub-rollback/app:web,"+id {
		t.Fatalf("forced remove: %v", deleted)
	}
}

func TestProjectImagesFromDocker(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /containers/json", func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Query().Get("filters"), "com.docker.compose.project=app") {
			t.Errorf("filters: %s", r.URL.RawQuery)
		}
		io.WriteString(w, `[
			{"Id":"1","Names":["/app-web-1"],"Image":"tainerhub-rollback/app/pinned:web","ImageID":"sha256:aaa","Labels":{"com.docker.compose.project":"app","com.docker.compose.service":"web"}},
			{"Id":"2","Names":["/app-db-1"],"Image":"postgres:16","ImageID":"sha256:bbb","Labels":{"com.docker.compose.project":"app","com.docker.compose.service":"db"}},
			{"Id":"3","Names":["/app-db-run-x"],"Image":"postgres:16","ImageID":"sha256:ccc","Labels":{"com.docker.compose.project":"app","com.docker.compose.service":"db","com.docker.compose.oneoff":"True"}}]`)
	})
	mux.HandleFunc("GET /images/{id}/json", func(w http.ResponseWriter, r *http.Request) {
		switch r.PathValue("id") {
		case "sha256:aaa":
			io.WriteString(w, `{"RepoTags":["tainerhub-rollback/app/pinned:web","tainerhub-rollback/app:web"],"RepoDigests":["nginx@sha256:d1"],"Created":"2026-01-01T00:00:00Z"}`)
		default:
			io.WriteString(w, `{"RepoTags":["postgres:16"],"RepoDigests":["other@sha256:x","postgres@sha256:d2"],"Created":"2025-01-01T00:00:00Z"}`)
		}
	})
	d := fakeDocker(t, mux)
	got, err := d.ProjectImages(t.Context(), "app")
	if err != nil {
		t.Fatal(err)
	}
	web, db := got["web"], got["db"]
	if len(got) != 2 || web.Ref != "nginx@sha256:d1" || web.ImageID != "sha256:aaa" || db.Digest != "postgres@sha256:d2" || db.ImageID != "sha256:bbb" || db.Created == "" {
		t.Fatalf("%+v", got)
	}
}
