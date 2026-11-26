package api

import (
	"bytes"
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prezmc/tainerhub/internal/registry"
	"github.com/prezmc/tainerhub/internal/store"
)

// N open streams cost one host query and one encode per change, and all of
// them get the very same bytes.
func TestFleetFeedSharedAcrossSubscribers(t *testing.T) {
	reg := registry.New()
	var lists, builds atomic.Int32
	f := newFleetFeed(reg,
		func(context.Context) ([]store.Host, error) {
			lists.Add(1)
			return []store.Host{{ID: "h1", Name: "one"}}, nil
		},
		func(hs []store.Host) any {
			n := builds.Add(1)
			return map[string]any{"hosts": hs, "n": n}
		})
	f.debounce = time.Millisecond

	const n = 5
	type sub struct {
		release func()
		last    []byte
	}
	subs := make([]*sub, n)
	for i := range subs {
		subs[i] = &sub{release: f.subscribe()}
	}
	defer func() {
		for _, s := range subs {
			s.release()
		}
	}()

	// awaitAll waits until every subscriber sees a frame newer than its last
	// and checks they all see the same bytes.
	awaitAll := func(step string) {
		t.Helper()
		var wg sync.WaitGroup
		got := make([][]byte, n)
		for i, s := range subs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				deadline := time.After(5 * time.Second)
				for {
					frame, _, next := f.current()
					if frame != nil && !bytes.Equal(frame, s.last) {
						s.last, got[i] = frame, frame
						return
					}
					select {
					case <-next:
					case <-deadline:
						return
					}
				}
			}()
		}
		wg.Wait()
		for i := range got {
			if got[i] == nil {
				t.Fatalf("%s: subscriber %d got no update", step, i)
			}
			if &got[i][0] != &got[0][0] {
				t.Fatalf("%s: subscriber %d got a separately encoded frame", step, i)
			}
		}
	}
	check := func(step string, wantLists, wantBuilds int32) {
		t.Helper()
		time.Sleep(20 * time.Millisecond) // let any extra (unwanted) work show up
		if l, b := lists.Load(), builds.Load(); l != wantLists || b != wantBuilds {
			t.Fatalf("%s: ListHosts=%d encodes=%d, want %d and %d", step, l, b, wantLists, wantBuilds)
		}
	}

	awaitAll("first")
	check("first", 1, 1)

	// A container change: re-encode once, no new host query.
	reg.AddActivity(registry.Activity{Kind: "container", Action: "start"})
	awaitAll("change")
	check("change", 1, 2)

	// A host row changed: one query and one encode.
	f.hostsChanged()
	awaitAll("host change")
	check("host change", 2, 3)

	// An agent connecting changes the online set, so the rows are re-read.
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	reg.Attach("h1", "one", cancel)
	awaitAll("connect")
	check("connect", 3, 4)
}

// The periodic safety-net re-read publishes at most once per period, and
// only when the payload actually changed.
func TestFleetFeedPeriodicRefreshSkipsIdenticalFrames(t *testing.T) {
	reg := registry.New()
	var lists atomic.Int32
	f := newFleetFeed(reg,
		func(context.Context) ([]store.Host, error) {
			lists.Add(1)
			return []store.Host{{ID: "h1", Name: "one"}}, nil
		},
		func(hs []store.Host) any { return map[string]any{"hosts": hs} })
	f.debounce = time.Millisecond
	f.maxAge = 40 * time.Millisecond

	release := f.subscribe()
	defer release()
	deadline := time.After(5 * time.Second)
	first, _, next := f.current()
	for first == nil {
		select {
		case <-next:
		case <-deadline:
			t.Fatal("no first frame")
		}
		first, _, next = f.current()
	}

	time.Sleep(10 * f.maxAge) // ~10 periodic re-reads
	if frame, _, _ := f.current(); &frame[0] != &first[0] {
		t.Fatal("an identical payload was republished")
	}
	select {
	case <-next:
		t.Fatal("streams were woken for an unchanged frame")
	default:
	}
	// One host read per period, not two (the timeout used to poke as well).
	if l := lists.Load(); l > 15 || l < 3 {
		t.Fatalf("ListHosts=%d over ~10 periods", l)
	}

	// A second subscriber joining a running producer gets the current frame
	// right away.
	release2 := f.subscribe()
	defer release2()
	if frame, _, _ := f.current(); frame == nil {
		t.Fatal("new subscriber got no frame")
	}
}
