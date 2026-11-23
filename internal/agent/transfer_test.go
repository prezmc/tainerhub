package agent

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/prezmc/tainerhub/internal/wire"
)

// ---- fakes ----

type memFile struct {
	dir      bool
	mode     int64
	uid, gid int
	data     string
	link     string // symlink target
}

type memVolume struct {
	info  VolumeInfo
	files map[string]memFile // path inside the volume ("" is the root)
}

// fakeTransferer keeps volumes in memory and reads and writes bind mounts on
// disk, standing in for Docker's helper-container copy API.
type fakeTransferer struct {
	mu      sync.Mutex
	volumes map[string]*memVolume
	helpers map[string]HelperMount
	removed []string // helpers removed
	putErr  error    // ArchivePut fails with this after reading the stream
	n       int
}

func newFakeTransferer() *fakeTransferer {
	return &fakeTransferer{volumes: map[string]*memVolume{}, helpers: map[string]HelperMount{}}
}

func (f *fakeTransferer) HelperImage(context.Context) (string, error) { return "sha256:agent", nil }

func (f *fakeTransferer) CreateHelper(_ context.Context, image string, m HelperMount) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if image != "sha256:agent" {
		return "", errors.New("wrong image")
	}
	if m.Volume != "" && f.volumes[m.Volume] == nil {
		return "", errors.New("no such volume")
	}
	f.n++
	id := fmt.Sprintf("helper%d", f.n)
	f.helpers[id] = m
	return id, nil
}

func (f *fakeTransferer) RemoveContainer(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.helpers, id)
	f.removed = append(f.removed, id)
	return nil
}

func (f *fakeTransferer) ArchiveGet(_ context.Context, id, p string) (io.ReadCloser, error) {
	f.mu.Lock()
	m, ok := f.helpers[id]
	f.mu.Unlock()
	if !ok || p != "/"+wire.TransferRoot {
		return nil, errors.New("bad archive request")
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if m.Volume != "" {
		f.mu.Lock()
		v := f.volumes[m.Volume]
		names := make([]string, 0, len(v.files))
		for n := range v.files {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			e := v.files[n]
			h := &tar.Header{Name: filepath.ToSlash(filepath.Join(wire.TransferRoot, n)), Mode: e.mode, Uid: e.uid, Gid: e.gid}
			switch {
			case e.dir:
				h.Typeflag, h.Name = tar.TypeDir, h.Name+"/"
			case e.link != "":
				h.Typeflag, h.Linkname = tar.TypeSymlink, e.link
			default:
				h.Typeflag, h.Size = tar.TypeReg, int64(len(e.data))
			}
			_ = tw.WriteHeader(h)
			_, _ = io.WriteString(tw, e.data)
		}
		f.mu.Unlock()
	} else {
		err := filepath.Walk(m.Bind, func(p string, fi os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(m.Bind, p)
			h, _ := tar.FileInfoHeader(fi, "")
			h.Name = filepath.ToSlash(filepath.Join(wire.TransferRoot, rel))
			if fi.IsDir() {
				h.Name += "/"
			}
			if err := tw.WriteHeader(h); err != nil {
				return err
			}
			if fi.Mode().IsRegular() {
				b, _ := os.ReadFile(p)
				_, _ = tw.Write(b)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	_ = tw.Close()
	return io.NopCloser(&buf), nil
}

func (f *fakeTransferer) ArchivePut(_ context.Context, id, p string, r io.Reader) error {
	f.mu.Lock()
	m, ok := f.helpers[id]
	f.mu.Unlock()
	if !ok || p != "/" {
		return errors.New("bad archive request")
	}
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		rel := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(h.Name, wire.TransferRoot), "/"), "/")
		data, _ := io.ReadAll(tr)
		if m.Volume != "" {
			f.mu.Lock()
			f.volumes[m.Volume].files[rel] = memFile{dir: h.Typeflag == tar.TypeDir, mode: h.Mode, uid: h.Uid, gid: h.Gid, data: string(data), link: h.Linkname}
			f.mu.Unlock()
			continue
		}
		dst := filepath.Join(m.Bind, rel)
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.WriteFile(dst, data, os.FileMode(h.Mode)&0o777); err != nil {
				return err
			}
		}
	}
	if f.putErr != nil {
		return f.putErr
	}
	return nil
}

func (f *fakeTransferer) VolumeInfo(_ context.Context, name string) (VolumeInfo, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.volumes[name]
	if !ok {
		return VolumeInfo{}, false, nil
	}
	return v.info, true, nil
}

func (f *fakeTransferer) CreateVolume(_ context.Context, name string, labels map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.volumes[name] != nil {
		return errors.New("exists")
	}
	f.volumes[name] = &memVolume{info: VolumeInfo{Driver: "local", Labels: labels}, files: map[string]memFile{}}
	return nil
}

func (f *fakeTransferer) RemoveVolume(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range f.helpers {
		if m.Volume == name {
			return errors.New("volume is in use")
		}
	}
	delete(f.volumes, name)
	return nil
}

// memHub joins uploads to downloads by transfer ID, like the hub does.
type memHub struct {
	mu    sync.Mutex
	pipes map[string]*memPipe
	// corrupt, if set, replaces the stream the receiver gets.
	corrupt []byte
}

type memPipe struct {
	pr *io.PipeReader
	pw *io.PipeWriter
}

func (h *memHub) pipe(id string) *memPipe {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.pipes == nil {
		h.pipes = map[string]*memPipe{}
	}
	p := h.pipes[id]
	if p == nil {
		pr, pw := io.Pipe()
		p = &memPipe{pr, pw}
		h.pipes[id] = p
	}
	return p
}

func (h *memHub) Upload(_ context.Context, id string, body io.Reader) error {
	p := h.pipe(id)
	_, err := io.Copy(p.pw, body)
	_ = p.pw.CloseWithError(err)
	return err
}

func (h *memHub) Download(_ context.Context, id string) (io.ReadCloser, error) {
	if h.corrupt != nil {
		return io.NopCloser(bytes.NewReader(h.corrupt)), nil
	}
	return h.pipe(id).pr, nil
}

// copyItem runs a sender and a receiver against each other.
func copyItem(t *testing.T, hub *memHub, from, to *transferRunner, item wire.TransferItem) (sent, got wire.TransferResult, sendErr, recvErr error) {
	t.Helper()
	c := wire.TransferCommand{ID: strings.Repeat("ab", 16), Item: item, MaxBytes: 1 << 30}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		rc := c
		rc.Role = wire.TransferReceive
		got, recvErr = to.run(context.Background(), rc)
		if recvErr != nil {
			_ = hub.pipe(c.ID).pr.CloseWithError(recvErr) // like the hub aborting
		}
	}()
	go func() {
		defer wg.Done()
		sc := c
		sc.Role = wire.TransferSend
		sent, sendErr = from.run(context.Background(), sc)
	}()
	wg.Wait()
	return
}

