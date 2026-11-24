package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSameOrigin(t *testing.T) {
	r := httptest.NewRequest("POST", "http://192.168.1.10:8585/api/logout", nil)
	if !sameOrigin("http://192.168.1.10:8585", "", r) {
		t.Fatal("same host refused without a public URL")
	}
	if sameOrigin("http://evil.example", "", r) || sameOrigin("null", "", r) {
		t.Fatal("foreign origin accepted without a public URL")
	}
	if !sameOrigin("https://hub.example", "https://hub.example", r) {
		t.Fatal("configured origin refused")
	}
	if !sameOrigin("http://192.168.1.10:8585", "https://hub.example", r) {
		t.Fatal("the address the request arrived on must keep working after HTTPS is set up")
	}
	if sameOrigin("http://evil.example", "https://hub.example", r) {
		t.Fatal("foreign origin accepted with a public URL")
	}
}

// DNS rebinding: an attacker's name resolves to the hub, so Origin and Host
// match. With a public URL set, only names the hub knows or local-only names
// pass; without one, LAN-by-name access must keep working.
func TestSameOriginRebinding(t *testing.T) {
	const pub = "https://hub.example.com"
	for host, want := range map[string]bool{
		"rebind.attacker.example:8585": false,
		"1.2.3.4.nip.io:8585":          false,
		"192.168.1.10:8585":            true,
		"[fd00::10]:8585":              true,
		"localhost:8585":               true,
		"nas:8585":                     true,
		"hub.lan:8585":                 true,
		"tainerhub.local:8585":         true,
		"docker.home.arpa:8585":        true,
		"hub.example.com:8585":         true, // the public URL's own name on the LAN port
		"agents.example.net:8585":      true, // the configured agent address
	} {
		if got := sameOrigin("http://"+host, pub, newReq(host), "agents.example.net:7443"); got != want {
			t.Errorf("with public URL, Host %s: got %v, want %v", host, got, want)
		}
	}
	// No public URL: plain-HTTP installs reached by any name keep working.
	for _, host := range []string{"192.168.1.10:8585", "hostname.lan:8585", "docker.corp.example.com:8585"} {
		if !sameOrigin("http://"+host, "", newReq(host)) {
			t.Errorf("without public URL, Host %s refused", host)
		}
	}
}

func newReq(host string) *http.Request {
	return httptest.NewRequest("POST", "http://"+host+"/api/logout", nil)
}
