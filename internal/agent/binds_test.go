package agent

import "testing"

func TestSystemBind(t *testing.T) {
	for _, s := range []string{"/var/run/docker.sock", "/run/dbus", "/etc/localtime", "/dev/dri", "/", "/sys/fs/cgroup", "/var/lib/docker/volumes/x", "/home/me/podman.sock"} {
		if !systemBind(s) {
			t.Errorf("%s should be a system path", s)
		}
	}
	for _, s := range []string{"/srv/appdata/sonarr", "/opt/tainerhub/stacks/app/config", "/mnt/media", "/home/me/apps/x", "/etcetera/x", "/runner"} {
		if systemBind(s) {
			t.Errorf("%s isn't a system path", s)
		}
	}
}
