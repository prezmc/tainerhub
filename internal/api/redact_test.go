package api

import (
	"reflect"
	"testing"

	"github.com/prezmc/tainerhub/internal/wire"
)

func TestRedactForViewer(t *testing.T) {
	d := wire.ContainerDetail{
		Env:        []string{"DB_PASSWORD=hunter2", "TZ=UTC"},
		Entrypoint: []string{"/entry.sh", "--token", "abc"},
		Cmd:        []string{"serve", "--password=x"},
		Labels: map[string]string{
			"com.docker.compose.project":                    "media",
			"traefik.http.middlewares.auth.basicauth.users": "admin:$apr1$hash",
			"org.opencontainers.image.version":              "1.2.3",
			"com.example.api-key":                           "sk-123",
		},
	}
	redactForViewer(&d)
	if want := []string{"DB_PASSWORD=", "TZ="}; !reflect.DeepEqual(d.Env, want) {
		t.Errorf("env = %v", d.Env)
	}
	if want := []string{"/entry.sh", hiddenValue, hiddenValue}; !reflect.DeepEqual(d.Entrypoint, want) {
		t.Errorf("entrypoint = %v", d.Entrypoint)
	}
	if want := []string{hiddenValue, hiddenValue}; !reflect.DeepEqual(d.Cmd, want) {
		t.Errorf("cmd = %v (all of it is arguments when there's an entrypoint)", d.Cmd)
	}
	if d.Labels["com.docker.compose.project"] != "media" || d.Labels["org.opencontainers.image.version"] != "1.2.3" {
		t.Errorf("compose and OCI labels should stay readable: %v", d.Labels)
	}
	if d.Labels["traefik.http.middlewares.auth.basicauth.users"] != hiddenValue || d.Labels["com.example.api-key"] != hiddenValue {
		t.Errorf("other label values should be hidden: %v", d.Labels)
	}

	// Without an entrypoint the command's first element is the program.
	d2 := wire.ContainerDetail{Cmd: []string{"postgres", "-c", "password=x"}}
	redactForViewer(&d2)
	if want := []string{"postgres", hiddenValue, hiddenValue}; !reflect.DeepEqual(d2.Cmd, want) {
		t.Errorf("cmd = %v", d2.Cmd)
	}
}

func TestRedactHidesHealthOutput(t *testing.T) {
	d := &wire.ContainerDetail{}
	d.State.Health = &wire.DetailHealth{Log: []wire.HealthProbe{{ExitCode: 1, Output: "auth failed for token abc123"}}}
	redactForViewer(d)
	if p := d.State.Health.Log[0]; p.Output != "" || p.ExitCode != 1 {
		t.Fatalf("probe = %+v; want output hidden, exit code kept", p)
	}
}
