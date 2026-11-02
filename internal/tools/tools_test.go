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

type fakeConfluence struct{ err error }

func (f fakeConfluence) Search(context.Context, atlassian.Auth, confluence.SearchParams) ([]confluence.SearchResult, error) {
	if f.err != nil {
		return nil, f.err
	}
	return []confluence.SearchResult{
		{ID: "1", Title: "Секретный заголовок", Excerpt: "секретное содержимое"},
		{ID: "2", Title: "Другой", Excerpt: "текст"},
	}, nil
}

func (f fakeConfluence) GetPage(context.Context, atlassian.Auth, string) (*confluence.Page, error) {
	return nil, f.err
}

type fakeJira struct{}

func (fakeJira) Search(context.Context, atlassian.Auth, jira.SearchParams) (*jira.SearchResult, error) {
	return &jira.SearchResult{Issues: []jira.IssueSummary{}}, nil
}
func (fakeJira) GetIssue(context.Context, atlassian.Auth, string) (*jira.Issue, error) {
	return &jira.Issue{}, nil
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
	if e.Status != statusOK || e.Results != 2 || e.Principal != "alice" || e.Tool != "confluence_search" {
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
