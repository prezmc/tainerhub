package agent

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/prezmc/tainerhub/internal/wire"
)

func TestNameVolumes(t *testing.T) {
	anon := strings.Repeat("ab", 32)
	hc := json.RawMessage(`{"Binds":["data:/data","/srv/x:/x:ro"],"Mounts":[{"Type":"volume","Target":"/m"}],"Memory":5}`)
	mounts := []exportMount{
		{Type: "volume", Name: "data", Destination: "/data", RW: true},
		{Type: "bind", Source: "/srv/x", Destination: "/x"},
		{Type: "volume", Name: anon, Destination: "/cache", RW: true}, // from the image's VOLUME line
		{Type: "volume", Name: "ro-anon", Destination: "/ro"},
		{Type: "volume", Name: "mnt-anon", Destination: "/m", RW: true}, // --mount type=volume,dst=/m
	}
	out, err := nameVolumes(hc, mounts)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Binds  []string
		Mounts []struct{ Type, Source, Target string }
		Memory int
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	want := []string{"data:/data", "/srv/x:/x:ro", anon + ":/cache", "ro-anon:/ro:ro"}
	if !reflect.DeepEqual(got.Binds, want) {
		t.Errorf("binds %v\nwant  %v", got.Binds, want)
	}
	if len(got.Mounts) != 1 || got.Mounts[0].Source != "mnt-anon" {
		t.Errorf("mounts %+v", got.Mounts)
	}
	if got.Memory != 5 {
		t.Error("other settings lost")
	}
}

