package atlassian

import (
	"slices"
	"strings"
	"testing"
)

func TestSearchWords(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"Иван Иванов", []string{"Иван", "Иванов"}},
		{`  регламент   выкаток  `, []string{"регламент", "выкаток"}},
		{`x" OR space = "CS`, []string{"x", "space", "CS"}},
		{`#1234 — v2.0`, []string{"#1234", "v2.0"}},
		{`a AND b or c NOT d`, []string{"a", "b", "or", "c", "d"}},
		{`(title:secret*)~2 + -foo`, []string{"title", "secret", "2", "foo"}},
		{"line\nbreak\ttab\x00nul", []string{"line", "break", "tab", "nul"}},
		{`"""`, nil},
	}
	for _, tt := range tests {
		if got := SearchWords(tt.in); !slices.Equal(got, tt.want) {
			t.Errorf("SearchWords(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
	long := strings.Repeat("w ", 50)
	if n := len(SearchWords(long)); n != MaxSearchWords {
		t.Errorf("word limit: got %d", n)
	}
}

func TestQuote(t *testing.T) {
	tests := map[string]string{
		`plain`:            `"plain"`,
		`a"b`:              `"a\"b"`,
		`back\slash`:       `"back\\slash"`,
		`x" OR space="CS`:  `"x\" OR space=\"CS"`,
		"new\nline":        `"new line"`,
		`Кадровые вопросы`: `"Кадровые вопросы"`,
	}
	for in, want := range tests {
		if got := Quote(in); got != want {
			t.Errorf("Quote(%q) = %s, want %s", in, got, want)
		}
	}
}

// FuzzQuote checks that a quoted literal can never be terminated early.
func FuzzQuote(f *testing.F) {
	for _, s := range []string{`"`, `\"`, `\\"`, "a\"b\\", `" OR 1=1 OR "`} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		q := Quote(s)
		if q[0] != '"' || q[len(q)-1] != '"' {
			t.Fatalf("not wrapped: %s", q)
		}
		inner := q[1 : len(q)-1]
		escaped := false
		for i := 0; i < len(inner); i++ {
			switch {
			case escaped:
				escaped = false
			case inner[i] == '\\':
				escaped = true
			case inner[i] == '"':
				t.Fatalf("unescaped quote at %d in %s", i, q)
			}
		}
		if escaped {
			t.Fatalf("dangling escape in %s", q)
		}
	})
}
