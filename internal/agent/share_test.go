package agent

import (
	"errors"
	"strings"
	"testing"

	"github.com/prezmc/tainerhub/internal/wire"
)

func TestScrubShareError(t *testing.T) {
	s := &wire.ShareVolume{Type: wire.ShareCIFS, Server: "192.0.2.10", Path: "media", Username: "u", Password: "hunter2x"}
	msg := "error while mounting volume '/var/lib/docker/volumes/m/_data': failed to mount local volume: mount //192.0.2.10/media:/var/lib/docker/volumes/m/_data, flags: 0x0, data: addr=192.0.2.10,username=u,password=hunter2x,vers=3.0: permission denied"
	got := scrubShareError(msg, s)
	if strings.Contains(got, "hunter2x") || strings.Contains(got, "username=") {
		t.Fatalf("options left in %q", got)
	}
	if !strings.HasSuffix(got, "options hidden: permission denied") {
		t.Fatalf("got %q", got)
	}
	err := explainShareError(errors.New("docker: "+msg), s)
	if strings.Contains(err.Error(), "hunter2x") || !strings.Contains(err.Error(), "turned down the login") {
		t.Fatalf("explained as %q", err)
	}
	// The password is hidden even where Docker quotes it elsewhere.
	if got := scrubShareError("bad password hunter2x", s); strings.Contains(got, "hunter2x") {
		t.Fatalf("got %q", got)
	}
}

func TestShareHint(t *testing.T) {
	cifs := &wire.ShareVolume{Type: wire.ShareCIFS, Server: "nas", Path: "media"}
	nfs := &wire.ShareVolume{Type: wire.ShareNFS, Server: "nas", Path: "/v"}
	for _, c := range []struct {
		s         *wire.ShareVolume
		err, want string
	}{
		{cifs, "permission denied", "username and password"},
		{nfs, "permission denied", "export allows this host"},
		{cifs, "no such file or directory", "no share called media"},
		{cifs, "no such device", "install cifs-utils"},
		{nfs, "unknown filesystem type 'nfs'", "nfs-common"},
		{cifs, "invalid argument", "cifs-utils"},
		{cifs, "host is down", "try another version"},
		{cifs, "operation now in progress", "can't reach nas"},
		{cifs, "something else", "didn't mount"},
	} {
		if got := shareHint(c.err, c.s); !strings.Contains(got, c.want) {
			t.Errorf("%s %q: %q lacks %q", c.s.Type, c.err, got, c.want)
		}
	}
}
