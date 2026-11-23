package registry

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/prezmc/tainerhub/internal/wire"
)

// simAgent answers transfer commands the way a real agent does: the sender
// uploads data, the receiver downloads it, and each reports what it saw.
type simAgent struct {
	a    *Agent
	data []byte // what this agent sends
	// recvErr / sendErr make that side fail after (or instead of) moving data.
	recvErr, sendErr string
	stall            bool // the sender never sends anything
	cancelled        chan string
}

func attachSim(t *testing.T, r *Registry, id string, s *simAgent) {
	t.Helper()
	s.a = r.Attach(id, id, func() {})
	s.a.Handle(wire.AgentMessage{Hello: &wire.Hello{Transfer: true}}, nil)
	s.cancelled = make(chan string, 4)
	go func() {
		for {
			select {
			case <-s.a.Done():
				return
			case m := <-s.a.Out():
				if m.Command == nil {
					continue
				}
				if m.Command.Cancel != "" {
					s.cancelled <- m.Command.Cancel
					continue
				}
				if c := m.Command.Transfer; c != nil {
					go s.run(r, m.Command.ID, *c)
				}
			}
		}
	}()
}

func (s *simAgent) run(r *Registry, cmdID string, c wire.TransferCommand) {
	res := wire.CommandResult{CommandID: cmdID}
	t := r.Transfer(c.ID)
	var tr wire.TransferResult
	var err error
	switch {
	case t == nil:
		err = errors.New("no transfer")
	case c.Role == wire.TransferSend:
		if s.stall {
			time.Sleep(time.Hour)
		}
		var n int64
		n, err = t.Upload(s.a.HostID, bytes.NewReader(s.data))
		tr = wire.TransferResult{Bytes: n, SHA256: sum(s.data)}
		if err == nil && s.sendErr != "" {
			err = errors.New(s.sendErr)
		}
	default:
		var buf bytes.Buffer
		var n int64
		n, err = t.Download(s.a.HostID, &buf, nil)
		tr = wire.TransferResult{Bytes: n, SHA256: sum(buf.Bytes())}
		if err == nil && s.recvErr != "" {
			err = errors.New(s.recvErr)
			t.Abort(err)
		}
	}
	if err != nil {
		res.Error = err.Error()
	} else {
		res.OK = true
		res.Data, _ = json.Marshal(tr)
	}
	s.a.Handle(wire.AgentMessage{Result: &res}, nil)
}

func sum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

var item = wire.TransferItem{Kind: wire.TransferVolume, Name: "app_db"}

func TestCopy(t *testing.T) {
	r := New()
	data := bytes.Repeat([]byte("0123456789"), 100_000)
	attachSim(t, r, "a", &simAgent{data: data})
	attachSim(t, r, "b", &simAgent{})
	var last int64
	res, err := r.Copy(context.Background(), "a", "b", item, 0, func(n int64) { last = n })
	if err != nil {
		t.Fatal(err)
	}
	if res.Bytes != int64(len(data)) || res.SHA256 != sum(data) || last != int64(len(data)) {
		t.Errorf("result %+v, progress %d", res, last)
	}
	if len(r.xfers) != 0 {
		t.Error("transfer not dropped")
	}
}

func TestCopyFailures(t *testing.T) {
	ctx := context.Background()
	t.Run("receiver fails", func(t *testing.T) {
		r := New()
		attachSim(t, r, "a", &simAgent{data: []byte("data")})
		attachSim(t, r, "b", &simAgent{recvErr: "no space left on device"})
		_, err := r.Copy(ctx, "a", "b", item, 0, nil)
		if err == nil || !strings.Contains(err.Error(), "no space left") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("too big", func(t *testing.T) {
		r := New()
		attachSim(t, r, "a", &simAgent{data: make([]byte, 5000)})
		attachSim(t, r, "b", &simAgent{})
		if _, err := r.Copy(ctx, "a", "b", item, 1000, nil); !errors.Is(err, ErrTransferTooBig) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("stall", func(t *testing.T) {
		old := transferStall
		transferStall = 1500 * time.Millisecond
		defer func() { transferStall = old }()
		r := New()
		sender := &simAgent{stall: true}
		attachSim(t, r, "a", sender)
		attachSim(t, r, "b", &simAgent{})
		_, err := r.Copy(ctx, "a", "b", item, 0, nil)
		if !errors.Is(err, ErrTransferStalled) {
			t.Fatalf("err = %v", err)
		}
		select {
		case <-sender.cancelled:
		case <-time.After(2 * time.Second):
			t.Error("the stalled sender wasn't told to stop")
		}
	})
	t.Run("old agent", func(t *testing.T) {
		r := New()
		attachSim(t, r, "a", &simAgent{})
		b := r.Attach("b", "b", func() {})
		b.Handle(wire.AgentMessage{Hello: &wire.Hello{}}, nil)
		if _, err := r.Copy(ctx, "a", "b", item, 0, nil); !errors.Is(err, ErrTransferOld) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("offline and same host", func(t *testing.T) {
		r := New()
		attachSim(t, r, "a", &simAgent{})
		if _, err := r.Copy(ctx, "a", "nope", item, 0, nil); !errors.Is(err, ErrOffline) {
			t.Errorf("offline: %v", err)
		}
		if _, err := r.Copy(ctx, "a", "a", item, 0, nil); err == nil {
			t.Error("copy to the same host allowed")
		}
	})
}

func TestTransferOnlyItsHosts(t *testing.T) {
	r := New()
	tr := r.newTransfer("src", "dst", 100)
	if _, err := tr.Upload("dst", strings.NewReader("x")); !errors.Is(err, ErrNoTransfer) {
		t.Errorf("upload by the receiver: %v", err)
	}
	if _, err := tr.Download("src", io.Discard, nil); !errors.Is(err, ErrNoTransfer) {
		t.Errorf("download by the sender: %v", err)
	}
	go func() { _, _ = tr.Download("dst", io.Discard, nil) }()
	if _, err := tr.Upload("src", strings.NewReader("hello")); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.Upload("src", strings.NewReader("again")); !errors.Is(err, ErrTransferClaimed) {
		t.Errorf("second upload: %v", err)
	}
	if tr.Moved() != 5 {
		t.Errorf("moved %d", tr.Moved())
	}
}
