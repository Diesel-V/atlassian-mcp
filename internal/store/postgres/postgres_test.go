package postgres

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

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

func TestAuditObjectsSearchable(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	for _, objs := range [][]string{{"12345", "OD-1"}, nil} {
		if err := s.InsertAudit(ctx, &store.AuditEntry{Time: time.Now(), Tool: "t", Principal: "alice", Objects: objs}); err != nil {
			t.Fatal(err)
		}
	}
	var who string
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT principal, count(*) OVER () FROM audit_log WHERE '12345' = ANY(objects)`).Scan(&who, &n); err != nil {
		t.Fatal(err)
	}
	if who != "alice" || n != 1 {
		t.Fatalf("lookup by object: %s %d", who, n)
	}
	var empty int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE objects = '{}'`).Scan(&empty); err != nil || empty != 1 {
		t.Fatalf("nil objects stored as %d empty arrays, err %v", empty, err)
	}
}

func TestCheckSchema(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if err := s.CheckSchema(ctx); err != nil {
		t.Fatalf("fresh schema: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = (SELECT max(version) FROM schema_migrations)`); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckSchema(ctx); !errors.Is(err, ErrSchemaOutdated) {
		t.Fatalf("pending migration: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `DROP TABLE schema_migrations`); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckSchema(ctx); !errors.Is(err, ErrSchemaOutdated) {
		t.Fatalf("never migrated: %v", err)
	}
}

// TestLeastPrivilegeRoles applies deploy/postgres-roles.sql and checks that the
// runtime role works but cannot change the schema.
func TestLeastPrivilegeRoles(t *testing.T) {
	admin := openTest(t)
	ctx := context.Background()
	exec := func(st *Store, q string) error { _, err := st.pool.Exec(ctx, q); return err }

	cleanup := func() {
		for _, q := range []string{
			`DROP TABLE IF EXISTS oauth_tokens, api_keys, agents, audit_log, schema_migrations`,
			`DO $$ BEGIN IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname='amcp_app') THEN DROP OWNED BY amcp_app; DROP ROLE amcp_app; END IF; END $$`,
			`DO $$ BEGIN IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname='amcp_migrator') THEN DROP OWNED BY amcp_migrator; DROP ROLE amcp_migrator; END IF; END $$`,
		} {
			if err := exec(admin, q); err != nil {
				t.Fatalf("cleanup: %v", err)
			}
		}
	}
	cleanup()
	t.Cleanup(cleanup)

	sql, err := os.ReadFile("../../../deploy/postgres-roles.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{string(sql), `ALTER ROLE amcp_migrator PASSWORD 'm'`, `ALTER ROLE amcp_app PASSWORD 'a'`} {
		if err := exec(admin, q); err != nil {
			t.Fatalf("roles: %v", err)
		}
	}

	as := func(user, pass string) *Store {
		u, err := url.Parse(os.Getenv("TEST_DATABASE_URL"))
		if err != nil {
			t.Fatal(err)
		}
		u.User = url.UserPassword(user, pass)
		sealer, _ := crypto.NewSealer(bytes.Repeat([]byte{7}, 32))
		st, err := Open(ctx, u.String(), sealer)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(st.Close)
		return st
	}
	migrator, app := as("amcp_migrator", "m"), as("amcp_app", "a")

	if err := app.Migrate(ctx); err == nil {
		t.Fatal("runtime role created the schema")
	}
	if err := migrator.Migrate(ctx); err != nil {
		t.Fatalf("migrator: %v", err)
	}
	if err := app.CheckSchema(ctx); err != nil {
		t.Fatalf("runtime role cannot read the schema version: %v", err)
	}

	if err := app.PutToken(ctx, &store.OAuthToken{Username: "alice", Provider: store.Jira, AccessToken: "a", RefreshToken: "r"}); err != nil {
		t.Fatalf("runtime write: %v", err)
	}
	if _, err := app.GetToken(ctx, "alice", store.Jira); err != nil {
		t.Fatalf("runtime read: %v", err)
	}
	if err := app.InsertAudit(ctx, &store.AuditEntry{Time: time.Now().Add(-48 * time.Hour), Tool: "t"}); err != nil {
		t.Fatalf("runtime audit: %v", err)
	}
	if n, err := app.PurgeAudit(ctx, time.Now().Add(-time.Hour)); err != nil || n != 1 {
		t.Fatalf("runtime retention: %d %v", n, err)
	}
	for _, ddl := range []string{`CREATE TABLE evil (x int)`, `DROP TABLE audit_log`, `ALTER TABLE audit_log DROP COLUMN error`, `TRUNCATE audit_log`} {
		if err := exec(app, ddl); err == nil {
			t.Errorf("runtime role was allowed: %s", ddl)
		}
	}
}
