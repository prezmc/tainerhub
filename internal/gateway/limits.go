package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/prezmc/tainerhub/internal/wire"
)

// The gateway listens on the internet in many setups, so everything a
// stranger (or a compromised agent) can make it do is bounded here.
const (
	// Failed enrollments allowed per IP address in enrollWindow. Successes
	// are not counted, so enrolling a whole fleet behind one NAT works.
	enrollFailures = 10
	enrollWindow   = 15 * time.Minute

	// Connections one IP address may hold open before showing a client
	// certificate. Generous, because many agents may share a NAT address
	// and all reconnect at once after a hub restart.
	maxUnauthConnsPerIP = 32

	// bodyTimeout bounds reading an enroll or renew request body, which is
	// a few KiB: a client trickling it in a byte at a time is dropped.
	bodyTimeout = 10 * time.Second

	// Per-agent message rates. Normal agents send a snapshot every 10-30s
	// (a few close together around events) and Docker events in bursts of
	// a few per container during compose up.
	eventBurst    = 100
	eventsPerSec  = 20
	snapshotEvery = 500 * time.Millisecond // at most 2 snapshots/s are applied
)

// failLimiter counts failures per IP address in fixed windows.
type failLimiter struct {
	max    int
	window time.Duration

	mu sync.Mutex
	m  map[string]*failWindow
}

type failWindow struct {
	n     int
	until time.Time
}

func newFailLimiter(max int, window time.Duration) *failLimiter {
	return &failLimiter{max: max, window: window, m: map[string]*failWindow{}}
}

// blocked reports whether ip has used up its failures for now.
func (l *failLimiter) blocked(ip string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	w := l.m[ip]
	return w != nil && now.Before(w.until) && w.n >= l.max
}

func (l *failLimiter) fail(ip string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.m) > 10000 { // forget finished windows so the map can't grow forever
		for k, w := range l.m {
			if !now.Before(w.until) {
				delete(l.m, k)
			}
		}
	}
	w := l.m[ip]
	if w == nil || !now.Before(w.until) {
		w = &failWindow{until: now.Add(l.window)}
		l.m[ip] = w
	}
	w.n++
}

// connCap limits the connections each IP address holds open without having
// presented a client certificate signed by our CA (enrolling agents, or
// anyone else). It works as an http.Server ConnState hook. Agents with a
// certificate are let through regardless, once their first request arrives.
type connCap struct {
	max int

	mu      sync.Mutex
	perIP   map[string]int
	counted map[net.Conn]string
}

func newConnCap(max int) *connCap {
	return &connCap{max: max, perIP: map[string]int{}, counted: map[net.Conn]string{}}
}

func (c *connCap) state(conn net.Conn, s http.ConnState) {
	switch s {
	case http.StateNew:
		ip := addrIP(conn.RemoteAddr())
		c.mu.Lock()
		if c.perIP[ip] >= c.max {
			c.mu.Unlock()
			_ = conn.Close() // the server sees the closed conn and moves on
			return
		}
		c.perIP[ip]++
		c.counted[conn] = ip
		c.mu.Unlock()
	case http.StateActive:
		// The TLS handshake is done by now, and the handshake has already
		// checked any client certificate against our CA.
		if tc, ok := conn.(*tls.Conn); ok && len(tc.ConnectionState().PeerCertificates) > 0 {
			c.release(conn)
		}
	case http.StateClosed, http.StateHijacked:
		c.release(conn)
	}
}

func (c *connCap) release(conn net.Conn) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ip, ok := c.counted[conn]
	if !ok {
		return
	}
	delete(c.counted, conn)
	if c.perIP[ip]--; c.perIP[ip] <= 0 {
		delete(c.perIP, ip)
	}
}

func addrIP(a net.Addr) string {
	if a == nil {
		return ""
	}
	if h, _, err := net.SplitHostPort(a.String()); err == nil {
		return h
	}
	return a.String()
}

// tokenBucket allows burst messages at once and rate per second after that.
type tokenBucket struct {
	burst, rate float64
	tokens      float64
	last        time.Time
}

func newTokenBucket(burst int, rate float64, now time.Time) *tokenBucket {
	return &tokenBucket{burst: float64(burst), rate: rate, tokens: float64(burst), last: now}
}

func (b *tokenBucket) allow(now time.Time) bool {
	b.tokens = min(b.burst, b.tokens+now.Sub(b.last).Seconds()*b.rate)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// snapshotPrefix is how every snapshot message from our agents starts
// (json.Marshal of a wire.AgentMessage with only Snapshot set).
var snapshotPrefix = []byte(`{"snapshot":`)

// snapshotGate applies an agent's snapshots at most once per interval and
// keeps only the latest one in between. Snapshots can be up to 8 MiB, so the
// read loop hands over the raw bytes and decoding happens here, at the
// limited rate, rather than for every message received.
type snapshotGate struct {
	mu   sync.Mutex
	raw  []byte         // latest undecoded snapshot message, or
	snap *wire.Snapshot // latest snapshot decoded by the read loop
	sum  [32]byte
	wake chan struct{}
}

func newSnapshotGate() *snapshotGate { return &snapshotGate{wake: make(chan struct{}, 1)} }

// putRaw queues a raw snapshot message, replacing any not yet applied.
// raw must not be reused by the caller.
func (g *snapshotGate) putRaw(raw []byte) {
	g.mu.Lock()
	g.raw, g.snap = raw, nil
	g.mu.Unlock()
	g.signal()
}

// put queues an already decoded snapshot (one that did not look like our
// agents' usual encoding), replacing any not yet applied.
func (g *snapshotGate) put(s *wire.Snapshot, sum [32]byte) {
	g.mu.Lock()
	g.raw, g.snap, g.sum = nil, s, sum
	g.mu.Unlock()
	g.signal()
}

func (g *snapshotGate) signal() {
	select {
	case g.wake <- struct{}{}:
	default:
	}
}

// run applies queued snapshots until ctx ends, at most one per every.
func (g *snapshotGate) run(ctx context.Context, every time.Duration, apply func(*wire.Snapshot, [32]byte)) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-g.wake:
		}
		g.mu.Lock()
		raw, snap, sum := g.raw, g.snap, g.sum
		g.raw, g.snap = nil, nil
		g.mu.Unlock()
		if raw != nil {
			var m wire.AgentMessage
			if json.Unmarshal(raw, &m) == nil && m.Snapshot != nil {
				snap, sum = m.Snapshot, containersSum(raw)
			}
		}
		if snap == nil {
			continue
		}
		apply(snap, sum)
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}

// containersSum hashes a snapshot message from its container list onwards,
// leaving out the timestamp that comes first, so two snapshots of the same
// containers hash the same. Only used to skip needless UI refreshes: a
// strangely encoded message at worst refreshes the UI when not needed.
func containersSum(raw []byte) [32]byte {
	if i := bytes.Index(raw, []byte(`"containers":`)); i >= 0 {
		raw = raw[i:]
	}
	return sha256.Sum256(raw)
}
