package wire

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
)

// Network share volumes: a Docker volume with the local driver that mounts
// an SMB/CIFS share or an NFS export when a container starts, created
// through Docker's API by the agent (OpVolumeCreateShare). The share's
// login is part of the volume's options, which Docker keeps on the host;
// the hub never stores it.

const (
	ShareCIFS = "cifs"
	ShareNFS  = "nfs"
)

// ShareVolume is OpVolumeCreateShare's request.
type ShareVolume struct {
	Name   string `json:"name"`
	Type   string `json:"type"`   // ShareCIFS or ShareNFS
	Server string `json:"server"` // IP address or host name
	// Path is the share name for CIFS ("media", or "media/sub/folder") and
	// the export path for NFS ("/volume1/media").
	Path string `json:"path"`

	// CIFS only. An empty Username mounts as guest.
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	Domain   string `json:"domain,omitempty"`
	UID      string `json:"uid,omitempty"`      // owner the files appear to have
	GID      string `json:"gid,omitempty"`      // group the files appear to have
	FileMode string `json:"fileMode,omitempty"` // octal, e.g. 0664
	DirMode  string `json:"dirMode,omitempty"`  // octal, e.g. 0775

	// Version is the SMB dialect for CIFS (CIFSVersions) or the NFS version
	// (NFSVersions). Empty picks the default (3.0 for SMB, 4 for NFS).
	Version string `json:"version,omitempty"`
	// ReadOnly mounts the share read-only.
	ReadOnly bool `json:"readOnly,omitempty"`
}

// ShareInfo describes a volume that mounts a network share, for lists. It
// never carries the volume's options (they can hold the share's password).
type ShareInfo struct {
	Type   string `json:"type"`   // "cifs" or "nfs"
	Server string `json:"server"` // as written in the volume's options
	Path   string `json:"path"`   // share name (CIFS) or export path (NFS)
}

// ShareCreated is OpVolumeCreateShare's result.
type ShareCreated struct {
	Name    string `json:"name"`
	Entries int    `json:"entries"` // files and folders at the top of the share
}

var (
	CIFSVersions = []string{"3.1.1", "3.02", "3.0", "2.1", "2.0", "1.0", "default"}
	NFSVersions  = []string{"4.2", "4.1", "4", "3"}
)

// MaxShareField bounds each text field of a ShareVolume.
const MaxShareField = 256

// optionUnsafe are characters that would break out of a value in the
// mount option string (o=a=1,b=2), or out of the compose file a user may
// copy it into.
const (
	optionUnsafe   = ",\"'\\="
	passwordUnsafe = ",\"'"
)

// ValidShareVolume checks a share volume request. Everything that ends up
// in the mount options is checked to stay one value: no commas, quotes or
// line breaks.
func ValidShareVolume(s *ShareVolume) error {
	if s == nil {
		return errors.New("no share to create")
	}
	if !ValidContainerRef(s.Name) {
		return errors.New("the volume name may use letters, digits, _ . and - (not first)")
	}
	if err := validShareServer(s.Server); err != nil {
		return err
	}
	for _, f := range []struct {
		label, v, unsafe string
	}{
		{"path", s.Path, optionUnsafe},
		{"username", s.Username, optionUnsafe},
		{"domain", s.Domain, optionUnsafe},
		// A password may hold = and \ (the kernel splits each option at its
		// first =), but never a comma or a quote.
		{"password", s.Password, passwordUnsafe},
	} {
		if len(f.v) > MaxShareField {
			return fmt.Errorf("the %s is too long", f.label)
		}
		for _, c := range f.v {
			if c < 0x20 || c == 0x7f {
				return fmt.Errorf("the %s can't contain line breaks or control characters", f.label)
			}
		}
		if strings.ContainsAny(f.v, f.unsafe) {
			if f.label == "password" {
				return errors.New("the password can't contain commas or quotes: Docker hands it to the mount in a comma-separated list. Change it on the NAS, or write the volume in a compose file yourself")
			}
			return fmt.Errorf("the %s can't contain commas, quotes, backslashes or =", f.label)
		}
	}
	switch s.Type {
	case ShareCIFS:
		p := strings.Trim(s.Path, "/")
		if p == "" {
			return errors.New("which share? Enter its name, as in //server/share")
		}
		if strings.Contains(p, "..") {
			return errors.New("the share path can't contain ..")
		}
		if s.Password != "" && s.Username == "" {
			return errors.New("a password needs a username")
		}
		for _, f := range []struct{ label, v string }{{"uid", s.UID}, {"gid", s.GID}} {
			if f.v == "" {
				continue
			}
			if n, err := strconv.ParseUint(f.v, 10, 32); err != nil || n > 1<<31 {
				return fmt.Errorf("the %s must be a number", f.label)
			}
		}
		for _, f := range []struct{ label, v string }{{"file mode", s.FileMode}, {"folder mode", s.DirMode}} {
			if f.v == "" {
				continue
			}
			if !validOctalMode(f.v) {
				return fmt.Errorf("the %s must be octal, like 0775", f.label)
			}
		}
		if s.Version != "" && !contains(CIFSVersions, s.Version) {
			return fmt.Errorf("unknown SMB version %q (use one of %s)", s.Version, strings.Join(CIFSVersions, ", "))
		}
	case ShareNFS:
		if !strings.HasPrefix(s.Path, "/") {
			return errors.New("the NFS export path starts with /, as in /volume1/media")
		}
		if strings.Contains(s.Path, "..") || strings.ContainsAny(s.Path, " ") {
			return errors.New("the export path can't contain .. or spaces")
		}
		if s.Username != "" || s.Password != "" || s.Domain != "" || s.UID != "" || s.GID != "" || s.FileMode != "" || s.DirMode != "" {
			return errors.New("NFS shares don't take a username, password, owner or modes (the server decides who owns the files)")
		}
		if s.Version != "" && !contains(NFSVersions, s.Version) {
			return fmt.Errorf("unknown NFS version %q (use one of %s)", s.Version, strings.Join(NFSVersions, ", "))
		}
	default:
		return errors.New("the share type must be cifs or nfs")
	}
	return nil
}

