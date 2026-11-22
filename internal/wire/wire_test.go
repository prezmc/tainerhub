package wire

import (
	"strings"
	"testing"
)

func TestJoinTokenRoundTrip(t *testing.T) {
	in := JoinToken{ID: "abc", Secret: "xyz", CAFingerprint: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}
	out, err := ParseJoinToken("  " + in.Encode() + "\n")
	if err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Fatalf("got %+v", out)
	}
	for _, bad := range []string{"", "thj1_", "thj1_!!!", "xyz_abc", JoinToken{ID: "a"}.Encode()} {
		if _, err := ParseJoinToken(bad); err == nil {
			t.Errorf("expected error for %q", bad)
		}
	}
}

func TestValidContainerRef(t *testing.T) {
	for _, ok := range []string{"abc123", "my-app_1.web", "0f3e9a"} {
		if !ValidContainerRef(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", "../etc", "a/b", "a?x=1", "a%2Fb", "-rf", ".hidden", "a b", string(make([]byte, 129))} {
		if ValidContainerRef(bad) {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestDecoderLimit(t *testing.T) {
	big := strings.Repeat("x", MaxMessageBytes+10)
	d := NewDecoder(strings.NewReader(`{"ping":true}` + "\n\n" + `{"command":{"id":"` + big + `"}}` + "\n"))
	var m HubMessage
	if err := d.Decode(&m); err != nil || !m.Ping {
		t.Fatalf("first message: %v %+v", err, m)
	}
	if err := d.Decode(&m); err == nil {
		t.Fatal("oversized message should fail")
	}
}
