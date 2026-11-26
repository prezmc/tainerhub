package agent

import (
	"context"
	"net"
	"net/http"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/prezmc/tainerhub/internal/wire"
)

// portsEngine is a Docker Engine with one running container publishing 8080
// on every address (IPv4 and IPv6) and 9000/udp on loopback only, one on the
// host network, and one stopped container with port bindings.
func portsEngine(t *testing.T) *SocketDocker {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "docker.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /containers/json", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[
			{"Id":"aaa111","Names":["/web"],"Image":"nginx","State":"running","HostConfig":{"NetworkMode":"bridge"},
			 "Ports":[{"IP":"::","PrivatePort":80,"PublicPort":8080,"Type":"tcp"},{"IP":"0.0.0.0","PrivatePort":80,"PublicPort":8080,"Type":"tcp"},
			          {"IP":"127.0.0.1","PrivatePort":9000,"PublicPort":9000,"Type":"udp"},{"PrivatePort":443,"Type":"tcp"}]},
			{"Id":"bbb222","Names":["/dns"],"Image":"pihole","State":"running","HostConfig":{"NetworkMode":"host"},"Ports":[]}
		]`))
	})
	mux.HandleFunc("GET /containers/running/json", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"Id":"running","Name":"/web","State":{"Status":"running"},
			"HostConfig":{"PortBindings":{"80/tcp":[{"HostIp":"","HostPort":"8080"}]}},
			"NetworkSettings":{"Ports":{"80/tcp":[{"HostIp":"0.0.0.0","HostPort":"8080"},{"HostIp":"::","HostPort":"8080"}],
			                            "9000/udp":[{"HostIp":"127.0.0.1","HostPort":"9000"}],
			                            "53/udp":[{"HostIp":"::1","HostPort":"5353"}],
			                            "443/tcp":null}}}`))
	})
	mux.HandleFunc("GET /containers/stopped/json", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"Id":"stopped","Name":"/old","State":{"Status":"exited"},
			"HostConfig":{"PortBindings":{"80/tcp":[{"HostIp":"","HostPort":"8081"}],"443/tcp":[{"HostIp":"","HostPort":""}]}},
			"NetworkSettings":{"Ports":{}}}`))
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return NewSocketDocker(sock)
}

func TestListPublishedPorts(t *testing.T) {
	cs, err := portsEngine(t).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 2 {
		t.Fatalf("list: %+v", cs)
	}
	dns, web := cs[0], cs[1]
	if !dns.HostNetwork || web.HostNetwork || len(dns.Published) != 0 {
		t.Fatalf("host network: dns %+v, web %+v", dns, web)
	}
	want := []wire.PortBinding{
		{IP: "0.0.0.0", HostPort: 8080, Port: 80, Proto: "tcp"},
		{IP: "::", HostPort: 8080, Port: 80, Proto: "tcp"},
		{IP: "127.0.0.1", HostPort: 9000, Port: 9000, Proto: "udp"},
	}
	if !reflect.DeepEqual(web.Published, want) {
		t.Fatalf("published: %+v", web.Published)
	}
	if !reflect.DeepEqual(web.Ports, []string{"443/tcp", "8080->80/tcp", "9000->9000/udp"}) {
		t.Fatalf("ports: %v", web.Ports)
	}
}

func TestInspectPorts(t *testing.T) {
	d := portsEngine(t)
	ctx := context.Background()
	got, err := d.inspect(ctx, "running")
	if err != nil {
		t.Fatal(err)
	}
	want := []wire.DetailPort{
		{Container: "443/tcp"},
		{Container: "53/udp", Host: "[::1]:5353", IP: "::1"},
		{Container: "80/tcp", Host: "8080", IP: "0.0.0.0"}, // :: is the same binding
		{Container: "9000/udp", Host: "127.0.0.1:9000", IP: "127.0.0.1"},
	}
	if !reflect.DeepEqual(got.Ports, want) {
		t.Fatalf("running: %+v", got.Ports)
	}

	// Stopped: what it publishes when it starts, from its settings.
	got, err = d.inspect(ctx, "stopped")
	if err != nil {
		t.Fatal(err)
	}
	want = []wire.DetailPort{{Container: "443/tcp"}, {Container: "80/tcp", Host: "8081"}}
	if !reflect.DeepEqual(got.Ports, want) {
		t.Fatalf("stopped: %+v", got.Ports)
	}
}
