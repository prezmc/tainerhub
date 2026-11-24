package main

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

type fakePurger struct{ cutoffs chan time.Time }

func (f *fakePurger) PurgeAudit(_ context.Context, before time.Time) (int64, error) {
	f.cutoffs <- before
	return 3, nil
}

func TestAuditJanitor(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Retention 0 keeps everything: the janitor never deletes.
	f := &fakePurger{cutoffs: make(chan time.Time, 1)}
	runAuditJanitor(context.Background(), f, 0, log) // returns at once
	if len(f.cutoffs) != 0 {
		t.Fatal("retention 0 must not purge")
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { runAuditJanitor(ctx, f, 30*24*time.Hour, log); close(done) }()
	select {
	case cut := <-f.cutoffs:
		if d := time.Since(cut) - 30*24*time.Hour; d < 0 || d > time.Minute {
			t.Fatalf("cutoff %v is not 30 days ago", cut)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("janitor didn't purge at start")
	}
	cancel()
	<-done
}

func TestAuditRetentionConfig(t *testing.T) {
	t.Setenv("TAINERHUB_DATABASE_URL", "postgres://x@y/z")
	for in, want := range map[string]time.Duration{"": 365 * 24 * time.Hour, "0": 0, "7": 7 * 24 * time.Hour} {
		t.Setenv("TAINERHUB_AUDIT_RETENTION_DAYS", in)
		c, err := loadConfig()
		if err != nil || c.AuditRetention != want {
			t.Fatalf("%q: got %v, %v; want %v", in, c.AuditRetention, err, want)
		}
	}
	for _, bad := range []string{"-1", "a year"} {
		t.Setenv("TAINERHUB_AUDIT_RETENTION_DAYS", bad)
		if _, err := loadConfig(); err == nil {
			t.Fatalf("%q: want an error", bad)
		}
	}
}
