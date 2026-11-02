package offboard

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"atlassian-mcp/internal/store"
	"atlassian-mcp/internal/store/memory"
)

func TestUserWithdrawsEverything(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	st := memory.New()
	key := func(id, user string, exp time.Time) {
		_ = st.CreateAPIKey(ctx, &store.APIKey{ID: id, Username: user, Name: id, SecretHash: "h", CreatedAt: now.Add(-time.Hour), ExpiresAt: exp})
	}
	key("k-active", "leaver", now.Add(24*time.Hour))
	key("k-expired", "leaver", now.Add(-time.Minute))
	key("k-other", "stayer", now.Add(24*time.Hour))
	for _, p := range []store.Provider{store.Confluence, store.Jira} {
		_ = st.PutToken(ctx, &store.OAuthToken{Username: "leaver", Provider: p, AccessToken: "a", Expiry: now})
	}
	_ = st.PutToken(ctx, &store.OAuthToken{Username: "stayer", Provider: store.Jira, AccessToken: "b", Expiry: now})
	_ = st.PutAgent(ctx, &store.Agent{ClientID: "ticket-bot", Owner: "Leaver", Enabled: true})
	_ = st.PutAgent(ctx, &store.Agent{ClientID: "other-bot", Owner: "stayer", Enabled: true})

	rep, err := User(ctx, st, " Leaver ", "root", now)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rep.RevokedKeys, []string{"k-active"}) ||
		!slices.Equal(rep.Disconnected, []store.Provider{store.Confluence, store.Jira}) ||
		!slices.Equal(rep.OwnedAgents, []string{"ticket-bot"}) {
		t.Fatalf("report = %+v", rep)
	}

	if k, _ := st.GetAPIKey(ctx, "k-active"); k.Active(now) {
		t.Fatal("key still active")
	}
	if ps, _ := st.ConnectedProviders(ctx, "leaver"); len(ps) != 0 {
		t.Fatalf("grants left: %v", ps)
	}
	// Nobody else is affected; owned agents keep working until a human decides.
	if k, _ := st.GetAPIKey(ctx, "k-other"); !k.Active(now) {
		t.Fatal("other user's key revoked")
	}
	if ps, _ := st.ConnectedProviders(ctx, "stayer"); len(ps) != 1 {
		t.Fatal("other user's grant deleted")
	}
	if a, _ := st.GetAgent(ctx, "ticket-bot"); !a.Enabled {
		t.Fatal("owned agent disabled without a decision")
	}

	entries := st.AuditEntries()
	if len(entries) != 1 || entries[0].Tool != "offboard" || entries[0].Principal != "root" || entries[0].SubjectKind != "admin" {
		t.Fatalf("audit = %+v", entries)
	}
	var params map[string]any
	if err := json.Unmarshal(entries[0].Params, &params); err != nil || params["username"] != "leaver" {
		t.Fatalf("audit params = %s", entries[0].Params)
	}

	// Idempotent.
	rep, err = User(ctx, st, "leaver", "root", now)
	if err != nil || len(rep.RevokedKeys) != 0 || len(rep.Disconnected) != 0 {
		t.Fatalf("second run: %+v %v", rep, err)
	}
}

func TestUserRequiresUsername(t *testing.T) {
	if _, err := User(context.Background(), memory.New(), "  ", "root", time.Now()); err == nil {
		t.Fatal("empty username accepted")
	}
}
