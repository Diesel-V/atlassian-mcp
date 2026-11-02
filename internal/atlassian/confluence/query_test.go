package confluence

import (
	"errors"
	"testing"
	"time"
)

func TestBuildCQL(t *testing.T) {
	tests := []struct {
		name string
		p    SearchParams
		want string
	}{
		{
			"all words",
			SearchParams{Query: "Иван Иванов"},
			`text ~ "Иван" AND text ~ "Иванов" AND type in ("page", "blogpost")`,
		},
		{
			"phrase",
			SearchParams{Query: "регламент выкаток", Mode: ModePhrase},
			`text ~ "\"регламент выкаток\"" AND type in ("page", "blogpost")`,
		},
		{
			"title only with spaces",
			SearchParams{Query: "Kafka", TitleOnly: true, Spaces: []string{"TECHOPS", " DBA ", "~s.sidorov"}},
			`title ~ "Kafka" AND space in ("TECHOPS", "DBA", "~s.sidorov") AND type in ("page", "blogpost")`,
		},
		{
			"author only",
			SearchParams{Author: "i.ivanov"},
			`creator = "i.ivanov" AND type in ("page", "blogpost") ORDER BY lastmodified DESC`,
		},
		{
			"modified after",
			SearchParams{Query: "ретро", ModifiedAfter: time.Date(2026, 10, 1, 15, 0, 0, 0, time.UTC)},
			`text ~ "ретро" AND lastmodified >= "2026-10-01" AND type in ("page", "blogpost")`,
		},
		{
			"injection attempt is reduced to words",
			SearchParams{Query: `x" OR space = "CS" OR text ~ "y`},
			`text ~ "x" AND text ~ "space" AND text ~ "CS" AND text ~ "text" AND text ~ "y" AND type in ("page", "blogpost")`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := BuildCQL(tt.p)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("\n got: %s\nwant: %s", got, tt.want)
			}
		})
	}
}

func TestBuildCQLRejects(t *testing.T) {
	tests := map[string]SearchParams{
		"empty":            {},
		"only punctuation": {Query: `"*()"`},
		"bad mode":         {Query: "x", Mode: "regex"},
		"space injection":  {Query: "x", Spaces: []string{`TECHOPS") OR space in ("CS`}},
		"author injection": {Author: `a" OR creator != "b`},
		"empty space":      {Query: "x", Spaces: []string{" "}},
	}
	for name, p := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := BuildCQL(p); !errors.Is(err, ErrInvalidParams) {
				t.Fatalf("err = %v, want ErrInvalidParams", err)
			}
		})
	}
}
