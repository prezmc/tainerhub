package registry

import (
	"context"
	"testing"
	"time"

	"github.com/prezmc/tainerhub/internal/wire"
)

// A stack's final chunk carries its outcome, so it must reach the consumer
// even when the stream's buffer is full of output nobody has read yet.
func TestStackFinalChunkSurvivesFullBuffer(t *testing.T) {
	r := New()
	a := r.Attach("h1", "host-1", func() {})
	ch, err := a.Stack(context.Background(), wire.StackCommand{Action: wire.StackStop, Name: "web"})
	if err != nil {
		t.Fatal(err)
	}
	var cmd wire.HubMessage
	for cmd.Command == nil { // skip the watching state sent at attach
		select {
		case cmd = <-a.Out():
		case <-time.After(time.Second):
			t.Fatal("command not sent")
		}
	}
	if cmd.Command == nil || cmd.Command.Stack == nil || cmd.Command.Stack.Name != "web" {
		t.Fatalf("unexpected command %+v", cmd)
	}
	id := cmd.Command.ID
	for i := 0; i < 1000; i++ { // far more than the buffer holds
		a.Handle(wire.AgentMessage{Log: &wire.LogChunk{CommandID: id, Data: []byte("line\n")}}, nil)
	}
	a.Handle(wire.AgentMessage{Log: &wire.LogChunk{CommandID: id, EOF: true, Error: "docker compose exited with code 1"}}, nil)

	var last wire.LogChunk
	for c := range ch {
		last = c
	}
	if !last.EOF || last.Error != "docker compose exited with code 1" {
		t.Fatalf("final chunk lost: %+v", last)
	}
}

func TestStackStreamEndsWhenHostDisconnects(t *testing.T) {
	r := New()
	a := r.Attach("h1", "host-1", func() {})
	ch, err := a.Stack(context.Background(), wire.StackCommand{Action: wire.StackStop, Name: "web"})
	if err != nil {
		t.Fatal(err)
	}
	<-a.Out()
	r.Detach(a)
	var last wire.LogChunk
	for c := range ch {
		last = c
	}
	if last.Error != ErrOffline.Error() {
		t.Fatalf("got %+v, want offline error", last)
	}
}

func TestStackRejectsInvalidCommand(t *testing.T) {
	a := New().Attach("h1", "host-1", func() {})
	if _, err := a.Stack(context.Background(), wire.StackCommand{Action: wire.StackStop, Name: "../x"}); err == nil {
		t.Fatal("expected an error")
	}
}

func TestWatchTellsAgentsFirstAndLast(t *testing.T) {
	r := New()
	a := r.Attach("h1", "one", func() {})
	next := func() *bool {
		select {
		case m := <-a.Out():
			return m.Watching
		default:
			return nil
		}
	}
	if w := next(); w == nil || *w {
		t.Fatalf("on attach with nobody watching: want watching=false, got %v", w)
	}
	rel1 := r.Watch()
	if w := next(); w == nil || !*w {
		t.Fatalf("first watcher: want watching=true, got %v", w)
	}
	rel2 := r.Watch()
	if w := next(); w != nil {
		t.Fatalf("second watcher shouldn't notify, got %v", *w)
	}
	rel1()
	rel1() // releasing twice is harmless
	if w := next(); w != nil {
		t.Fatalf("one watcher left shouldn't notify, got %v", *w)
	}
	rel2()
	if w := next(); w == nil || *w {
		t.Fatalf("last watcher gone: want watching=false, got %v", w)
	}
	b := r.Attach("h2", "two", func() {})
	r.Watch()
	if m := <-b.Out(); m.Watching == nil || *m.Watching {
		t.Fatal("new agent should first hear the state at attach")
	}
}
