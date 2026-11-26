package wire

import "testing"

func TestLooksLikePlaceholder(t *testing.T) {
	yes := []string{
		"/path/to/prowlarr/data", "/PATH_TO_YOUR_CONFIG", "/path_to/config", "path/to/media",
		"/your/media", "/mnt/your-media", "YOUR_DOMAIN", "yourpassword", "<your-domain>", "<config dir>",
		"changeme", "CHANGE_ME", "change-this", "replace_with_a_secret", "xxxxxx", "todo",
	}
	no := []string{
		"", "/srv/media", "./config", "/opt/tainerhub/stacks/app/data", "/var/run/docker.sock",
		"Europe/London", "/mnt/yourself", "pathtology", "1000", "postgres://db:5432/app", "/data/mypath/to",
		"secret-key-123", "https://example.com/youri", "/home/user/todolist",
	}
	for _, s := range yes {
		if !LooksLikePlaceholder(s) {
			t.Errorf("%q should look like a placeholder", s)
		}
	}
	for _, s := range no {
		if LooksLikePlaceholder(s) {
			t.Errorf("%q shouldn't look like a placeholder", s)
		}
	}
}
