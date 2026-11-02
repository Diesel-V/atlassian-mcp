package identity

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
)

// fakeRealm serves OIDC discovery and JWKS like a Keycloak realm and signs tokens.
type fakeRealm struct {
	srv    *httptest.Server
	key    *rsa.PrivateKey
	signer jose.Signer
}

func newFakeRealm(t *testing.T) *fakeRealm {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	r := &fakeRealm{key: key}
	r.signer, err = jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: key, KeyID: "k1"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	r.srv = httptest.NewServer(mux)
	t.Cleanup(r.srv.Close)
	mux.HandleFunc("/realms/main/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		iss := r.srv.URL + "/realms/main"
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": iss, "jwks_uri": iss + "/certs",
			"authorization_endpoint": iss + "/auth", "token_endpoint": iss + "/token",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/realms/main/certs", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	})
	return r
}

func (r *fakeRealm) issuer() string { return r.srv.URL + "/realms/main" }

func (r *fakeRealm) sign(t *testing.T, signer jose.Signer, claims map[string]any) string {
	t.Helper()
	payload, _ := json.Marshal(claims)
	jws, err := signer.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	s, err := jws.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestKeycloakVerifier(t *testing.T) {
	realm := newFakeRealm(t)
	ctx := context.Background()
	v, err := NewKeycloakVerifier(ctx, realm.issuer(), "preferred_username", realm.srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	base := func() map[string]any {
		return map[string]any{
			"iss": realm.issuer(), "aud": "account", "azp": "open-webui",
			"preferred_username": "i.ivanov@example.com",
			"exp":                now.Add(time.Hour).Unix(), "iat": now.Unix(),
		}
	}

	c, err := v.Verify(ctx, realm.sign(t, realm.signer, base()))
	if err != nil {
		t.Fatal(err)
	}
	if c.AuthorizedParty != "open-webui" || c.Username != "i.ivanov@example.com" || c.Expiry.Unix() != now.Add(time.Hour).Unix() {
		t.Fatalf("claims = %+v", c)
	}

	expired := base()
	expired["exp"] = now.Add(-time.Minute).Unix()
	otherIssuer := base()
	otherIssuer["iss"] = "https://evil.example/realms/main"
	otherKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	forger, _ := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: otherKey, KeyID: "k1"}}, nil)

	for name, tok := range map[string]string{
		"expired":      realm.sign(t, realm.signer, expired),
		"other issuer": realm.sign(t, realm.signer, otherIssuer),
		"forged":       realm.sign(t, forger, base()),
		"garbage":      "a.b.c",
	} {
		if _, err := v.Verify(ctx, tok); err == nil {
			t.Errorf("%s token accepted", name)
		}
	}
}
