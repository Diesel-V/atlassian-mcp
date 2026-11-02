package identity

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"

	"atlassian-mcp/internal/store"
)

// subjectKey is the TokenInfo.Extra key holding the Subject.
const subjectKey = "subject"

// Resolver maps a bearer credential to a Subject.
type Resolver struct {
	JWT         TokenVerifier
	Keys        store.APIKeys
	Agents      store.Agents
	UserClients []string // azp values accepted as end-user tokens
	StripDomain string   // removed from usernames: alice@example.com -> alice
	Now         func() time.Time
	Log         *slog.Logger
}

// Resolve authenticates a raw bearer token.
func (r *Resolver) Resolve(ctx context.Context, raw string) (Subject, time.Time, error) {
	if strings.HasPrefix(raw, APIKeyPrefix) {
		return r.resolveKey(ctx, raw)
	}
	return r.resolveJWT(ctx, raw)
}

func (r *Resolver) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Resolver) resolveKey(ctx context.Context, raw string) (Subject, time.Time, error) {
	id, secret, err := ParseAPIKey(raw)
	if err != nil {
		return Subject{}, time.Time{}, auth.ErrInvalidToken
	}
	k, err := r.Keys.GetAPIKey(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return Subject{}, time.Time{}, auth.ErrInvalidToken
	}
	if err != nil {
		return Subject{}, time.Time{}, err
	}
	now := r.now()
	if !SecretMatches(secret, k.SecretHash) || !k.Active(now) {
		return Subject{}, time.Time{}, auth.ErrInvalidToken
	}
	if err := r.Keys.TouchAPIKey(ctx, id, now); err != nil && r.Log != nil {
		r.Log.Warn("touch api key", "key_id", id, "err", err)
	}
	return Subject{Kind: KindAPIKey, Username: k.Username, KeyID: k.ID}, k.ExpiresAt, nil
}

func (r *Resolver) resolveJWT(ctx context.Context, raw string) (Subject, time.Time, error) {
	c, err := r.JWT.Verify(ctx, raw)
	if err != nil {
		return Subject{}, time.Time{}, fmt.Errorf("%w: %v", auth.ErrInvalidToken, err)
	}
	if c.AuthorizedParty == "" {
		return Subject{}, time.Time{}, auth.ErrInvalidToken
	}
	if slices.Contains(r.UserClients, c.AuthorizedParty) {
		user := NormalizeUsername(c.Username, r.StripDomain)
		if user == "" || strings.HasPrefix(user, "service-account-") {
			return Subject{}, time.Time{}, auth.ErrInvalidToken
		}
		return Subject{Kind: KindUser, Username: user}, c.Expiry, nil
	}
	a, err := r.Agents.GetAgent(ctx, c.AuthorizedParty)
	if errors.Is(err, store.ErrNotFound) || (err == nil && !a.Enabled) {
		return Subject{}, c.Expiry, ErrForbidden
	}
	if err != nil {
		return Subject{}, time.Time{}, err
	}
	return Subject{Kind: KindAgent, AgentID: a.ClientID}, c.Expiry, nil
}

// ErrForbidden means the token is valid but its client is not allowed to use the service.
var ErrForbidden = errors.New("identity: client not registered")

// NormalizeUsername lower-cases and strips the configured e-mail domain.
func NormalizeUsername(name, stripDomain string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if stripDomain != "" {
		name = strings.TrimSuffix(name, "@"+strings.ToLower(stripDomain))
	}
	return name
}

// ScopeAccess is granted to every subject allowed to use the service. The MCP
// handler requires it, so a valid token of an unregistered client is answered
// with 403 rather than 401: the caller is known, it is just not allowed in.
const ScopeAccess = "atlassian-mcp:access"

// TokenVerifier adapts the resolver to the MCP SDK bearer middleware.
func (r *Resolver) TokenVerifier() auth.TokenVerifier {
	return func(ctx context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
		s, exp, err := r.Resolve(ctx, token)
		if errors.Is(err, ErrForbidden) {
			if r.Log != nil {
				r.Log.Warn("token from unregistered or disabled client rejected")
			}
			// No subject and no access scope: the middleware answers 403.
			return &auth.TokenInfo{Expiration: exp}, nil
		}
		if errors.Is(err, auth.ErrInvalidToken) {
			return nil, auth.ErrInvalidToken // do not echo verification details to the client
		}
		if err != nil {
			if r.Log != nil {
				r.Log.Error("resolve bearer token", "err", err)
			}
			return nil, errors.New("internal error") // the SDK writes the message to the response
		}
		return &auth.TokenInfo{
			Scopes:     []string{ScopeAccess},
			Expiration: exp,
			UserID:     s.Principal(),
			Extra:      map[string]any{subjectKey: s},
		}, nil
	}
}

// SubjectFromTokenInfo extracts the subject placed by TokenVerifier.
func SubjectFromTokenInfo(ti *auth.TokenInfo) (Subject, bool) {
	if ti == nil {
		return Subject{}, false
	}
	s, ok := ti.Extra[subjectKey].(Subject)
	return s, ok
}
