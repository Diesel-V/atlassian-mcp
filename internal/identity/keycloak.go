package identity

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

// Claims are the token fields the service relies on.
type Claims struct {
	AuthorizedParty string    // azp: the Keycloak client that obtained the token
	Username        string    // value of the configured username claim
	Groups          []string  // value of the configured groups claim (Keycloak group mapper)
	Expiry          time.Time // exp
}

// TokenVerifier checks a JWT's signature, issuer and expiry and returns its claims.
type TokenVerifier interface {
	Verify(ctx context.Context, raw string) (*Claims, error)
}

// KeycloakVerifier validates Keycloak access tokens against the realm JWKS.
type KeycloakVerifier struct {
	verifier      *oidc.IDTokenVerifier
	usernameClaim string
	groupsClaim   string
}

// NewKeycloakVerifier discovers the realm at issuer. httpClient may carry a corporate CA.
func NewKeycloakVerifier(ctx context.Context, issuer, usernameClaim, groupsClaim string, httpClient *http.Client) (*KeycloakVerifier, error) {
	if httpClient != nil {
		ctx = oidc.ClientContext(ctx, httpClient)
	}
	provider, err := oidc.NewProvider(ctx, issuer)
	if err != nil {
		return nil, fmt.Errorf("identity: discover %s: %w", issuer, err)
	}
	// Access tokens carry aud=account, so the audience is checked through azp instead.
	v := provider.Verifier(&oidc.Config{SkipClientIDCheck: true})
	return &KeycloakVerifier{verifier: v, usernameClaim: usernameClaim, groupsClaim: groupsClaim}, nil
}

// Verify implements TokenVerifier.
func (k *KeycloakVerifier) Verify(ctx context.Context, raw string) (*Claims, error) {
	tok, err := k.verifier.Verify(ctx, raw)
	if err != nil {
		return nil, err
	}
	var all map[string]any
	if err := tok.Claims(&all); err != nil {
		return nil, err
	}
	azp, _ := all["azp"].(string)
	user, _ := all[k.usernameClaim].(string)
	return &Claims{AuthorizedParty: azp, Username: user, Groups: StringList(all[k.groupsClaim]), Expiry: tok.Expiry}, nil
}

// StringList reads a JSON claim that holds a list of strings (or a single string).
func StringList(v any) []string {
	switch t := v.(type) {
	case string:
		return []string{t}
	case []any:
		out := make([]string, 0, len(t))
		for _, x := range t {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// InGroup reports whether groups contains required. Keycloak sends groups either
// as names or as full paths ("/ai/pilot"); a leading slash is ignored on both sides.
func InGroup(groups []string, required string) bool {
	required = strings.TrimPrefix(strings.TrimSpace(required), "/")
	if required == "" {
		return true
	}
	for _, g := range groups {
		if strings.TrimPrefix(strings.TrimSpace(g), "/") == required {
			return true
		}
	}
	return false
}