func validShareServer(s string) error {
	if s == "" {
		return errors.New("enter the server's IP address or host name")
	}
	if ip := net.ParseIP(s); ip != nil {
		return nil
	}
	if len(s) > 253 {
		return errors.New("the server name is too long")
	}
	for _, label := range strings.Split(s, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return fmt.Errorf("%q isn't an IP address or host name", s)
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return fmt.Errorf("%q isn't an IP address or host name (no // or share name here)", s)
			}
		}
	}
	return nil
}

func validOctalMode(s string) bool {
	if len(s) < 3 || len(s) > 4 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '7' {
			return false
		}
	}
	return true
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// ShareDriverOpts returns the local driver's options for a share volume
// (type, device, o), as given to Docker's volume create and as written
// under driver_opts in a compose file. Call ValidShareVolume first.
func ShareDriverOpts(s *ShareVolume) map[string]string {
	var o []string
	switch s.Type {
	case ShareCIFS:
		o = append(o, "addr="+s.Server)
		if s.Username == "" {
			o = append(o, "guest")
		} else {
			o = append(o, "username="+s.Username, "password="+s.Password)
		}
		if s.Domain != "" {
			o = append(o, "domain="+s.Domain)
		}
		v := s.Version
		if v == "" {
			v = "3.0"
		}
		o = append(o, "vers="+v)
		for _, kv := range [][2]string{{"uid", s.UID}, {"gid", s.GID}, {"file_mode", s.FileMode}, {"dir_mode", s.DirMode}} {
			if kv[1] != "" {
				o = append(o, kv[0]+"="+kv[1])
			}
		}
		if s.ReadOnly {
			o = append(o, "ro")
		}
		return map[string]string{"type": "cifs", "device": "//" + hostForDevice(s.Server) + "/" + strings.Trim(s.Path, "/"), "o": strings.Join(o, ",")}
	case ShareNFS:
		v := s.Version
		if v == "" {
			v = "4"
		}
		o = append(o, "addr="+s.Server, "nfsvers="+v)
		if v == "3" {
			// NFSv3 locking needs rpc.statd on the host, which Docker hosts
			// rarely run; nolock keeps locks on this host.
			o = append(o, "nolock")
		}
		if s.ReadOnly {
			o = append(o, "ro")
		} else {
			o = append(o, "rw")
		}
		return map[string]string{"type": "nfs", "device": ":" + s.Path, "o": strings.Join(o, ",")}
	}
	return nil
}

func hostForDevice(server string) string {
	if ip := net.ParseIP(server); ip != nil && ip.To4() == nil {
		return "[" + server + "]"
	}
	return server
}

// ShareFromOptions recognises a local-driver volume that mounts a network
// share from its driver options, without returning any secret.
func ShareFromOptions(driver string, opts map[string]string) *ShareInfo {
	if driver != "local" || opts == nil {
		return nil
	}
	t := strings.ToLower(opts["type"])
	dev := opts["device"]
	addr := ""
	for _, kv := range strings.Split(opts["o"], ",") {
		if k, v, ok := strings.Cut(kv, "="); ok && k == "addr" {
			addr = v
		}
	}
	switch t {
	case "cifs", "smb3":
		rest, ok := strings.CutPrefix(dev, "//")
		if !ok {
			return nil
		}
		server, path, _ := strings.Cut(rest, "/")
		if addr != "" && server == "" {
			server = addr
		}
		return &ShareInfo{Type: ShareCIFS, Server: server, Path: path}
	case "nfs", "nfs4":
		server, path, ok := strings.Cut(dev, ":")
		if !ok {
			return nil
		}
		if server == "" {
			server = addr
		}
		return &ShareInfo{Type: ShareNFS, Server: server, Path: path}
	}
	return nil
}
