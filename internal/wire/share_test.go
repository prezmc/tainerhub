package wire

import (
	"strings"
	"testing"
)

func TestValidShareVolume(t *testing.T) {
	ok := func() *ShareVolume {
		return &ShareVolume{Name: "media", Type: ShareCIFS, Server: "nas.lan", Path: "media", Username: "u", Password: "p=w\\d!", UID: "1000", GID: "1000", FileMode: "0664", DirMode: "0775", Version: "3.0"}
	}
	if err := ValidShareVolume(ok()); err != nil {
		t.Fatal(err)
	}
	nfs := &ShareVolume{Name: "media", Type: ShareNFS, Server: "10.0.0.5", Path: "/volume1/media", Version: "4.1"}
	if err := ValidShareVolume(nfs); err != nil {
		t.Fatal(err)
	}
	bad := map[string]func(s *ShareVolume){
		"comma in password":    func(s *ShareVolume) { s.Password = "a,b" },
		"quote in password":    func(s *ShareVolume) { s.Password = `a"b` },
		"newline in password":  func(s *ShareVolume) { s.Password = "a\nb" },
		"comma in username":    func(s *ShareVolume) { s.Username = "u,uid=0" },
		"equals in username":   func(s *ShareVolume) { s.Username = "u=x" },
		"quote in share":       func(s *ShareVolume) { s.Path = "me'dia" },
		"comma in share":       func(s *ShareVolume) { s.Path = "a,b" },
		"dotdot share":         func(s *ShareVolume) { s.Path = "a/../b" },
		"empty share":          func(s *ShareVolume) { s.Path = "/" },
		"no server":            func(s *ShareVolume) { s.Server = "" },
		"unc server":           func(s *ShareVolume) { s.Server = "//nas/media" },
		"server with option":   func(s *ShareVolume) { s.Server = "nas,uid=0" },
		"bad uid":              func(s *ShareVolume) { s.UID = "root" },
		"bad mode":             func(s *ShareVolume) { s.FileMode = "0999" },
		"bad version":          func(s *ShareVolume) { s.Version = "9" },
		"password, no user":    func(s *ShareVolume) { s.Username = "" },
		"bad name":             func(s *ShareVolume) { s.Name = "-x" },
		"bad type":             func(s *ShareVolume) { s.Type = "smbfs" },
		"control char in user": func(s *ShareVolume) { s.Username = "a\tb" },
		"long password":        func(s *ShareVolume) { s.Password = strings.Repeat("x", MaxShareField+1) },
	}
	for name, mod := range bad {
		s := ok()
		mod(s)
		if err := ValidShareVolume(s); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	for name, s := range map[string]*ShareVolume{
		"relative export": {Name: "m", Type: ShareNFS, Server: "nas", Path: "volume1"},
		"nfs password":    {Name: "m", Type: ShareNFS, Server: "nas", Path: "/v", Password: "x", Username: "u"},
		"nfs version":     {Name: "m", Type: ShareNFS, Server: "nas", Path: "/v", Version: "3.0"},
	} {
		if err := ValidShareVolume(s); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := ValidOperation(Operation{Kind: OpVolumeCreateShare}); err == nil {
		t.Error("create share without a share accepted")
	}
	if err := ValidOperation(Operation{Kind: OpVolumesList, Share: ok()}); err == nil {
		t.Error("a share on another operation accepted")
	}
	if err := ValidOperation(Operation{Kind: OpVolumeCreateShare, Share: ok()}); err != nil {
		t.Error(err)
	}
}

func TestShareDriverOpts(t *testing.T) {
	for _, c := range []struct {
		in   ShareVolume
		want map[string]string
	}{
		{ShareVolume{Type: ShareCIFS, Server: "192.0.2.10", Path: "/media/", Username: "u", Password: "p", UID: "1000", GID: "1000"},
			map[string]string{"type": "cifs", "device": "//192.0.2.10/media", "o": "addr=192.0.2.10,username=u,password=p,vers=3.0,uid=1000,gid=1000"}},
		{ShareVolume{Type: ShareCIFS, Server: "nas", Path: "pub", Version: "2.1", Domain: "WG", Username: "u", FileMode: "0644", DirMode: "0755", ReadOnly: true},
			map[string]string{"type": "cifs", "device": "//nas/pub", "o": "addr=nas,username=u,password=,domain=WG,vers=2.1,file_mode=0644,dir_mode=0755,ro"}},
		{ShareVolume{Type: ShareCIFS, Server: "nas", Path: "pub"},
			map[string]string{"type": "cifs", "device": "//nas/pub", "o": "addr=nas,guest,vers=3.0"}},
		{ShareVolume{Type: ShareCIFS, Server: "2001:db8::1", Path: "pub"},
			map[string]string{"type": "cifs", "device": "//[2001:db8::1]/pub", "o": "addr=2001:db8::1,guest,vers=3.0"}},
		{ShareVolume{Type: ShareNFS, Server: "192.0.2.10", Path: "/volume1/media"},
			map[string]string{"type": "nfs", "device": ":/volume1/media", "o": "addr=192.0.2.10,nfsvers=4,rw"}},
		{ShareVolume{Type: ShareNFS, Server: "nas", Path: "/x", Version: "3", ReadOnly: true},
			map[string]string{"type": "nfs", "device": ":/x", "o": "addr=nas,nfsvers=3,nolock,ro"}},
	} {
		got := ShareDriverOpts(&c.in)
		for k, v := range c.want {
			if got[k] != v {
				t.Errorf("%+v: %s = %q, want %q", c.in, k, got[k], v)
			}
		}
		info := ShareFromOptions("local", got)
		if info == nil || info.Type != c.in.Type || info.Path != strings.Trim(c.in.Path, "/") && info.Path != c.in.Path {
			t.Errorf("ShareFromOptions(%v) = %+v", got, info)
		}
	}
	if ShareFromOptions("local", map[string]string{"type": "none", "device": "/srv", "o": "bind"}) != nil {
		t.Error("a bind volume taken for a share")
	}
	if got := ShareFromOptions("local", map[string]string{"type": "nfs4", "device": "nas:/x", "o": "addr=nas"}); got == nil || got.Server != "nas" || got.Path != "/x" {
		t.Errorf("nfs4 = %+v", got)
	}
}
