package tools

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"atlassian-mcp/internal/atlassian"
	"atlassian-mcp/internal/atlassian/confluence"
	"atlassian-mcp/internal/atlassian/jira"
	"atlassian-mcp/internal/identity"
	"atlassian-mcp/internal/metrics"
	"atlassian-mcp/internal/scope"
	"atlassian-mcp/internal/store"
)

type fakeCreds struct {
	err         error
	invalidated []store.Provider
}

func (f *fakeCreds) Auth(context.Context, identity.Subject, store.Provider) (atlassian.Auth, error) {
	if f.err != nil {
		return nil, f.err
	}
	return atlassian.Bearer("t"), nil
}

func (f *fakeCreds) Invalidate(_ context.Context, _ identity.Subject, p store.Provider) error {
	f.invalidated = append(f.invalidated, p)
	return nil
}

// fakeConfluence returns page 1 in TECHOPS and page 2 in HR, whatever was asked:
// the scope filter must hold even if the product ignores the query restriction.
type fakeConfluence struct {
	err    error
	params *confluence.SearchParams // last search
	gets   *int
}

func (f fakeConfluence) Search(_ context.Context, _ atlassian.Auth, p confluence.SearchParams) ([]confluence.SearchResult, error) {
	if f.params != nil {
		*f.params = p
	}
	if f.err != nil {
		return nil, f.err
	}
	return []confluence.SearchResult{
		{ID: "1", Space: "TECHOPS", Title: "Секретный заголовок", Excerpt: "секретное содержимое"},
		{ID: "2", Space: "HR", Title: "Другой", Excerpt: "текст"},
	}, nil
}

func (f fakeConfluence) GetPage(_ context.Context, _ atlassian.Auth, ref string) (*confluence.Page, error) {
	if f.gets != nil {
		*f.gets++
	}
	if f.err != nil {
		return nil, f.err
	}
	space := map[string]string{"1": "TECHOPS", "2": "HR"}[ref]
	return &confluence.Page{ID: ref, Space: space, Body: "тело страницы " + ref}, nil
}

// fakeJira returns OD-1 and HR-2 from any search; OD-9 was moved to HR-9.
type fakeJira struct {
	params *jira.SearchParams
	gets   *int
}

func (f fakeJira) Search(_ context.Context, _ atlassian.Auth, p jira.SearchParams) (*jira.SearchResult, error) {
	if f.params != nil {
		*f.params = p
	}
	return &jira.SearchResult{Total: 2, Issues: []jira.IssueSummary{{Key: "OD-1"}, {Key: "HR-2"}}}, nil
}

func (f fakeJira) GetIssue(_ context.Context, _ atlassian.Auth, key string) (*jira.Issue, error) {
	if f.gets != nil {
		*f.gets++
	}
	if key == "OD-9" {
		key = "HR-9"
	}
	return &jira.Issue{IssueSummary: jira.IssueSummary{Key: key}, Description: "описание " + key}, nil
}

type recAudit struct {
	mu      sync.Mutex
	entries []*store.AuditEntry
}

func (r *recAudit) Record(_ context.Context, e *store.AuditEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = append(r.entries, e)
}

type denyAll struct{}

func (denyAll) Allow(string) bool { return false }

type allowAll struct{}

func (allowAll) Allow(string) bool { return true }

func newService(creds *fakeCreds, conf ConfluenceAPI) (*Service, *recAudit) {
	a := &recAudit{}
	return &Service{
		Confluence: conf, Jira: fakeJira{}, Creds: creds, Audit: a, Limiter: allowAll{},
		Spaces: scope.All(), Projects: scope.All(),
		Metrics: NewMetrics(metrics.NewRegistry()), PublicURL: "https://mcp.example",
	}, a
}

func reqFor(s identity.Subject) *mcp.CallToolRequest {
	return &mcp.CallToolRequest{Extra: &mcp.RequestExtra{TokenInfo: &auth.TokenInfo{Extra: map[string]any{"subject": s}}}}
}

