// Package audit records every tool invocation without document content.
package audit

import (
	"context"
	"log/slog"
	"time"

	"atlassian-mcp/internal/store"
)

// Recorder writes audit entries to the store and the structured log.
// A failure to persist is logged but never fails the user's request.
type Recorder struct {
	Store store.Audit
	Log   *slog.Logger
}

// Record persists e.
func (r *Recorder) Record(ctx context.Context, e *store.AuditEntry) {
	r.Log.Info("tool call",
		"tool", e.Tool, "subject_kind", e.SubjectKind, "principal", e.Principal, "key_id", e.KeyID,
		"status", e.Status, "results", e.Results, "duration_ms", e.DurationMS, "error", e.Error)

	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	if err := r.Store.InsertAudit(ctx, e); err != nil {
		r.Log.Error("audit insert failed", "tool", e.Tool, "principal", e.Principal, "err", err)
	}
}

// Retention deletes audit entries older than Keep.
type Retention struct {
	Store store.Audit
	Keep  time.Duration
	Now   func() time.Time
	Log   *slog.Logger
}

// PurgeOnce removes expired entries and returns how many were deleted.
func (r *Retention) PurgeOnce(ctx context.Context) (int64, error) {
	now := time.Now
	if r.Now != nil {
		now = r.Now
	}
	n, err := r.Store.PurgeAudit(ctx, now().Add(-r.Keep))
	if err != nil {
		return 0, err
	}
	if n > 0 {
		r.Log.Info("audit entries purged", "count", n, "retention", r.Keep.String())
	}
	return n, nil
}

// Run purges immediately and then every interval until ctx is cancelled.
// Errors are logged; the next tick retries.
func (r *Retention) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if _, err := r.PurgeOnce(ctx); err != nil && ctx.Err() == nil {
			r.Log.Error("audit purge failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
