package agent

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/prezmc/tainerhub/internal/wire"
)

// incomingPrefix names the folder a stack folder is received into before it
// is renamed into place, so a failed copy never leaves a half-filled stack.
const incomingPrefix = ".tainerhub-incoming-"

// transferRunner sends or receives one item. hub opens the hub side of the
// stream: a POST of the sender's data, or a GET for the receiver.
type transferRunner struct {
	docker    Transferer
	stacksDir string // "" when stacks aren't available: no stack folders then
	hub       hubStreams
	log       interface{ Warn(string, ...any) }
}

type hubStreams interface {
	Upload(ctx context.Context, id string, body io.Reader) error
	Download(ctx context.Context, id string) (io.ReadCloser, error)
}

func (t *transferRunner) run(ctx context.Context, c wire.TransferCommand) (wire.TransferResult, error) {
	if err := wire.ValidTransferCommand(c); err != nil {
		return wire.TransferResult{}, err
	}
	if c.Item.Kind == wire.TransferStackDir && t.stacksDir == "" {
		return wire.TransferResult{}, errors.New("stacks aren't set up on this host")
	}
	if c.Role == wire.TransferSend {
		return t.send(ctx, c)
	}
	return t.receive(ctx, c)
}

// ---------------- sending ----------------

func (t *transferRunner) send(ctx context.Context, c wire.TransferCommand) (wire.TransferResult, error) {
	var m HelperMount
	switch c.Item.Kind {
	case wire.TransferVolume:
		v, ok, err := t.docker.VolumeInfo(ctx, c.Item.Name)
		if err != nil {
			return wire.TransferResult{}, err
		}
		if !ok {
			return wire.TransferResult{}, fmt.Errorf("volume %s doesn't exist on this host", c.Item.Name)
		}
		if err := copyableVolume(v); err != nil {
			return wire.TransferResult{}, fmt.Errorf("volume %s: %w", c.Item.Name, err)
		}
		m.Volume = c.Item.Name
	case wire.TransferStackDir:
		dir := filepath.Join(t.stacksDir, c.Item.Name)
		st, err := os.Stat(dir)
		if err != nil || !st.IsDir() {
			return wire.TransferResult{}, fmt.Errorf("the stack folder %s doesn't exist on this host", dir)
		}
		m.Bind = dir
	}
	helper, cleanup, err := t.helper(ctx, m)
	if err != nil {
		return wire.TransferResult{}, err
	}
	defer cleanup()

	src, err := t.docker.ArchiveGet(ctx, helper, "/"+wire.TransferRoot)
	if err != nil {
		return wire.TransferResult{}, fmt.Errorf("read the data: %w", err)
	}
	defer src.Close()

	// The archive is checked and re-written on its way out: entries are kept
	// under th-data/, and a stack folder's own staging folder is left out.
	pr, pw := io.Pipe()
	files := make(chan int, 1)
	go func() {
		n, err := filterTar(src, pw, c.Item.Kind == wire.TransferStackDir)
		files <- n
		_ = pw.CloseWithError(err)
	}()
	out := newMeter(pr, c.MaxBytes)
	err = t.hub.Upload(ctx, c.ID, out)
	_ = pr.CloseWithError(errors.New("upload ended"))
	n := <-files
	if err != nil {
		if out.err != nil {
			err = out.err
		}
		return wire.TransferResult{}, err
	}
	return wire.TransferResult{Bytes: out.n, SHA256: out.sum(), Files: n}, nil
}

// copyableVolume refuses volumes whose data lives elsewhere.
func copyableVolume(v VolumeInfo) error {
	if v.Driver != "" && v.Driver != "local" {
		return fmt.Errorf("it uses the %s driver, which keeps its data elsewhere; it isn't copied", v.Driver)
	}
	if len(v.Options) > 0 {
		return errors.New("it mounts storage from elsewhere (NFS, CIFS or a bind); it isn't copied")
	}
	return nil
}

// ---------------- receiving ----------------

