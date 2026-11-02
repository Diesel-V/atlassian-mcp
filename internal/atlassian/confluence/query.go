// Package confluence implements search and page retrieval against Confluence Data Center.
package confluence

import (
	"regexp"
	"strings"
	"time"

	"atlassian-mcp/internal/atlassian"
)

// Mode controls how free-text words are combined.
type Mode string

const (
	// ModeAll matches content containing every word in any order (default).
	ModeAll Mode = "all"
	// ModePhrase matches the exact phrase.
	ModePhrase Mode = "phrase"
)

// SearchParams is a validated-on-build search request.
type SearchParams struct {
	Query         string
	Mode          Mode
	Spaces        []string
	Author        string
	ModifiedAfter time.Time
	TitleOnly     bool
	Limit         int
}

var (
	spaceKeyRe = regexp.MustCompile(`^~?[A-Za-z0-9][A-Za-z0-9._-]{0,254}$`)
	usernameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@-]{0,254}$`)
)

// ErrInvalidParams is re-exported for convenience.
var ErrInvalidParams = atlassian.ErrInvalidParams

var invalid = atlassian.Invalid

// BuildCQL renders params into CQL. Every user-controlled value is either
// reduced to plain words or validated against a strict pattern, and always quoted.
func BuildCQL(p SearchParams) (string, error) {
	var clauses []string

	words := atlassian.SearchWords(p.Query)
	if strings.TrimSpace(p.Query) != "" && len(words) == 0 {
		return "", invalid("query contains no searchable words")
	}
	field := "text"
	if p.TitleOnly {
		field = "title"
	}
	switch p.Mode {
	case "", ModeAll:
		for _, w := range words {
			clauses = append(clauses, field+" ~ "+atlassian.Quote(w))
		}
	case ModePhrase:
		if len(words) > 0 {
			// The inner quotes make Lucene treat the words as a phrase.
			clauses = append(clauses, field+" ~ "+atlassian.Quote(`"`+strings.Join(words, " ")+`"`))
		}
	default:
		return "", invalid("mode must be %q or %q", ModeAll, ModePhrase)
	}

	if len(p.Spaces) > 0 {
		quoted := make([]string, 0, len(p.Spaces))
		for _, s := range p.Spaces {
			s = strings.TrimSpace(s)
			if !spaceKeyRe.MatchString(s) {
				return "", invalid("invalid space key %q", s)
			}
			quoted = append(quoted, atlassian.Quote(s))
		}
		clauses = append(clauses, "space in ("+strings.Join(quoted, ", ")+")")
	}

	if p.Author != "" {
		if !usernameRe.MatchString(p.Author) {
			return "", invalid("invalid author username %q", p.Author)
		}
		clauses = append(clauses, "creator = "+atlassian.Quote(p.Author))
	}

	if !p.ModifiedAfter.IsZero() {
		clauses = append(clauses, "lastmodified >= "+atlassian.Quote(p.ModifiedAfter.Format("2006-01-02")))
	}

	if len(clauses) == 0 {
		return "", invalid("specify a query, author or spaces")
	}
	clauses = append(clauses, `type in ("page", "blogpost")`)

	cql := strings.Join(clauses, " AND ")
	if len(words) == 0 {
		cql += " ORDER BY lastmodified DESC" // no relevance ranking without text
	}
	return cql, nil
}
