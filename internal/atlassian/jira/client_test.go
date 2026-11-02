package jira

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"atlassian-mcp/internal/atlassian"
)

func newTestClient(t *testing.T, maxChars int, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return NewClient(srv.URL+"/", srv.Client(), 10, maxChars)
}

func TestSearch(t *testing.T) {
	c := newTestClient(t, 1000, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/rest/api/2/search" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var body struct {
			JQL        string   `json:"jql"`
			MaxResults int      `json:"maxResults"`
			Fields     []string `json:"fields"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.JQL != `project in ("OD") AND assignee = currentUser() ORDER BY updated DESC` || body.MaxResults != 10 {
			t.Errorf("request = %+v", body)
		}
		_, _ = io.WriteString(w, `{"total":42,"issues":[{"key":"OD-7","fields":{"summary":"Тарифы",
			"status":{"name":"In Progress"},"assignee":{"name":"e.k","displayName":"Эльдар"},"updated":"2026-10-30"}}]}`)
	})
	res, err := c.Search(context.Background(), nil, SearchParams{Projects: []string{"OD"}, Assignee: "me", Limit: 99})
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 42 || len(res.Issues) != 1 {
		t.Fatalf("res = %+v", res)
	}
	is := res.Issues[0]
	if is.Key != "OD-7" || is.Status != "In Progress" || is.Assignee != "Эльдар" || !strings.HasSuffix(is.URL, "/browse/OD-7") ||
		strings.Contains(is.URL, "//browse") {
		t.Errorf("issue = %+v", is)
	}
}

func TestGetIssue(t *testing.T) {
	var comments []string
	for i := 1; i <= 25; i++ {
		comments = append(comments, fmt.Sprintf(`{"author":{"name":"u%d"},"created":"c%d","body":"comment %d"}`, i, i, i))
	}
	c := newTestClient(t, 60, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rest/api/2/issue/OD-7" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"errorMessages":["Issue does not exist or you do not have permission to see it."]}`)
			return
		}
		if !strings.Contains(r.URL.Query().Get("fields"), "comment") {
			t.Errorf("fields = %s", r.URL.Query().Get("fields"))
		}
		_, _ = io.WriteString(w, `{"key":"OD-7","fields":{"summary":"Тарифы","project":{"name":"ОД"},
			"reporter":{"displayName":"Виктория"},"labels":["tariffs"],"description":"Описание задачи",
			"comment":{"comments":[`+strings.Join(comments, ",")+`]}}}`)
	})
	ctx := context.Background()

	is, err := c.GetIssue(ctx, nil, " od-7 ")
	if err != nil {
		t.Fatal(err)
	}
	if is.Project != "ОД" || is.Reporter != "Виктория" || is.Description != "Описание задачи" || !is.Truncated {
		t.Errorf("issue = %+v", is)
	}
	if len(is.Comments) == 0 || is.Comments[0].Body != "comment 6" || is.Comments[0].Author != "u6" {
		t.Errorf("must keep the latest 20 comments, got first %+v", is.Comments)
	}
	total := len([]rune(is.Description))
	for _, cm := range is.Comments {
		total += len([]rune(cm.Body))
	}
	if total > 60 {
		t.Errorf("content budget exceeded: %d", total)
	}

	if _, err := c.GetIssue(ctx, nil, "SEC-1"); !errors.Is(err, atlassian.ErrNotFound) {
		t.Errorf("missing: err = %v", err)
	}
	if _, err := c.GetIssue(ctx, nil, "OD-7/../../admin"); !errors.Is(err, atlassian.ErrInvalidParams) {
		t.Errorf("path injection: err = %v", err)
	}
}

func TestWhoAmI(t *testing.T) {
	c := newTestClient(t, 10, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"name":"i.ivanov","displayName":"Иван"}`)
	})
	if u, err := c.WhoAmI(context.Background(), atlassian.Bearer("t")); err != nil || u != "i.ivanov" {
		t.Fatalf("WhoAmI = %q %v", u, err)
	}
}
