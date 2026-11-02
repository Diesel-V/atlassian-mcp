package atlassian

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
)

// MaxSearchWords bounds the number of terms taken from free text.
const MaxSearchWords = 12

// luceneSpecial are characters with meaning in the Lucene syntax used by
// CQL/JQL "~" operators. Free text is reduced to plain words before it is quoted,
// so user input can only ever be searched for, never interpreted.
const luceneSpecial = `+-&|!(){}[]^"~*?:\/'`

var luceneOperators = map[string]bool{"AND": true, "OR": true, "NOT": true, "TO": true}

// SearchWords normalises free text into at most MaxSearchWords plain terms.
func SearchWords(text string) []string {
	cleaned := strings.Map(func(r rune) rune {
		if strings.ContainsRune(luceneSpecial, r) || unicode.IsControl(r) {
			return ' '
		}
		return r
	}, text)
	var words []string
	for _, w := range strings.Fields(cleaned) {
		if luceneOperators[w] || !strings.ContainsFunc(w, isWordRune) {
			continue
		}
		words = append(words, w)
		if len(words) == MaxSearchWords {
			break
		}
	}
	return words
}

func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

// Quote renders s as a CQL/JQL string literal. Both languages use
// double quotes with backslash escapes; control characters are dropped.
func Quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case unicode.IsControl(r):
			b.WriteByte(' ')
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// ErrInvalidParams wraps validation failures; the message is safe to show to the model.
var ErrInvalidParams = errors.New("invalid parameters")

// Invalid formats an ErrInvalidParams error.
func Invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidParams, fmt.Sprintf(format, args...))
}

// ParseDate parses an optional YYYY-MM-DD tool argument.
func ParseDate(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}, Invalid("date %q must be YYYY-MM-DD", s)
	}
	return t, nil
}
