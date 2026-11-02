package jira

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"atlassian-mcp/internal/atlassian"
)

// Client talks to the Jira Data Center REST API v2.
type Client struct {
	api             *atlassian.Client
	baseURL         string
	maxResults      int
	maxContentChars int
	maxComments     int
}

// NewClient creates a client for baseURL (e.g. https://jira.example.com).
func NewClient(baseURL string, httpClient *http.Client, maxResults, maxContentChars int) *Client {
	base := strings.TrimRight(baseURL, "/")
	return &Client{
		api:             &atlassian.Client{BaseURL: base, HTTP: httpClient},
		baseURL:         base,
		maxResults:      maxResults,
		maxContentChars: maxContentChars,
		maxComments:     20,
	}
}

// IssueSummary is a search hit.
type IssueSummary struct {
	Key      string `json:"key"`
	Summary  string `json:"summary"`
	Status   string `json:"status,omitempty"`
	Type     string `json:"type,omitempty"`
	Priority string `json:"priority,omitempty"`
	Assignee string `json:"assignee,omitempty"`
	Updated  string `json:"updated,omitempty"`
	URL      string `json:"url"`
}

// Comment is an issue comment.
type Comment struct {
	Author  string `json:"author,omitempty"`
	Created string `json:"created,omitempty"`
	Body    string `json:"body"`
}

// Issue is a full issue view.
type Issue struct {
	IssueSummary
	Project     string    `json:"project,omitempty"`
	Reporter    string    `json:"reporter,omitempty"`
	Created     string    `json:"created,omitempty"`
	Labels      []string  `json:"labels,omitempty"`
	Description string    `json:"description,omitempty"`
	Comments    []Comment `json:"comments,omitempty"`
	Truncated   bool      `json:"truncated,omitempty"`
}

// SearchResult is the outcome of a search.
type SearchResult struct {
	Total  int            `json:"total"`
	Issues []IssueSummary `json:"issues"`
}

type named struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
}

type apiFields struct {
	Summary     string   `json:"summary"`
	Status      *named   `json:"status"`
	IssueType   *named   `json:"issuetype"`
	Priority    *named   `json:"priority"`
	Assignee    *named   `json:"assignee"`
	Reporter    *named   `json:"reporter"`
	Project     *named   `json:"project"`
	Created     string   `json:"created"`
	Updated     string   `json:"updated"`
	Labels      []string `json:"labels"`
	Description string   `json:"description"`
	Comment     *struct {
		Comments []struct {
			Author  *named `json:"author"`
			Created string `json:"created"`
			Body    string `json:"body"`
		} `json:"comments"`
	} `json:"comment"`
}

type apiIssue struct {
	Key    string    `json:"key"`
	Fields apiFields `json:"fields"`
}

var summaryFields = []string{"summary", "status", "issuetype", "priority", "assignee", "updated"}

// Search runs a JQL search.
func (c *Client) Search(ctx context.Context, auth atlassian.Auth, p SearchParams) (*SearchResult, error) {
	jql, err := BuildJQL(p)
	if err != nil {
		return nil, err
	}
	limit := p.Limit
	if limit <= 0 || limit > c.maxResults {
		limit = c.maxResults
	}
	req := map[string]any{"jql": jql, "maxResults": limit, "fields": summaryFields}
	var resp struct {
		Total  int        `json:"total"`
		Issues []apiIssue `json:"issues"`
	}
	if err := c.api.DoJSON(ctx, auth, http.MethodPost, "/rest/api/2/search", req, &resp); err != nil {
		return nil, err
	}
	out := &SearchResult{Total: resp.Total, Issues: make([]IssueSummary, 0, len(resp.Issues))}
	for _, is := range resp.Issues {
		out.Issues = append(out.Issues, c.summary(is))
	}
	return out, nil
}

// GetIssue loads one issue with description and the latest comments.
func (c *Client) GetIssue(ctx context.Context, auth atlassian.Auth, key string) (*Issue, error) {
	key = strings.ToUpper(strings.TrimSpace(key))
	if !ValidIssueKey(key) {
		return nil, atlassian.Invalid("invalid issue key %q", key)
	}
	fields := append([]string{"project", "reporter", "created", "labels", "description", "comment"}, summaryFields...)
	path := "/rest/api/2/issue/" + url.PathEscape(key) + "?fields=" + strings.Join(fields, ",")
	var is apiIssue
	if err := c.api.DoJSON(ctx, auth, http.MethodGet, path, nil, &is); err != nil {
		return nil, err
	}

	out := &Issue{IssueSummary: c.summary(is), Created: is.Fields.Created, Labels: is.Fields.Labels}
	out.Project = displayName(is.Fields.Project)
	out.Reporter = displayName(is.Fields.Reporter)

	budget := c.maxContentChars
	out.Description, out.Truncated = take(is.Fields.Description, &budget)
	if is.Fields.Comment != nil {
		comments := is.Fields.Comment.Comments
		if len(comments) > c.maxComments {
			comments = comments[len(comments)-c.maxComments:]
			out.Truncated = true
		}
		for _, cm := range comments {
			body, cut := take(cm.Body, &budget)
			out.Comments = append(out.Comments, Comment{Author: displayName(cm.Author), Created: cm.Created, Body: body})
			out.Truncated = out.Truncated || cut
			if budget == 0 {
				break
			}
		}
	}
	return out, nil
}

// WhoAmI returns the username the credentials belong to.
func (c *Client) WhoAmI(ctx context.Context, auth atlassian.Auth) (string, error) {
	var me named
	if err := c.api.DoJSON(ctx, auth, http.MethodGet, "/rest/api/2/myself", nil, &me); err != nil {
		return "", err
	}
	if me.Name == "" {
		return "", fmt.Errorf("%w: anonymous user", atlassian.ErrUnauthorized)
	}
	return me.Name, nil
}

func (c *Client) summary(is apiIssue) IssueSummary {
	return IssueSummary{
		Key:      is.Key,
		Summary:  is.Fields.Summary,
		Status:   displayName(is.Fields.Status),
		Type:     displayName(is.Fields.IssueType),
		Priority: displayName(is.Fields.Priority),
		Assignee: displayName(is.Fields.Assignee),
		Updated:  is.Fields.Updated,
		URL:      c.baseURL + "/browse/" + is.Key,
	}
}

func displayName(n *named) string {
	if n == nil {
		return ""
	}
	if n.DisplayName != "" {
		return n.DisplayName
	}
	return n.Name
}

// take returns s limited to the remaining rune budget and decrements it.
func take(s string, budget *int) (string, bool) {
	r := []rune(s)
	if len(r) <= *budget {
		*budget -= len(r)
		return s, false
	}
	out := string(r[:*budget])
	*budget = 0
	return out, true
}