// ---- tests ----

func TestTransferVolume(t *testing.T) {
	src, dst := newFakeTransferer(), newFakeTransferer()
	labels := map[string]string{"com.docker.compose.project": "app", "com.docker.compose.volume": "db"}
	src.volumes["app_db"] = &memVolume{info: VolumeInfo{Driver: "local", Labels: labels}, files: map[string]memFile{
		"":          {dir: true, mode: 0o755},
		"pg":        {dir: true, mode: 0o700, uid: 999, gid: 999},
		"pg/data":   {mode: 0o600, uid: 999, gid: 999, data: "rows"},
		"link":      {mode: 0o777, link: "pg/data"},
		"top-level": {mode: 0o644, data: strings.Repeat("x", 100_000)},
	}}
	hub := &memHub{}
	from := &transferRunner{docker: src, hub: hub}
	to := &transferRunner{docker: dst, hub: hub}

	sent, got, sendErr, recvErr := copyItem(t, hub, from, to, wire.TransferItem{Kind: wire.TransferVolume, Name: "app_db", Labels: labels})
	if sendErr != nil || recvErr != nil {
		t.Fatalf("send %v, receive %v", sendErr, recvErr)
	}
	if sent.Bytes == 0 || sent != got {
		t.Fatalf("sent %+v, got %+v", sent, got)
	}
	if got.Files != 5 {
		t.Errorf("files = %d, want 5", got.Files)
	}
	v := dst.volumes["app_db"]
	if v == nil || v.info.Labels["com.docker.compose.project"] != "app" {
		t.Fatalf("volume not created with its labels: %+v", v)
	}
	for name, want := range src.volumes["app_db"].files {
		if got := v.files[name]; got != want {
			t.Errorf("%q: got %+v, want %+v", name, got, want)
		}
	}
	if len(src.helpers) != 0 || len(dst.helpers) != 0 {
		t.Errorf("helpers left behind: %v %v", src.helpers, dst.helpers)
	}
}

