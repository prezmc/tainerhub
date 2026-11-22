package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"math/big"
	"strings"
	"sync"
	"time"
	"unicode"
)

func subtleEq(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

func validUsername(s string) bool {
	if len(s) < 2 || len(s) > 32 {
		return false
	}
	for _, r := range s {
		if !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '.' || r == '-' || r == '_') {
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

// Limiter counts failed sign-in attempts per client IP in a sliding window.
type Limiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	fails  map[string][]time.Time
}

func NewLimiter(max int, window time.Duration) *Limiter {
	return &Limiter{max: max, window: window, fails: map[string][]time.Time{}}
}

func (l *Limiter) prune(key string, now time.Time) []time.Time {
	ts := l.fails[key]
	i := 0
	for i < len(ts) && now.Sub(ts[i]) > l.window {
		i++
	}
	ts = ts[i:]
	if len(ts) == 0 {
		delete(l.fails, key)
	} else {
		l.fails[key] = ts
	}
	return ts
}

func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.prune(key, time.Now())) < l.max
}

func (l *Limiter) Fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if len(l.fails) > 50000 { // bound memory under a distributed flood
		for k := range l.fails {
			l.prune(k, now)
		}
	}
	l.fails[key] = append(l.prune(key, now), now)
}
