package audit

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"atlassian-mcp/internal/store"
	"atlassian-mcp/internal/store/memory"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

type failingAudit struct{ store.Audit }

func (failingAudit) InsertAudit(context.Context, *store.AuditEntry) error {
	return errors.New("db down")
}

func TestRecordPersistsAndSurvivesStoreFailure(t *testing.T) {
	st := memory.New()
	(&Recorder{Store: st, Log: discard}).Record(context.Background(), &store.AuditEntry{Tool: "jira_search", Principal: "alice"})
	if got := st.AuditEntries(); len(got) != 1 || got[0].Principal != "alice" {
		t.Fatalf("entries = %+v", got)
	}
	// Must not panic or block when the store fails.
	(&Recorder{Store: failingAudit{}, Log: discard}).Record(context.Background(), &store.AuditEntry{Tool: "x"})
}

func TestRetention(t *testing.T) {
	ctx := context.Background()
	st := memory.New()
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	for _, age := range []time.Duration{400 * 24 * time.Hour, 366 * 24 * time.Hour, 364 * 24 * time.Hour, time.Hour} {
		_ = st.InsertAudit(ctx, &store.AuditEntry{Time: now.Add(-age), Tool: "t"})
	}
	r := &Retention{Store: st, Keep: 365 * 24 * time.Hour, Now: func() time.Time { return now }, Log: discard}
	n, err := r.PurgeOnce(ctx)
	if err != nil || n != 2 {
		t.Fatalf("purged %d, err %v; want 2", n, err)
	}
	for _, e := range st.AuditEntries() {
		if now.Sub(e.Time) > r.Keep {
			t.Fatalf("expired entry kept: %v", e.Time)
		}
	}
}

func TestRetentionRunStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	st := memory.New()
	_ = st.InsertAudit(ctx, &store.AuditEntry{Time: time.Now().Add(-48 * time.Hour)})
	done := make(chan struct{})
	go func() {
		(&Retention{Store: st, Keep: 24 * time.Hour, Log: discard}).Run(ctx, time.Hour)
		close(done)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for len(st.AuditEntries()) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("first purge did not run immediately")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop on cancel")
	}
}