func TestValidContainerCreate(t *testing.T) {
	ok := func() *wire.ContainerCreate {
		return &wire.ContainerCreate{Name: "web", Spec: json.RawMessage(`{"Image":"nginx:1"}`), Networks: []wire.NetworkSpec{{Name: "net", Driver: "bridge"}}}
	}
	if err := wire.ValidOperation(wire.Operation{Kind: wire.OpContainerCreate, Create: ok()}); err != nil {
		t.Fatal(err)
	}
	bad := map[string]func(*wire.ContainerCreate){
		"name":     func(c *wire.ContainerCreate) { c.Name = "../x" },
		"own name": func(c *wire.ContainerCreate) { c.Name = "tainerhub-agent" },
		"compose": func(c *wire.ContainerCreate) {
			c.Spec = json.RawMessage(`{"Image":"a","Labels":{"com.docker.compose.project":"p"}}`)
		},
		"image":       func(c *wire.ContainerCreate) { c.Spec = json.RawMessage(`{"Image":"a//b"}`) },
		"spec":        func(c *wire.ContainerCreate) { c.Spec = json.RawMessage(`not json`) },
		"net driver":  func(c *wire.ContainerCreate) { c.Networks[0].Driver = "macvlan" },
		"net name":    func(c *wire.ContainerCreate) { c.Networks[0].Name = "a/b" },
		"builtin net": func(c *wire.ContainerCreate) { c.Networks[0].Builtin = true },
	}
	for name, f := range bad {
		c := ok()
		f(c)
		if wire.ValidOperation(wire.Operation{Kind: wire.OpContainerCreate, Create: c}) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if wire.ValidOperation(wire.Operation{Kind: wire.OpContainerCreate}) == nil {
		t.Error("create without a container accepted")
	}
	if wire.ValidOperation(wire.Operation{Kind: wire.OpImagesList, Create: ok()}) == nil {
		t.Error("a container on another operation accepted")
	}
}

// fakeDocker serves a few Docker API calls on a unix socket.
func fakeDocker(t *testing.T, mux *http.ServeMux) *SocketDocker {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "docker.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { srv.Close() })
	return NewSocketDocker(sock)
}

func TestExportContainer(t *testing.T) {
	anon := strings.Repeat("cd", 32)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /containers/{id}/json", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{
			"Id": "aaaaaaaaaaaa1111", "Name": "/web", "Image": "sha256:old",
			"State": {"Running": true},
			"Config": {"Image": "nginx:1", "Hostname": "aaaaaaaaaaaa", "Env": ["PATH=/bin", "MODE=prod"], "Cmd": ["nginx"], "Labels": {"me": "1"}},
			"HostConfig": {"Binds": ["data:/data", "/srv/conf:/conf"], "NetworkMode": "web-net", "Privileged": false,
				"PortBindings": {"80/tcp": [{"HostIp": "", "HostPort": "8081"}], "443/tcp": [{"HostPort": ""}]}},
			"Mounts": [
				{"Type": "volume", "Name": "data", "Destination": "/data", "Driver": "local", "RW": true},
				{"Type": "volume", "Name": "`+anon+`", "Destination": "/cache", "Driver": "local", "RW": true},
				{"Type": "bind", "Source": "/srv/conf", "Destination": "/conf", "RW": true}
			],
			"NetworkSettings": {"Networks": {"web-net": {"Aliases": ["aaaaaaaaaaaa", "web"], "IPAMConfig": null}}}
		}`)
	})
	mux.HandleFunc("GET /images/{id}/json", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"Config": {"Env": ["PATH=/bin"], "Cmd": ["nginx"], "Labels": {}}}`)
	})
	mux.HandleFunc("GET /system/df", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"Volumes": [{"Name": "data", "Driver": "local", "UsageData": {"Size": 4096}},
			{"Name": "`+anon+`", "Driver": "local", "Labels": {"com.docker.volume.anonymous": ""}, "UsageData": {"Size": 10}}]}`)
	})
	mux.HandleFunc("GET /containers/json", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `[{"Names": ["/web"], "Mounts": [{"Type": "volume", "Name": "data"}]},
			{"Names": ["/backup"], "Mounts": [{"Type": "volume", "Name": "data"}]}]`)
	})
	mux.HandleFunc("GET /networks/{name}", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"Driver": "bridge", "Internal": true, "Labels": {"x": "y"}, "IPAM": {"Config": [{"Subnet": "172.30.0.0/16"}]}}`)
	})
	d := fakeDocker(t, mux)
	op := wire.Operation{Kind: wire.OpContainerExport, Target: "web"}
	if err := wire.ValidOperation(op); err != nil {
		t.Fatal(err)
	}
	res, err := d.Operate(context.Background(), op)
	if err != nil {
		t.Fatal(err)
	}
	e := res.(wire.ContainerExport)
	if e.Name != "web" || e.Image != "nginx:1" || !e.Running || len(e.Problems) != 0 {
		t.Errorf("export %+v", e)
	}
	if !reflect.DeepEqual(e.Ports, []string{"8081/tcp"}) || !reflect.DeepEqual(e.Binds, []string{"/srv/conf"}) {
		t.Errorf("ports %v binds %v", e.Ports, e.Binds)
	}
	if len(e.Volumes) != 2 || e.Volumes[0].Size != 4096 || !slices.Equal(e.Volumes[0].UsedBy, []string{"backup"}) || !e.Volumes[1].Anonymous {
		t.Errorf("volumes %+v", e.Volumes)
	}
	if len(e.Networks) != 1 || e.Networks[0].Name != "web-net" || !e.Networks[0].Internal || e.Networks[0].IPAM != nil {
		t.Errorf("networks %+v (no fixed address: the subnet isn't kept)", e.Networks)
	}
	var spec struct {
		Image            string
		Hostname         string
		Env              []string
		Cmd              []string
		HostConfig       struct{ Binds []string }
		NetworkingConfig struct {
			EndpointsConfig map[string]struct{ Aliases []string }
		}
	}
	if err := json.Unmarshal(e.Spec, &spec); err != nil {
		t.Fatal(err)
	}
	if spec.Image != "nginx:1" || spec.Hostname != "" || !slices.Equal(spec.Env, []string{"MODE=prod"}) || spec.Cmd != nil {
		t.Errorf("spec %+v", spec)
	}
	if !slices.Equal(spec.HostConfig.Binds, []string{"data:/data", "/srv/conf:/conf", anon + ":/cache"}) {
		t.Errorf("binds %v", spec.HostConfig.Binds)
	}
	if a := spec.NetworkingConfig.EndpointsConfig["web-net"].Aliases; !slices.Equal(a, []string{"web"}) {
		t.Errorf("aliases %v", a)
	}
}