func TestTransferRefusals(t *testing.T) {
	src, dst := newFakeTransferer(), newFakeTransferer()
	src.volumes["nfs"] = &memVolume{info: VolumeInfo{Driver: "local", Options: map[string]string{"type": "nfs"}}, files: map[string]memFile{}}
	src.volumes["plugin"] = &memVolume{info: VolumeInfo{Driver: "rexray"}, files: map[string]memFile{}}
	src.volumes["data"] = &memVolume{info: VolumeInfo{Driver: "local"}, files: map[string]memFile{"": {dir: true, mode: 0o755}}}
	dst.volumes["data"] = &memVolume{info: VolumeInfo{Driver: "local"}, files: map[string]memFile{"keep": {data: "mine"}}}
	from := &transferRunner{docker: src, hub: &memHub{}}
	to := &transferRunner{docker: dst, hub: &memHub{}}
	ctx := context.Background()
	send := func(name string) error {
		_, err := from.run(ctx, wire.TransferCommand{ID: strings.Repeat("cd", 16), Role: wire.TransferSend, Item: wire.TransferItem{Kind: wire.TransferVolume, Name: name}, MaxBytes: 1 << 20})
		return err
	}
	if err := send("nfs"); err == nil || !strings.Contains(err.Error(), "elsewhere") {
		t.Errorf("NFS volume: %v", err)
	}
	if err := send("plugin"); err == nil || !strings.Contains(err.Error(), "driver") {
		t.Errorf("plugin volume: %v", err)
	}
	if err := send("missing"); err == nil || !strings.Contains(err.Error(), "doesn't exist") {
		t.Errorf("missing volume: %v", err)
	}
	_, err := to.run(ctx, wire.TransferCommand{ID: strings.Repeat("cd", 16), Role: wire.TransferReceive, Item: wire.TransferItem{Kind: wire.TransferVolume, Name: "data"}, MaxBytes: 1 << 20})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("existing volume on the receiver: %v", err)
	}
	if dst.volumes["data"].files["keep"].data != "mine" {
		t.Error("the receiver's existing volume was touched")
	}
	noStacks := &transferRunner{docker: dst, hub: &memHub{}}
	_, err = noStacks.run(ctx, wire.TransferCommand{ID: strings.Repeat("cd", 16), Role: wire.TransferReceive, Item: wire.TransferItem{Kind: wire.TransferStackDir, Name: "app"}, MaxBytes: 1 << 20})
	if err == nil || !strings.Contains(err.Error(), "stacks aren't set up") {
		t.Errorf("stack folder without stacks: %v", err)
	}
}

func TestTransferReceiveCleansUp(t *testing.T) {
	// A stream with an entry outside th-data/ is refused and the new volume removed.
	var bad bytes.Buffer
	tw := tar.NewWriter(&bad)
	_ = tw.WriteHeader(&tar.Header{Name: wire.TransferRoot + "/", Typeflag: tar.TypeDir, Mode: 0o755})
	_ = tw.WriteHeader(&tar.Header{Name: wire.TransferRoot + "/../etc/passwd", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1})
	_, _ = tw.Write([]byte("x"))
	_ = tw.Close()

	dst := newFakeTransferer()
	to := &transferRunner{docker: dst, hub: &memHub{corrupt: bad.Bytes()}}
	_, err := to.run(context.Background(), wire.TransferCommand{ID: strings.Repeat("ef", 16), Role: wire.TransferReceive, Item: wire.TransferItem{Kind: wire.TransferVolume, Name: "v"}, MaxBytes: 1 << 20})
	if err == nil || !strings.Contains(err.Error(), "unexpected path") {
		t.Fatalf("err = %v", err)
	}
	if dst.volumes["v"] != nil {
		t.Error("the partly copied volume was left behind")
	}
	if len(dst.helpers) != 0 {
		t.Error("helper left behind")
	}

	// Docker failing to unpack also removes the volume.
	src := newFakeTransferer()
	src.volumes["v"] = &memVolume{info: VolumeInfo{Driver: "local"}, files: map[string]memFile{"": {dir: true, mode: 0o755}, "f": {mode: 0o644, data: "hi"}}}
	dst = newFakeTransferer()
	dst.putErr = errors.New("no space left on device")
	hub := &memHub{}
	_, _, _, recvErr := copyItem(t, hub, &transferRunner{docker: src, hub: hub}, &transferRunner{docker: dst, hub: hub}, wire.TransferItem{Kind: wire.TransferVolume, Name: "v"})
	if recvErr == nil || !strings.Contains(recvErr.Error(), "no space left") {
		t.Fatalf("receive err = %v", recvErr)
	}
	if dst.volumes["v"] != nil {
		t.Error("volume kept after a failed unpack")
	}
}

