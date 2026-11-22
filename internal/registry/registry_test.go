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
	select {
	case cmd = <-a.Out():
	case <-time.After(time.Second):
		t.Fatal("command not sent")
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
