package registry

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/prezmc/tainerhub/internal/wire"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

// checkHost is a connected agent that answers update checks: at once, or
// when hold is closed.
type checkHost struct {
	a    *Agent
	cmds chan wire.Command
	hold chan struct{}
}

func connectCheckHost(t *testing.T, r *Registry, id string, hold chan struct{}) *checkHost {
	t.Helper()
	h := &checkHost{a: r.Attach(id, id, func() {}), cmds: make(chan wire.Command, 16), hold: hold}
	go func() {
		for {
			select {
			case <-h.a.Done():
				return
			case m := <-h.a.Out():
				if m.Command == nil || m.Command.Op == nil {
					continue
				}
				h.cmds <- *m.Command
				go func(id string) {
					if h.hold != nil {
						<-h.hold
					}
					h.a.Handle(wire.AgentMessage{Result: &wire.CommandResult{CommandID: id, OK: true, Data: []byte(`{"checked":1,"images":{}}`)}}, nil)
				}(m.Command.ID)
			}
		}
	}()
	return h
}

// sent returns how many check commands arrived (waiting briefly for ones
// started by the last tick).
func (h *checkHost) sent(t *testing.T, want int) {
	t.Helper()
	got := 0
	timeout := time.After(200 * time.Millisecond)
	for got < want {
		select {
		case c := <-h.cmds:
			if c.Op.Kind != wire.OpImagesCheckUpdates {
				t.Fatalf("unexpected op %q", c.Op.Kind)
			}
			got++
		case <-timeout:
			t.Fatalf("%s: got %d checks, want %d", h.a.HostID, got, want)
		}
	}
	select {
	case <-h.cmds:
		t.Fatalf("%s: more than %d checks", h.a.HostID, want)
	case <-time.After(20 * time.Millisecond):
	}
}

func newCheckRig(every time.Duration) (*Registry, *UpdateChecker, *fakeClock, time.Time) {
	t0 := time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC)
	clk := &fakeClock{t: t0}
	r := New()
	r.now = clk.Now
	// Half the allowed spread, so timings are exact.
	u := &UpdateChecker{Reg: r, Every: every, jitter: func(max time.Duration) time.Duration { return max / 2 }}
	return r, u, clk, t0
}

func TestUpdateCheckFirstAfterConnectThenPeriodic(t *testing.T) {
	r, u, clk, t0 := newCheckRig(6 * time.Hour)
	ctx := context.Background()
	h := connectCheckHost(t, r, "h1", nil)

	u.tick(ctx) // plans the first check: 2 min + half of 1 min after connect
	first := t0.Add(2*time.Minute + 30*time.Second)
	clk.Set(first.Add(-time.Second))
	u.tick(ctx)
	h.sent(t, 0)
	clk.Set(first)
	u.tick(ctx)
	u.wg.Wait()
	h.sent(t, 1)
	if got := r.ImagesCheckedAt("h1"); !got.Equal(first) {
		t.Fatalf("checked at %v, want %v", got, first)
	}

	// Next: 6 h after it finished, plus half of the 30 min spread.
	next := first.Add(6*time.Hour + 15*time.Minute)
	clk.Set(next.Add(-time.Second))
	u.tick(ctx)
	h.sent(t, 0)
	clk.Set(next)
	u.tick(ctx)
	u.wg.Wait()
	h.sent(t, 1)
}

func TestUpdateCheckJitterBounds(t *testing.T) {
	r, u, clk, t0 := newCheckRig(6 * time.Hour)
	u.jitter = nil // real randomness
	ctx := context.Background()
	var hosts []*checkHost
	for _, id := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"} {
		hosts = append(hosts, connectCheckHost(t, r, id, nil))
	}
	u.tick(ctx)
	dues := map[time.Time]bool{}
	for _, h := range hosts {
		d := u.plan[h.a].due
		if d.Before(t0.Add(2*time.Minute)) || !d.Before(t0.Add(3*time.Minute)) {
			t.Fatalf("first check at +%v, want within [2m, 3m)", d.Sub(t0))
		}
		dues[d] = true
	}
	if len(dues) < 2 {
		t.Fatal("hosts connecting together must not all check at the same moment")
	}
	// Run them all (at most updateCheckParallel per tick).
	clk.Set(t0.Add(3 * time.Minute))
	for i := 0; i < len(hosts); i++ {
		u.tick(ctx)
		u.wg.Wait()
	}
	last := t0.Add(3 * time.Minute)
	for _, h := range hosts {
		h.sent(t, 1)
		d := u.plan[h.a].due
		if d.Before(last.Add(6*time.Hour)) || !d.Before(last.Add(6*time.Hour+30*time.Minute)) {
			t.Fatalf("next check at +%v after the last, want within [6h, 6h30m)", d.Sub(last))
		}
	}
}

