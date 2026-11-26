package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestParseOSStatusApt(t *testing.T) {
	out := `os=Debian GNU/Linux 12 (bookworm)
kernel=6.1.0-26-amd64
reboot=yes
reboot_pkgs=linux-image-6.1.0-27-amd64 
manager=apt
refresh_failed=yes
refresh_error=W: Failed to fetch https://download.docker.com/linux/debian/dists/bookworm/InRelease  Could not resolve 'download.docker.com' 
inst=docker-ce [5:29.10.0-1~debian.12~bookworm] (5:29.10.1-1~debian.12~bookworm Docker CE:bookworm [amd64])
inst=libssl3 [3.0.14-1~deb12u2] (3.0.15-1~deb12u1 Debian-Security:12/stable-security [amd64])
inst=linux-image-6.1.0-28-amd64 (6.1.119-1 Debian:12.8/stable [amd64])
garbage line
`
	s := parseOSStatus(out)
	if s.OS != "Debian GNU/Linux 12 (bookworm)" || s.Kernel != "6.1.0-26-amd64" || s.Manager != "apt" {
		t.Fatalf("header: %+v", s)
	}
	if !s.RebootRequired || len(s.RebootPackages) != 1 || !s.RefreshFailed || s.RefreshError == "" {
		t.Fatalf("flags: %+v", s)
	}
	if len(s.Packages) != 3 || s.Security != 1 {
		t.Fatalf("packages: %+v", s.Packages)
	}
	d := s.Packages[0]
	if d.Name != "docker-ce" || d.From != "5:29.10.0-1~debian.12~bookworm" || d.To != "5:29.10.1-1~debian.12~bookworm" || d.Origin != "Docker CE:bookworm" || d.Security {
		t.Fatalf("docker-ce: %+v", d)
	}
	if p := s.Packages[1]; !p.Security || p.Origin != "Debian-Security:12/stable-security" {
		t.Fatalf("libssl3: %+v", p)
	}
	if p := s.Packages[2]; p.From != "" || p.To != "6.1.119-1" {
		t.Fatalf("new kernel: %+v", p)
	}
}

func TestParseOSStatusDnf(t *testing.T) {
	s := parseOSStatus("manager=dnf\nrpm=openssl-libs.x86_64 1:3.2.2-9.fc41 updates\nrpm=vim-minimal.x86_64 2:9.1.825-1.fc41 updates\nsec=openssl-libs-1:3.2.2-9.fc41.x86_64\n")
	if len(s.Packages) != 2 || s.Security != 1 || !s.Packages[0].Security || s.Packages[1].Security {
		t.Fatalf("%+v", s)
	}
	if s := parseOSStatus("os=Something\n"); s.Manager != "unknown" || s.Packages == nil {
		t.Fatalf("unknown: %+v", s)
	}
}

// The script runs on the host's /bin/sh: it must at least parse.
func TestOSStatusScriptParses(t *testing.T) {
	f := filepath.Join(t.TempDir(), "os.sh")
	if err := os.WriteFile(f, []byte(osStatusScript), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("sh", "-n", f).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
}
