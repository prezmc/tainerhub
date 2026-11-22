package api

import (
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
