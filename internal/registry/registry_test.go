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
		for {
			select {
			case m := <-a.Out():
				if m.StatsFromDocker != nil {
					continue // sent at attach too
				}
				return m.Watching
			default:
				return nil
			}
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

func TestStatsFromDockerTellsAgentNowAndAtAttach(t *testing.T) {
	r := New()
	a := r.Attach("h1", "one", func() {})
	statsMsg := func(a *Agent) *bool {
		for {
			select {
			case m := <-a.Out():
				if m.StatsFromDocker != nil {
					return m.StatsFromDocker
				}
			default:
				return nil
			}
		}
	}
	if v := statsMsg(a); v == nil || *v {
		t.Fatalf("attach: want stats_from_docker=false, got %v", v)
	}
	r.SetStatsFromDocker("h1", true)
	if v := statsMsg(a); v == nil || !*v {
		t.Fatalf("switch: want true, got %v", v)
	}
	r.SetStatsFromDocker("h2", true) // offline host: remembered
	if got := r.StatsFromDockerHosts(); len(got) != 2 || got[0] != "h1" || got[1] != "h2" {
		t.Fatalf("hosts = %v", got)
	}
	b := r.Attach("h2", "two", func() {})
	if v := statsMsg(b); v == nil || !*v {
		t.Fatalf("reconnect: want true, got %v", v)
	}
	r.SetStatsFromDocker("h1", false)
	if r.StatsFromDocker("h1") || len(r.StatsFromDockerHosts()) != 1 {
		t.Fatal("switching back didn't stick")
	}
}

// Snapshots with the same container-list hash don't wake the UI; a new hash
// does, and so does the first snapshot (nothing to compare with yet).
func TestHandleSnapshotComparesHashes(t *testing.T) {
	r := New()
	a := r.Attach("h1", "host-1", func() {})
	woke := func(f func()) bool {
		ch := r.Changed()
		f()
		select {
		case <-ch:
			return true
		default:
			return false
		}
	}
	cs := []wire.Container{{ID: "c1", Name: "web"}}
	if !woke(func() { a.HandleSnapshot(&wire.Snapshot{At: 1, Containers: cs}, [32]byte{1}) }) {
		t.Fatal("first snapshot did not notify")
	}
	if woke(func() { a.HandleSnapshot(&wire.Snapshot{At: 2, Containers: cs}, [32]byte{1}) }) {
		t.Fatal("unchanged snapshot notified")
	}
	if !woke(func() { a.HandleSnapshot(&wire.Snapshot{At: 3, Containers: nil}, [32]byte{2}) }) {
		t.Fatal("changed snapshot did not notify")
	}
	if st := a.Status(); len(st.Containers) != 0 || st.SnapshotAt.Unix() != 3 {
		t.Fatalf("status %+v", st)
	}
}
