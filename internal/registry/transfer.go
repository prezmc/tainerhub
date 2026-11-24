package registry

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prezmc/tainerhub/internal/wire"
)

// A transfer moves one volume or stack folder from one agent to another.
// The sending agent uploads a tar stream to the hub's agent gateway and the
// receiving agent downloads it; the hub joins the two through a pipe and
// keeps nothing. Agents still only ever connect out to the hub.

var (
	ErrNoTransfer      = errors.New("no such transfer")
	ErrTransferClaimed = errors.New("transfer already in progress")
	ErrTransferStalled = errors.New("the transfer stalled: no data moved for 2 minutes")
	ErrTransferTooBig  = errors.New("the data is larger than expected")
	ErrTransferOld     = errors.New("the agent is too old to copy data; update it to the latest tainerhub-agent image")
)

// transferStall is how long a transfer may go without moving any data.
var transferStall = 2 * time.Minute

// Transfer is one item's stream between two hosts.
type Transfer struct {
	ID       string
	Src, Dst string // host IDs allowed to upload and download
	Max      int64

	pr *io.PipeReader
	pw *io.PipeWriter

	upClaimed, downClaimed atomic.Bool
	moved                  atomic.Int64 // bytes relayed so far
	last                   atomic.Int64 // unix nanos of the last data moved (or creation)

	abortOnce sync.Once
	aborted   chan struct{}
	abortErr  error
}

func (r *Registry) newTransfer(src, dst string, max int64) *Transfer {
	pr, pw := io.Pipe()
	t := &Transfer{ID: newTransferID(), Src: src, Dst: dst, Max: max, pr: pr, pw: pw, aborted: make(chan struct{})}
	t.last.Store(time.Now().UnixNano())
	r.xferMu.Lock()
	if r.xfers == nil {
		r.xfers = map[string]*Transfer{}
	}
	r.xfers[t.ID] = t
	r.xferMu.Unlock()
	return t
}

func (r *Registry) dropTransfer(t *Transfer) {
	r.xferMu.Lock()
	delete(r.xfers, t.ID)
	r.xferMu.Unlock()
}

// abortTransfers ends every active transfer to or from hostID with err.
func (r *Registry) abortTransfers(hostID string, err error) {
	r.xferMu.Lock()
	var hit []*Transfer
	for _, t := range r.xfers {
		if t.Src == hostID || t.Dst == hostID {
			hit = append(hit, t)
		}
	}
	r.xferMu.Unlock()
	for _, t := range hit {
		t.Abort(err)
	}
}

// Transfer finds an active transfer by ID.
func (r *Registry) Transfer(id string) *Transfer {
	r.xferMu.Lock()
	defer r.xferMu.Unlock()
	return r.xfers[id]
}

func newTransferID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Moved is how many bytes have passed through so far.
func (t *Transfer) Moved() int64 { return t.moved.Load() }

// Abort ends both sides of the transfer with err.
func (t *Transfer) Abort(err error) {
	t.abortOnce.Do(func() {
		t.abortErr = err
		close(t.aborted)
		_ = t.pw.CloseWithError(err)
		_ = t.pr.CloseWithError(err)
	})
}

// Upload copies the sending agent's tar stream into the transfer. It
// returns once the receiver has read all of it, or on error. Only hostID
// (the sender) may upload, once.
func (t *Transfer) Upload(hostID string, body io.Reader) (int64, error) {
	if hostID != t.Src {
		return 0, ErrNoTransfer
	}
	if !t.upClaimed.CompareAndSwap(false, true) {
		return 0, ErrTransferClaimed
	}
	n, err := io.Copy(t.pw, &countReader{r: io.LimitReader(body, t.Max+1), t: t})
	switch {
	case err != nil:
		t.Abort(fmt.Errorf("upload: %w", err))
	case n > t.Max:
		err = ErrTransferTooBig
		t.Abort(err)
	default:
		_ = t.pw.Close() // the receiver sees EOF
	}
	return n, err
}

// Download copies the transfer to the receiving agent. flush, if set, is
// called after each write so data doesn't sit in buffers. Only hostID (the
// receiver) may download, once.
func (t *Transfer) Download(hostID string, w io.Writer, flush func()) (int64, error) {
	if hostID != t.Dst {
		return 0, ErrNoTransfer
	}
	if !t.downClaimed.CompareAndSwap(false, true) {
		return 0, ErrTransferClaimed
	}
	n, err := io.Copy(&flushWriter{w: w, flush: flush}, t.pr)
	if err != nil {
		t.Abort(fmt.Errorf("download: %w", err))
	}
	return n, err
}

type countReader struct {
	r io.Reader
	t *Transfer
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 {
		c.t.moved.Add(int64(n))
		c.t.last.Store(time.Now().UnixNano())
	}
	return n, err
}

type flushWriter struct {
	w     io.Writer
	flush func()
}

func (f *flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if err == nil && f.flush != nil {
		f.flush()
	}
	return n, err
}

// CopyResult describes a finished copy.
type CopyResult struct {
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
	Files  int    `json:"files"`
}

