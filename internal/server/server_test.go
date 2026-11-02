package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"atlassian-mcp/internal/atlassian"
	"atlassian-mcp/internal/atlassian/confluence"
	"atlassian-mcp/internal/atlassian/jira"
	"atlassian-mcp/internal/audit"
	"atlassian-mcp/internal/identity"
	"atlassian-mcp/internal/metrics"
	"atlassian-mcp/internal/offboard"
	"atlassian-mcp/internal/ratelimit"
	"atlassian-mcp/internal/store"
	"atlassian-mcp/internal/store/memory"
	"atlassian-mcp/internal/tools"
)

// End-to-end tests: a real MCP client talks to the full server stack, which talks
// to fake Confluence and Jira that enforce per-token visibility like the real products.

type fakeJWT map[string]*identity.Claims

func (f fakeJWT) Verify(_ context.Context, raw string) (*identity.Claims, error) {
	if c, ok := f[raw]; ok {
		return c, nil
	}
	return nil, errors.New("bad token")
}

// fakeAtlassian records which credentials each request carried.
type fakeAtlassian struct {
	mu       sync.Mutex
	authSeen []string
	cqlSeen  []string
	reject   map[string]bool // tokens that get HTTP 401
	hold     chan struct{}   // requests with "Bearer at-slow" wait until it is closed
}

