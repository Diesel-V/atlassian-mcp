package jira

import (
	"errors"
	"strings"
	"testing"
	"time"

	"atlassian-mcp/internal/atlassian"
)

func TestBuildJQL(t *testing.T) {
	tests := []struct {
		name string
		p    SearchParams
		want string
	}{
		{"text", SearchParams{Text: "падает оплата"},
			`text ~ "падает оплата" ORDER BY updated DESC`},
		{"filters", SearchParams{Projects: []string{"od", " PROIL "}, Statuses: []string{"In Progress", "В работе"}, Assignee: "me"},
			`project in ("OD", "PROIL") AND status in ("In Progress", "В работе") AND assignee = currentUser() ORDER BY updated DESC`},
		{"assignee and date", SearchParams{Assignee: "p.petrov", UpdatedAfter: time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)},
			`assignee = "p.petrov" AND updated >= "2026-10-20" ORDER BY updated DESC`},
		{"status with quote is escaped", SearchParams{Statuses: []string{`Done") OR project = "SEC`}},
			`status in ("Done\") OR project = \"SEC") ORDER BY updated DESC`},
		{"text injection reduced", SearchParams{Text: `x" OR project = "SEC`},
			`text ~ "x project SEC" ORDER BY updated DESC`},
		{"raw jql passthrough", SearchParams{JQL: `  project = OD AND resolution is EMPTY  `},
			`project = OD AND resolution is EMPTY`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := BuildJQL(tt.p)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("\n got: %s\nwant: %s", got, tt.want)
			}
		})
	}
}

func TestBuildJQLRejects(t *testing.T) {
	tests := map[string]SearchParams{
		"empty":           {},
		"bad project":     {Projects: []string{`OD" OR 1=1`}},
		"bad assignee":    {Assignee: `a" OR assignee != "b`},
		"empty status":    {Statuses: []string{" "}},
		"jql plus filter": {JQL: "project = OD", Text: "x"},
		"jql too long":    {JQL: strings.Repeat("a", maxJQLLength+1)},
		"punctuation":     {Text: "?!*"},
	}
	for name, p := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := BuildJQL(p); !errors.Is(err, atlassian.ErrInvalidParams) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestValidIssueKey(t *testing.T) {
	for k, want := range map[string]bool{"OD-1": true, "PROIL_2-12345": true, "od-1": false, "OD-0": false, "OD": false, "OD-1 OR": false} {
		if got := ValidIssueKey(k); got != want {
			t.Errorf("ValidIssueKey(%q) = %v", k, got)
		}
	}
}
