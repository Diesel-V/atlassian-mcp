package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"atlassian-mcp/internal/identity"
)

// LoginProvider authenticates browser users (OpenID Connect authorization code flow).
type LoginProvider interface {
	AuthCodeURL(state, nonce, verifier string) string
	// Exchange returns the normalised username of the authenticated user.
	Exchange(ctx context.Context, code, verifier, nonce string) (string, error)
}

// KeycloakLogin implements LoginProvider with a confidential Keycloak client.
type KeycloakLogin struct {
	config        oauth2.Config
	verifier      *oidc.IDTokenVerifier
	usernameClaim string
	stripDomain   string
	http          *http.Client
}

// NewKeycloakLogin discovers the realm and configures the web client.
func NewKeycloakLogin(ctx context.Context, issuer, clientID, clientSecret, redirectURL, usernameClaim, stripDomain string,
	httpClient *http.Client) (*KeycloakLogin, error) {
	if httpClient != nil {
		ctx = oidc.ClientContext(ctx, httpClient)
	}
	p, err := oidc.NewProvider(ctx, issuer)
	if err != nil {
		return nil, fmt.Errorf("web: discover %s: %w", issuer, err)
	}
	return &KeycloakLogin{
		config: oauth2.Config{
			ClientID: clientID, ClientSecret: clientSecret, RedirectURL: redirectURL,
			Endpoint: p.Endpoint(), Scopes: []string{oidc.ScopeOpenID, "profile", "email"},
		},
		verifier:      p.Verifier(&oidc.Config{ClientID: clientID}),
		usernameClaim: usernameClaim,
		stripDomain:   stripDomain,
		http:          httpClient,
	}, nil
}

func (k *KeycloakLogin) AuthCodeURL(state, nonce, verifier string) string {
	return k.config.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier))
}

func (k *KeycloakLogin) Exchange(ctx context.Context, code, verifier, nonce string) (string, error) {
	if k.http != nil {
		ctx = context.WithValue(ctx, oauth2.HTTPClient, k.http)
	}
	tok, err := k.config.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return "", err
	}
	rawID, ok := tok.Extra("id_token").(string)
	if !ok {
		return "", errors.New("web: no id_token in response")
	}
	idt, err := k.verifier.Verify(ctx, rawID)
	if err != nil {
		return "", err
	}
	if idt.Nonce != nonce {
		return "", errors.New("web: nonce mismatch")
	}
	var claims map[string]any
	if err := idt.Claims(&claims); err != nil {
		return "", err
	}
	name, _ := claims[k.usernameClaim].(string)
	user := identity.NormalizeUsername(name, k.stripDomain)
	if user == "" {
		return "", fmt.Errorf("web: claim %q is empty", k.usernameClaim)
	}
	return user, nil
}
