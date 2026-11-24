package registry

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/prezmc/tainerhub/internal/wire"
)

// Image update checks are scheduled by the hub rather than by each agent on
// its own, because only the hub has the saved registry logins: a check it
// sends (OpImagesCheckUpdates) goes through Operate, so the AddAuth hook
// attaches the logins for the host's images, and private images get checked
// too. Agents still run their own uncredentialed check as a fallback for
// older hubs, but skip it after a recent check from the hub.
const (
	// The first check runs a couple of minutes after an agent connects,
	// once its first snapshot (which decides the logins sent) is in, spread
	// over another minute so hosts reconnecting after a hub restart don't
	// all ask their registries at once.
	updateCheckFirst  = 2 * time.Minute
	updateCheckSpread = time.Minute
	// updateCheckMinGap keeps a host whose connection flaps from being
	// checked on every reconnect.
	updateCheckMinGap = 30 * time.Minute
	// updateCheckRetry is how soon a check that failed outright (host went
	// offline, Docker didn't answer) is tried again.
	updateCheckRetry = 30 * time.Minute
	// updateCheckParallel bounds how many hosts check at the same time, so
	// a large fleet's checks are staggered.
	updateCheckParallel = 4
	updateCheckTick     = 15 * time.Second
)

// hostCheck is a host's image update check state, kept across reconnects.
type hostCheck struct {
	last    time.Time // when the last check the hub sent finished; zero: none yet
	running int       // checks in flight (scheduled or asked for in the UI)
}

func (r *Registry) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

func (r *Registry) check(hostID string) *hostCheck {
	if r.checks == nil {
		r.checks = map[string]*hostCheck{}
	}
	c := r.checks[hostID]
	if c == nil {
		c = &hostCheck{}
		r.checks[hostID] = c
	}
	return c
}

func (r *Registry) beginImageCheck(hostID string) {
	r.checkMu.Lock()
	r.check(hostID).running++
	r.checkMu.Unlock()
}

func (r *Registry) endImageCheck(hostID string, ok bool) {
	r.checkMu.Lock()
	c := r.check(hostID)
	c.running--
	if ok {
		c.last = r.clock()
	}
	r.checkMu.Unlock()
	if ok {
		r.notify() // browsers' host lists show when it was checked
	}
}

// ImagesCheckedAt is when the hub last finished an image update check on
// the host (zero: not since the hub started).
func (r *Registry) ImagesCheckedAt(hostID string) time.Time {
	r.checkMu.Lock()
	defer r.checkMu.Unlock()
	if c := r.checks[hostID]; c != nil {
		return c.last
	}
	return time.Time{}
}

func (r *Registry) imageCheckRunning(hostID string) bool {
	r.checkMu.Lock()
	defer r.checkMu.Unlock()
	c := r.checks[hostID]
	return c != nil && c.running > 0
}

// UpdateChecker runs each connected host's image update check on a
// schedule: shortly after it connects, then every Every.
type UpdateChecker struct {
	Reg   *Registry
	Every time.Duration // 0 turns scheduled checks off
	Log   *slog.Logger

	jitter func(max time.Duration) time.Duration // nil: random in [0, max)

	mu       sync.Mutex
	plan     map[*Agent]*checkPlan // by connection: a reconnect plans a new first check
	inflight int
	wg       sync.WaitGroup
}

type checkPlan struct {
	due     time.Time
	base    time.Time // the host's last check when due was worked out
	running bool
}

// Run schedules checks until ctx ends.
func (u *UpdateChecker) Run(ctx context.Context) {
	if u.Every <= 0 {
		return
	}
	t := time.NewTicker(updateCheckTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			u.wg.Wait()
			return
		case <-t.C:
			u.tick(ctx)
		}
	}
}

func (u *UpdateChecker) rand(max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}
	if u.jitter != nil {
		return u.jitter(max)
	}
	return rand.N(max)
}

// spread is the random extra delay after each periodic check, so hosts
// checked together drift apart instead of hitting registries in step.
func (u *UpdateChecker) spread() time.Duration { return u.rand(u.Every / 12) }

// tick starts the checks that are due.
func (u *UpdateChecker) tick(ctx context.Context) {
	now := u.Reg.clock()
	agents := u.Reg.All()
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.plan == nil {
		u.plan = map[*Agent]*checkPlan{}
	}
	live := make(map[*Agent]bool, len(agents))
	for _, a := range agents {
		live[a] = true
		last := u.Reg.ImagesCheckedAt(a.HostID)
		p := u.plan[a]
		switch {
		case p == nil:
			p = &checkPlan{due: now.Add(updateCheckFirst + u.rand(updateCheckSpread)), base: last}
			if !last.IsZero() && p.due.Before(last.Add(updateCheckMinGap)) {
				p.due = last.Add(updateCheckMinGap + u.rand(updateCheckSpread))
			}
			u.plan[a] = p
		case !p.running && last.After(p.base):
			// A check finished (ours, or one asked for in the UI): the
			// next is a full period after it.
			p.due, p.base = last.Add(u.Every+u.spread()), last
		}
		if p.running || now.Before(p.due) || u.inflight >= updateCheckParallel {
			continue
		}
		select {
		case <-a.Done():
			continue // went offline since All()
		default:
		}
		if u.Reg.imageCheckRunning(a.HostID) {
			continue // one is already running (asked for in the UI): don't stack another
		}
		p.running = true
		u.inflight++
		u.wg.Add(1)
		go u.run(ctx, a, p)
	}
	for a := range u.plan {
		if !live[a] {
			delete(u.plan, a) // disconnected: a reconnect starts over
		}
	}
}

func (u *UpdateChecker) run(ctx context.Context, a *Agent, p *checkPlan) {
	defer u.wg.Done()
	start := u.Reg.clock()
	_, err := a.Operate(ctx, wire.Operation{Kind: wire.OpImagesCheckUpdates})
	if u.Log != nil {
		u.Log.Debug("scheduled image update check", "host", a.Name, "took", u.Reg.clock().Sub(start).Round(time.Millisecond), "err", err)
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	u.inflight--
	p.running = false
	last := u.Reg.ImagesCheckedAt(a.HostID)
	if err != nil || !last.After(p.base) {
		p.due = u.Reg.clock().Add(min(updateCheckRetry, u.Every))
		return
	}
	p.due, p.base = last.Add(u.Every+u.spread()), last
}
