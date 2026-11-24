package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/prezmc/tainerhub/internal/registry"
	"github.com/prezmc/tainerhub/internal/store"
)

// fleetFeed builds the /api/events fleet payload once per change and hands
// the same bytes to every open stream, instead of each browser tab querying
// Postgres and encoding the whole fleet itself. The host rows are cached and
// only re-read when they may have changed.
type fleetFeed struct {
	reg      *registry.Registry
	list     func(context.Context) ([]store.Host, error) // host rows (Postgres)
	build    func([]store.Host) any                      // payload from the host rows
	debounce time.Duration                               // coalesces bursts of changes
	maxAge   time.Duration                               // safety net: re-read hosts this often

	mu     sync.Mutex
	subs   int
	stop   context.CancelFunc
	frame  []byte        // latest SSE message; never modified once published
	next   chan struct{} // closed when frame is replaced
	stale  bool          // host rows changed since they were read
	poke   chan struct{} // wakes the producer after a host change
	hosts  []store.Host
	readAt time.Time
	online []string // connected host IDs when hosts were read
}

const (
	fleetDebounce = 250 * time.Millisecond
	fleetMaxAge   = 30 * time.Second
	fleetRetry    = 2 * time.Second
)

func newFleetFeed(reg *registry.Registry, list func(context.Context) ([]store.Host, error), build func([]store.Host) any) *fleetFeed {
	return &fleetFeed{reg: reg, list: list, build: build, debounce: fleetDebounce, maxAge: fleetMaxAge,
		next: make(chan struct{}), poke: make(chan struct{}, 1)}
}

// subscribe starts the producer for the first stream and stops it after the
// last one leaves.
func (f *fleetFeed) subscribe() (release func()) {
	f.mu.Lock()
	f.subs++
	if f.subs == 1 {
		ctx, cancel := context.WithCancel(context.Background())
		f.stop = cancel
		f.frame = nil // may be old; streams wait for a fresh one
		go f.run(ctx)
	}
	f.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			f.mu.Lock()
			f.subs--
			if f.subs == 0 {
				f.stop()
			}
			f.mu.Unlock()
		})
	}
}

// current returns the latest message (nil before the first one) and a
// channel that closes when a newer one is ready.
func (f *fleetFeed) current() ([]byte, <-chan struct{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.frame, f.next
}

// hostsChanged marks the cached host rows out of date (a host was added,
// renamed, removed or touched by its agent) and pushes a fresh update.
func (f *fleetFeed) hostsChanged() {
	f.markStale()
	select {
	case f.poke <- struct{}{}:
	default:
	}
}

func (f *fleetFeed) markStale() {
	f.mu.Lock()
	f.stale = true
	f.mu.Unlock()
}

func (f *fleetFeed) run(ctx context.Context) {
	for {
		changed := f.reg.Changed() // subscribe before reading, so no change is missed
		wait := f.maxAge
		if !f.publish(ctx) {
			wait = fleetRetry
		}
		select {
		case <-ctx.Done():
			return
		case <-changed:
		case <-f.poke:
		case <-time.After(wait):
			// The loop is about to publish anyway; poking as well would
			// queue a second, back-to-back publish.
			f.markStale()
		}
		// Coalesce bursts (e.g. a compose stack starting) into one update.
		select {
		case <-ctx.Done():
			return
		case <-time.After(f.debounce):
		}
	}
}

// publish builds and encodes one update. It reports false when the host
// rows couldn't be read and nothing was published.
func (f *fleetFeed) publish(ctx context.Context) bool {
	hosts, err := f.hostRows(ctx)
	if err != nil {
		return false
	}
	b, err := json.Marshal(f.build(hosts))
	if err != nil {
		return false
	}
	frame := fmt.Appendf(nil, "event: fleet\ndata: %s\n\n", b)
	f.mu.Lock()
	// Nothing changed (e.g. the periodic re-read): don't wake every stream to
	// resend the same bytes. A new first subscriber clears frame, so it
	// always gets one; later subscribers read the current frame directly.
	if bytes.Equal(frame, f.frame) {
		f.mu.Unlock()
		return true
	}
	f.frame = frame
	close(f.next)
	f.next = make(chan struct{})
	f.mu.Unlock()
	return true
}

// hostRows returns the cached host rows, re-reading them when they're
// marked stale, too old, or an agent connected or disconnected (its
// last-seen time and info live in the row).
func (f *fleetFeed) hostRows(ctx context.Context) ([]store.Host, error) {
	online := make([]string, 0, 8)
	for _, a := range f.reg.All() {
		online = append(online, a.HostID)
	}
	slices.Sort(online)

	f.mu.Lock()
	fresh := !f.stale && f.hosts != nil && time.Since(f.readAt) < f.maxAge && slices.Equal(online, f.online)
	hosts := f.hosts
	f.stale = false // a change after this point marks it stale again
	f.mu.Unlock()
	if fresh {
		return hosts, nil
	}

	qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	hosts, err := f.list(qctx)
	if err != nil {
		f.mu.Lock()
		f.stale = true
		cached := f.hosts
		f.mu.Unlock()
		if cached != nil {
			return cached, nil // better slightly old rows than no update
		}
		return nil, err
	}
	if hosts == nil {
		hosts = []store.Host{} // non-nil marks the cache as filled
	}
	f.mu.Lock()
	f.hosts, f.readAt, f.online = hosts, time.Now(), online
	f.mu.Unlock()
	return hosts, nil
}

// fleetFeed returns the server's shared fleet producer.
func (s *Server) fleetFeed() *fleetFeed {
	s.fleetOnce.Do(func() {
		s.fleet = newFleetFeed(s.Reg, s.Store.ListHosts, func(hs []store.Host) any {
			return map[string]any{"hosts": s.hostViewsFrom(hs), "containers": s.containerViews(), "activity": s.Reg.Activity(40)}
		})
	})
	return s.fleet
}

// HostsChanged tells open fleet streams that host rows changed outside the
// API handlers (e.g. an agent's hello updated its info and last-seen time).
func (s *Server) HostsChanged() { s.fleetFeed().hostsChanged() }