func TestTransferTooBig(t *testing.T) {
	src, dst := newFakeTransferer(), newFakeTransferer()
	src.volumes["v"] = &memVolume{info: VolumeInfo{Driver: "local"}, files: map[string]memFile{"": {dir: true, mode: 0o755}, "big": {mode: 0o644, data: strings.Repeat("x", 50_000)}}}
	hub := &memHub{}
	c := wire.TransferCommand{ID: strings.Repeat("12", 16), Role: wire.TransferSend, Item: wire.TransferItem{Kind: wire.TransferVolume, Name: "v"}, MaxBytes: 10_000}
	go func() { _, _ = io.Copy(io.Discard, hub.pipe(c.ID).pr) }()
	if _, err := (&transferRunner{docker: src, hub: hub}).run(context.Background(), c); !errors.Is(err, errTooBig) {
		t.Errorf("sender err = %v, want too big", err)
	}
	if len(src.helpers) != 0 || len(dst.helpers) != 0 {
		t.Error("helper left behind")
	}
}

func TestTransferStackDir(t *testing.T) {
	srcStacks, dstStacks := t.TempDir(), t.TempDir()
	app := filepath.Join(srcStacks, "app")
	for _, d := range []string{"data/sub", stagingDir} {
		if err := os.MkdirAll(filepath.Join(app, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{"compose.yaml": "services: {}\n", "data/sub/f.txt": "hello", stagingDir + "/compose.yaml": "half-written"}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(app, name), []byte(body), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	// An empty folder already there (e.g. from an earlier removal) is fine.
	if err := os.Mkdir(filepath.Join(dstStacks, "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	hub := &memHub{}
	from := &transferRunner{docker: newFakeTransferer(), stacksDir: srcStacks, hub: hub}
	to := &transferRunner{docker: newFakeTransferer(), stacksDir: dstStacks, hub: hub}
	sent, got, sendErr, recvErr := copyItem(t, hub, from, to, wire.TransferItem{Kind: wire.TransferStackDir, Name: "app"})
	if sendErr != nil || recvErr != nil {
		t.Fatalf("send %v, receive %v", sendErr, recvErr)
	}
	if sent != got {
		t.Errorf("sent %+v, got %+v", sent, got)
	}
	b, err := os.ReadFile(filepath.Join(dstStacks, "app", "data", "sub", "f.txt"))
	if err != nil || string(b) != "hello" {
		t.Fatalf("copied file: %q %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(dstStacks, "app", stagingDir)); !os.IsNotExist(err) {
		t.Errorf("staging folder was copied: %v", err)
	}
	ents, _ := os.ReadDir(dstStacks)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), incomingPrefix) {
			t.Errorf("incoming folder left: %s", e.Name())
		}
	}

	// A folder that already has files is never overwritten.
	_, err = to.run(context.Background(), wire.TransferCommand{ID: strings.Repeat("34", 16), Role: wire.TransferReceive, Item: wire.TransferItem{Kind: wire.TransferStackDir, Name: "app"}, MaxBytes: 1 << 20})
	if err == nil || !strings.Contains(err.Error(), "isn't empty") {
		t.Errorf("err = %v", err)
	}
}

func TestEntryName(t *testing.T) {
	ok := []string{"th-data", "th-data/", "th-data/a/b", "./th-data/x", "th-data/a/../b"}
	bad := []string{"/th-data/x", "th-data/../x", "../th-data", "etc/passwd", "th-dataX/y", ""}
	for _, n := range ok {
		if _, err := entryName(n); err != nil {
			t.Errorf("%q refused: %v", n, err)
		}
	}
	for _, n := range bad {
		if _, err := entryName(n); err == nil {
			t.Errorf("%q accepted", n)
		}
	}
}

func TestCheckTarRefusesDevicesAndOutsideLinks(t *testing.T) {
	for _, h := range []*tar.Header{
		{Name: "th-data/null", Typeflag: tar.TypeChar, Mode: 0o666},
		{Name: "th-data/l", Typeflag: tar.TypeLink, Linkname: "/etc/shadow"},
	} {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		_ = tw.WriteHeader(h)
		_ = tw.Close()
		if _, err := checkTar(&buf, io.Discard); err == nil {
			t.Errorf("%s accepted", h.Name)
		}
	}
}
