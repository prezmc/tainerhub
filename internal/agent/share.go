package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/prezmc/tainerhub/internal/wire"
)

// Network share volumes (OpVolumeCreateShare): the agent creates a volume
// with the local driver and the share's mount options, then proves it
// mounts by starting a short-lived helper container (no network, the
// volume mounted read-only) that lists the share. Docker mounts the share
// when a container using the volume starts, so this is the same mount the
// user's containers will get. If it fails, the volume is removed again, so
// a broken share never sits there looking usable.

// shareLabel marks volumes TainerHub created for a network share.
const shareLabel = "tainerhub.share"

// shareCheckTimeout bounds the whole check: an unreachable server can keep
// a CIFS mount waiting for a long time.
const shareCheckTimeout = 90 * time.Second

// writableMark starts the line ShareCheck prints about writing.
const writableMark = "TH-WRITABLE="

// ShareCheck is `tainerhub-agent share-check [write]`, run in the helper
// container: it prints the names at the top of /th-data, one per line.
// With write it then creates and removes a test file and prints
// TH-WRITABLE=yes, or TH-WRITABLE=no and the reason.
func ShareCheck(write bool) error {
	return shareCheckIn(os.Stdout, "/"+wire.TransferRoot, write)
}

func shareCheckIn(w io.Writer, root string, write bool) error {
	ents, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, e := range ents {
		fmt.Fprintln(w, e.Name())
	}
	if write {
		f := fmt.Sprintf("%s/.tainerhub-write-test-%d", root, os.Getpid())
		if err := os.WriteFile(f, []byte("ok\n"), 0o644); err != nil {
			var pe *os.PathError
			if errors.As(err, &pe) {
				err = pe.Err
			}
			fmt.Fprintln(w, writableMark+"no "+err.Error())
		} else {
			_ = os.Remove(f)
			fmt.Fprintln(w, writableMark+"yes")
		}
	}
	return nil
}

// checksWrite: whether the check also writes. Only for a CIFS share meant
// for writing: the check then runs as the UID the files belong to, as the
// apps do. NFS permissions depend on who the app runs as on the server,
// which TainerHub can't know.
func checksWrite(s *wire.ShareVolume) bool {
	return s.Type == wire.ShareCIFS && !s.ReadOnly
}

func (d *SocketDocker) createShare(ctx context.Context, s *wire.ShareVolume) (wire.ShareCreated, error) {
	cmd := []string{"/tainerhub-agent", "share-check"}
	if checksWrite(s) {
		cmd = append(cmd, "write")
	}
	return d.createShareWith(ctx, s, d.HelperImage, cmd)
}

// createShareWith creates the volume and checks it with a helper made
// from helperImage's image running cmd (which lists /th-data, one name per
// line).
func (d *SocketDocker) createShareWith(ctx context.Context, s *wire.ShareVolume, helperImage func(context.Context) (string, error), cmd []string) (wire.ShareCreated, error) {
	res := wire.ShareCreated{Name: s.Name}
	if err := wire.ValidShareVolume(s); err != nil {
		return res, err
	}
	if _, exists, err := d.VolumeInfo(ctx, s.Name); err != nil {
		return res, err
	} else if exists {
		return res, fmt.Errorf("a volume called %s already exists on this host: pick another name", s.Name)
	}
	if err := shareProbe(ctx, s); err != nil {
		return res, err
	}
	image, err := helperImage(ctx)
	if err != nil {
		return res, err
	}
	resp, err := d.doBody(ctx, http.MethodPost, "/volumes/create", nil, map[string]any{
		"Name":       s.Name,
		"Driver":     "local",
		"DriverOpts": wire.ShareDriverOpts(s),
		"Labels":     map[string]string{shareLabel: s.Type},
	})
	if err != nil {
		return res, errors.New(scrubShareError(err.Error(), s))
	}
	resp.Body.Close()

	chk, helper, err := d.checkShare(ctx, s, image, cmd)
	if cerr := d.cleanupShare(helper, s.Name, err != nil); cerr != nil && err != nil {
		err = fmt.Errorf("%w. Docker is still busy with the mount; TainerHub removes the volume once it gives up (or remove %s under Resources → Volumes later)", err, s.Name)
	}
	if err != nil {
		return res, err
	}
	res.Entries, res.Writable, res.WriteError = chk.entries, chk.writable, chk.writeErr
	return res, nil
}

// shareProbe checks the server takes connections on the share's port
// before anything is created: a mount of an unreachable server can keep
// Docker busy for minutes. It's a variable so tests can skip it.
var shareProbe = probeShareServer

