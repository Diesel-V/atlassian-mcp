package postgres

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"atlassian-mcp/internal/crypto"
	"atlassian-mcp/internal/store"
	"atlassian-mcp/internal/store/storetest"
)

// Integration tests run only when TEST_DATABASE_URL points to a disposable database.
// Example: docker run --rm -e POSTGRES_PASSWORD=p -p 5432:5432 postgres:17
//
//	TEST_DATABASE_URL=postgres://postgres:p@localhost:5432/postgres?sslmode=disable go test ./...
func openTest(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	sealer, err := crypto.NewSealer(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, dsn, sealer)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	for _, q := range []string{
		`DROP TABLE IF EXISTS oauth_tokens, api_keys, agents, audit_log, schema_migrations`,
	} {
		if _, err := s.pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil { // idempotent
		t.Fatalf("second migrate: %v", err)
	}
	return s
}

func TestConformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) store.Store { return openTest(t) })
}

func TestSecretsAreEncryptedAtRest(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if err := s.PutToken(ctx, &store.OAuthToken{Username: "alice", Provider: store.Confluence,
		AccessToken: "plain-access", RefreshToken: "plain-refresh"}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutAgent(ctx, &store.Agent{ClientID: "bot", Name: "b", Owner: "o", Enabled: true,
		Credentials: map[store.Provider]string{store.Jira: "plain-pat"}}); err != nil {
		t.Fatal(err)
	}
	var dump string
	err := s.pool.QueryRow(ctx, `SELECT
		(SELECT string_agg(access_token || refresh_token, '') FROM oauth_tokens) ||
		(SELECT string_agg(credentials, '') FROM agents)`).Scan(&dump)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"plain-access", "plain-refresh", "plain-pat"} {
		if strings.Contains(dump, secret) {
			t.Errorf("%q stored in plaintext", secret)
		}
	}

	// A ciphertext moved to another user's row must not decrypt.
	if _, err := s.pool.Exec(ctx, `INSERT INTO oauth_tokens (username, provider, access_token, refresh_token, expiry)
		SELECT 'mallory', provider, access_token, refresh_token, expiry FROM oauth_tokens WHERE username='alice'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetToken(ctx, "mallory", store.Confluence); err == nil {
		t.Fatal("token copied to another user decrypted successfully")
	}
}
