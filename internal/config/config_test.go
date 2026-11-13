package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const key32 = "MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTIzNDU2Nzg5MDE=" // 32 bytes

func baseEnv() map[string]string {
	return map[string]string{
		"PUBLIC_URL":                 "https://mcp.example.com/",
		"DATABASE_URL":               "postgres://u:p@db/amcp",
		"ENCRYPTION_KEY":             key32,
		"SESSION_KEY":                key32,
		"KEYCLOAK_ISSUER":            "https://auth.example.com/realms/main",
		"KEYCLOAK_WEB_CLIENT_ID":     "atlassian-mcp",
		"KEYCLOAK_WEB_CLIENT_SECRET": "s",
		"CONFLUENCE_URL":             "https://confluence.example.com/",
		"CONFLUENCE_CLIENT_ID":       "cid",
		"CONFLUENCE_CLIENT_SECRET":   "csec",
		"CONFLUENCE_ALLOWED_SPACES":  "TECHOPS, ~i.ivanov",
	}
}

func load(env map[string]string) (*Config, error) {
	return LoadFrom(func(k string) string { return env[k] })
}

func TestLoadDefaults(t *testing.T) {
	c, err := load(baseEnv())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.PublicURL != "https://mcp.example.com" {
		t.Errorf("PublicURL not trimmed: %q", c.PublicURL)
	}
	if !c.Confluence.Enabled() || c.Jira.Enabled() {
		t.Errorf("providers: confluence=%v jira=%v", c.Confluence.Enabled(), c.Jira.Enabled())
	}
	if c.Confluence.BaseURL != "https://confluence.example.com" {
		t.Errorf("Confluence URL not trimmed: %q", c.Confluence.BaseURL)
	}
	if got := strings.Join(c.Confluence.Scopes, ","); got != "READ" {
		t.Errorf("default scopes = %q", got)
	}
	if got := strings.Join(c.KeycloakUserClients, ","); got != "open-webui" {
		t.Errorf("default user clients = %q", got)
	}
	if c.APIKeyDefaultTTL != 90*24*time.Hour || c.RateLimitPerMin != 60 || c.AuditRetention != 365*24*time.Hour {
		t.Errorf("unexpected defaults: %+v", c)
	}
	// Pilot-safe defaults: keys and agents off, migrations on, groups claim "groups".
	if c.EnableAPIKeys || c.EnableAgents || !c.MigrateOnStart || c.GroupsClaim != "groups" || c.RequiredGroup != "" {
		t.Errorf("pilot defaults: %+v", c)
	}
	if c.MigrateDatabaseURL != c.DatabaseURL || c.TLSCertFile != "" {
		t.Errorf("migrate url / tls: %+v", c)
	}
	if got := strings.Join(c.Confluence.Allowed, "|"); got != "TECHOPS|~i.ivanov" {
		t.Errorf("allowed spaces = %q", got)
	}
}

