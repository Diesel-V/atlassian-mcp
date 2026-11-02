// Package tools exposes Confluence and Jira as MCP tools.
// Every call runs under the caller's own Atlassian credentials and is audited.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"atlassian-mcp/internal/atlassian"
	"atlassian-mcp/internal/atlassian/confluence"
	"atlassian-mcp/internal/atlassian/jira"
	"atlassian-mcp/internal/identity"
	"atlassian-mcp/internal/metrics"
	"atlassian-mcp/internal/store"
)

// ConfluenceAPI is the subset of the Confluence client used by tools.
type ConfluenceAPI interface {
	Search(ctx context.Context, auth atlassian.Auth, p confluence.SearchParams) ([]confluence.SearchResult, error)
	GetPage(ctx context.Context, auth atlassian.Auth, ref string) (*confluence.Page, error)
}

// JiraAPI is the subset of the Jira client used by tools.
type JiraAPI interface {
	Search(ctx context.Context, auth atlassian.Auth, p jira.SearchParams) (*jira.SearchResult, error)
	GetIssue(ctx context.Context, auth atlassian.Auth, key string) (*jira.Issue, error)
}

// CredentialSource resolves per-subject Atlassian credentials.
type CredentialSource interface {
	Auth(ctx context.Context, s identity.Subject, p store.Provider) (atlassian.Auth, error)
	Invalidate(ctx context.Context, s identity.Subject, p store.Provider) error
}

// Auditor records tool calls.
type Auditor interface {
	Record(ctx context.Context, e *store.AuditEntry)
}

// RateLimiter decides whether a principal may call a tool now.
type RateLimiter interface {
	Allow(key string) bool
}

// Metrics are the tool-level instruments.
type Metrics struct {
	Calls    *metrics.CounterVec
	Duration *metrics.HistogramVec
}

// NewMetrics registers tool metrics in r.
func NewMetrics(r *metrics.Registry) *Metrics {
	return &Metrics{
		Calls:    r.NewCounterVec("amcp_tool_calls_total", "MCP tool calls by tool, subject kind and status.", "tool", "subject_kind", "status"),
		Duration: r.NewHistogramVec("amcp_tool_duration_seconds", "MCP tool call latency.", metrics.DefaultDurationBuckets, "tool"),
	}
}

// Service holds tool dependencies. Confluence or Jira may be nil when disabled.
type Service struct {
	Confluence ConfluenceAPI
	Jira       JiraAPI
	Creds      CredentialSource
	Audit      Auditor
	Limiter    RateLimiter
	Metrics    *Metrics
	PublicURL  string
	Now        func() time.Time
}

// Audit statuses.
const (
	statusOK           = "ok"
	statusError        = "error"
	statusInvalid      = "invalid"
	statusNotFound     = "not_found"
	statusNotConnected = "not_connected"
	statusRateLimited  = "rate_limited"
	statusUnauth       = "unauthenticated"
)

// Register adds the enabled tools to server.
func (s *Service) Register(server *mcp.Server) {
	if s.Confluence != nil {
		mcp.AddTool(server, &mcp.Tool{
			Name: "confluence_search",
			Description: "Search Confluence pages and blog posts the current user is allowed to read. " +
				"By default every word must occur (any order); use mode=phrase for an exact phrase. " +
				"Filter by space keys, author username (e.g. i.ivanov) or modification date. " +
				"Use the person's name exactly as given; never invent a patronymic. " +
				"Returns titles, excerpts and links; call confluence_get_page to read a page.",
			Annotations: readOnly("Search Confluence"),
		}, s.confluenceSearch)
		mcp.AddTool(server, &mcp.Tool{
			Name:        "confluence_get_page",
			Description: "Read a Confluence page by numeric ID or by its URL. Returns the page as Markdown with metadata and link.",
			Annotations: readOnly("Read Confluence page"),
		}, s.confluenceGetPage)
	}
	if s.Jira != nil {
		mcp.AddTool(server, &mcp.Tool{
			Name: "jira_search",
			Description: "Search Jira issues the current user is allowed to see. Use structured filters " +
				"(text, projects, statuses, assignee — 'me' for the current user, updated_after) or a raw JQL query, not both.",
			Annotations: readOnly("Search Jira"),
		}, s.jiraSearch)
		mcp.AddTool(server, &mcp.Tool{
			Name:        "jira_get_issue",
			Description: "Read a Jira issue by key (e.g. OD-123): summary, status, people, description and the latest comments.",
			Annotations: readOnly("Read Jira issue"),
		}, s.jiraGetIssue)
	}
}

