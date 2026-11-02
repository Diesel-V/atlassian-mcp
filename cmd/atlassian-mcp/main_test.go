package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"atlassian-mcp/internal/config"
	"atlassian-mcp/internal/store"
	"atlassian-mcp/internal/store/memory"
)

func TestAgentCLI(t *testing.T) {
	ctx := context.Background()
	st := memory.New()
	run := func(stdin string, args ...string) (string, error) {
		var out bytes.Buffer
		err := runAgentCmd(ctx, st, args, strings.NewReader(stdin), &out)
		return out.String(), err
	}

	if _, err := run("", "add", "-client-id", "bot", "-name", "Tickets", "-owner", "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := run("", "add", "-client-id", "bot", "-name", "x", "-owner", "y"); err == nil {
		t.Fatal("duplicate agent accepted")
	}
	if _, err := run("pat-123\n", "set-token", "-client-id", "bot", "-provider", "jira"); err != nil {
		t.Fatal(err)
	}
	a, _ := st.GetAgent(ctx, "bot")
	if a.Credentials[store.Jira] != "pat-123" || !a.Enabled {
		t.Fatalf("agent = %+v", a)
	}

	out, err := run("", "list")
	if err != nil || !strings.Contains(out, "bot") || !strings.Contains(out, "jira") || strings.Contains(out, "pat-123") {
		t.Fatalf("list output (must not show tokens):\n%s %v", out, err)
	}

	if _, err := run("", "disable", "-client-id", "bot"); err != nil {
		t.Fatal(err)
	}
	if a, _ := st.GetAgent(ctx, "bot"); a.Enabled {
		t.Fatal("not disabled")
	}
	if _, err := run("", "remove-token", "-client-id", "bot", "-provider", "jira"); err != nil {
		t.Fatal(err)
	}
	if a, _ := st.GetAgent(ctx, "bot"); a.Credentials[store.Jira] != "" {
		t.Fatal("token not removed")
	}

	for _, args := range [][]string{
		{"set-token", "-client-id", "bot", "-provider", "gitlab"},
		{"set-token", "-client-id", "ghost", "-provider", "jira"},
		{"enable"},
		{"frobnicate", "-client-id", "bot"},
	} {
		if _, err := run("x", args...); err == nil {
			t.Errorf("%v: expected error", args)
		}
	}
	if _, err := run("  ", "set-token", "-client-id", "bot", "-provider", "jira"); err == nil {
		t.Error("empty token accepted")
	}
}

func TestUserOffboardCLI(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	st := memory.New()
	_ = st.CreateAPIKey(ctx, &store.APIKey{ID: "k1", Username: "leaver", Name: "oc", SecretHash: "h", CreatedAt: now, ExpiresAt: now.Add(time.Hour)})
	_ = st.PutToken(ctx, &store.OAuthToken{Username: "leaver", Provider: store.Jira, AccessToken: "a", Expiry: now})
	_ = st.PutAgent(ctx, &store.Agent{ClientID: "ticket-bot", Owner: "leaver", Enabled: true})

	var out bytes.Buffer
	if err := runUserCmd(ctx, st, []string{"offboard", "-username", "leaver", "-operator", "secops"}, &out, now); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "revoked 1 key(s)") || !strings.Contains(out.String(), "ticket-bot") {
		t.Fatalf("output:\n%s", out.String())
	}
	if e := st.AuditEntries(); len(e) != 1 || e[0].Principal != "secops" {
		t.Fatalf("audit = %+v", e)
	}
	for _, args := range [][]string{{"offboard"}, {"delete", "-username", "x"}, {"offboard", "-bogus"}} {
		if err := runUserCmd(ctx, st, args, &out, now); err == nil {
			t.Errorf("%v: expected error", args)
		}
	}
}

func TestHTTPClientTimeout(t *testing.T) {
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer slow.Close()
	defer close(release)

	c, err := httpClient(&config.Config{HTTPTimeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := c.Get(slow.URL); err == nil {
		t.Fatal("request to a hanging server did not time out")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("timeout took %v", d)
	}

	if _, err := httpClient(&config.Config{CAFile: "/nonexistent"}); err == nil {
		t.Fatal("missing CA_FILE accepted")
	}
}
