// Package jira implements issue search and retrieval against Jira Data Center.
package jira

import (
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"atlassian-mcp/internal/atlassian"
)

// SearchParams describes an issue search. Either JQL or the structured
// fields are used; JQL runs as-is under the caller's own Jira permissions.
type SearchParams struct {
	Text         string
	JQL          string
	Projects     []string
	Statuses     []string
	Assignee     string // username or "me"
	UpdatedAfter time.Time
	Limit        int
}

const maxJQLLength = 2000

var (
	projectKeyRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,254}$`)
	issueKeyRe   = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,254}-[1-9][0-9]{0,9}$`)
	usernameRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@-]{0,254}$`)
)

// ValidIssueKey reports whether key looks like PROJ-123.
func ValidIssueKey(key string) bool { return issueKeyRe.MatchString(key) }

// BuildJQL renders params into JQL.
func BuildJQL(p SearchParams) (string, error) {
	if raw := strings.TrimSpace(p.JQL); raw != "" {
		if p.Text != "" || len(p.Projects) > 0 || len(p.Statuses) > 0 || p.Assignee != "" || !p.UpdatedAfter.IsZero() {
			return "", atlassian.Invalid("use either jql or structured filters, not both")
		}
		if utf8.RuneCountInString(raw) > maxJQLLength {
			return "", atlassian.Invalid("jql is longer than %d characters", maxJQLLength)
		}
		return raw, nil
	}

	var clauses []string
	words := atlassian.SearchWords(p.Text)
	if strings.TrimSpace(p.Text) != "" && len(words) == 0 {
		return "", atlassian.Invalid("text contains no searchable words")
	}
	if len(words) > 0 {
		clauses = append(clauses, "text ~ "+atlassian.Quote(strings.Join(words, " ")))
	}

	if len(p.Projects) > 0 {
		quoted := make([]string, 0, len(p.Projects))
		for _, k := range p.Projects {
			k = strings.ToUpper(strings.TrimSpace(k))
			if !projectKeyRe.MatchString(k) {
				return "", atlassian.Invalid("invalid project key %q", k)
			}
			quoted = append(quoted, atlassian.Quote(k))
		}
		clauses = append(clauses, "project in ("+strings.Join(quoted, ", ")+")")
	}

	if len(p.Statuses) > 0 {
		quoted := make([]string, 0, len(p.Statuses))
		for _, s := range p.Statuses {
			s = strings.TrimSpace(s)
			if s == "" || utf8.RuneCountInString(s) > 100 {
				return "", atlassian.Invalid("invalid status %q", s)
			}
			quoted = append(quoted, atlassian.Quote(s))
		}
		clauses = append(clauses, "status in ("+strings.Join(quoted, ", ")+")")
	}

	switch a := strings.TrimSpace(p.Assignee); {
	case a == "":
	case strings.EqualFold(a, "me"):
		clauses = append(clauses, "assignee = currentUser()")
	case usernameRe.MatchString(a):
		clauses = append(clauses, "assignee = "+atlassian.Quote(a))
	default:
		return "", atlassian.Invalid("invalid assignee %q", a)
	}

	if !p.UpdatedAfter.IsZero() {
		clauses = append(clauses, "updated >= "+atlassian.Quote(p.UpdatedAfter.Format("2006-01-02")))
	}

	if len(clauses) == 0 {
		return "", atlassian.Invalid("specify text, jql or at least one filter")
	}
	return strings.Join(clauses, " AND ") + " ORDER BY updated DESC", nil
}
