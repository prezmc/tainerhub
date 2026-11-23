package gateway

import (
	"testing"
	"time"
)

func TestTokenBucket(t *testing.T) {
	now := time.Unix(0, 0)
	b := newTokenBucket(3, 2, now)
	for i := range 3 {
		if !b.allow(now) {
			t.Fatalf("burst message %d refused", i+1)
		}
	}
	if b.allow(now) {
		t.Fatal("allowed beyond the burst")
	}
	now = now.Add(time.Second) // refills 2
	if !b.allow(now) || !b.allow(now) || b.allow(now) {
		t.Fatal("refill wrong")
	}
	now = now.Add(time.Hour) // never beyond the burst
	n := 0
	for b.allow(now) {
		n++
	}
	if n != 3 {
		t.Fatalf("%d allowed after a long pause, want 3", n)
	}
}

func TestFailLimiterWindow(t *testing.T) {
	l := newFailLimiter(2, time.Minute)
	now := time.Unix(0, 0)
	l.fail("1.2.3.4", now)
	if l.blocked("1.2.3.4", now) {
		t.Fatal("blocked after one failure")
	}
	l.fail("1.2.3.4", now)
	if !l.blocked("1.2.3.4", now) || l.blocked("5.6.7.8", now) {
		t.Fatal("wrong address blocked")
	}
	if l.blocked("1.2.3.4", now.Add(time.Minute)) {
		t.Fatal("still blocked after the window")
	}
}

// The container-list hash ignores the timestamp in front of it.
func TestContainersSum(t *testing.T) {
	a := containersSum([]byte(`{"snapshot":{"at":1,"containers":[{"id":"x"}]}}`))
	b := containersSum([]byte(`{"snapshot":{"at":2,"containers":[{"id":"x"}]}}`))
	c := containersSum([]byte(`{"snapshot":{"at":2,"containers":[{"id":"y"}]}}`))
	if a != b || a == c {
		t.Fatal("hash should depend on the containers only")
	}
}
