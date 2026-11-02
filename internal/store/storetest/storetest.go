// Package storetest is a conformance suite every store.Store implementation must pass.
package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"atlassian-mcp/internal/store"
)

// Run executes the suite. newStore must return an empty store.
func Run(t *testing.T, newStore func(t *testing.T) store.Store) {
	t.Run("Tokens", func(t *testing.T) { testTokens(t, newStore(t)) })
	t.Run("APIKeys", func(t *testing.T) { testAPIKeys(t, newStore(t)) })
	t.Run("Agents", func(t *testing.T) { testAgents(t, newStore(t)) })
	t.Run("Audit", func(t *testing.T) { testAudit(t, newStore(t)) })
}

func ts(sec int64) time.Time { return time.Unix(1_800_000_000+sec, 0).UTC() }

func testTokens(t *testing.T, s store.Store) {
	ctx := context.Background()
	if _, err := s.GetToken(ctx, "alice", store.Confluence); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing token: err = %v", err)
	}
	tok := &store.OAuthToken{Username: "alice", Provider: store.Confluence, AccessToken: "a1", RefreshToken: "r1", Expiry: ts(100)}
	must(t, s.PutToken(ctx, tok))
	got, err := s.GetToken(ctx, "alice", store.Confluence)
	must(t, err)
	if got.AccessToken != "a1" || got.RefreshToken != "r1" || !got.Expiry.Equal(ts(100)) {
		t.Fatalf("got %+v", got)
	}

	// Upsert replaces the grant.
	must(t, s.PutToken(ctx, &store.OAuthToken{Username: "alice", Provider: store.Confluence, AccessToken: "a2", RefreshToken: "r2", Expiry: ts(200)}))
	got, _ = s.GetToken(ctx, "alice", store.Confluence)
	if got.AccessToken != "a2" || got.RefreshToken != "r2" {
		t.Fatalf("upsert: got %+v", got)
	}

	must(t, s.PutToken(ctx, &store.OAuthToken{Username: "alice", Provider: store.Jira, AccessToken: "j", Expiry: ts(1)}))
	must(t, s.PutToken(ctx, &store.OAuthToken{Username: "bob", Provider: store.Jira, AccessToken: "b", Expiry: ts(1)}))
	ps, err := s.ConnectedProviders(ctx, "alice")
	must(t, err)
	if len(ps) != 2 || ps[0] != store.Confluence || ps[1] != store.Jira {
		t.Fatalf("providers = %v", ps)
	}

	n, err := s.CountConnected(ctx)
	must(t, err)
	if n[store.Confluence] != 1 || n[store.Jira] != 2 {
		t.Fatalf("connected = %v", n)
	}

	must(t, s.DeleteToken(ctx, "alice", store.Confluence))
	if _, err := s.GetToken(ctx, "alice", store.Confluence); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("after delete: err = %v", err)
	}
	must(t, s.DeleteToken(ctx, "alice", store.Confluence)) // idempotent
	if _, err := s.GetToken(ctx, "bob", store.Jira); err != nil {
		t.Fatalf("other user's token affected: %v", err)
	}
}