func readOnly(title string) *mcp.ToolAnnotations {
	f := false
	return &mcp.ToolAnnotations{Title: title, ReadOnlyHint: true, OpenWorldHint: &f}
}

// --- Confluence ---

type ConfluenceSearchInput struct {
	Query         string   `json:"query,omitempty" jsonschema:"Words, a person's name or a phrase to search for"`
	Mode          string   `json:"mode,omitempty" jsonschema:"all (default): every word must occur; phrase: exact phrase"`
	Spaces        []string `json:"spaces,omitempty" jsonschema:"Limit to these space keys, e.g. TECHOPS"`
	Author        string   `json:"author,omitempty" jsonschema:"Username of the page creator, e.g. i.ivanov"`
	ModifiedAfter string   `json:"modified_after,omitempty" jsonschema:"Only content modified on or after this date, YYYY-MM-DD"`
	TitleOnly     bool     `json:"title_only,omitempty" jsonschema:"Search in titles only"`
	Limit         int      `json:"limit,omitempty" jsonschema:"Maximum number of results"`
}

type ConfluenceSearchOutput struct {
	Results []confluence.SearchResult `json:"results"`
}

func (s *Service) confluenceSearch(ctx context.Context, req *mcp.CallToolRequest, in ConfluenceSearchInput) (*mcp.CallToolResult, ConfluenceSearchOutput, error) {
	var out ConfluenceSearchOutput
	err := s.run(ctx, req, "confluence_search", store.Confluence, in, func(auth atlassian.Auth) (int, error) {
		after, err := atlassian.ParseDate(in.ModifiedAfter)
		if err != nil {
			return 0, err
		}
		res, err := s.Confluence.Search(ctx, auth, confluence.SearchParams{
			Query: in.Query, Mode: confluence.Mode(in.Mode), Spaces: in.Spaces, Author: in.Author,
			ModifiedAfter: after, TitleOnly: in.TitleOnly, Limit: in.Limit,
		})
		out.Results = res
		return len(res), err
	})
	return nil, out, err
}

type ConfluenceGetPageInput struct {
	Page string `json:"page" jsonschema:"Page ID or full Confluence URL"`
}

func (s *Service) confluenceGetPage(ctx context.Context, req *mcp.CallToolRequest, in ConfluenceGetPageInput) (*mcp.CallToolResult, *confluence.Page, error) {
	var out *confluence.Page
	err := s.run(ctx, req, "confluence_get_page", store.Confluence, in, func(auth atlassian.Auth) (int, error) {
		p, err := s.Confluence.GetPage(ctx, auth, in.Page)
		out = p
		return boolToInt(p != nil), err
	})
	return nil, out, err
}

// --- Jira ---

type JiraSearchInput struct {
	Text         string   `json:"text,omitempty" jsonschema:"Words to search in summary, description and comments"`
	JQL          string   `json:"jql,omitempty" jsonschema:"Raw JQL query; cannot be combined with other filters"`
	Projects     []string `json:"projects,omitempty" jsonschema:"Project keys, e.g. OD"`
	Statuses     []string `json:"statuses,omitempty" jsonschema:"Status names, e.g. In Progress"`
	Assignee     string   `json:"assignee,omitempty" jsonschema:"Assignee username, or 'me' for the current user"`
	UpdatedAfter string   `json:"updated_after,omitempty" jsonschema:"Only issues updated on or after this date, YYYY-MM-DD"`
	Limit        int      `json:"limit,omitempty" jsonschema:"Maximum number of issues"`
}

func (s *Service) jiraSearch(ctx context.Context, req *mcp.CallToolRequest, in JiraSearchInput) (*mcp.CallToolResult, *jira.SearchResult, error) {
	var out *jira.SearchResult
	err := s.run(ctx, req, "jira_search", store.Jira, in, func(auth atlassian.Auth) (int, error) {
		after, err := atlassian.ParseDate(in.UpdatedAfter)
		if err != nil {
			return 0, err
		}
		res, err := s.Jira.Search(ctx, auth, jira.SearchParams{
			Text: in.Text, JQL: in.JQL, Projects: in.Projects, Statuses: in.Statuses,
			Assignee: in.Assignee, UpdatedAfter: after, Limit: in.Limit,
		})
		out = res
		if res == nil {
			return 0, err
		}
		return len(res.Issues), err
	})
	return nil, out, err
}

type JiraGetIssueInput struct {
	Key string `json:"key" jsonschema:"Issue key, e.g. OD-123"`
}

