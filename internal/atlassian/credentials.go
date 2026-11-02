package atlassian

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/sync/singleflight"

	"atlassian-mcp/internal/identity"
	"atlassian-mcp/internal/store"
)

var (
	// ErrNotConnected means the user has not granted (or has revoked) access to the provider.
	ErrNotConnected = errors.New("atlassian: access not connected")
	// ErrNoAgentAccess means the agent has no credentials for the provider.
	ErrNoAgentAccess = errors.New("atlassian: agent has no access to this product")
)

// refreshMargin renews access tokens slightly before they expire.
const refreshMargin = time.Minute

// Credentials resolves the Atlassian credentials of a subject.
// There is deliberately no shared fallback account.
type Credentials struct {
	Tokens store.Tokens
	Agents store.Agents
	OAuth  map[store.Provider]*OAuthProvider
	Now    func() time.Time
	Log    *slog.Logger

	refresh singleflight.Group
}

func (c *Credentials) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Auth returns request credentials for subject s on provider p.
func (c *Credentials) Auth(ctx context.Context, s identity.Subject, p store.Provider) (Auth, error) {
	switch s.Kind {
	case identity.KindUser, identity.KindAPIKey:
		tok, err := c.userToken(ctx, s.Username, p)
		if err != nil {
			return nil, err
		}
		return Bearer(tok), nil
	case identity.KindAgent:
		a, err := c.Agents.GetAgent(ctx, s.AgentID)
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrNoAgentAccess
		}
		if err != nil {
			return nil, err
		}
		pat := a.Credentials[p]
		if !a.Enabled || pat == "" {
			return nil, ErrNoAgentAccess
		}
		return Bearer(pat), nil
	}
	return nil, fmt.Errorf("atlassian: unknown subject kind %q", s.Kind)
}

// Invalidate drops a user's grant after the product rejected it,
// so the next call asks the user to reconnect instead of failing repeatedly.
func (c *Credentials) Invalidate(ctx context.Context, s identity.Subject, p store.Provider) error {
	if !s.ActsAsUser() {
		return nil
	}
	return c.Tokens.DeleteToken(ctx, s.Username, p)
}

func (c *Credentials) userToken(ctx context.Context, user string, p store.Provider) (string, error) {
	t, err := c.Tokens.GetToken(ctx, user, p)
	if errors.Is(err, store.ErrNotFound) {
		return "", ErrNotConnected
	}
	if err != nil {
		return "", err
	}
	if c.fresh(t) {
		return t.AccessToken, nil
	}
	// Collapse concurrent refreshes for the same grant: refresh tokens rotate,
	// so two parallel refreshes would invalidate each other.
	v, err, _ := c.refresh.Do(user+"|"+string(p), func() (any, error) {
		return c.refreshToken(context.WithoutCancel(ctx), user, p)
	})
	if err != nil {
		return "", err
	}
	return v.(string), nil
}

func (c *Credentials) fresh(t *store.OAuthToken) bool {
	return t.Expiry.IsZero() || c.now().Add(refreshMargin).Before(t.Expiry)
}

func (c *Credentials) refreshToken(ctx context.Context, user string, p store.Provider) (string, error) {
	// Re-read: another replica may have refreshed already.
	t, err := c.Tokens.GetToken(ctx, user, p)
	if errors.Is(err, store.ErrNotFound) {
		return "", ErrNotConnected
	}
	if err != nil {
		return "", err
	}
	if c.fresh(t) {
		return t.AccessToken, nil
	}
	o, ok := c.OAuth[p]
	if !ok {
		return "", fmt.Errorf("atlassian: provider %s not configured", p)
	}
	nt, err := o.Refresh(ctx, t.RefreshToken)
	if err != nil {
		var re *oauth2.RetrieveError
		if errors.As(err, &re) && re.Response != nil &&
			(re.Response.StatusCode == http.StatusBadRequest || re.Response.StatusCode == http.StatusUnauthorized) {
			// invalid_grant: revoked, expired or already used. The user must reconnect.
			if derr := c.Tokens.DeleteToken(ctx, user, p); derr != nil && c.Log != nil {
				c.Log.Error("delete revoked token", "user", user, "provider", p, "err", derr)
			}
			if c.Log != nil {
				c.Log.Info("refresh token rejected, grant removed", "user", user, "provider", p)
			}
			return "", ErrNotConnected
		}
		return "", fmt.Errorf("%w: refresh token: %v", ErrUpstream, err)
	}
	updated := &store.OAuthToken{
		Username:     user,
		Provider:     p,
		AccessToken:  nt.AccessToken,
		RefreshToken: nt.RefreshToken,
		Expiry:       nt.Expiry,
	}
	if updated.RefreshToken == "" {
		updated.RefreshToken = t.RefreshToken
	}
	if err := c.Tokens.PutToken(ctx, updated); err != nil {
		return "", err
	}
	return updated.AccessToken, nil
}

// SaveGrant stores tokens obtained from the authorization code flow.
func (c *Credentials) SaveGrant(ctx context.Context, user string, p store.Provider, t *oauth2.Token) error {
	if t.AccessToken == "" {
		return errors.New("atlassian: empty access token")
	}
	return c.Tokens.PutToken(ctx, &store.OAuthToken{
		Username: user, Provider: p,
		AccessToken: t.AccessToken, RefreshToken: t.RefreshToken, Expiry: t.Expiry,
	})
}