// Copy moves one item from host src to host dst and checks that both sides
// saw the same data. progress, if set, is called about once a second with
// the bytes moved so far. Cancelling ctx stops both agents.
func (r *Registry) Copy(ctx context.Context, src, dst string, item wire.TransferItem, maxBytes int64, progress func(int64)) (CopyResult, error) {
	if src == dst {
		return CopyResult{}, errors.New("source and destination are the same host")
	}
	if maxBytes <= 0 || maxBytes > wire.MaxTransferBytes {
		maxBytes = wire.MaxTransferBytes
	}
	from, to := r.Get(src), r.Get(dst)
	if from == nil || to == nil {
		return CopyResult{}, ErrOffline
	}
	for _, a := range []*Agent{from, to} {
		if h := a.Status().Hello; h == nil || !h.Transfer {
			return CopyResult{}, fmt.Errorf("%s: %w", a.Name, ErrTransferOld)
		}
	}
	t := r.newTransfer(src, dst, maxBytes)
	defer r.dropTransfer(t)
	cmd := func(role string) wire.Command {
		return wire.Command{Transfer: &wire.TransferCommand{ID: t.ID, Role: role, Item: item, MaxBytes: maxBytes}}
	}
	recv, send := cmd(wire.TransferReceive), cmd(wire.TransferSend)
	if err := wire.ValidTransferCommand(*recv.Transfer); err != nil {
		return CopyResult{}, err
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type outcome struct {
		res wire.TransferResult
		err error
	}
	run := func(a *Agent, c wire.Command, out chan<- outcome) {
		res, err := a.longCall(ctx, c)
		var tr wire.TransferResult
		if err == nil {
			if jerr := json.Unmarshal(res.Data, &tr); jerr != nil {
				err = fmt.Errorf("unreadable result: %w", jerr)
			}
		}
		if err != nil {
			err = fmt.Errorf("%s: %w", a.Name, err)
			t.Abort(err) // unblocks the other side
		}
		out <- outcome{tr, err}
	}
	recvDone, sendDone := make(chan outcome, 1), make(chan outcome, 1)
	go run(to, recv, recvDone) // the receiver first: it creates the target, then waits for data
	go run(from, send, sendDone)

	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	stopped := ctx.Done() // set to nil once handled, so it isn't selected again
	var got, sent *outcome
	for got == nil || sent == nil {
		select {
		case o := <-recvDone:
			got = &o
		case o := <-sendDone:
			sent = &o
		case <-tick.C:
			if progress != nil {
				progress(t.Moved())
			}
			if time.Since(time.Unix(0, t.last.Load())) > transferStall {
				t.Abort(ErrTransferStalled)
				cancel()
			}
		case <-stopped:
			t.Abort(ctx.Err())
			stopped = nil
		}
	}
	if progress != nil {
		progress(t.Moved())
	}
	// The first failure is the cause; the other side usually just saw the
	// stream end. A stall is reported as such.
	select {
	case <-t.aborted:
		if errors.Is(t.abortErr, ErrTransferStalled) || errors.Is(t.abortErr, ErrTransferTooBig) {
			return CopyResult{}, t.abortErr
		}
	default:
	}
	if got.err != nil && sent.err != nil {
		return CopyResult{}, firstErr(t, got.err)
	}
	if sent.err != nil {
		return CopyResult{}, sent.err
	}
	if got.err != nil {
		return CopyResult{}, got.err
	}
	if got.res.Bytes != sent.res.Bytes || got.res.SHA256 != sent.res.SHA256 {
		return CopyResult{}, fmt.Errorf("the copy doesn't match: %s sent %d bytes, %s received %d", from.Name, sent.res.Bytes, to.Name, got.res.Bytes)
	}
	return CopyResult{Bytes: got.res.Bytes, SHA256: got.res.SHA256, Files: got.res.Files}, nil
}

// firstErr picks the error that caused the abort, when both sides failed.
func firstErr(t *Transfer, fallback error) error {
	select {
	case <-t.aborted:
		if t.abortErr != nil {
			return t.abortErr
		}
	default:
	}
	return fallback
}

// longCall sends a command that may run for hours and waits for its result.
// If ctx ends first, it tells the agent to stop.
func (a *Agent) longCall(ctx context.Context, cmd wire.Command) (wire.CommandResult, error) {
	id := newID()
	cmd.ID = id
	ch := make(chan wire.CommandResult, 1)
	a.mu.Lock()
	select {
	case <-a.done:
		a.mu.Unlock()
		return wire.CommandResult{}, ErrOffline
	default:
	}
	a.pending[id] = ch
	a.mu.Unlock()
	if err := a.send(ctx, wire.HubMessage{Command: &cmd}); err != nil {
		a.forget(id)
		return wire.CommandResult{}, err
	}
	select {
	case res := <-ch:
		if !res.OK {
			if res.Error == "" {
				res.Error = "operation failed"
			}
			return res, errors.New(res.Error)
		}
		return res, nil
	case <-ctx.Done():
		a.forget(id)
		stop, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = a.send(stop, wire.HubMessage{Command: &wire.Command{ID: newID(), Cancel: id}})
		cancel()
		return wire.CommandResult{}, ctx.Err()
	}
}
