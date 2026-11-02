// Package identity determines who is calling the MCP server.
package identity

import "context"

// Kind is the way a subject authenticated.
type Kind string

const (
	// KindUser is an end user authenticated by a Keycloak token (e.g. forwarded by Open WebUI).
	KindUser Kind = "user"
	// KindAPIKey is an agent acting on behalf of a user with that user's personal key.
	KindAPIKey Kind = "api_key"
	// KindAgent is an autonomous agent authenticated by Keycloak client credentials.
	KindAgent Kind = "agent"
)

// Subject is the authenticated caller.
type Subject struct {
	Kind Kind
	// Username is the Atlassian username for KindUser and KindAPIKey.
	Username string
	// KeyID identifies the personal key for KindAPIKey.
	KeyID string
	// AgentID is the Keycloak client ID for KindAgent.
	AgentID string
}

// Principal is a stable identifier used for audit and rate limiting.
func (s Subject) Principal() string {
	if s.Kind == KindAgent {
		return "agent:" + s.AgentID
	}
	return s.Username
}

// ActsAsUser reports whether the subject uses a user's own Atlassian grant.
func (s Subject) ActsAsUser() bool { return s.Kind == KindUser || s.Kind == KindAPIKey }

type ctxKey struct{}

// WithSubject returns a context carrying s.
func WithSubject(ctx context.Context, s Subject) context.Context {
	return context.WithValue(ctx, ctxKey{}, s)
}

// FromContext extracts the subject stored by WithSubject.
func FromContext(ctx context.Context) (Subject, bool) {
	s, ok := ctx.Value(ctxKey{}).(Subject)
	return s, ok
}