func TestUpdateCheckNoOverlap(t *testing.T) {
	r, u, clk, t0 := newCheckRig(time.Hour)
	ctx := context.Background()
	hold := make(chan struct{})
	h := connectCheckHost(t, r, "h1", hold)
	u.tick(ctx)
	clk.Set(t0.Add(10 * time.Minute))
	u.tick(ctx)
	h.sent(t, 1)
	// Still running hours later: no second check stacked on it.
	clk.Set(t0.Add(5 * time.Hour))
	u.tick(ctx)
	h.sent(t, 0)
	close(hold)
	u.wg.Wait()

	// A check asked for in the UI is running when one is due: skip it.
	hold2 := make(chan struct{})
	h2 := connectCheckHost(t, r, "h2", hold2)
	u.tick(ctx) // plan h2
	done := make(chan struct{})
	go func() {
		_, _ = h2.a.Operate(ctx, wire.Operation{Kind: wire.OpImagesCheckUpdates})
		close(done)
	}()
	h2.sent(t, 1)
	clk.Set(t0.Add(6 * time.Hour))
	u.tick(ctx)
	h2.sent(t, 0)
	close(hold2)
	<-done
	u.wg.Wait()
}

func TestUpdateCheckManualCheckPushesScheduleOut(t *testing.T) {
	r, u, clk, t0 := newCheckRig(6 * time.Hour)
	ctx := context.Background()
	h := connectCheckHost(t, r, "h1", nil)
	u.tick(ctx)
	clk.Set(t0.Add(time.Minute))
	if _, err := h.a.Operate(ctx, wire.Operation{Kind: wire.OpImagesCheckUpdates}); err != nil {
		t.Fatal(err)
	}
	h.sent(t, 1)
	clk.Set(t0.Add(3 * time.Minute)) // when the first scheduled check would have run
	u.tick(ctx)
	h.sent(t, 0)
	clk.Set(t0.Add(time.Minute + 6*time.Hour + 15*time.Minute))
	u.tick(ctx)
	u.wg.Wait()
	h.sent(t, 1)
}

func TestUpdateCheckSkipsOfflineHosts(t *testing.T) {
	r, u, clk, t0 := newCheckRig(6 * time.Hour)
	ctx := context.Background()
	h := connectCheckHost(t, r, "h1", nil)
	u.tick(ctx)
	r.Detach(h.a)
	clk.Set(t0.Add(time.Hour))
	u.tick(ctx)
	u.wg.Wait()
	h.sent(t, 0)
	if len(u.plan) != 0 {
		t.Fatal("a disconnected host's plan must be dropped")
	}

	// Reconnecting soon after a check waits out the minimum gap instead
	// of checking again on every reconnect.
	h2 := connectCheckHost(t, r, "h2", nil)
	u.tick(ctx)
	clk.Set(t0.Add(time.Hour + 3*time.Minute))
	u.tick(ctx)
	u.wg.Wait()
	h2.sent(t, 1)
	checked := r.ImagesCheckedAt("h2")
	r.Detach(h2.a)
	h3 := connectCheckHost(t, r, "h2", nil)
	u.tick(ctx)
	clk.Set(checked.Add(updateCheckMinGap - time.Second))
	u.tick(ctx)
	h3.sent(t, 0)
	clk.Set(checked.Add(updateCheckMinGap + updateCheckSpread))
	u.tick(ctx)
	u.wg.Wait()
	h3.sent(t, 1)
}

func TestUpdateCheckParallelLimit(t *testing.T) {
	r, u, clk, t0 := newCheckRig(6 * time.Hour)
	ctx := context.Background()
	hold := make(chan struct{})
	var hosts []*checkHost
	for _, id := range []string{"a", "b", "c", "d", "e", "f"} {
		hosts = append(hosts, connectCheckHost(t, r, id, hold))
	}
	u.tick(ctx)
	clk.Set(t0.Add(time.Hour))
	u.tick(ctx)
	n := 0
	for _, h := range hosts {
		select {
		case <-h.cmds:
			n++
		case <-time.After(100 * time.Millisecond):
		}
	}
	if n != updateCheckParallel {
		t.Fatalf("%d checks at once, want %d", n, updateCheckParallel)
	}
	close(hold)
	u.wg.Wait()
	u.tick(ctx) // the rest go now
	u.wg.Wait()
	for _, h := range hosts {
		select {
		case <-h.cmds:
			n++
		default:
		}
	}
	if n != len(hosts) {
		t.Fatalf("%d hosts checked, want %d", n, len(hosts))
	}
}

func TestUpdateCheckOffWhenZero(t *testing.T) {
	done := make(chan struct{})
	go func() {
		(&UpdateChecker{Reg: New()}).Run(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run with Every 0 must return straight away")
	}
}

// Scheduled checks go through Operate, so the AddAuth hook attaches the
// saved registry logins, just as for "Check for updates".
func TestUpdateCheckCarriesLogins(t *testing.T) {
	r, u, clk, t0 := newCheckRig(6 * time.Hour)
	r.AddAuth = func(_ context.Context, a *Agent, cmd *wire.Command) {
		if cmd.Op != nil && cmd.Op.Kind == wire.OpImagesCheckUpdates {
			cmd.Op.Auth = []wire.RegistryAuth{{Registry: "ghcr.io", Username: "octo", Password: "pat"}}
		}
	}
	ctx := context.Background()
	h := connectCheckHost(t, r, "h1", nil)
	u.tick(ctx)
	clk.Set(t0.Add(time.Hour))
	u.tick(ctx)
	u.wg.Wait()
	select {
	case c := <-h.cmds:
		if len(c.Op.Auth) != 1 || c.Op.Auth[0].Registry != "ghcr.io" {
			t.Fatalf("scheduled check carried %v", c.Op.Auth)
		}
	case <-time.After(time.Second):
		t.Fatal("no check sent")
	}
}