var alice = identity.Subject{Kind: identity.KindUser, Username: "alice"}

func TestRunSuccess(t *testing.T) {
	svc, a := newService(&fakeCreds{}, fakeConfluence{})
	_, out, err := svc.confluenceSearch(context.Background(), reqFor(alice), ConfluenceSearchInput{Query: "x", ModifiedAfter: "2026-10-01"})
	if err != nil || len(out.Results) != 2 {
		t.Fatalf("out = %+v, err = %v", out, err)
	}
	e := a.entries[0]
	if e.Status != statusOK || e.Results != 2 || e.Principal != "alice" || e.Tool != "confluence_search" ||
		strings.Join(e.Objects, ",") != "1,2" {
		t.Fatalf("audit = %+v", e)
	}
	// The query is recorded for investigations; document content never is.
	params := string(e.Params)
	if !strings.Contains(params, `"modified_after":"2026-10-01"`) || strings.Contains(params, "секрет") {
		t.Fatalf("audit params = %s", params)
	}
}

func TestRunErrors(t *testing.T) {
	tests := []struct {
		name       string
		creds      *fakeCreds
		conf       fakeConfluence
		in         ConfluenceSearchInput
		wantStatus string
		wantText   string
	}{
		{"not connected", &fakeCreds{err: atlassian.ErrNotConnected}, fakeConfluence{}, ConfluenceSearchInput{Query: "x"},
			statusNotConnected, "https://mcp.example/connect/confluence"},
		{"agent without access", &fakeCreds{err: atlassian.ErrNoAgentAccess}, fakeConfluence{}, ConfluenceSearchInput{Query: "x"},
			statusNotConnected, "агенту не выдан доступ к Confluence"},
		{"bad date", &fakeCreds{}, fakeConfluence{}, ConfluenceSearchInput{Query: "x", ModifiedAfter: "вчера"},
			statusInvalid, "YYYY-MM-DD"},
		{"not found", &fakeCreds{}, fakeConfluence{err: atlassian.ErrNotFound}, ConfluenceSearchInput{Query: "x"},
			statusNotFound, "нет прав доступа"},
		{"unclassified api error", &fakeCreds{}, fakeConfluence{err: &atlassian.APIError{}}, ConfluenceSearchInput{Query: "x"},
			statusError, "ошибка обращения к Confluence"},
		{"upstream", &fakeCreds{}, fakeConfluence{err: atlassian.ErrUpstream}, ConfluenceSearchInput{Query: "x"},
			statusError, "повторите позже"},
		{"timeout", &fakeCreds{}, fakeConfluence{err: context.DeadlineExceeded}, ConfluenceSearchInput{Query: "x"},
			statusError, "не ответил вовремя"},
		{"internal cred error hides details", &fakeCreds{err: errors.New("pq: password authentication failed")}, fakeConfluence{},
			ConfluenceSearchInput{Query: "x"}, statusError, "внутренняя ошибка"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, a := newService(tt.creds, tt.conf)
			_, _, err := svc.confluenceSearch(context.Background(), reqFor(alice), tt.in)
			if err == nil || !strings.Contains(err.Error(), tt.wantText) {
				t.Fatalf("err = %v, want %q", err, tt.wantText)
			}
			if strings.Contains(err.Error(), "password") {
				t.Fatalf("internal detail leaked: %v", err)
			}
			if got := a.entries[0].Status; got != tt.wantStatus {
				t.Fatalf("status = %s, want %s", got, tt.wantStatus)
			}
		})
	}
}

func TestUnauthorizedInvalidatesGrant(t *testing.T) {
	creds := &fakeCreds{}
	svc, a := newService(creds, fakeConfluence{err: atlassian.ErrUnauthorized})
	_, _, err := svc.confluenceSearch(context.Background(), reqFor(alice), ConfluenceSearchInput{Query: "x"})
	if err == nil || !strings.Contains(err.Error(), "/connect/confluence") {
		t.Fatalf("err = %v", err)
	}
	if len(creds.invalidated) != 1 || creds.invalidated[0] != store.Confluence || a.entries[0].Status != statusNotConnected {
		t.Fatalf("invalidated = %v, audit = %+v", creds.invalidated, a.entries[0])
	}
}

