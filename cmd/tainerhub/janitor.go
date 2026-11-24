package main

import (
	"context"
	"log/slog"
	"time"
)

type auditPurger interface {
	PurgeAudit(ctx context.Context, before time.Time) (int64, error)
}

// runAuditJanitor deletes audit log entries older than keep, once at start
// and then every few hours. Every failed sign-in adds a row, so without it
// the table only ever grows. keep <= 0 keeps everything.
func runAuditJanitor(ctx context.Context, st auditPurger, keep time.Duration, log *slog.Logger) {
	if keep <= 0 {
		return
	}
	t := time.NewTicker(6 * time.Hour)
	defer t.Stop()
	for {
		n, err := st.PurgeAudit(ctx, time.Now().Add(-keep))
		switch {
		case err != nil && ctx.Err() == nil:
			log.Warn("purge old audit log entries", "err", err)
		case n > 0:
			log.Info("purged old audit log entries", "count", n, "older_than_days", int(keep.Hours()/24))
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
