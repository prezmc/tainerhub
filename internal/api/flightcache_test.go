package api

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A caller that gives up must not cancel the shared fetch or leave its
// cancellation cached for everyone else; concurrent callers share one fetch.
func TestFlightCacheCancelledCallerDoesNotPoison(t *testing.T) {
	var c flightCache[string]
	var calls atomic.Int32
	release := make(chan struct{})
	fetch := func(ctx context.Context) (string, error) {
		calls.Add(1)
		select {
		case <-release:
			return "v1", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.get(ctx, time.Hour, time.Minute, fetch); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled caller: got %v", err)
	}

	var wg sync.WaitGroup
	results := make([]string, 5)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			v, err := c.get(context.Background(), time.Hour, time.Minute, fetch)
			if err != nil {
				t.Errorf("caller %d: %v", i, err)
			}
			results[i] = v
		}(i)
	}
	time.Sleep(20 * time.Millisecond) // let them all queue on the one fetch
	close(release)
	wg.Wait()
	for i, v := range results {
		if v != "v1" {
			t.Errorf("caller %d got %q", i, v)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("fetch ran %d times, want 1", n)
	}
	if v, err := c.get(context.Background(), time.Hour, time.Minute, fetch); v != "v1" || err != nil || calls.Load() != 1 {
		t.Fatalf("cached read: %q, %v (fetches %d)", v, err, calls.Load())
	}
}

// Once a value is known, an expired cache answers at once with it while a
// refresh runs in the background, and a failed refresh keeps the old value.
func TestFlightCacheServesStaleWhileRefreshing(t *testing.T) {
	var c flightCache[string]
	c.at, c.val = time.Now().Add(-2*time.Hour), "old"
	block := make(chan struct{})
	fetched := make(chan struct{})
	fetch := func(ctx context.Context) (string, error) {
		<-block
		defer close(fetched)
		return "", errors.New("GitHub down")
	}
	start := time.Now()
	if v, _ := c.get(context.Background(), time.Hour, time.Minute, fetch); v != "old" {
		t.Fatalf("got %q, want the stale value", v)
	}
	if time.Since(start) > time.Second {
		t.Fatal("a stale read waited for the fetch")
	}
	close(block)
	<-fetched
	time.Sleep(10 * time.Millisecond)
	v, err := c.get(context.Background(), time.Hour, time.Minute, fetch)
	if v != "old" || err == nil {
		t.Fatalf("after a failed refresh: %q, %v; want the old value and the error", v, err)
	}
}
