package config

import (
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