func probeShareServer(ctx context.Context, s *wire.ShareVolume) error {
	// SMB is on 445, or 139 on old NetBIOS-only servers.
	ports, kind := []string{"445", "139"}, "SMB"
	if s.Type == wire.ShareNFS {
		ports, kind = []string{"2049"}, "NFS"
	}
	// The agent's container may not resolve every name the host does
	// (.local names, say); then leave it to Docker.
	ips, err := net.DefaultResolver.LookupHost(ctx, s.Server)
	if err != nil || len(ips) == 0 {
		return nil
	}
	why := "no answer"
	for _, port := range ports {
		dctx, cancel := context.WithTimeout(ctx, 6*time.Second)
		var dialer net.Dialer
		c, err := dialer.DialContext(dctx, "tcp", net.JoinHostPort(ips[0], port))
		cancel()
		if err == nil {
			c.Close()
			return nil
		}
		if errors.Is(err, syscall.ECONNREFUSED) {
			why = "connection refused"
		} else {
			break // a server that doesn't answer won't on the next port either
		}
	}
	return fmt.Errorf("can't reach %s on port %s (%s) from this host: check the address and that %s file sharing is on", s.Server, ports[0], why, kind)
}

// cleanupShare removes the check container and, after a failed check, the
// volume. A mount that's still hanging keeps both busy, so whatever can't
// go now is retried in the background for a while.
func (d *SocketDocker) cleanupShare(helper, volume string, removeVolume bool) error {
	try := func(ctx context.Context) error {
		for {
			var err error
			if helper != "" {
				if err = d.RemoveContainer(ctx, helper); err == nil || isNoSuch(err) {
					helper, err = "", nil
				}
			}
			if helper == "" && removeVolume {
				if err = d.RemoveVolume(ctx, volume); err == nil || isNoSuch(err) {
					removeVolume, err = false, nil
				}
			}
			if helper == "" && !removeVolume {
				return nil
			}
			select {
			case <-ctx.Done():
				return err
			case <-time.After(time.Second):
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	err := try(ctx)
	cancel()
	if err != nil {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			_ = try(ctx)
		}()
	}
	return err
}

func isNoSuch(err error) bool {
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "no such") || strings.Contains(m, "not found")
}

// checkShare mounts the volume in a helper container and counts what it
// lists. The error explains a failed mount in plain words.
// shareCheck is what the helper found.
type shareCheck struct {
	entries  int
	writable *bool // nil when not checked
	writeErr string
}

func (d *SocketDocker) checkShare(ctx context.Context, s *wire.ShareVolume, image string, cmd []string) (chk shareCheck, helper string, err error) {
	ctx, cancel := context.WithTimeout(ctx, shareCheckTimeout)
	defer cancel()
	write := checksWrite(s)
	body := map[string]any{
		"Image":           image,
		"Entrypoint":      cmd,
		"Labels":          map[string]string{helperLabel: "share-check"},
		"NetworkDisabled": true,
		"HostConfig": map[string]any{
			"Mounts": []map[string]any{{
				"Type": "volume", "Source": s.Name, "Target": "/" + wire.TransferRoot, "ReadOnly": !write,
				"VolumeOptions": map[string]any{"NoCopy": true},
			}},
			"NetworkMode": "none",
			"SecurityOpt": []string{"no-new-privileges:true"},
		},
	}
	if write && s.UID != "" {
		// Write as the owner the files appear to have, like the apps do.
		gid := s.GID
		if gid == "" {
			gid = s.UID
		}
		body["User"] = s.UID + ":" + gid
	}
	resp, err := d.doBody(ctx, http.MethodPost, "/containers/create", nil, body)
	if err != nil {
		return chk, "", fmt.Errorf("create the check container: %s", scrubShareError(err.Error(), s))
	}
	var created struct{ Id string }
	if err := jsonDecode(resp, &created); err != nil || created.Id == "" {
		return chk, "", fmt.Errorf("create the check container: %v", err)
	}
	id := created.Id
	// Starting the container is when Docker mounts the share.
	if resp, err := d.do(ctx, http.MethodPost, "/containers/"+id+"/start", nil); err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return chk, id, explainShareError(err, s)
	} else {
		resp.Body.Close()
	}
	resp, err = d.do(ctx, http.MethodPost, "/containers/"+id+"/wait", nil)
	if err != nil {
		return chk, id, explainShareError(err, s)
	}
	var waited struct{ StatusCode int }
	if err := jsonDecode(resp, &waited); err != nil {
		return chk, id, err
	}
	out := map[string]string{}
	for _, stream := range []string{"stdout", "stderr"} {
		resp, err := d.do(ctx, http.MethodGet, "/containers/"+id+"/logs", url.Values{stream: {"1"}})
		if err != nil {
			return chk, id, err
		}
		var buf bytes.Buffer
		err = demux(io.LimitReader(resp.Body, 1<<20), &buf)
		resp.Body.Close()
		if err != nil && !errors.Is(err, io.EOF) {
			return chk, id, err
		}
		out[stream] = buf.String()
	}
	if waited.StatusCode != 0 {
		msg := strings.TrimSpace(out["stderr"])
		if msg == "" {
			msg = fmt.Sprintf("exit code %d", waited.StatusCode)
		}
		return chk, id, fmt.Errorf("the share mounted, but its files can't be listed: %s", scrubShareError(msg, s))
	}
	chk = parseShareCheck(out["stdout"])
	return chk, id, nil
}