func (s *Service) jiraGetIssue(ctx context.Context, req *mcp.CallToolRequest, in JiraGetIssueInput) (*mcp.CallToolResult, *jira.Issue, error) {
	var out *jira.Issue
	err := s.run(ctx, req, "jira_get_issue", store.Jira, in, func(auth atlassian.Auth) (int, error) {
		is, err := s.Jira.GetIssue(ctx, auth, in.Key)
		out = is
		return boolToInt(is != nil), err
	})
	return nil, out, err
}

// --- shared execution path ---

// run authenticates, rate-limits, resolves credentials, calls fn, then audits.
// The returned error is shown to the model, so it is always user-facing and secret-free.
func (s *Service) run(ctx context.Context, req *mcp.CallToolRequest, tool string, p store.Provider, params any,
	fn func(atlassian.Auth) (results int, err error)) error {
	start := s.now()

	subject, ok := identity.Subject{}, false
	if req != nil && req.Extra != nil {
		subject, ok = identity.SubjectFromTokenInfo(req.Extra.TokenInfo)
	}
	entry := &store.AuditEntry{Time: start, Tool: tool, Params: marshalParams(params)}
	finish := func(status string, results int, err error) error {
		entry.Status, entry.Results = status, results
		entry.DurationMS = s.now().Sub(start).Milliseconds()
		if err != nil {
			entry.Error = truncate(err.Error(), 500)
		}
		s.Audit.Record(ctx, entry)
		s.Metrics.Calls.Inc(tool, entry.SubjectKind, status)
		s.Metrics.Duration.Observe(s.now().Sub(start).Seconds(), tool)
		return err
	}

	if !ok {
		return finish(statusUnauth, 0, errors.New("не удалось определить пользователя"))
	}
	entry.SubjectKind, entry.Principal, entry.KeyID = string(subject.Kind), subject.Principal(), subject.KeyID

	if !s.Limiter.Allow(subject.Principal()) {
		return finish(statusRateLimited, 0, errors.New("слишком много запросов, повторите через минуту"))
	}

	auth, err := s.Creds.Auth(ctx, subject, p)
	if err != nil {
		return finish(s.classifyCredErr(err, p))
	}

	results, err := fn(auth)
	if err == nil {
		return finish(statusOK, results, nil)
	}
	if errors.Is(err, atlassian.ErrUnauthorized) {
		// The product rejected a token we believed valid: drop it and ask to reconnect.
		_ = s.Creds.Invalidate(ctx, subject, p)
		return finish(s.classifyCredErr(atlassian.ErrNotConnected, p))
	}
	return finish(classifyCallErr(err, p))
}

func (s *Service) classifyCredErr(err error, p store.Provider) (string, int, error) {
	switch {
	case errors.Is(err, atlassian.ErrNotConnected):
		return statusNotConnected, 0, fmt.Errorf(
			"доступ к %s не подключён. Попросите пользователя открыть %s/connect/%s и разрешить доступ, затем повторите запрос",
			p.Title(), s.PublicURL, p)
	case errors.Is(err, atlassian.ErrNoAgentAccess):
		return statusNotConnected, 0, fmt.Errorf("агенту не выдан доступ к %s", p.Title())
	case errors.Is(err, atlassian.ErrUpstream):
		return statusError, 0, fmt.Errorf("%s временно недоступен, повторите позже", p.Title())
	default:
		return statusError, 0, errors.New("внутренняя ошибка при получении доступа")
	}
}

func classifyCallErr(err error, p store.Provider) (string, int, error) {
	var apiErr *atlassian.APIError
	switch {
	case errors.Is(err, atlassian.ErrInvalidParams):
		return statusInvalid, 0, err
	case errors.Is(err, atlassian.ErrNotFound):
		return statusNotFound, 0, fmt.Errorf("не найдено в %s или нет прав доступа", p.Title())
	case errors.Is(err, atlassian.ErrBadRequest) && errors.As(err, &apiErr) && apiErr.Message != "":
		return statusInvalid, 0, fmt.Errorf("%s отклонил запрос: %s", p.Title(), apiErr.Message)
	case errors.Is(err, atlassian.ErrBadRequest):
		return statusInvalid, 0, fmt.Errorf("%s отклонил запрос", p.Title())
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled):
		return statusError, 0, fmt.Errorf("%s не ответил вовремя", p.Title())
	default:
		return statusError, 0, fmt.Errorf("ошибка обращения к %s, повторите позже", p.Title())
	}
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func marshalParams(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
