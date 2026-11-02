package identity

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

// Claims are the token fields the service relies on.
type Claims struct {
	AuthorizedParty string    // azp: the Keycloak client that obtained the token
	Username        string    // value of the configured username claim
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
}

// NewKeycloakVerifier discovers the realm at issuer. httpClient may carry a corporate CA.
func NewKeycloakVerifier(ctx context.Context, issuer, usernameClaim string, httpClient *http.Client) (*KeycloakVerifier, error) {
	if httpClient != nil {
		ctx = oidc.ClientContext(ctx, httpClient)
	}
	provider, err := oidc.NewProvider(ctx, issuer)
	if err != nil {
		return nil, fmt.Errorf("identity: discover %s: %w", issuer, err)
	}
	// Access tokens carry aud=account, so the audience is checked through azp instead.
	v := provider.Verifier(&oidc.Config{SkipClientIDCheck: true})
	return &KeycloakVerifier{verifier: v, usernameClaim: usernameClaim}, nil
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
	return &Claims{AuthorizedParty: azp, Username: user, Expiry: tok.Expiry}, nil
}