func (f *fakeAtlassian) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a := r.Header.Get("Authorization")
		f.mu.Lock()
		f.authSeen = append(f.authSeen, a)
		if cql := r.URL.Query().Get("cql"); cql != "" {
			f.cqlSeen = append(f.cqlSeen, cql)
		}
		reject := f.reject[a]
		f.mu.Unlock()
		if a == "Bearer at-slow" {
			select {
			case <-f.hold:
			case <-r.Context().Done():
				return
			}
		}
		if reject || a == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/rest/api/search":
			// Visibility is decided by the token, like Confluence permissions.
			if a == "Bearer at-hr" {
				_, _ = io.WriteString(w, `{"results":[{"content":{"id":"7","type":"page","title":"Отпуска","space":{"key":"LLM"},"_links":{"webui":"/x/7"}},"excerpt":"HR"}]}`)
				return
			}
			_, _ = io.WriteString(w, `{"results":[{"content":{"id":"1","type":"page","title":"Регламент","space":{"key":"TECHOPS"},"_links":{"webui":"/x/1"}},"excerpt":"выкатки"}]}`)
		case "/rest/api/2/search":
			_, _ = io.WriteString(w, `{"total":1,"issues":[{"key":"OD-1","fields":{"summary":"Тест"}}]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func (f *fakeAtlassian) lastAuth() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.authSeen) == 0 {
		return ""
	}
	return f.authSeen[len(f.authSeen)-1]
}

func (f *fakeAtlassian) requests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.authSeen)
}

type testEnv struct {
	url   string
	st    *memory.Store
	atl   *fakeAtlassian
	calls *metrics.CounterVec
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st := memory.New()
	atl := &fakeAtlassian{reject: map[string]bool{}, hold: make(chan struct{})}
	atlSrv := httptest.NewServer(atl.handler())
	t.Cleanup(atlSrv.Close)

	exp := time.Now().Add(time.Hour)
	resolver := &identity.Resolver{
		JWT: fakeJWT{
			"kc-alice":  {AuthorizedParty: "open-webui", Username: "alice@example.com", Expiry: exp},
			"kc-hr":     {AuthorizedParty: "open-webui", Username: "hr@example.com", Expiry: exp},
			"kc-newbie": {AuthorizedParty: "open-webui", Username: "newbie", Expiry: exp},
			"kc-slow":   {AuthorizedParty: "open-webui", Username: "slow", Expiry: exp},
			"kc-bot":    {AuthorizedParty: "ticket-bot", Expiry: exp},
			"kc-rogue":  {AuthorizedParty: "rogue-client", Expiry: exp},
		},
		Keys: st, Agents: st, UserClients: []string{"open-webui"}, StripDomain: "example.com", Log: log,
	}

	ctx := context.Background()
	far := time.Now().Add(24 * time.Hour)
	_ = st.PutToken(ctx, &store.OAuthToken{Username: "alice", Provider: store.Confluence, AccessToken: "at-alice", RefreshToken: "r", Expiry: far})
	_ = st.PutToken(ctx, &store.OAuthToken{Username: "alice", Provider: store.Jira, AccessToken: "at-alice-jira", RefreshToken: "r", Expiry: far})
	_ = st.PutToken(ctx, &store.OAuthToken{Username: "slow", Provider: store.Confluence, AccessToken: "at-slow", RefreshToken: "r", Expiry: far})
	_ = st.PutToken(ctx, &store.OAuthToken{Username: "hr", Provider: store.Confluence, AccessToken: "at-hr", RefreshToken: "r", Expiry: far})
	_ = st.PutAgent(ctx, &store.Agent{ClientID: "ticket-bot", Name: "Tickets", Owner: "alice", Enabled: true,
		Credentials: map[store.Provider]string{store.Jira: "pat-bot"}})

	reg := metrics.NewRegistry()
	tm := tools.NewMetrics(reg)
	conf, err := confluence.NewClient(atlSrv.URL, atlSrv.Client(), 20, 10000)
	if err != nil {
		t.Fatal(err)
	}
	svc := &tools.Service{
		Confluence: conf,
		Jira:       jira.NewClient(atlSrv.URL, atlSrv.Client(), 20, 10000),
		Creds:      &atlassian.Credentials{Tokens: st, Agents: st, Log: log},
		Audit:      &audit.Recorder{Store: st, Log: log},
		Limiter:    ratelimit.New(1000),
		Metrics:    tm,
		PublicURL:  "https://mcp.example",
	}
	router := Router(MCPHandler(svc, resolver, "test", log), http.NotFoundHandler(), st, log)
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	return &testEnv{url: srv.URL, st: st, atl: atl, calls: tm.Calls}
}

type bearerTransport struct{ token string }

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

func (e *testEnv) connect(t *testing.T, token string) (*mcp.ClientSession, error) {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1"}, nil)
	return client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:             e.url + "/mcp",
		HTTPClient:           &http.Client{Transport: bearerTransport{token}},
		DisableStandaloneSSE: true,
		MaxRetries:           -1,
	}, nil)
}

func (e *testEnv) session(t *testing.T, token string) *mcp.ClientSession {
	t.Helper()
	cs, err := e.connect(t, token)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func call(t *testing.T, cs *mcp.ClientSession, tool string, args map[string]any) (*mcp.CallToolResult, string) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	var text strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text.WriteString(tc.Text)
		}
	}
	return res, text.String()
}

func TestUnauthenticatedRejected(t *testing.T) {
	e := newTestEnv(t)
	cases := map[string]int{
		"":        http.StatusUnauthorized,
		"garbage": http.StatusUnauthorized,
		"amcp_0123456789abcdef_" + strings.Repeat("A", 43): http.StatusUnauthorized,
		"kc-rogue": http.StatusForbidden, // valid Keycloak token, client not in the agent registry
	}
	for tok, want := range cases {
		req, _ := http.NewRequest(http.MethodPost, e.url+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("token %q: status %d, want %d", tok, resp.StatusCode, want)
		}
		if strings.Contains(string(b), "bad token") {
			t.Errorf("verification details leaked: %s", b)
		}
	}
	if e.atl.requests() != 0 {
		t.Fatal("unauthenticated request reached Atlassian")
	}
}

func TestToolsListed(t *testing.T) {
	e := newTestEnv(t)
	res, err := e.session(t, "kc-alice").ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range res.Tools {
		names = append(names, tl.Name)
		if tl.Annotations == nil || !tl.Annotations.ReadOnlyHint {
			t.Errorf("%s is not marked read-only", tl.Name)
		}
	}
	slices.Sort(names)
	if strings.Join(names, ",") != "confluence_get_page,confluence_search,jira_get_issue,jira_search" {
		t.Fatalf("tools = %v", names)
	}
}

func TestUserSearchesUnderOwnRights(t *testing.T) {
	e := newTestEnv(t)

	res, text := call(t, e.session(t, "kc-alice"), "confluence_search", map[string]any{"query": "регламент"})
	if res.IsError || !strings.Contains(text, "Регламент") || strings.Contains(text, "Отпуска") {
		t.Fatalf("alice: %v %s", res.IsError, text)
	}
	if e.atl.lastAuth() != "Bearer at-alice" {
		t.Fatalf("alice's request used %q", e.atl.lastAuth())
	}

	res, text = call(t, e.session(t, "kc-hr"), "confluence_search", map[string]any{"query": "отпуск"})
	if res.IsError || !strings.Contains(text, "Отпуска") {
		t.Fatalf("hr: %s", text)
	}
	if e.atl.lastAuth() != "Bearer at-hr" {
		t.Fatalf("hr's request used %q", e.atl.lastAuth())
	}

	var structured struct {
		Results []confluence.SearchResult `json:"results"`
	}
	raw, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(raw, &structured); err != nil || len(structured.Results) != 1 || structured.Results[0].Space != "LLM" {
		t.Fatalf("structured content: %s %v", raw, err)
	}

	entries := e.st.AuditEntries()
	last := entries[len(entries)-1]
	if last.Principal != "hr" || last.SubjectKind != "user" || last.Tool != "confluence_search" || last.Status != "ok" || last.Results != 1 ||
		!strings.Contains(string(last.Params), "отпуск") {
		t.Fatalf("audit = %+v", last)
	}
	if strings.Contains(string(last.Params), "Отпуска") {
		t.Fatal("audit contains document content")
	}
	if e.calls.Value("confluence_search", "user", "ok") != 2 {
		t.Fatalf("metric = %v", e.calls.Value("confluence_search", "user", "ok"))
	}
}

func TestNotConnectedUserGetsLinkAndNoFallback(t *testing.T) {
	e := newTestEnv(t)
	before := e.atl.requests()
	res, text := call(t, e.session(t, "kc-newbie"), "confluence_search", map[string]any{"query": "x"})
	if !res.IsError || !strings.Contains(text, "https://mcp.example/connect/confluence") {
		t.Fatalf("result: %v %s", res.IsError, text)
	}
	if e.atl.requests() != before {
		t.Fatal("a request reached Confluence without the user's own grant")
	}
	entries := e.st.AuditEntries()
	if s := entries[len(entries)-1].Status; s != "not_connected" {
		t.Fatalf("audit status = %s", s)
	}
}

func TestAPIKeyActsAsOwner(t *testing.T) {
	e := newTestEnv(t)
	full, id, secret, _ := identity.NewAPIKey()
	_ = e.st.CreateAPIKey(context.Background(), &store.APIKey{ID: id, Username: "alice", Name: "opencode",
		SecretHash: identity.HashSecret(secret), CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)})

	res, _ := call(t, e.session(t, full), "jira_search", map[string]any{"assignee": "me"})
	if res.IsError || e.atl.lastAuth() != "Bearer at-alice-jira" {
		t.Fatalf("api key: %v %s", res.IsError, e.atl.lastAuth())
	}
	entries := e.st.AuditEntries()
	last := entries[len(entries)-1]
	if last.SubjectKind != "api_key" || last.Principal != "alice" || last.KeyID != id {
		t.Fatalf("audit = %+v", last)
	}

	_ = e.st.RevokeAPIKey(context.Background(), "alice", id, time.Now())
	if _, err := e.connect(t, full); err == nil {
		t.Fatal("revoked key still accepted")
	}
}

func TestOffboardedUserLosesKeysAndGrants(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	full, id, secret, _ := identity.NewAPIKey()
	_ = e.st.CreateAPIKey(ctx, &store.APIKey{ID: id, Username: "alice", Name: "opencode",
		SecretHash: identity.HashSecret(secret), CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)})
	cs := e.session(t, full)
	if res, _ := call(t, cs, "jira_search", map[string]any{"assignee": "me"}); res.IsError {
		t.Fatal("key did not work before offboarding")
	}

	if _, err := offboard.User(ctx, e.st, "alice", "secops", time.Now()); err != nil {
		t.Fatal(err)
	}
	before := e.atl.requests()
	if _, err := e.connect(t, full); err == nil {
		t.Fatal("key of an offboarded user still accepted")
	}
	// Her SSO token (valid until Keycloak blocks her) no longer reaches any data either.
	_, text := call(t, e.session(t, "kc-alice"), "confluence_search", map[string]any{"query": "регламент"})
	if !strings.Contains(text, "https://mcp.example/connect/confluence") || e.atl.requests() != before {
		t.Fatalf("offboarded user reached Atlassian: %q", text)
	}
}

func TestAgentUsesOnlyItsOwnServiceAccount(t *testing.T) {
	e := newTestEnv(t)
	cs := e.session(t, "kc-bot")

	res, _ := call(t, cs, "jira_search", map[string]any{"text": "оплата"})
	if res.IsError || e.atl.lastAuth() != "Bearer pat-bot" {
		t.Fatalf("agent jira: %v %s", res.IsError, e.atl.lastAuth())
	}
	before := e.atl.requests()
	res, text := call(t, cs, "confluence_search", map[string]any{"query": "x"})
	if !res.IsError || !strings.Contains(text, "агенту не выдан доступ") || e.atl.requests() != before {
		t.Fatalf("agent confluence: %v %s", res.IsError, text)
	}
	entries := e.st.AuditEntries()
	if p := entries[len(entries)-1].Principal; p != "agent:ticket-bot" {
		t.Fatalf("principal = %s", p)
	}
}

func TestInjectionCannotWidenScope(t *testing.T) {
	e := newTestEnv(t)
	call(t, e.session(t, "kc-alice"), "confluence_search", map[string]any{
		"query": `x" OR space = "CS`, "spaces": []string{"TECHOPS"},
	})
	e.atl.mu.Lock()
	cql := e.atl.cqlSeen[len(e.atl.cqlSeen)-1]
	e.atl.mu.Unlock()
	if strings.Contains(cql, `space = "CS"`) || !strings.Contains(cql, `space in ("TECHOPS")`) {
		t.Fatalf("cql = %s", cql)
	}

	res, text := call(t, e.session(t, "kc-alice"), "confluence_search", map[string]any{
		"query": "x", "spaces": []string{`TECHOPS") OR space in ("CS`},
	})
	if !res.IsError || !strings.Contains(text, "invalid space key") {
		t.Fatalf("bad space accepted: %s", text)
	}
}

func TestRejectedTokenIsDroppedAndUserAskedToReconnect(t *testing.T) {
	e := newTestEnv(t)
	e.atl.mu.Lock()
	e.atl.reject["Bearer at-alice"] = true
	e.atl.mu.Unlock()

	res, text := call(t, e.session(t, "kc-alice"), "confluence_search", map[string]any{"query": "x"})
	if !res.IsError || !strings.Contains(text, "/connect/confluence") {
		t.Fatalf("result: %s", text)
	}
	if _, err := e.st.GetToken(context.Background(), "alice", store.Confluence); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("rejected grant kept")
	}
}

func TestSlowUserDoesNotBlockOthers(t *testing.T) {
	e := newTestEnv(t)
	slow := e.session(t, "kc-slow")
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = slow.CallTool(context.Background(), &mcp.CallToolParams{Name: "confluence_search", Arguments: map[string]any{"query": "x"}})
	}()
	// While the slow user's request hangs in Confluence, another user is served.
	other := e.session(t, "kc-alice")
	finished := make(chan bool, 1)
	go func() {
		res, err := other.CallTool(context.Background(), &mcp.CallToolParams{Name: "confluence_search", Arguments: map[string]any{"query": "регламент"}})
		finished <- err == nil && !res.IsError
	}()
	select {
	case ok := <-finished:
		if !ok {
			t.Fatal("other user's search failed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a slow request blocked other users")
	}
	close(e.atl.hold)
	<-done
}

func TestHealth(t *testing.T) {
	e := newTestEnv(t)
	for _, p := range []string{"/healthz", "/readyz"} {
		resp, err := http.Get(e.url + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s: %d", p, resp.StatusCode)
		}
	}
}
