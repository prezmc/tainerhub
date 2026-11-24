package gateway_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/prezmc/tainerhub/internal/wire"
)

// session opens an agent stream with cert that sends nothing. read says
// whether the test client reads what the hub sends; ended closes when the
// hub ends the stream (only noticed when reading).
func (h *renewHarness) session(t *testing.T, cert tls.Certificate, read bool) (ended <-chan struct{}) {
	t.Helper()
	pr, pw := io.Pipe()
	t.Cleanup(func() { pw.Close() })
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+wire.PathConnect, pr)
	resp, err := h.client(cert).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("connect = %d", resp.StatusCode)
	}
	done := make(chan struct{})
	if read {
		go func() { _, _ = io.Copy(io.Discard, resp.Body); close(done) }()
	}
	waitFor(t, "session attached", func() bool { return h.reg.Get(renewHostID) != nil })
	return done
}

// fillWriter queues large messages for the agent until the hub's writer is
// stuck because the agent isn't reading.
func (h *renewHarness) fillWriter(t *testing.T) {
	t.Helper()
	a := h.reg.Get(renewHostID)
	data := bytes.Repeat([]byte("x"), 256<<10)
	for range 400 {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		err := a.ExecInput(ctx, "s", data)
		cancel()
		if err != nil {
			return // the queue is full: the writer is blocked
		}
	}
	t.Fatal("the hub kept writing to an agent that reads nothing")
}

// An agent that stops reading can't pin the hub's connection handler: its
// blocked write times out and the agent is dropped.
func TestStuckAgentDroppedByWriteDeadline(t *testing.T) {
	h := newRenewHarness(t) // DeadAfter 2s
	cert := h.issue(t, time.Hour)
	h.session(t, cert, false)
	h.fillWriter(t)
	waitFor(t, "stuck agent dropped", func() bool { return h.reg.Get(renewHostID) == nil })
}

// Disconnecting a host whose writer is stuck takes effect at once, not
// after the write deadline.
func TestDisconnectStuckAgentPromptly(t *testing.T) {
	h := newRenewHarness(t)
	cert := h.issue(t, time.Hour)
	h.session(t, cert, false)
	h.fillWriter(t)
	start := time.Now()
	h.reg.Disconnect(renewHostID)
	waitFor(t, "agent detached", func() bool { return h.reg.Get(renewHostID) == nil })
	if d := time.Since(start); d > time.Second {
		t.Fatalf("detaching a stuck agent took %v", d)
	}
}

// A stalled database doesn't keep a gone agent registered: it is detached
// first, and the last-seen update is given up after a while.
func TestDisconnectDetachesBeforeTouchingDB(t *testing.T) {
	h := newRenewHarness(t)
	cert := h.issue(t, time.Hour)
	h.store.mu.Lock()
	h.store.stallTouch = true
	h.store.mu.Unlock()
	h.session(t, cert, true)
	start := time.Now()
	h.reg.Disconnect(renewHostID)
	waitFor(t, "agent detached", func() bool { return h.reg.Get(renewHostID) == nil })
	if d := time.Since(start); d > time.Second {
		t.Fatalf("detaching took %v with the database stalled", d)
	}
	waitFor(t, "last-seen update", func() bool {
		h.store.mu.Lock()
		defer h.store.mu.Unlock()
		return h.store.touchDeadline
	})
}

// Re-enrolling a host ends a session still using its previous certificate
// straight away, and that certificate stops working.
func TestReenrollDropsOldSession(t *testing.T) {
	h := newRenewHarness(t)
	old := h.issue(t, time.Hour)
	ended := h.session(t, old, true)
	if code, _ := enroll(t, h, h.addToken("t-re", "secret")); code != http.StatusOK {
		t.Fatalf("re-enroll = %d", code)
	}
	select {
	case <-ended:
	case <-time.After(2 * time.Second):
		t.Fatal("session with the superseded certificate still open")
	}
	if code := h.connectStatus(t, old); code != http.StatusForbidden {
		t.Fatalf("connect with the superseded certificate = %d, want 403", code)
	}
}
