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
)

// When a newer image has taken over a container's tag, Docker lists the
// container's image as a bare ID. List must report the name the container
// was created with, asking Docker once per container.
func TestListResolvesBareImageIDs(t *testing.T) {
	dir, err := os.MkdirTemp("", "li") // short: macOS limits socket paths to 104 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("ab", 32)
	var inspects atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /containers/json", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[
			{"Id":"c1","Names":["/solo"],"Image":"sha256:` + id + `","State":"running"},
			{"Id":"c2","Names":["/web"],"Image":"nginx:1.27","State":"running"},
			{"Id":"c3","Names":["/pinned"],"Image":"sha256:` + id + `","State":"running"}
		]`))
	})
	mux.HandleFunc("GET /containers/{id}/json", func(w http.ResponseWriter, r *http.Request) {
		inspects.Add(1)
		switch r.PathValue("id") {
		case "c1":
			w.Write([]byte(`{"Config":{"Image":"localhost:5000/demo/hello:1"}}`))
		case "c3": // really created from an ID
			w.Write([]byte(`{"Config":{"Image":"sha256:` + id + `"}}`))
		default:
			http.NotFound(w, r)
		}
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })

	d := NewSocketDocker(sock)
	for round := 0; round < 2; round++ {
		cs, err := d.List(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]string{}
		for _, c := range cs {
			got[c.Name] = c.Image
		}
		if got["solo"] != "localhost:5000/demo/hello:1" || got["web"] != "nginx:1.27" || got["pinned"] != "sha256:"+id {
			t.Fatalf("round %d: images %v", round, got)
		}
	}
	if n := inspects.Load(); n != 2 {
		t.Fatalf("%d inspects over two lists; want 2 (cached per container)", n)
	}
}
