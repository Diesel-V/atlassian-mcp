package atlassian

import (
	"context"
	"net/http"

	"golang.org/x/oauth2"

	"atlassian-mcp/internal/store"
)

// WhoAmIer returns the username behind a set of credentials.
type WhoAmIer interface {
	WhoAmI(ctx context.Context, auth Auth) (string, error)
}

// OAuthProvider performs the OAuth 2.0 authorization code flow (with PKCE)
// against a Data Center product configured with an incoming application link.
type OAuthProvider struct {
	Provider store.Provider
	Product  WhoAmIer
	config   oauth2.Config
	http     *http.Client
}

// NewOAuthProvider builds a provider. redirectURL must match the one registered
// in the application link exactly.
func NewOAuthProvider(p store.Provider, baseURL, clientID, clientSecret, redirectURL string, scopes []string,
	httpClient *http.Client, product WhoAmIer) *OAuthProvider {
	return &OAuthProvider{
		Provider: p,
		Product:  product,
		http:     httpClient,
		config: oauth2.Config{
			ClientID:     clientID,
			ClientSecret: clientSecret,
			RedirectURL:  redirectURL,
			Scopes:       scopes,
			Endpoint: oauth2.Endpoint{
				AuthURL:   baseURL + "/rest/oauth2/latest/authorize",
				TokenURL:  baseURL + "/rest/oauth2/latest/token",
				AuthStyle: oauth2.AuthStyleInParams,
			},
		},
	}
}

func (o *OAuthProvider) ctx(ctx context.Context) context.Context {
	if o.http != nil {
		return context.WithValue(ctx, oauth2.HTTPClient, o.http)
	}
	return ctx
}

// AuthCodeURL returns the consent URL for the given state and PKCE verifier.
func (o *OAuthProvider) AuthCodeURL(state, verifier string) string {
	return o.config.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier))
}

// Exchange trades an authorization code for tokens.
func (o *OAuthProvider) Exchange(ctx context.Context, code, verifier string) (*oauth2.Token, error) {
	return o.config.Exchange(o.ctx(ctx), code, oauth2.VerifierOption(verifier))
}

// WhoAmI returns the username behind auth using the product API.
func (o *OAuthProvider) WhoAmI(ctx context.Context, auth Auth) (string, error) {
	return o.Product.WhoAmI(ctx, auth)
}

// Refresh obtains a new access token. Data Center rotates refresh tokens,
// so the returned token may carry a new refresh token.
func (o *OAuthProvider) Refresh(ctx context.Context, refreshToken string) (*oauth2.Token, error) {
	src := o.config.TokenSource(o.ctx(ctx), &oauth2.Token{RefreshToken: refreshToken})
	return src.Token()
}
