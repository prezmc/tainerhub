package agent

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/prezmc/tainerhub/internal/wire"
)

// After Pull & redeploy (or a recreate) a container runs the registry's
// newer image: it must stop showing an update right away, while a container
// still on the old image keeps showing it, without a new update check.
func TestUpdateBadgeClearsOnceTheContainerRunsTheNewImage(t *testing.T) {
	dir, err := os.MkdirTemp("", "ub") // short: macOS limits socket paths to 104 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var inspects atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /containers/json", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[
			{"Id":"c1","Names":["/db-new"],"Image":"postgres:16","ImageID":"sha256:new","State":"running"},
			{"Id":"c2","Names":["/db-old"],"Image":"postgres:16","ImageID":"sha256:old","State":"running"},
			{"Id":"c3","Names":["/web"],"Image":"nginx:1.27","ImageID":"sha256:web","State":"running"}
		]`))
	})
	mux.HandleFunc("GET /images/{id}/json", func(w http.ResponseWriter, r *http.Request) {
		inspects.Add(1)
		switch r.PathValue("id") {
		case "sha256:new":
			w.Write([]byte(`{"RepoDigests":["postgres@sha256:bbb"]}`))
		case "sha256:old":
			w.Write([]byte(`{"RepoDigests":["postgres@sha256:aaa"]}`))
		default:
			http.NotFound(w, r)
		}
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })

	d := NewSocketDocker(sock)
	d.updates.set(wire.ImageUpdates{
		Images: map[string]bool{"postgres:16": true, "nginx:1.27": false},
		Remote: map[string]string{"postgres:16": "sha256:bbb"},
	})
	for round := 0; round < 2; round++ {
		cs, err := d.List(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]bool{}
		for _, c := range cs {
			got[c.Name] = c.UpdateAvailable
		}
		if got["db-new"] || !got["db-old"] || got["web"] {
			t.Fatalf("round %d: updates %v; want only db-old", round, got)
		}
	}
	if n := inspects.Load(); n != 2 {
		t.Fatalf("%d image inspects over two lists; want 2 (cached per image)", n)
	}

	// A later check that fails for the image keeps its answer and digest.
	d.updates.set(wire.ImageUpdates{Images: map[string]bool{}, Errors: map[string]string{"postgres:16": "offline"}})
	if n, digest := d.updates.newer("postgres:16"); !n || digest != "sha256:bbb" {
		t.Fatalf("after a failed check: newer %v digest %q", n, digest)
	}
}

// Two stacks share postgres:16. One was updated, so the tag on disk is the
// registry's image, but the other still runs the old one: the check must
// say that container is behind, not "up to date".
func TestCheckUpdatesComparesEachContainersImage(t *testing.T) {
	dir, err := os.MkdirTemp("", "cu")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	oldID, newID := strings.Repeat("a1", 32), strings.Repeat("b2", 32)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /containers/json", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[
			{"Id":"c1","Names":["/a-db-1"],"Image":"postgres:16","ImageID":"sha256:` + newID + `","State":"running"},
			{"Id":"c2","Names":["/b-db-1"],"Image":"sha256:` + oldID + `","ImageID":"sha256:` + oldID + `","State":"running"}
		]`))
	})
	mux.HandleFunc("GET /containers/c2/json", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"Config":{"Image":"postgres:16"}}`))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch p := r.URL.Path; {
		case strings.HasPrefix(p, "/distribution/"):
			w.Write([]byte(`{"Descriptor":{"Digest":"sha256:bbb"}}`))
		case p == "/images/postgres:16/json", p == "/images/sha256:"+newID+"/json":
			w.Write([]byte(`{"RepoDigests":["postgres@sha256:bbb"]}`))
		case p == "/images/sha256:"+oldID+"/json":
			w.Write([]byte(`{"RepoDigests":["postgres@sha256:aaa"]}`))
		default:
			http.NotFound(w, r)
		}
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })

	d := NewSocketDocker(sock)
	res, err := d.checkUpdates(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Images["postgres:16"] || res.Containers["c1"] || !res.Containers["c2"] {
		t.Fatalf("images %v containers %v; want postgres:16 behind for c2 only", res.Images, res.Containers)
	}
	d.updates.set(res)
	cs, err := d.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cs {
		if want := c.Name == "b-db-1"; c.UpdateAvailable != want {
			t.Fatalf("%s: update %v, want %v", c.Name, c.UpdateAvailable, want)
		}
	}
}
