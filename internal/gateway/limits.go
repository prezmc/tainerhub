package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"errors"
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

	// Connections one IP address may hold open after a handshake without a
	// client certificate. Generous, because many agents may share a NAT
	// address and enroll together.
	maxUnauthConnsPerIP = 32

	// certlessGrace is how long a connection without a client certificate
	// may stay open unless it is enrolling. Enrolling agents send their
	// request straight away; nothing else works without a certificate.
	certlessGrace = 5 * time.Second

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

// connCap limits the connections each IP address holds open after a TLS
// handshake in which it showed no client certificate (enrolling agents, or
// anyone else), and closes each of those within its grace period unless it
// is busy enrolling. Connections are only counted once their handshake is
// done, so strangers sharing a NAT address with agents can't use up the
// allowance before the agents have shown their certificates, and agents
// with a certificate are never counted at all.
type connCap struct {
	max   int
	grace time.Duration

	mu    sync.Mutex
	perIP map[string]int
	conns map[net.Conn]*certless // keyed by the raw connection under TLS
}

// certless is one counted connection.
type certless struct {
	conn      net.Conn
	ip        string
	timer     *time.Timer
	enrolling int // enrollment requests being served on it
}

func newConnCap(max int, grace time.Duration) *connCap {
	return &connCap{max: max, grace: grace, perIP: map[string]int{}, conns: map[net.Conn]*certless{}}
}

var errTooManyConns = errors.New("too many connections without a client certificate from this address")

// tlsConfig wraps base so each connection's handshake reports to the cap.
func (c *connCap) tlsConfig(base *tls.Config) *tls.Config {
	cfg := base.Clone()
	cfg.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		raw := hello.Conn
		per := base.Clone()
		verify := per.VerifyConnection
		per.VerifyConnection = func(cs tls.ConnectionState) error {
			if verify != nil {
				if err := verify(cs); err != nil {
					return err
				}
			}
			// With a certificate the handshake goes on to prove the client
			// holds its key, or fails; either way it isn't counted.
			if len(cs.PeerCertificates) > 0 {
				return nil
			}
			return c.admit(raw)
		}
		return per, nil
	}
	return cfg
}

// admit counts a connection that finished its handshake without a client
// certificate, refusing it if its address already has too many.
func (c *connCap) admit(conn net.Conn) error {
	ip := addrIP(conn.RemoteAddr())
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.conns[conn]; ok {
		return nil
	}
	if c.perIP[ip] >= c.max {
		return errTooManyConns
	}
	c.perIP[ip]++
	cl := &certless{conn: conn, ip: ip}
	cl.timer = time.AfterFunc(c.grace, func() { c.expire(cl) })
	c.conns[conn] = cl
	return nil
}

// expire closes a certificate-less connection whose time is up, unless it
// is enrolling right now; then it gets another grace period.
func (c *connCap) expire(cl *certless) {
	c.mu.Lock()
	if c.conns[cl.conn] != cl {
		c.mu.Unlock()
		return
	}
	if cl.enrolling > 0 {
		cl.timer.Reset(c.grace)
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()
	_ = cl.conn.Close() // the server notices and reports it closed
}

// enrolling marks the request's connection as serving an enrollment until
// the returned func is called, so it isn't closed part way through.
func (c *connCap) enrolling(ctx context.Context) (done func()) {
	conn, _ := ctx.Value(rawConnKey{}).(net.Conn)
	if c == nil || conn == nil {
		return func() {}
	}
	c.mu.Lock()
	cl := c.conns[conn]
	if cl != nil {
		cl.enrolling++
	}
	c.mu.Unlock()
	return func() {
		if cl == nil {
			return
		}
		c.mu.Lock()
		cl.enrolling--
		c.mu.Unlock()
	}
}

// rawConnKey holds the request's raw connection in its context.
type rawConnKey struct{}

// connContext is an http.Server ConnContext hook recording the raw
// connection, which is what the cap knows connections by.
func (c *connCap) connContext(ctx context.Context, conn net.Conn) context.Context {
	return context.WithValue(ctx, rawConnKey{}, rawConn(conn))
}

func rawConn(conn net.Conn) net.Conn {
	if tc, ok := conn.(*tls.Conn); ok {
		return tc.NetConn()
	}
	return conn
}

// state is an http.Server ConnState hook that forgets closed connections.
func (c *connCap) state(conn net.Conn, s http.ConnState) {
	if s == http.StateClosed || s == http.StateHijacked {
		c.release(rawConn(conn))
	}
}

func (c *connCap) release(conn net.Conn) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cl, ok := c.conns[conn]
	if !ok {
		return
	}
	cl.timer.Stop()
	delete(c.conns, conn)
	if c.perIP[cl.ip]--; c.perIP[cl.ip] <= 0 {
		delete(c.perIP, cl.ip)
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