func (t *transferRunner) receive(ctx context.Context, c wire.TransferCommand) (res wire.TransferResult, err error) {
	var m HelperMount
	var undo func()
	var finish func() error
	switch c.Item.Kind {
	case wire.TransferVolume:
		_, exists, err := t.docker.VolumeInfo(ctx, c.Item.Name)
		if err != nil {
			return res, err
		}
		if exists {
			return res, fmt.Errorf("a volume called %s already exists on this host", c.Item.Name)
		}
		if err := t.docker.CreateVolume(ctx, c.Item.Name, c.Item.Labels); err != nil {
			return res, fmt.Errorf("create volume %s: %w", c.Item.Name, err)
		}
		m.Volume = c.Item.Name
		undo = func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := t.docker.RemoveVolume(ctx, c.Item.Name); err != nil {
				t.warn("couldn't remove the partly copied volume", "volume", c.Item.Name, "err", err)
			}
		}
	case wire.TransferStackDir:
		final := filepath.Join(t.stacksDir, c.Item.Name)
		if ents, err := os.ReadDir(final); err == nil && len(ents) > 0 {
			return res, fmt.Errorf("the stack folder %s already exists on this host and isn't empty", final)
		}
		incoming := filepath.Join(t.stacksDir, incomingPrefix+c.ID)
		if err := os.Mkdir(incoming, 0o755); err != nil {
			return res, err
		}
		m.Bind = incoming
		undo = func() {
			if err := os.RemoveAll(incoming); err != nil {
				// Files that belong to other users can't be removed without
				// capabilities the agent doesn't have.
				t.warn("couldn't remove a partly copied stack folder; delete it by hand", "path", incoming, "err", err)
			}
		}
		finish = func() error {
			_ = os.Remove(final) // an empty folder left from before
			return os.Rename(incoming, final)
		}
	}
	defer func() {
		if err != nil {
			undo()
		}
	}()

	helper, cleanup, err := t.helper(ctx, m)
	if err != nil {
		return res, err
	}
	defer cleanup()

	body, err := t.hub.Download(ctx, c.ID)
	if err != nil {
		return res, err
	}
	defer body.Close()

	in := newMeter(body, c.MaxBytes)
	pr, pw := io.Pipe()
	type checked struct {
		files int
		err   error
	}
	done := make(chan checked, 1)
	go func() {
		n, err := checkTar(in, pw)
		if err == nil {
			// The tar reader stops at the end-of-archive marker; read the
			// padding after it too, so the checksum covers every byte sent.
			_, err = io.Copy(io.Discard, in)
		}
		done <- checked{n, err}
		_ = pw.CloseWithError(err)
	}()
	putErr := t.docker.ArchivePut(ctx, helper, "/", pr)
	_ = pr.CloseWithError(errUnpackEnded)
	chk := <-done
	switch {
	case in.err != nil:
		return res, in.err
	case chk.err != nil && !errors.Is(chk.err, errUnpackEnded):
		return res, chk.err // a bad or cut-short stream explains Docker's error too
	case putErr != nil:
		return res, fmt.Errorf("unpack the data: %w", putErr)
	case chk.err != nil:
		return res, chk.err
	}
	n := chk.files
	if finish != nil {
		if err := finish(); err != nil {
			return res, err
		}
	}
	return wire.TransferResult{Bytes: in.n, SHA256: in.sum(), Files: n}, nil
}

// ---------------- shared ----------------

// helper creates the helper container and returns a func that removes it.
func (t *transferRunner) helper(ctx context.Context, m HelperMount) (string, func(), error) {
	image, err := t.docker.HelperImage(ctx)
	if err != nil {
		return "", nil, err
	}
	id, err := t.docker.CreateHelper(ctx, image, m)
	if err != nil {
		return "", nil, fmt.Errorf("create helper container: %w", err)
	}
	return id, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := t.docker.RemoveContainer(ctx, id); err != nil {
			t.warn("couldn't remove a transfer helper container", "id", id, "err", err)
		}
	}, nil
}

func (t *transferRunner) warn(msg string, args ...any) {
	if t.log != nil {
		t.log.Warn(msg, args...)
	}
}

// meter counts and hashes what passes through it, failing past max bytes.
type meter struct {
	r   io.Reader
	h   hash.Hash
	n   int64
	max int64
	err error // set when max was passed
}

var (
	errTooBig      = errors.New("the data is larger than the transfer allows")
	errUnpackEnded = errors.New("docker stopped reading the data")
)

func newMeter(r io.Reader, max int64) *meter { return &meter{r: r, h: sha256.New(), max: max} }

func (m *meter) Read(p []byte) (int, error) {
	if m.err != nil {
		return 0, m.err
	}
	n, err := m.r.Read(p)
	if n > 0 {
		m.h.Write(p[:n])
		m.n += int64(n)
		if m.n > m.max {
			m.err = errTooBig
			return n, m.err
		}
	}
	return n, err
}

func (m *meter) sum() string { return hex.EncodeToString(m.h.Sum(nil)) }

