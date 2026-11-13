// Package scope restricts the service to approved Confluence spaces and Jira
// projects. It is enforced on the server, independently of what the caller
// may see in the product itself.
package scope

import (
	"fmt"
	"slices"
	"strings"

	"atlassian-mcp/internal/atlassian"
)

// Set is an allowlist of space or project keys. The zero value allows nothing,
// so a forgotten configuration fails closed.
type Set struct {
	all  bool
	keys []string // as configured, used in queries
}

// All allows every key.
func All() Set { return Set{all: true} }

// Of allows exactly keys (deduplicated case-insensitively, order kept).
func Of(keys ...string) Set {
	var s Set
	for _, k := range keys {
		if k = strings.TrimSpace(k); k != "" && !s.Allows(k) {
			s.keys = append(s.keys, k)
		}
	}
	return s
}

// Parse builds a set from configuration: ["*"] means all keys.
func Parse(items []string) Set {
	if len(items) == 1 && strings.TrimSpace(items[0]) == "*" {
		return All()
	}
	return Of(items...)
}

// Unrestricted reports whether every key is allowed.
func (s Set) Unrestricted() bool { return s.all }

// Keys returns the allowed keys; nil when unrestricted.
func (s Set) Keys() []string { return slices.Clone(s.keys) }

// Allows reports whether key is in the set. Keys compare case-insensitively.
func (s Set) Allows(key string) bool {
	if s.all {
		return true
	}
	key = strings.TrimSpace(key)
	return key != "" && slices.ContainsFunc(s.keys, func(k string) bool { return strings.EqualFold(k, key) })
}

// Restrict turns the keys a caller asked for into the keys to query.
// No request means "all allowed keys" (nil when unrestricted); a key outside
// the set is rejected with a message listing what is available.
func (s Set) Restrict(requested []string, noun string) ([]string, error) {
	if len(requested) == 0 {
		return s.Keys(), nil
	}
	if s.all {
		return requested, nil
	}
	if len(s.keys) == 0 {
		return nil, atlassian.Invalid("no %s are available", noun)
	}
	for _, k := range requested {
		if !s.Allows(k) {
			return nil, atlassian.Invalid("%s %q is not available; available %s: %s", noun, strings.TrimSpace(k), noun, s.String())
		}
	}
	return requested, nil
}

// String lists the allowed keys for messages and tool descriptions.
func (s Set) String() string {
	if s.all {
		return "*"
	}
	return strings.Join(s.keys, ", ")
}

// Describe is a sentence for tool descriptions; empty when unrestricted.
func (s Set) Describe(noun string) string {
	if s.all {
		return ""
	}
	return fmt.Sprintf(" Only these %s are available: %s.", noun, s.String())
}