func TestRateLimitAndMissingSubject(t *testing.T) {
	svc, a := newService(&fakeCreds{}, fakeConfluence{})
	svc.Limiter = denyAll{}
	if _, _, err := svc.confluenceSearch(context.Background(), reqFor(alice), ConfluenceSearchInput{Query: "x"}); err == nil {
		t.Fatal("rate limit not applied")
	}
	if a.entries[0].Status != statusRateLimited {
		t.Fatalf("status = %s", a.entries[0].Status)
	}

	svc, a = newService(&fakeCreds{}, fakeConfluence{})
	if _, _, err := svc.confluenceSearch(context.Background(), &mcp.CallToolRequest{}, ConfluenceSearchInput{Query: "x"}); err == nil {
		t.Fatal("call without subject allowed")
	}
	if a.entries[0].Status != statusUnauth {
		t.Fatalf("status = %s", a.entries[0].Status)
	}
}

func TestRegisterOnlyEnabledProducts(t *testing.T) {
	svc, _ := newService(&fakeCreds{}, nil)
	svc.Confluence = nil
	srv := mcp.NewServer(&mcp.Implementation{Name: "t"}, nil)
	svc.Register(srv) // must not panic and must skip Confluence
	svc.Jira = nil
	svc.Register(mcp.NewServer(&mcp.Implementation{Name: "t"}, nil))
}

// --- pilot scope ---

func scoped(t *testing.T) (*Service, *recAudit, *confluence.SearchParams, *jira.SearchParams, *int) {
	t.Helper()
	cp, jp, gets := &confluence.SearchParams{}, &jira.SearchParams{}, new(int)
	svc, a := newService(&fakeCreds{}, fakeConfluence{params: cp, gets: gets})
	svc.Jira = fakeJira{params: jp, gets: gets}
	svc.Spaces, svc.Projects = scope.Of("TECHOPS"), scope.Of("OD")
	return svc, a, cp, jp, gets
}

func TestScopeConfluenceSearch(t *testing.T) {
	svc, a, params, _, _ := scoped(t)
	ctx := context.Background()

	_, out, err := svc.confluenceSearch(ctx, reqFor(alice), ConfluenceSearchInput{Query: "x"})
	if err != nil || strings.Join(params.Spaces, ",") != "TECHOPS" {
		t.Fatalf("default spaces = %v, err %v", params.Spaces, err)
	}
	if len(out.Results) != 1 || out.Results[0].ID != "1" || strings.Join(a.entries[0].Objects, ",") != "1" {
		t.Fatalf("result outside the scope passed on: %+v, audit %v", out.Results, a.entries[0].Objects)
	}

	*params = confluence.SearchParams{}
	_, _, err = svc.confluenceSearch(ctx, reqFor(alice), ConfluenceSearchInput{Query: "x", Spaces: []string{"HR"}})
	if err == nil || !strings.Contains(err.Error(), "TECHOPS") || params.Query != "" {
		t.Fatalf("space outside the scope: err=%v, searched=%+v", err, params)
	}
	if a.entries[1].Status != statusInvalid {
		t.Fatalf("status = %s", a.entries[1].Status)
	}
}

func TestScopeConfluenceGetPage(t *testing.T) {
	svc, a, _, _, _ := scoped(t)
	ctx := context.Background()
	if _, p, err := svc.confluenceGetPage(ctx, reqFor(alice), ConfluenceGetPageInput{Page: "1"}); err != nil || p == nil {
		t.Fatalf("page in scope: %v", err)
	}
	_, p, err := svc.confluenceGetPage(ctx, reqFor(alice), ConfluenceGetPageInput{Page: "2"})
	if p != nil || err == nil || !strings.Contains(err.Error(), "нет прав доступа") || strings.Contains(err.Error(), "HR") {
		t.Fatalf("page outside the scope: page=%+v err=%v", p, err)
	}
	e := a.entries[1]
	if e.Status != statusOutOfScope || e.Results != 0 || strings.Join(e.Objects, ",") != "2" {
		t.Fatalf("audit = %+v", e)
	}
}

