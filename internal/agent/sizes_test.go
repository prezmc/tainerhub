package agent

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// Sizes come from one /system/df call, are cached, and List reports them
// without asking Docker to measure again.
func TestRefreshSizes(t *testing.T) {
	dir, err := os.MkdirTemp("", "sz") // short: macOS limits socket paths to 104 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var dfs atomic.Int32
	var types []string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /system/df", func(w http.ResponseWriter, r *http.Request) {
		dfs.Add(1)
		types = r.URL.Query()["type"]
		w.Write([]byte(`{
			"Containers":[
				{"Id":"c1","SizeRw":4096,"Mounts":[{"Type":"volume","Name":"data"},{"Type":"volume","Name":"data"},{"Type":"bind","Name":""}]},
				{"Id":"c2","SizeRw":0,"Mounts":[{"Type":"volume","Name":"data"},{"Type":"volume","Name":"nas"},{"Type":"volume","Name":"unmeasured"},{"Type":"volume","Name":"plugin"}]}
			],
			"Volumes":[
				{"Name":"data","Driver":"local","UsageData":{"Size":1000000,"RefCount":2}},
				{"Name":"nas","Driver":"local","Options":{"type":"nfs","o":"addr=192.0.2.1","device":":/export"},"UsageData":{"Size":99999999}},
				{"Name":"unmeasured","Driver":"local","UsageData":{"Size":-1}},
				{"Name":"plugin","Driver":"someplugin","UsageData":{"Size":5}}
			]
		}`))
	})
	mux.HandleFunc("GET /containers/json", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Has("size") {
			t.Error("List must not ask Docker for sizes")
		}
		w.Write([]byte(`[
			{"Id":"c1","Names":["/app"],"Image":"nginx:1.27","State":"running"},
			{"Id":"c2","Names":["/db"],"Image":"postgres:17","State":"running"},
			{"Id":"c3","Names":["/new"],"Image":"redis:7","State":"running"}
		]`))
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })

	d := NewSocketDocker(sock)
	ctx := context.Background()

	cs, err := d.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cs {
		if c.Size != nil {
			t.Fatalf("%s has a size before any measurement: %+v", c.Name, c.Size)
		}
	}

	if ok, err := d.RefreshSizes(ctx, time.Hour); err != nil || !ok {
		t.Fatalf("first refresh: measured=%v err=%v", ok, err)
	}
	if len(types) != 2 || types[0] != "container" || types[1] != "volume" {
		t.Errorf("df asked for %v, want only containers and volumes", types)
	}
	if ok, _ := d.RefreshSizes(ctx, time.Hour); ok || dfs.Load() != 1 {
		t.Fatalf("a fresh measurement must be reused (measured=%v, df calls=%d)", ok, dfs.Load())
	}

	cs, err = d.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][2]int64{}
	for _, c := range cs {
		if c.Size == nil {
			if c.ID != "c3" {
				t.Errorf("%s has no size", c.Name)
			}
			continue
		}
		if c.Size.At == 0 {
			t.Errorf("%s: no measurement time", c.Name)
		}
		got[c.ID] = [2]int64{c.Size.RW, c.Size.Volumes}
	}
	// c1 mounts data twice (counted once); c2 shares it, and its NFS share,
	// unmeasured and plugin volumes don't count.
	want := map[string][2]int64{"c1": {4096, 1000000}, "c2": {0, 1000000}}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("%s: got rw,volumes %v, want %v", id, got[id], w)
		}
	}

	// Once the measurement is older than maxAge, measure again.
	if ok, err := d.RefreshSizes(ctx, 0); err != nil || !ok || dfs.Load() != 2 {
		t.Fatalf("stale refresh: measured=%v err=%v df calls=%d", ok, err, dfs.Load())
	}
}