func TestExportContainerProblems(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /containers/{id}/json", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"Id": "b1", "Name": "/side", "Image": "sha256:x",
			"Config": {"Image": "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"},
			"HostConfig": {"NetworkMode": "container:main", "VolumesFrom": ["main"], "Links": ["/db:/side/db"]},
			"NetworkSettings": {"Networks": {}}}`)
	})
	mux.HandleFunc("GET /images/{id}/json", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) })
	d := fakeDocker(t, mux)
	e, err := d.ExportContainer(context.Background(), "side")
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(e.Problems, " | ")
	for _, want := range []string{"another container's network", "--volumes-from", "--link", "image has no name"} {
		if !strings.Contains(got, want) {
			t.Errorf("problems %q lack %q", got, want)
		}
	}
}

func TestCreateContainer(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	failStart := false
	mux := http.NewServeMux()
	note := func(s string) {
		mu.Lock()
		calls = append(calls, s)
		mu.Unlock()
	}
	mux.HandleFunc("GET /networks", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `[{"Name": "bridge"}, {"Name": "have"}]`)
	})
	mux.HandleFunc("POST /networks/create", func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Name, Driver string
			Internal     bool
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		note("create network " + b.Name + " " + b.Driver)
		_, _ = io.WriteString(w, `{"Id": "n1"}`)
	})
	mux.HandleFunc("POST /containers/create", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		note("create " + r.URL.Query().Get("name") + " " + string(b))
		_, _ = io.WriteString(w, `{"Id": "c1"}`)
	})
	mux.HandleFunc("POST /containers/{id}/start", func(w http.ResponseWriter, r *http.Request) {
		note("start " + r.PathValue("id"))
		if failStart {
			w.WriteHeader(500)
			_, _ = io.WriteString(w, `{"message": "port is already allocated"}`)
			return
		}
		w.WriteHeader(204)
	})
	mux.HandleFunc("DELETE /containers/{id}", func(w http.ResponseWriter, r *http.Request) {
		note("remove " + r.PathValue("id"))
		w.WriteHeader(204)
	})
	mux.HandleFunc("DELETE /networks/{id}", func(w http.ResponseWriter, r *http.Request) {
		note("remove network " + r.PathValue("id"))
		w.WriteHeader(204)
	})
	d := fakeDocker(t, mux)
	c := &wire.ContainerCreate{Name: "web", Spec: json.RawMessage(`{"Image":"nginx:1"}`), Start: true,
		Networks: []wire.NetworkSpec{{Name: "have", Driver: "bridge"}, {Name: "new", Driver: "bridge"}}}
	res, err := d.CreateContainer(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if res.ID != "c1" || !slices.Equal(res.Networks, []string{"new"}) {
		t.Errorf("result %+v", res)
	}
	want := []string{"create network new bridge", `create web {"Image":"nginx:1"}`, "start c1"}
	if !slices.Equal(calls, want) {
		t.Errorf("calls %q\nwant  %q", calls, want)
	}

	// A failed start removes what it made.
	calls, failStart = nil, true
	if _, err := d.CreateContainer(context.Background(), c); err == nil || !strings.Contains(err.Error(), "already allocated") {
		t.Fatalf("err = %v", err)
	}
	want = []string{"create network new bridge", `create web {"Image":"nginx:1"}`, "start c1", "remove c1", "remove network new"}
	if !slices.Equal(calls, want) {
		t.Errorf("calls %q\nwant  %q", calls, want)
	}
}
