package api

import "testing"

func TestParseHTTPSURL(t *testing.T) {
	good := map[string]string{
		"tainerhub.example.com":          "https://tainerhub.example.com",
		"https://TainerHub.Example.com/": "https://tainerhub.example.com",
		"https://hub.example.com:8443":   "https://hub.example.com:8443",
	}
	for in, want := range good {
		u, err := parseHTTPSURL(in)
		if err != nil || u.String() != want {
			t.Errorf("parseHTTPSURL(%q) = %v, %v; want %s", in, u, err, want)
		}
	}
	for _, in := range []string{"", "http://hub.example.com", "https://hub.example.com/path", "https://u:p@hub.example.com", "https://hub.example.com/?q=1"} {
		if u, err := parseHTTPSURL(in); err == nil {
			t.Errorf("parseHTTPSURL(%q) = %v; want an error", in, u)
		}
	}
}

func TestParsePrefixes(t *testing.T) {
	got := parsePrefixes("192.168.1.20, 172.16.0.0/12 junk ::1")
	want := []string{"192.168.1.20/32", "172.16.0.0/12", "::1/128"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i].String() != want[i] {
			t.Errorf("got %v, want %v", got, want)
		}
	}
}
