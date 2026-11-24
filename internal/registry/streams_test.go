package registry

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prezmc/tainerhub/internal/wire"
)

// drain empties an agent's outgoing queue (as the gateway's writer would)
// and returns the commands in it.
func drain(a *Agent) []*wire.Command {
	var cmds []*wire.Command
	for {
		select {
		case m := <-a.Out():
			if m.Command != nil {
				cmds = append(cmds, m.Command)
			}
		default:
			return cmds
		}
	}
}

var testExec = wire.ExecStart{ContainerID: "web", Cmd: []string{"sh"}, Cols: 80, Rows: 24}

// Logs, terminals and stack operations each have their own cap, so open
// log views never block a deploy or a terminal.
func TestStreamCapsPerKind(t *testing.T) {
	r := New()
	a := r.Attach("h1", "host-1", func() {})
	ctx := context.Background()
	var stops []func()
	for range MaxLogStreams {
		_, stop, err := a.Logs(ctx, wire.LogsRequest{ContainerID: "web"})
		if err != nil {
			t.Fatal(err)
		}
		stops = append(stops, stop)
		drain(a)
	}
	if _, _, err := a.Logs(ctx, wire.LogsRequest{ContainerID: "web"}); !errors.Is(err, ErrTooManyLogs) {
		t.Fatalf("log stream beyond the cap: %v", err)
	}
	for i := range MaxExecStreams {
		if _, _, _, err := a.Exec(ctx, testExec); err != nil {
			t.Fatalf("terminal %d with all log streams open: %v", i+1, err)
		}
		drain(a)
	}
	_, _, _, err := a.Exec(ctx, testExec)
	if !errors.Is(err, ErrTooManyExecs) || !errors.Is(err, ErrTooManyLogs) {
		t.Fatalf("terminal beyond the cap: %v", err)
	}
	for i := range MaxStackStreams {
		if _, err := a.Stack(ctx, wire.StackCommand{Action: wire.StackStop, Name: "web"}); err != nil {
			t.Fatalf("stack operation %d with logs and terminals full: %v", i+1, err)
		}
		drain(a)
	}
	if _, err := a.Stack(ctx, wire.StackCommand{Action: wire.StackStop, Name: "web"}); !errors.Is(err, ErrTooManyStackOps) {
		t.Fatalf("stack operation beyond the cap: %v", err)
	}
	// Closing a log stream frees a log slot, and only that.
	stops[0]()
	drain(a)
	if _, _, err := a.Logs(ctx, wire.LogsRequest{ContainerID: "web"}); err != nil {
		t.Fatalf("log stream after one closed: %v", err)
	}
	if _, _, _, err := a.Exec(ctx, testExec); !errors.Is(err, ErrTooManyExecs) {
		t.Fatalf("terminal slot freed by a log stream: %v", err)
	}
}

// Terminal output is buffered far more than log output, and if even that
// overflows the session ends with an error instead of silently losing bytes.
func TestExecOutputNotSilentlyDropped(t *testing.T) {
	r := New()
	a := r.Attach("h1", "host-1", func() {})
	ctx := context.Background()
	id, ch, _, err := a.Exec(ctx, testExec)
	if err != nil {
		t.Fatal(err)
	}
	drain(a)
	// A burst well beyond what a log stream would keep all arrives.
	const burst = 4 * streamBuffer
	for range burst {
		a.Handle(wire.AgentMessage{Log: &wire.LogChunk{CommandID: id, Data: []byte("x")}}, nil)
	}
	if len(ch) != burst {
		t.Fatalf("%d of %d terminal chunks kept", len(ch), burst)
	}
	for len(ch) < cap(ch) {
		a.Handle(wire.AgentMessage{Log: &wire.LogChunk{CommandID: id, Data: []byte("x")}}, nil)
	}
	a.Handle(wire.AgentMessage{Log: &wire.LogChunk{CommandID: id, Data: []byte("overflow")}}, nil)
	var last wire.LogChunk
	for c := range ch { // closed after the final chunk
		last = c
	}
	if !last.EOF || last.Error != ErrExecOverflow.Error() {
		t.Fatalf("final chunk %+v, want the overflow error", last)
	}
	cancelled := false
	for _, c := range drain(a) {
		cancelled = cancelled || c.Cancel == id
	}
	if !cancelled {
		t.Fatal("the agent was not told to close the terminal")
	}
	// Its slot is free again.
	for range MaxExecStreams {
		if _, _, _, err := a.Exec(ctx, testExec); err != nil {
			t.Fatal(err)
		}
		drain(a)
	}
}

// Disconnecting a host (as on re-enrollment or revocation) also ends the
// transfers it takes part in, whichever side it is on.
func TestDisconnectAbortsTransfers(t *testing.T) {
	r := New()
	r.Attach("h1", "host-1", func() {})
	out := r.newTransfer("h1", "h2", 100)
	in := r.newTransfer("h3", "h1", 100)
	other := r.newTransfer("h2", "h3", 100)
	r.Disconnect("h1")
	for _, tr := range []*Transfer{out, in} {
		select {
		case <-tr.aborted:
		case <-time.After(time.Second):
			t.Fatalf("transfer %s->%s not aborted", tr.Src, tr.Dst)
		}
	}
	select {
	case <-other.aborted:
		t.Fatal("unrelated transfer aborted")
	default:
	}
}
