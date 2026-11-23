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
					frame, next := f.current()
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
