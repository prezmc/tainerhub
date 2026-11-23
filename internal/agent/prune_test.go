package agent

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/prezmc/tainerhub/internal/wire"
)

// TestPruneAllImagesSweeps covers a Docker whose own prune removes nothing:
// every image no container uses is then removed tag by tag, and one Docker
// refuses is reported with its reason.
func TestPruneAllImagesSweeps(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "docker.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	deleted := []string{}
	layers := int64(1000)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /containers/json", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"Names":["/web"],"ImageID":"sha256:aaa"}]`))
	})
	mux.HandleFunc("GET /images/json", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[
			{"Id":"sha256:aaa","RepoTags":["nginx:1.27"],"Size":100,"Created":5},
			{"Id":"sha256:ccc","RepoTags":["observe/enrol:dev"],"Size":100,"Created":4},
			{"Id":"sha256:ddd","RepoTags":["observe/a:dev","observe/b:dev"],"Size":100,"Created":3},
			{"Id":"sha256:eee","RepoTags":["observe/stuck:dev"],"Size":100,"Created":2},
			{"Id":"sha256:fff0123456789abcdef","RepoTags":[],"Size":100,"Created":1}]`))
	})
	mux.HandleFunc("POST /images/prune", func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Query().Get("filters"), `"dangling":["false"]`) {
			t.Errorf("all-unused prune should send dangling=false: %s", r.URL.RawQuery)
		}
		w.Write([]byte(`{"ImagesDeleted":null,"SpaceReclaimed":0}`))
	})
	mux.HandleFunc("DELETE /images/{ref...}", func(w http.ResponseWriter, r *http.Request) {
		ref, _ := url.PathUnescape(r.PathValue("ref"))
		if r.URL.Query().Get("force") != "" {
			t.Errorf("image removal must not force: %s", r.URL.RawQuery)
		}
		switch ref {
		case "nginx:1.27", "sha256:aaa":
			t.Errorf("removed %s, which a container uses", ref)
		case "observe/stuck:dev":
			w.WriteHeader(http.StatusConflict)
			w.Write([]byte(`{"message":"conflict: unable to remove repository reference \"observe/stuck:dev\" (must force) - container 1a2b is using its referenced image eee"}`))
			return
		}
		mu.Lock()
		deleted = append(deleted, ref)
		layers -= 100
		mu.Unlock()
		w.Write([]byte(`[]`))
	})
	mux.HandleFunc("GET /system/df", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		fmt.Fprintf(w, `{"LayersSize":%d}`, layers)
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })

	d := NewSocketDocker(sock)
	res, err := d.Operate(t.Context(), wire.Operation{Kind: wire.OpImagesPrune, Arg: "all"})
	if err != nil {
		t.Fatal(err)
	}
	pr := res.(wire.PruneResult)
	if pr.Deleted != 3 {
		t.Errorf("deleted %d images, want 3 (enrol, a+b, the untagged one)", pr.Deleted)
	}
	if want := "observe/enrol:dev|observe/a:dev|observe/b:dev|sha256:fff0123456789abcdef"; strings.Join(deleted, "|") != want {
		t.Errorf("removed %v", deleted)
	}
	if len(pr.Kept) != 1 || pr.Kept[0].Name != "observe/stuck:dev" || !strings.Contains(pr.Kept[0].Reason, "container 1a2b is using") {
		t.Errorf("kept %+v", pr.Kept)
	}
	if pr.Reclaimed != 400 {
		t.Errorf("reclaimed %d, want 400 (from Docker's disk usage before and after)", pr.Reclaimed)
	}
}
