package agent

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
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

// TestPruneAllVolumesKeepsShares: a full volume prune never removes a
// volume that mounts a network share, even when no container uses it.
func TestPruneAllVolumesKeepsShares(t *testing.T) {
	dir, err := os.MkdirTemp("", "thv") // short: unix socket paths are limited
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	deleted := []string{}
	shares := true
	mux := http.NewServeMux()
	mux.HandleFunc("GET /containers/json", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"Names":["/app"],"Mounts":[{"Type":"volume","Name":"used"}]}]`))
	})
	mux.HandleFunc("GET /volumes", func(w http.ResponseWriter, r *http.Request) {
		share := `{"Name":"media","Driver":"local","Options":{"type":"cifs","device":"//192.0.2.10/media","o":"addr=192.0.2.10,username=u,password=pw"}},`
		if !shares {
			share = ""
		}
		w.Write([]byte(`{"Volumes":[` + share + `
			{"Name":"used","Driver":"local"},
			{"Name":"old","Driver":"local"},
			{"Name":"busy","Driver":"local"}]}`))
	})
	mux.HandleFunc("GET /system/df", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"Volumes":[{"Name":"old","UsageData":{"Size":300}},{"Name":"media","UsageData":{"Size":0}}]}`))
	})
	pruned := 0
	mux.HandleFunc("POST /volumes/prune", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		pruned++
		mu.Unlock()
		w.Write([]byte(`{"VolumesDeleted":["old","busy"],"SpaceReclaimed":300}`))
	})
	mux.HandleFunc("DELETE /volumes/{name}", func(w http.ResponseWriter, r *http.Request) {
		n := r.PathValue("name")
		if n == "busy" {
			w.WriteHeader(http.StatusConflict)
			w.Write([]byte(`{"message":"remove busy: volume is in use - [abc]"}`))
			return
		}
		mu.Lock()
		deleted = append(deleted, n)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })

	d := NewSocketDocker(sock)
	res, err := d.Operate(t.Context(), wire.Operation{Kind: wire.OpVolumesPrune, Arg: "all"})
	if err != nil {
		t.Fatal(err)
	}
	pr := res.(wire.PruneResult)
	if strings.Join(deleted, ",") != "old" || pruned != 0 {
		t.Fatalf("deleted %v, Docker prune calls %d", deleted, pruned)
	}
	if pr.Deleted != 1 || pr.Reclaimed != 300 || len(pr.Kept) != 2 || pr.Kept[0].Name != "media" || !strings.Contains(pr.Kept[0].Reason, "SMB //192.0.2.10/media") || strings.Contains(pr.Kept[0].Reason, "pw") {
		t.Fatalf("result %+v", pr)
	}
	if pr.Kept[1].Name != "busy" || !strings.Contains(pr.Kept[1].Reason, "in use") {
		t.Fatalf("kept %+v", pr.Kept)
	}

	// Without unused shares, Docker's own prune runs as before.
	shares = false
	if _, err := d.Operate(t.Context(), wire.Operation{Kind: wire.OpVolumesPrune, Arg: "all"}); err != nil || pruned != 1 {
		t.Fatalf("err %v, prune calls %d", err, pruned)
	}
}