func TestScopeJira(t *testing.T) {
	svc, a, _, params, gets := scoped(t)
	ctx := context.Background()

	_, out, err := svc.jiraSearch(ctx, reqFor(alice), JiraSearchInput{Text: "x"})
	if err != nil || strings.Join(params.Projects, ",") != "OD" || len(out.Issues) != 1 || out.Issues[0].Key != "OD-1" {
		t.Fatalf("search: projects=%v out=%+v err=%v", params.Projects, out, err)
	}
	if _, _, err := svc.jiraSearch(ctx, reqFor(alice), JiraSearchInput{JQL: "project = HR"}); err == nil || !strings.Contains(err.Error(), "raw jql") {
		t.Fatalf("raw jql with a project allowlist: %v", err)
	}
	if _, _, err := svc.jiraSearch(ctx, reqFor(alice), JiraSearchInput{Text: "x", Projects: []string{"HR"}}); err == nil {
		t.Fatal("project outside the scope accepted")
	}

	*gets = 0
	if _, is, err := svc.jiraGetIssue(ctx, reqFor(alice), JiraGetIssueInput{Key: "HR-5"}); is != nil || err == nil || *gets != 0 {
		t.Fatalf("issue outside the scope: issue=%v err=%v requests=%d", is, err, *gets)
	}
	if _, is, err := svc.jiraGetIssue(ctx, reqFor(alice), JiraGetIssueInput{Key: "OD-9"}); is != nil || err == nil {
		t.Fatalf("issue moved out of the scope returned: %+v %v", is, err)
	}
	last := a.entries[len(a.entries)-1]
	if last.Status != statusOutOfScope || strings.Join(last.Objects, ",") != "OD-9,HR-9" {
		t.Fatalf("audit for moved issue = %+v", last)
	}
	if _, is, err := svc.jiraGetIssue(ctx, reqFor(alice), JiraGetIssueInput{Key: " od-1 "}); err != nil || is.Key != "OD-1" {
		t.Fatalf("issue in scope: %+v %v", is, err)
	}

	// Unrestricted projects keep raw JQL available.
	svc.Projects = scope.All()
	if _, _, err := svc.jiraSearch(ctx, reqFor(alice), JiraSearchInput{JQL: "project = HR"}); err != nil {
		t.Fatalf("raw jql without allowlist: %v", err)
	}
}

func TestScopeZeroValueDeniesEverything(t *testing.T) {
	svc, _ := newService(&fakeCreds{}, fakeConfluence{})
	svc.Spaces, svc.Projects = scope.Set{}, scope.Set{}
	ctx := context.Background()
	if _, out, _ := svc.confluenceSearch(ctx, reqFor(alice), ConfluenceSearchInput{Query: "x"}); len(out.Results) != 0 {
		t.Fatalf("unconfigured scope returned %d results", len(out.Results))
	}
	if _, p, _ := svc.confluenceGetPage(ctx, reqFor(alice), ConfluenceGetPageInput{Page: "1"}); p != nil {
		t.Fatal("unconfigured scope returned a page")
	}
	if _, is, _ := svc.jiraGetIssue(ctx, reqFor(alice), JiraGetIssueInput{Key: "OD-1"}); is != nil {
		t.Fatal("unconfigured scope returned an issue")
	}
}

func TestToolDescriptionsListScope(t *testing.T) {
	svc, _, _, _, _ := scoped(t)
	if d := svc.jiraSearchDescription(); !strings.Contains(d, "Raw JQL is not available") || !strings.Contains(d, "OD") {
		t.Fatalf("restricted description = %q", d)
	}
	svc.Projects = scope.All()
	if d := svc.jiraSearchDescription(); !strings.Contains(d, "raw JQL query") {
		t.Fatalf("unrestricted description = %q", d)
	}
}
