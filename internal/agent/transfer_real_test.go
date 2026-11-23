package agent

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/prezmc/tainerhub/internal/wire"
)

// realHelper uses a public image for helper containers, since the test
// doesn't run inside the agent's container. Like the agent's image, it sets
// a user: Docker's copyUIDGID would chown everything to it, so owners only
// survive if the copy doesn't use it.
type realHelper struct{ *SocketDocker }

func (realHelper) HelperImage(context.Context) (string, error) {
	return "gcr.io/distroless/static-debian12:nonroot", nil
}

// renamed makes one Docker look like a second host by giving the receiver's
// copy of a volume another name.
type renamed struct {
	Transferer
	from, to string
}

func (r renamed) n(s string) string {
	if s == r.from {
		return r.to
	}
	return s
}
func (r renamed) CreateHelper(ctx context.Context, img string, m HelperMount) (string, error) {
	m.Volume = r.n(m.Volume)
	return r.Transferer.CreateHelper(ctx, img, m)
}
func (r renamed) VolumeInfo(ctx context.Context, name string) (VolumeInfo, bool, error) {
	return r.Transferer.VolumeInfo(ctx, r.n(name))
}
func (r renamed) CreateVolume(ctx context.Context, name string, l map[string]string) error {
	return r.Transferer.CreateVolume(ctx, r.n(name), l)
}
func (r renamed) RemoveVolume(ctx context.Context, name string) error {
	return r.Transferer.RemoveVolume(ctx, r.n(name))
}

// TestTransferRealDocker copies a volume whose files belong to another user
// through Docker's helper-container copy API and checks owners, modes and
// symlinks survive. Opt-in, like TestStackRealDocker:
//
//	TAINERHUB_REAL_DOCKER=1 go test ./internal/agent -run TransferReal -v
func TestTransferRealDocker(t *testing.T) {
	if os.Getenv("TAINERHUB_REAL_DOCKER") != "1" {
		t.Skip("set TAINERHUB_REAL_DOCKER=1 to run against a real Docker daemon")
	}
	socket := os.Getenv("DOCKER_SOCKET")
	if socket == "" {
		socket = "/var/run/docker.sock"
	}
	const src, dst = "tainerhub-realtest-src", "tainerhub-realtest-dst"
	docker := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("docker", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}
	cleanup := func() { _ = exec.Command("docker", "volume", "rm", "-f", src, dst).Run() }
	cleanup()
	defer cleanup()
	docker("volume", "create", "--label", "com.docker.compose.project=realtest", src)
	docker("run", "--rm", "-v", src+":/d", "alpine:3.20", "sh", "-c",
		"mkdir -p /d/pg && echo rows > /d/pg/base && chown -R 999:999 /d/pg && chmod 700 /d/pg && chmod 600 /d/pg/base && ln -s pg/base /d/link && echo hi > /d/top")

	d := realHelper{NewSocketDocker(socket)}
	hub := &memHub{}
	from := &transferRunner{docker: d, hub: hub}
	to := &transferRunner{docker: renamed{Transferer: d, from: src, to: dst}, hub: hub}
	sent, got, sendErr, recvErr := copyItem(t, hub, from, to, wire.TransferItem{Kind: wire.TransferVolume, Name: src, Labels: map[string]string{"com.docker.compose.project": "realtest"}})
	if sendErr != nil || recvErr != nil {
		t.Fatalf("send %v, receive %v", sendErr, recvErr)
	}
	if sent != got {
		t.Fatalf("sent %+v, got %+v", sent, got)
	}
	want := docker("run", "--rm", "-v", src+":/d", "alpine:3.20", "sh", "-c", "cd /d && ls -lnR . | awk '{print $1, $3, $4, $NF}'; cat pg/base")
	have := docker("run", "--rm", "-v", dst+":/d", "alpine:3.20", "sh", "-c", "cd /d && ls -lnR . | awk '{print $1, $3, $4, $NF}'; cat pg/base")
	if have != want {
		t.Errorf("copy differs.\nsource:\n%s\ncopy:\n%s", want, have)
	}
	if !strings.Contains(have, "drwx------ 999 999 pg") {
		t.Errorf("owner or mode of pg lost:\n%s", have)
	}
	if l := docker("volume", "inspect", "-f", "{{index .Labels \"com.docker.compose.project\"}}", dst); strings.TrimSpace(l) != "realtest" {
		t.Errorf("label = %q", l)
	}
	if left := docker("ps", "-aq", "--filter", "label="+helperLabel); strings.TrimSpace(left) != "" {
		t.Errorf("helper containers left: %s", left)
	}
}
