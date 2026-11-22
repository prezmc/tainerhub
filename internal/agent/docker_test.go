package agent

import (
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prezmc/tainerhub/internal/wire"
)

func frame(stream byte, s string) []byte {
	h := make([]byte, 8)
	h[0] = stream
	binary.BigEndian.PutUint32(h[4:], uint32(len(s)))
	return append(h, s...)
}

func fakeEngine(t *testing.T) *SocketDocker {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "docker.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /containers/json", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("all") != "1" {
			t.Error("expected all=1")
		}
		w.Write([]byte(`[{"Id":"abcdef1234567890","Names":["/web"],"Image":"nginx","ImageID":"sha256:aaa","State":"running","Status":"Up 1 hour",
			"Mounts":[{"Type":"volume","Name":"pgdata"}],"NetworkSettings":{"Networks":{"proxy":{}}},
			"Labels":{"com.docker.compose.project":"site"},
			"Ports":[{"IP":"0.0.0.0","PrivatePort":80,"PublicPort":8080,"Type":"tcp"},{"IP":"::","PrivatePort":80,"PublicPort":8080,"Type":"tcp"}]}]`))
	})
	mux.HandleFunc("POST /containers/{id}/stop", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") == "missing" {
			w.WriteHeader(404)
			w.Write([]byte(`{"message":"No such container: missing"}`))
			return
		}
		w.WriteHeader(204)
	})
	mux.HandleFunc("GET /containers/{id}/json", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"Config":{"Tty":false}}`))
	})
	mux.HandleFunc("GET /containers/{id}/logs", func(w http.ResponseWriter, r *http.Request) {
		w.Write(frame(1, "out line\n"))
		w.Write(frame(2, "err line\n"))
	})
	mux.HandleFunc("GET /images/json", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"Id":"sha256:aaa","RepoTags":["nginx:1.27"],"Size":190000000,"Created":2},{"Id":"sha256:bbb","RepoTags":["<none>:<none>"],"Size":5,"Created":1}]`))
	})
	mux.HandleFunc("POST /images/prune", func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Query().Get("filters"), `"dangling":["true"]`) {
			t.Errorf("default prune should be dangling only: %s", r.URL.RawQuery)
		}
		w.Write([]byte(`{"ImagesDeleted":[{"Deleted":"sha256:bbb"}],"SpaceReclaimed":5}`))
	})
	mux.HandleFunc("POST /images/create", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("fromImage") == "nope/missing" {
			w.Write([]byte(`{"status":"Pulling from nope/missing"}` + "\n" + `{"error":"manifest unknown"}` + "\n"))
			return
		}
		w.Write([]byte(`{"status":"Pulling from library/nginx"}` + "\n" + `{"status":"Downloading","progress":"[=>  ]"}` + "\n" + `{"status":"Status: Image is up to date for nginx:1.27"}` + "\n"))
	})
	mux.HandleFunc("GET /volumes", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"Volumes":[{"Name":"pgdata","Driver":"local","Labels":{"com.docker.compose.project":"tainerhub"}}]}`))
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return NewSocketDocker(sock)
}

func TestOperations(t *testing.T) {
	d := fakeEngine(t)
	ctx := context.Background()
	res, err := d.Operate(ctx, wire.Operation{Kind: wire.OpImagesList})
	if err != nil {
		t.Fatal(err)
	}
	imgs := res.([]wire.Image)
	if len(imgs) != 2 || imgs[0].Tags[0] != "nginx:1.27" || len(imgs[0].UsedBy) != 1 || imgs[0].UsedBy[0] != "web" || len(imgs[1].Tags) != 0 {
		t.Fatalf("images: %+v", imgs)
	}
	res, err = d.Operate(ctx, wire.Operation{Kind: wire.OpVolumesList})
	if err != nil {
		t.Fatal(err)
	}
	if v := res.([]wire.Volume); len(v) != 1 || v[0].Project != "tainerhub" || v[0].UsedBy[0] != "web" {
		t.Fatalf("volumes: %+v", v)
	}
	res, err = d.Operate(ctx, wire.Operation{Kind: wire.OpImagesPrune})
	if err != nil || res.(wire.PruneResult) != (wire.PruneResult{Deleted: 1, Reclaimed: 5}) {
		t.Fatalf("prune: %v %+v", err, res)
	}
	res, err = d.Operate(ctx, wire.Operation{Kind: wire.OpImagePull, Arg: "nginx:1.27"})
	if err != nil || res.(wire.PullResult).Status != "Status: Image is up to date for nginx:1.27" {
		t.Fatalf("pull: %v %+v", err, res)
	}
	if _, err := d.Operate(ctx, wire.Operation{Kind: wire.OpImagePull, Arg: "nope/missing"}); err == nil || err.Error() != "manifest unknown" {
		t.Fatalf("pull error: %v", err)
	}
	for _, bad := range []wire.Operation{
		{Kind: wire.OpImagePull, Arg: "../etc"},
		{Kind: wire.OpImageRemove, Target: "nginx"},
		{Kind: wire.OpVolumeRemove, Target: "a/b"},
		{Kind: wire.OpVolumesPrune, Arg: "everything"},
		{Kind: "system.prune"},
	} {
		if _, err := d.Operate(ctx, bad); err == nil {
			t.Errorf("%+v should be refused", bad)
		}
	}
}

func TestComputeUsage(t *testing.T) {
	// 2 of 4 cores busy for the sample: 200%; cgroup v2 page cache excluded.
	u := computeUsage(3_000, 1_000, 5_000, 1_000, 4, 600<<20, 2<<30, map[string]uint64{"inactive_file": 100 << 20})
	if u.CPU != 200 || u.Mem != 500<<20 || u.MemLimit != 2<<30 {
		t.Fatalf("got %+v", u)
	}
	// cgroup v1 key wins when present; no CPU delta means 0%.
	u = computeUsage(10, 10, 10, 10, 0, 300, 1000, map[string]uint64{"total_inactive_file": 100})
	if u.CPU != 0 || u.Mem != 200 {
		t.Fatalf("got %+v", u)
	}
}

func TestSocketDocker(t *testing.T) {
	d := fakeEngine(t)
	ctx := context.Background()

	cs, err := d.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 1 || cs[0].Name != "web" || cs[0].Project != "site" {
		t.Fatalf("list: %+v", cs)
	}
	if len(cs[0].Ports) != 1 || cs[0].Ports[0] != "8080->80/tcp" {
		t.Fatalf("ports should be de-duplicated: %v", cs[0].Ports)
	}

	if err := d.Action(ctx, "abc", "stop"); err != nil {
		t.Fatal(err)
	}
	if err := d.Action(ctx, "missing", "stop"); err == nil || !strings.Contains(err.Error(), "No such container") {
		t.Fatalf("expected docker error, got %v", err)
	}
	if err := d.Action(ctx, "abc", "explode"); err == nil {
		t.Fatal("unknown action should fail")
	}

	var buf bytes.Buffer
	if err := d.Logs(ctx, "abc", 50, false, &buf); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "out line\nerr line\n" {
		t.Fatalf("logs demux: %q", buf.String())
	}
}
