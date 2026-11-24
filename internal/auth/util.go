package auth

import (
	"container/list"
	"crypto/rand"
	"crypto/subtle"
	"math/big"
	"net/netip"
	"strings"
	"sync"
	"time"
	"unicode"
)

func subtleEq(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

// validUsername accepts 2-32 ASCII letters, digits, dots, dashes and
// underscores. ASCII only: Unicode letters allow look-alike names (Cyrillic
// "а" for Latin "a") that could pass for another user in the UI and audit
// log. It checks new names only; existing accounts keep signing in.
func validUsername(s string) bool {
	if len(s) < 2 || len(s) > 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func keyName(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "Passkey"
	}
	return truncate(s, 60)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// Recovery codes look like "7KQ4-M2XD-9PFA": 12 characters from an alphabet
// without look-alikes (no 0/O, 1/I/L), about 60 bits each.
const codeAlphabet = "23456789ABCDEFGHJKMNPQRSTUVWXYZ"

func newRecoveryCodes() (codes []string, hashes [][]byte) {
	for i := 0; i < recoveryCount; i++ {
		var b strings.Builder
		for j := 0; j < 12; j++ {
			if j > 0 && j%4 == 0 {
				b.WriteByte('-')
			}
			n, err := rand.Int(rand.Reader, big.NewInt(int64(len(codeAlphabet))))
			if err != nil {
				panic(err)
			}
			b.WriteByte(codeAlphabet[n.Int64()])
		}
		code := b.String()
		codes = append(codes, code)
		hashes = append(hashes, Hash(normalizeCode(code)))
	}
	return codes, hashes
}

func normalizeCode(s string) string {
	s = strings.ToUpper(s)
	var b strings.Builder
	for _, r := range s {
		if r == '-' || unicode.IsSpace(r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Limiter counts sign-in attempts per client in a sliding window. An attempt
// is reserved before the expensive work starts, so a burst of parallel
// requests can't all slip past the check; a successful one gives its slot
// back. IPv6 clients are counted per /64, since one host usually controls a
// whole /64 and could otherwise rotate addresses freely.
//
// Memory is bounded and every call is O(1) amortized, even under a flood
// from many addresses: clients are kept in a list ordered by their latest
// attempt, so expired ones are always at the front and cleanup only touches
// entries it removes (no scan of the whole table under the lock). Past
// maxKeys clients the least recently active one is dropped.
type Limiter struct {
	mu      sync.Mutex
	max     int
	window  time.Duration
	maxKeys int
	fails   map[string]*limitEntry
	order   list.List // of *limitEntry, oldest latest-attempt first
	now     func() time.Time
}

type limitEntry struct {
	key     string
	times   []time.Time // attempts in the window, oldest first
	touched time.Time   // latest Reserve: its place in order
	elem    *list.Element
}

// defaultLimiterKeys bounds the clients a Limiter tracks (a few MB at most).
const defaultLimiterKeys = 50000

func NewLimiter(max int, window time.Duration) *Limiter {
	return &Limiter{max: max, window: window, maxKeys: defaultLimiterKeys, fails: map[string]*limitEntry{}, now: time.Now}
}

// limitKey groups IPv6 addresses by their /64; anything else is used as is.
func limitKey(ip string) string {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return ip
	}
	a = a.Unmap()
	if a.Is4() {
		return a.String()
	}
	p, err := a.WithZone("").Prefix(64)
	if err != nil {
		return ip
	}
	return p.String()
}

func (l *Limiter) remove(e *limitEntry) {
	l.order.Remove(e.elem)
	delete(l.fails, e.key)
}

// sweep drops clients whose latest attempt has left the window. They sit at
// the front of order, so this stops at the first live one.
func (l *Limiter) sweep(now time.Time) {
	for f := l.order.Front(); f != nil; f = l.order.Front() {
		e := f.Value.(*limitEntry)
		if now.Sub(e.touched) <= l.window {
			return
		}
		l.remove(e)
	}
}

// prune drops key's expired attempts and returns its entry (nil if none left).
func (l *Limiter) prune(key string, now time.Time) *limitEntry {
	e := l.fails[key]
	if e == nil {
		return nil
	}
	i := 0
	for i < len(e.times) && now.Sub(e.times[i]) > l.window {
		i++
	}
	e.times = e.times[i:]
	if len(e.times) == 0 {
		l.remove(e)
		return nil
	}
	return e
}

// Allow reports whether ip has attempts left, without using one.
func (l *Limiter) Allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.prune(limitKey(ip), l.now())
	return e == nil || len(e.times) < l.max
}

// Reserve uses one attempt for ip if any are left, in one step, and reports
// whether it did. Call Release if the attempt turns out not to be a failure.
func (l *Limiter) Reserve(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	key, now := limitKey(ip), l.now()
	l.sweep(now)
	e := l.prune(key, now)
	if e != nil && len(e.times) >= l.max {
		return false
	}
	if e == nil {
		// Bound memory under a distributed flood: forget the client whose
		// latest attempt is oldest.
		for len(l.fails) >= l.maxKeys && l.order.Len() > 0 {
			l.remove(l.order.Front().Value.(*limitEntry))
		}
		e = &limitEntry{key: key}
		e.elem = l.order.PushBack(e)
		l.fails[key] = e
	} else {
		l.order.MoveToBack(e.elem)
	}
	e.times = append(e.times, now)
	e.touched = now
	return true
}

// Release gives back an attempt taken by Reserve.
func (l *Limiter) Release(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.prune(limitKey(ip), l.now())
	if e == nil {
		return
	}
	if len(e.times) == 1 {
		l.remove(e)
		return
	}
	// The entry keeps its place in order; at worst it is swept a little later.
	e.times = e.times[:len(e.times)-1]
}