func TestSecretsFromFiles(t *testing.T) {
	dir := t.TempDir()
	write := func(name, v string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(v+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	env := baseEnv()
	delete(env, "ENCRYPTION_KEY")
	delete(env, "CONFLUENCE_CLIENT_SECRET")
	env["ENCRYPTION_KEY_FILE"] = write("enc", key32)
	env["CONFLUENCE_CLIENT_SECRET_FILE"] = write("csec", "from-vault")
	c, err := load(env)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.EncryptionKey) != 32 || c.Confluence.ClientSecret != "from-vault" {
		t.Fatalf("file secrets not loaded: key=%d secret=%q", len(c.EncryptionKey), c.Confluence.ClientSecret)
	}

	env["CONFLUENCE_CLIENT_SECRET"] = "also-in-env"
	if _, err := load(env); err == nil || !strings.Contains(err.Error(), "not both") {
		t.Fatalf("both env and file accepted: %v", err)
	}
	delete(env, "CONFLUENCE_CLIENT_SECRET")
	env["ENCRYPTION_KEY_FILE"] = filepath.Join(dir, "missing")
	if _, err := load(env); err == nil || !strings.Contains(err.Error(), "ENCRYPTION_KEY_FILE") {
		t.Fatalf("missing file accepted: %v", err)
	}
}

func TestPilotSettings(t *testing.T) {
	env := baseEnv()
	env["KEYCLOAK_REQUIRED_GROUP"] = "/ai-pilot"
	env["ENABLE_API_KEYS"] = "true"
	env["MIGRATE_ON_START"] = "false"
	env["MIGRATE_DATABASE_URL"] = "postgres://owner@db/amcp"
	env["TLS_CERT_FILE"] = "/tls/crt"
	env["TLS_KEY_FILE"] = "/tls/key"
	env["CONFLUENCE_ALLOWED_SPACES"] = "*"
	c, err := load(env)
	if err != nil {
		t.Fatal(err)
	}
	if c.RequiredGroup != "/ai-pilot" || !c.EnableAPIKeys || c.EnableAgents || c.MigrateOnStart ||
		c.MigrateDatabaseURL != "postgres://owner@db/amcp" || c.TLSKeyFile != "/tls/key" {
		t.Fatalf("config = %+v", c)
	}
	if strings.Join(c.Confluence.Allowed, "") != "*" {
		t.Fatalf("allowed = %v", c.Confluence.Allowed)
	}
}

func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]string)
		want   string
	}{
		{"missing public url", func(e map[string]string) { delete(e, "PUBLIC_URL") }, "PUBLIC_URL is required"},
		{"bad key", func(e map[string]string) { e["ENCRYPTION_KEY"] = "c2hvcnQ=" }, "ENCRYPTION_KEY must be 32 bytes"},
		{"no providers", func(e map[string]string) { delete(e, "CONFLUENCE_URL") }, "at least one of CONFLUENCE_URL"},
		{"provider without secret", func(e map[string]string) { delete(e, "CONFLUENCE_CLIENT_SECRET") }, "CONFLUENCE_CLIENT_SECRET is required"},
		{"bad duration", func(e map[string]string) { e["HTTP_TIMEOUT"] = "soon" }, "HTTP_TIMEOUT"},
		{"ttl order", func(e map[string]string) { e["API_KEY_DEFAULT_TTL"] = "300h"; e["API_KEY_MAX_TTL"] = "100h" }, "must not exceed"},
		{"negative retention", func(e map[string]string) { e["AUDIT_RETENTION"] = "-1h" }, "AUDIT_RETENTION"},
		{"bad url", func(e map[string]string) { e["KEYCLOAK_ISSUER"] = "auth" }, "KEYCLOAK_ISSUER: invalid URL"},
		{"no allowlist", func(e map[string]string) { delete(e, "CONFLUENCE_ALLOWED_SPACES") }, "CONFLUENCE_ALLOWED_SPACES is required"},
		{"bad space key", func(e map[string]string) { e["CONFLUENCE_ALLOWED_SPACES"] = "TECH\") OR space" }, "invalid key"},
		{"star mixed", func(e map[string]string) { e["CONFLUENCE_ALLOWED_SPACES"] = "*,HR" }, "invalid key \"*\""},
		{"jira no allowlist", func(e map[string]string) {
			e["JIRA_URL"], e["JIRA_CLIENT_ID"], e["JIRA_CLIENT_SECRET"] = "https://jira.example.com", "j", "s"
		}, "JIRA_ALLOWED_PROJECTS is required"},
		{"bad project key", func(e map[string]string) {
			e["JIRA_URL"], e["JIRA_CLIENT_ID"], e["JIRA_CLIENT_SECRET"], e["JIRA_ALLOWED_PROJECTS"] = "https://jira.example.com", "j", "s", "od"
		}, "JIRA_ALLOWED_PROJECTS: invalid key"},
		{"tls half", func(e map[string]string) { e["TLS_CERT_FILE"] = "/c" }, "TLS_CERT_FILE and TLS_KEY_FILE"},
		{"bad bool", func(e map[string]string) { e["ENABLE_AGENTS"] = "maybe" }, "ENABLE_AGENTS"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := baseEnv()
			tt.mutate(env)
			_, err := load(env)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestLoadListsAndJira(t *testing.T) {
	env := baseEnv()
	env["KEYCLOAK_USER_CLIENTS"] = " open-webui , opencode ,"
	env["JIRA_URL"] = "https://jira.example.com"
	env["JIRA_CLIENT_ID"] = "j"
	env["JIRA_CLIENT_SECRET"] = "js"
	env["JIRA_SCOPES"] = "READ,WRITE"
	env["JIRA_ALLOWED_PROJECTS"] = "OD,DEVOPS"
	env["USERNAME_STRIP_DOMAIN"] = "@example.com"
	c, err := load(env)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(c.KeycloakUserClients, "|"); got != "open-webui|opencode" {
		t.Errorf("user clients = %q", got)
	}
	if !c.Jira.Enabled() || strings.Join(c.Jira.Scopes, ",") != "READ,WRITE" {
		t.Errorf("jira = %+v", c.Jira)
	}
	if c.UsernameStripDomain != "example.com" {
		t.Errorf("strip domain = %q", c.UsernameStripDomain)
	}
}

// TestEnvExampleLoads keeps deploy/.env.example valid: with its secret files in
// place it must produce a pilot-safe configuration.
func TestEnvExampleLoads(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/.env.example")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	env := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, _ := strings.Cut(line, "=")
		if strings.HasSuffix(k, "_FILE") && strings.HasPrefix(v, "/run/secrets/") {
			content := "secret"
			switch {
			case strings.HasSuffix(v, "_key"):
				content = key32
			case strings.HasSuffix(v, "database_url"):
				content = "postgres://amcp_app:x@db/amcp"
			}
			v = filepath.Join(dir, filepath.Base(v))
			if err := os.WriteFile(v, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		env[k] = v
	}
	env["CONFLUENCE_CLIENT_ID"], env["JIRA_CLIENT_ID"] = "c", "j"
	env["KEYCLOAK_ISSUER"] = "https://auth.example.com/realms/main"

	c, err := load(env)
	if err != nil {
		t.Fatalf(".env.example does not load: %v", err)
	}
	if c.EnableAPIKeys || c.EnableAgents || c.MigrateOnStart || c.RequiredGroup == "" || c.TLSCertFile == "" ||
		len(c.Confluence.Allowed) == 0 || c.Confluence.Allowed[0] == "*" || len(c.Jira.Allowed) == 0 {
		t.Fatalf(".env.example is not pilot-safe: %+v", c)
	}
}
