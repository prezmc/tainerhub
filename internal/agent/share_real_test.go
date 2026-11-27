package agent

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/prezmc/tainerhub/internal/wire"
)

// TestShareRealDocker creates share volumes against a real Docker daemon
// for servers that don't exist, and checks the volume is gone again, the
// options Docker got are the ones meant, and the password never shows up
// in the error. It is opt-in:
//
//	TAINERHUB_REAL_DOCKER=1 DOCKER_SOCKET=/var/run/docker.sock go test ./internal/agent -run ShareReal -v
func TestShareRealDocker(t *testing.T) {
	if os.Getenv("TAINERHUB_REAL_DOCKER") != "1" {
		t.Skip("set TAINERHUB_REAL_DOCKER=1 to run against a real Docker daemon")
	}
	socket := os.Getenv("DOCKER_SOCKET")
	if socket == "" {
		socket = "/var/run/docker.sock"
	}
	d := NewSocketDocker(socket)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	image := os.Getenv("TAINERHUB_TEST_IMAGE")
	if image == "" {
		image = "alpine:3"
	}
	ls := []string{"ls", "-A", "/" + wire.TransferRoot}
	img := func(context.Context) (string, error) { return image, nil }

	const pw = "Not-A-Real-Pass=9"
	cases := []*wire.ShareVolume{
		// TEST-NET-1 (RFC 5737): never routed, so nothing answers.
		{Name: "tainerhub-realtest-cifs", Type: wire.ShareCIFS, Server: "192.0.2.1", Path: "media", Username: "nobody", Password: pw, UID: "1000", GID: "1000", Version: "3.0"},
		{Name: "tainerhub-realtest-nfs", Type: wire.ShareNFS, Server: "192.0.2.1", Path: "/export/media"},
	}
	check := func(t *testing.T, s *wire.ShareVolume, want string) {
		t.Helper()
		_ = d.RemoveVolume(ctx, s.Name)
		start := time.Now()
		_, err := d.createShareWith(ctx, s, img, ls)
		if err == nil {
			t.Fatal("a share on an unreachable server mounted")
		}
		t.Logf("after %s: %v", time.Since(start).Round(time.Second), err)
		if strings.Contains(err.Error(), pw) {
			t.Fatalf("the password is in the error: %v", err)
		}
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q doesn't say %q", err, want)
		}
		if _, exists, verr := d.VolumeInfo(ctx, s.Name); verr != nil || exists {
			t.Fatalf("volume left behind after a failed mount (exists=%v, err=%v)", exists, verr)
		}
	}
	// With the reachability probe, nothing is created.
	for _, s := range cases {
		t.Run("probe-"+s.Type, func(t *testing.T) { check(t, s, "can't reach 192.0.2.1 on port") })
	}
	// Without it, Docker's mount fails and the volume is removed again.
	t.Run("mount-cifs", func(t *testing.T) {
		shareProbe = func(context.Context, *wire.ShareVolume) error { return nil }
		defer func() { shareProbe = probeShareServer }()
		check(t, cases[0], "can't reach 192.0.2.1")
	})

	// The options Docker records are the ones ShareDriverOpts builds, and a
	// name that's taken is refused before anything is changed.
	t.Run("options", func(t *testing.T) {
		s := cases[0]
		_ = d.RemoveVolume(ctx, s.Name)
		resp, err := d.doBody(ctx, "POST", "/volumes/create", nil, map[string]any{"Name": s.Name, "Driver": "local", "DriverOpts": wire.ShareDriverOpts(s)})
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		defer d.RemoveVolume(context.Background(), s.Name)
		v, ok, err := d.VolumeInfo(ctx, s.Name)
		if err != nil || !ok {
			t.Fatal(ok, err)
		}
		want := "addr=192.0.2.1,username=nobody,password=" + pw + ",vers=3.0,uid=1000,gid=1000"
		if v.Options["o"] != want || v.Options["device"] != "//192.0.2.1/media" || v.Options["type"] != "cifs" {
			t.Fatalf("options = %v", v.Options)
		}
		if got := wire.ShareFromOptions(v.Driver, v.Options); got == nil || got.Server != "192.0.2.1" || got.Path != "media" {
			t.Fatalf("ShareFromOptions = %+v", got)
		}
		if _, err := d.createShareWith(ctx, s, img, ls); err == nil || !strings.Contains(err.Error(), "already exists") {
			t.Fatalf("taken name: %v", err)
		}
		if _, ok, _ := d.VolumeInfo(ctx, s.Name); !ok {
			t.Fatal("refusing a taken name removed the existing volume")
		}
	})
}