// entryName checks a tar entry's name and returns it cleaned. Every entry
// must be th-data itself or inside it.
func entryName(name string) (string, error) {
	clean := path.Clean(strings.TrimPrefix(name, "./"))
	if strings.HasPrefix(clean, "/") || clean == ".." || strings.HasPrefix(clean, "../") ||
		(clean != wire.TransferRoot && !strings.HasPrefix(clean, wire.TransferRoot+"/")) {
		return "", fmt.Errorf("unexpected path in archive: %q", name)
	}
	return clean, nil
}

// checkTar copies a tar stream from r to w, refusing entries outside
// th-data/, hard links that point outside it, and device files. It returns
// the number of entries. Docker unpacks inside the helper container, which
// only has th-data mounted, but a stream from another host is checked anyway.
func checkTar(r io.Reader, w io.Writer) (int, error) {
	return copyTar(r, w, func(h *tar.Header) (bool, error) {
		switch h.Typeflag {
		case tar.TypeChar, tar.TypeBlock:
			return false, fmt.Errorf("device file in archive: %q", h.Name)
		case tar.TypeLink:
			if _, err := entryName(h.Linkname); err != nil {
				return false, fmt.Errorf("hard link out of the data: %q", h.Name)
			}
		}
		return true, nil
	})
}

// filterTar is checkTar for the sending side; with skipStaging it also leaves
// out the stack folder's own staging folder.
func filterTar(r io.Reader, w io.Writer, skipStaging bool) (int, error) {
	staging := wire.TransferRoot + "/" + stagingDir
	return copyTar(r, w, func(h *tar.Header) (bool, error) {
		if skipStaging && (h.Name == staging || strings.HasPrefix(h.Name, staging+"/")) {
			return false, nil
		}
		if h.Typeflag == tar.TypeLink {
			if _, err := entryName(h.Linkname); err != nil {
				return false, fmt.Errorf("hard link out of the data: %q", h.Name)
			}
		}
		return true, nil
	})
}

// copyTar re-writes a tar stream entry by entry. keep decides each entry
// (after its name has been checked and cleaned).
func copyTar(r io.Reader, w io.Writer, keep func(*tar.Header) (bool, error)) (int, error) {
	tr := tar.NewReader(r)
	tw := tar.NewWriter(w)
	n := 0
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return n, fmt.Errorf("read archive: %w", err)
		}
		if h.Name, err = entryName(h.Name); err != nil {
			return n, err
		}
		if h.Typeflag == tar.TypeDir {
			h.Name += "/"
		}
		ok, err := keep(h)
		if err != nil {
			return n, err
		}
		if !ok {
			continue
		}
		if err := tw.WriteHeader(h); err != nil {
			return n, err
		}
		if _, err := io.Copy(tw, tr); err != nil {
			return n, err
		}
		n++
	}
	return n, tw.Close()
}

// ---------------- the hub side ----------------

// hubClient opens transfer streams on the hub with the agent's certificate.
type hubClient struct{ a *Agent }

func (h hubClient) client() (*http.Client, error) {
	roots, err := h.a.caPool()
	if err != nil {
		return nil, err
	}
	cert := h.a.creds.Load()
	if cert == nil {
		return nil, errors.New("no client certificate")
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: h.a.hubTLS(*cert, roots), ForceAttemptHTTP2: true}}, nil
}

func (h hubClient) url(id string) string {
	return "https://" + h.a.cfg.HubAddr + wire.PathTransfer + id
}

func (h hubClient) Upload(ctx context.Context, id string, body io.Reader) error {
	c, err := h.client()
	if err != nil {
		return err
	}
	defer c.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.url(id), body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-tar")
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("send to the hub: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return hubError(resp)
	}
	var out struct{ Bytes int64 }
	_ = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&out)
	return nil
}

func (h hubClient) Download(ctx context.Context, id string) (io.ReadCloser, error) {
	c, err := h.client()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.url(id), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("receive from the hub: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, hubError(resp)
	}
	return &closeAll{ReadCloser: resp.Body, c: c}, nil
}

type closeAll struct {
	io.ReadCloser
	c *http.Client
}

func (c *closeAll) Close() error {
	err := c.ReadCloser.Close()
	c.c.CloseIdleConnections()
	return err
}

func hubError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	msg := strings.TrimSpace(string(b))
	if msg == "" {
		msg = resp.Status
	}
	return fmt.Errorf("hub: %s", msg)
}
