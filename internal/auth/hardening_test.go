package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func (f *fakeStore) UseRecoveryCode(ctx context.Context, userID string, hash []byte) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recoveryIDs = append(f.recoveryIDs, userID)
	return false, nil
}

// An unknown username must cost the same code lookup as a known one, so
// response times don't reveal which usernames exist.
func TestRecoveryUnknownUserStillLooksUpCode(t *testing.T) {
	f := &fakeStore{}
	s := testService(f, 10)
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	_, err := s.LoginWithRecoveryCode(httptest.NewRecorder(), r, "nobody", "AAAA-BBBB-CCCC")
	if err == nil || errors.Is(err, ErrRateLimited) {
		t.Fatalf("got %v, want a plain failure", err)
	}
	if len(f.recoveryIDs) != 1 || f.recoveryIDs[0] != noUserID {
		t.Fatalf("lookups %v, want one against the placeholder ID", f.recoveryIDs)
	}
	if len(s.Limiter.fails) != 1 {
		t.Fatal("the failed attempt should count against the client")
	}
}

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

func testLimiter(max int, window time.Duration, maxKeys int) (*Limiter, *fakeClock) {
	c := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	l := NewLimiter(max, window)
	l.now = c.now
	l.maxKeys = maxKeys
	return l, c
}

func TestLimiterSweepsExpiredClients(t *testing.T) {
	l, c := testLimiter(3, time.Minute, 1000)
	for i := 0; i < 500; i++ {
		l.Reserve(fmt.Sprintf("10.0.%d.%d", i/256, i%256))
	}
	c.t = c.t.Add(30 * time.Second)
	l.Reserve("192.0.2.1")
	if len(l.fails) != 501 {
		t.Fatalf("%d clients tracked, want 501 (none expired yet)", len(l.fails))
	}
	c.t = c.t.Add(31 * time.Second) // the first 500 are now out of the window
	l.Reserve("192.0.2.2")
	if len(l.fails) != 2 || l.order.Len() != 2 {
		t.Fatalf("%d clients tracked (list %d), want 2 after the sweep", len(l.fails), l.order.Len())
	}
}

func TestLimiterHardCapEvictsOldest(t *testing.T) {
	l, c := testLimiter(2, time.Hour, 3)
	for _, ip := range []string{"192.0.2.1", "192.0.2.2", "192.0.2.3"} {
		l.Reserve(ip)
		c.t = c.t.Add(time.Second)
	}
	l.Reserve("192.0.2.1") // now the most recently active
	l.Reserve("192.0.2.4") // over the cap: .2 is the oldest and goes
	if len(l.fails) != 3 {
		t.Fatalf("%d clients tracked, want the cap of 3", len(l.fails))
	}
	if _, ok := l.fails["192.0.2.2"]; ok {
		t.Fatal("the least recently active client should have been evicted")
	}
	if l.Reserve("192.0.2.1") {
		t.Fatal("a client kept under the cap must keep its count")
	}
}

func TestLimiterWindowStillSlides(t *testing.T) {
	l, c := testLimiter(2, time.Minute, 100)
	l.Reserve("192.0.2.1")
	c.t = c.t.Add(40 * time.Second)
	l.Reserve("192.0.2.1")
	if l.Reserve("192.0.2.1") {
		t.Fatal("third attempt in the window allowed")
	}
	c.t = c.t.Add(21 * time.Second) // the first attempt has expired
	if !l.Reserve("192.0.2.1") || l.Reserve("192.0.2.1") {
		t.Fatal("exactly one attempt should free up as the window slides")
	}
}
