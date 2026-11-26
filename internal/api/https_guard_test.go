package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"

	"github.com/prezmc/tainerhub/internal/auth"
)

func TestParseTrustedStrict(t *testing.T) {
	got, err := parseTrustedStrict("192.168.1.20, 172.18.0.0/16 fd00:1:2::/48 ::ffff:10.1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"192.168.1.20/32", "172.18.0.0/16", "fd00:1:2::/48", "10.1.2.3/32"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i].String() != want[i] {
			t.Errorf("got %v, want %v", got, want)
		}
	}
	if got, err := parseTrustedStrict(""); err != nil || len(got) != 0 {
		t.Errorf("empty input: %v, %v", got, err)
	}
	for _, bad := range []string{"0.0.0.0/0", "::/0", "10.0.0.0/8", "172.16.0.0/12", "2001:db8::/32", "::ffff:10.0.0.0/104", "junk", "192.168.1.20, nope"} {
		if _, err := parseTrustedStrict(bad); err == nil {
			t.Errorf("parseTrustedStrict(%q) should fail", bad)
		}
	}
}

func TestTrustedRangeTooBroad(t *testing.T) {
	for s, want := range map[string]bool{"0.0.0.0/0": true, "10.0.0.0/8": true, "172.17.0.0/16": false, "192.168.1.5/32": false, "::/0": true, "fd00::/47": true, "fd00::/48": false} {
		if got := TrustedRangeTooBroad(netip.MustParsePrefix(s)); got != want {
			t.Errorf("TrustedRangeTooBroad(%s) = %v, want %v", s, got, want)
		}
	}
}

func TestBlockedProbeAddr(t *testing.T) {
	for _, s := range []string{"127.0.0.1", "::1", "169.254.169.254", "fe80::1", "0.0.0.0", "::", "224.0.0.1", "ff02::1", "::ffff:127.0.0.1"} {
		if !blockedProbeAddr(netip.MustParseAddr(s)) {
			t.Errorf("%s should be blocked", s)
		}
	}
	for _, s := range []string{"192.168.1.20", "10.0.0.5", "172.17.0.2", "fd00::5", "203.0.113.9"} {
		if blockedProbeAddr(netip.MustParseAddr(s)) {
			t.Errorf("%s should be allowed (LAN and public proxies)", s)
		}
	}
}

func TestFetchRefusesLoopback(t *testing.T) {
	// Nothing needs to listen: the dial is refused before connecting.
	_, err := fetch(context.Background(), "https://127.0.0.1:1/api/https-check", true)
	if !errors.Is(err, errBlockedTarget) {
		t.Fatalf("got %v, want errBlockedTarget", err)
	}
	if msg := describeFetchError("hub.example.com", "8585", err); !strings.Contains(msg, "reserved address") {
		t.Errorf("unexpected message %q", msg)
	}
}

// A proxy that isn't trusted is spotted and shown in Settings; the trusted
// one, and plain LAN visits, aren't.
func TestNoteUntrustedProxy(t *testing.T) {
	a := &auth.Service{}
	pub, _ := url.Parse("https://hub.example.com")
	if err := a.Reconfigure(pub, []netip.Prefix{netip.MustParsePrefix("192.0.2.1/32")}); err != nil {
		t.Fatal(err)
	}
	s := &Server{Auth: a, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	seen := func() any {
		w := httptest.NewRecorder()
		s.httpsSettings(w, httptest.NewRequest(http.MethodGet, "/api/settings/https", nil))
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return out["untrustedProxy"]
	}
	visit := func(peer string, forwarded bool) {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = peer + ":5000"
		if forwarded {
			r.Header.Set("X-Forwarded-For", "198.51.100.7")
		}
		s.noteUntrustedProxy(r)
	}
	visit("192.0.2.1", true)     // the trusted proxy
	visit("192.168.1.40", false) // a LAN browser
	if v := seen(); v != nil {
		t.Fatalf("nothing untrusted yet, got %v", v)
	}
	visit("172.18.0.5", true)
	v, _ := seen().(map[string]any)
	if v["ip"] != "172.18.0.5" {
		t.Fatalf("untrusted proxy not reported: %v", v)
	}
}