// parseShareCheck reads the helper's output: one name per line, and the
// writable line when it checked writing.
func parseShareCheck(out string) shareCheck {
	var chk shareCheck
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		switch {
		case l == "":
		case strings.HasPrefix(l, writableMark):
			v := strings.TrimPrefix(l, writableMark)
			ok := v == "yes"
			chk.writable = &ok
			if !ok {
				chk.writeErr = strings.TrimSpace(strings.TrimPrefix(v, "no"))
			}
		default:
			chk.entries++
		}
	}
	return chk
}

// scrubShareError removes the share's password and the mount options from
// a Docker error. Docker's mount errors quote the whole option string
// ("…, data: addr=…,username=…,password=…: permission denied").
func scrubShareError(msg string, s *wire.ShareVolume) string {
	if s.Password != "" {
		msg = strings.ReplaceAll(msg, s.Password, "•••")
	}
	if i := strings.Index(msg, "data: "); i >= 0 {
		rest := msg[i:]
		if j := strings.LastIndex(rest, ": "); j > len("data:") {
			msg = msg[:i] + "options hidden" + rest[j:]
		} else {
			msg = msg[:i] + "options hidden"
		}
	}
	return msg
}

// explainShareError turns a failed mount into advice, keeping Docker's own
// words (without the password) at the end.
func explainShareError(err error, s *wire.ShareVolume) error {
	raw := scrubShareError(err.Error(), s)
	if errors.Is(err, context.DeadlineExceeded) {
		raw = "no answer within " + shareCheckTimeout.String()
	}
	return fmt.Errorf("%s (Docker said: %s)", shareHint(strings.ToLower(raw), s), raw)
}

func shareHint(low string, s *wire.ShareVolume) string {
	kind, pkg, what := "SMB", "cifs-utils", "share"
	if s.Type == wire.ShareNFS {
		kind, pkg, what = "NFS", "nfs-common (nfs-utils on Fedora/RHEL)", "export"
	}
	ver := s.Version
	if ver == "" {
		ver = map[string]string{wire.ShareCIFS: "3.0", wire.ShareNFS: "4"}[s.Type]
	}
	has := func(subs ...string) bool {
		for _, x := range subs {
			if strings.Contains(low, x) {
				return true
			}
		}
		return false
	}
	switch {
	case has("no answer within", "deadline exceeded", "timed out"):
		return fmt.Sprintf("%s didn't answer from this host: check the address, that %s file sharing is on, and that no firewall is in the way", s.Server, kind)
	case has("no such device", "unknown filesystem type", "wrong fs type"):
		return fmt.Sprintf("this host can't mount %s shares: install %s on the host (not in a container), then try again", kind, pkg)
	case has("permission denied", "access denied"):
		if s.Type == wire.ShareNFS {
			return fmt.Sprintf("%s refused this host: check that the export allows this host's IP address", s.Server)
		}
		return fmt.Sprintf("%s turned down the login: check the username and password, and that this user may open the share", s.Server)
	case has("no such file or directory", "not found"):
		return fmt.Sprintf("%s has no %s called %s (check the spelling; names are as the NAS shows them)", s.Server, what, s.Path)
	case has("host is down", "operation not supported", "protocol not supported", "not supported"):
		return fmt.Sprintf("%s didn't accept %s version %s: try another version", s.Server, kind, ver)
	case has("connection refused", "no route to host", "network is unreachable", "host is unreachable", "operation now in progress"):
		return fmt.Sprintf("can't reach %s's %s service from this host: check the address and that %s file sharing is on", s.Server, kind, kind)
	case has("invalid argument"):
		if s.Type == wire.ShareNFS {
			return fmt.Sprintf("the host rejected the mount: install %s on the host, or try another NFS version", pkg)
		}
		return fmt.Sprintf("the host rejected the mount: usually %s isn't installed on the host (it resolves host names and handles the login), or the server doesn't speak SMB %s; install it, or try another version", pkg, ver)
	case has("could not resolve", "no such host", "lookup"):
		return fmt.Sprintf("this host can't find %s: use its IP address instead", s.Server)
	}
	return fmt.Sprintf("the %s didn't mount", what)
}
