package wire

import (
	"reflect"
	"testing"
)

func TestSharedNetwork(t *testing.T) {
	for mode, want := range map[string]string{"container:gluetun": "gluetun", "service:vpn": "vpn", "host": "", "bridge": "", "": "", "mynet": ""} {
		if got := SharedNetworkTarget(mode); got != want {
			t.Errorf("%q: %q, want %q", mode, got, want)
		}
	}
	id := "f9d22246d25b8b3634bf355dc6a600a126f2efe761b2444e3efef783862cb213"
	gw := Container{ID: id, Name: "gluetun"}
	cs := []Container{
		gw,
		{ID: "b", Name: "by-id", NetworkOf: id},
		{ID: "c", Name: "by-name", NetworkOf: "gluetun"},
		{ID: "d", Name: "by-short-id", NetworkOf: id[:12]},
		{ID: "e", Name: "too-short", NetworkOf: id[:4]},
		{ID: "f", Name: "other", NetworkOf: "vpn2"},
		{ID: "g", Name: "plain"},
	}
	if got := NetworkUsers(gw, cs); !reflect.DeepEqual(got, []string{"by-id", "by-name", "by-short-id"}) {
		t.Errorf("users: %v", got)
	}
}