func testAPIKeys(t *testing.T, s store.Store) {
	ctx := context.Background()
	k := &store.APIKey{ID: "k1", Username: "alice", Name: "laptop", SecretHash: "h1", CreatedAt: ts(0), ExpiresAt: ts(1000)}
	must(t, s.CreateAPIKey(ctx, k))
	if err := s.CreateAPIKey(ctx, k); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate id: err = %v", err)
	}
	must(t, s.CreateAPIKey(ctx, &store.APIKey{ID: "k2", Username: "alice", Name: "ci", SecretHash: "h2", CreatedAt: ts(10), ExpiresAt: ts(1000)}))
	must(t, s.CreateAPIKey(ctx, &store.APIKey{ID: "k3", Username: "bob", Name: "x", SecretHash: "h3", CreatedAt: ts(5), ExpiresAt: ts(1000)}))

	got, err := s.GetAPIKey(ctx, "k1")
	must(t, err)
	if got.Username != "alice" || got.SecretHash != "h1" || !got.Active(ts(1)) {
		t.Fatalf("got %+v", got)
	}

	list, err := s.ListAPIKeys(ctx, "alice")
	must(t, err)
	if len(list) != 2 || list[0].ID != "k2" || list[1].ID != "k1" {
		t.Fatalf("list order/content wrong: %v, %v", ids(list), list)
	}

	if err := s.RevokeAPIKey(ctx, "bob", "k1", ts(2)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("revoke foreign key: err = %v", err)
	}
	must(t, s.RevokeAPIKey(ctx, "alice", "k1", ts(2)))
	got, _ = s.GetAPIKey(ctx, "k1")
	if got.RevokedAt == nil || got.Active(ts(3)) {
		t.Fatalf("not revoked: %+v", got)
	}

	must(t, s.TouchAPIKey(ctx, "k2", ts(7)))
	got, _ = s.GetAPIKey(ctx, "k2")
	if got.LastUsedAt == nil || !got.LastUsedAt.Equal(ts(7)) {
		t.Fatalf("touch: %+v", got)
	}
	if _, err := s.GetAPIKey(ctx, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing key: err = %v", err)
	}
}

func testAgents(t *testing.T, s store.Store) {
	ctx := context.Background()
	if _, err := s.GetAgent(ctx, "bot"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing agent: err = %v", err)
	}
	a := &store.Agent{ClientID: "bot", Name: "Bot", Owner: "alice", Enabled: true,
		Credentials: map[store.Provider]string{store.Confluence: "pat-c"}, CreatedAt: ts(0)}
	must(t, s.PutAgent(ctx, a))
	a.Credentials[store.Confluence] = "mutated-after-put"

	got, err := s.GetAgent(ctx, "bot")
	must(t, err)
	if got.Credentials[store.Confluence] != "pat-c" || !got.Enabled || got.Owner != "alice" {
		t.Fatalf("got %+v", got)
	}

	got.Enabled = false
	got.Credentials[store.Jira] = "pat-j"
	must(t, s.PutAgent(ctx, got))
	got2, _ := s.GetAgent(ctx, "bot")
	if got2.Enabled || got2.Credentials[store.Jira] != "pat-j" || !got2.CreatedAt.Equal(ts(0)) {
		t.Fatalf("update: %+v", got2)
	}

	must(t, s.PutAgent(ctx, &store.Agent{ClientID: "alpha", Name: "A", Owner: "bob", Enabled: true, CreatedAt: ts(1)}))
	list, err := s.ListAgents(ctx)
	must(t, err)
	if len(list) != 2 || list[0].ClientID != "alpha" || list[1].ClientID != "bot" {
		t.Fatalf("list = %+v", list)
	}
}

func testAudit(t *testing.T, s store.Store) {
	e := &store.AuditEntry{Time: ts(0), SubjectKind: "user", Principal: "alice", Tool: "confluence_search",
		Params: json.RawMessage(`{"query":"x"}`), Results: 3, Status: "ok", DurationMS: 12}
	ctx := context.Background()
	must(t, s.InsertAudit(ctx, e))
	newer := *e
	newer.Time = ts(100)
	must(t, s.InsertAudit(ctx, &newer))

	n, err := s.PurgeAudit(ctx, ts(50))
	must(t, err)
	if n != 1 {
		t.Fatalf("purged %d entries, want 1", n)
	}
	if n, _ := s.PurgeAudit(ctx, ts(50)); n != 0 {
		t.Fatalf("second purge removed %d entries", n)
	}
	if n, _ := s.PurgeAudit(ctx, ts(101)); n != 1 {
		t.Fatalf("newer entry not purged after its time: %d", n)
	}
}

func ids(keys []*store.APIKey) []string {
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = k.ID
	}
	return out
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
