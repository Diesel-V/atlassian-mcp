// Package config loads and validates service configuration from environment variables.
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Provider holds OAuth 2.0 settings of one Atlassian Data Center product.
type Provider struct {
	BaseURL      string
	ClientID     string
	ClientSecret string
	Scopes       []string
}

// Enabled reports whether the provider is configured.
func (p Provider) Enabled() bool { return p.BaseURL != "" }

// Config is the full service configuration.
type Config struct {
	ListenAddr  string
	MetricsAddr string
	PublicURL   string
	DatabaseURL string
	CAFile      string

	EncryptionKey []byte // 32 bytes, encrypts stored Atlassian credentials
	SessionKey    []byte // 32 bytes, protects browser cookies

	KeycloakIssuer          string
	KeycloakUserClients     []string // azp values accepted as end-user tokens (e.g. open-webui)
	KeycloakWebClientID     string
	KeycloakWebClientSecret string
	UsernameClaim           string
	UsernameStripDomain     string

	Confluence Provider
	Jira       Provider

	APIKeyDefaultTTL time.Duration
	APIKeyMaxTTL     time.Duration
	RateLimitPerMin  int
	HTTPTimeout      time.Duration
	MaxSearchResults int
	MaxContentChars  int
	SessionTTL       time.Duration
	AuditRetention   time.Duration // 0 keeps audit entries forever
}

// Load reads configuration from the process environment.
func Load() (*Config, error) { return LoadFrom(os.Getenv) }

// LoadFrom reads configuration using getenv; it is separated for tests.
func LoadFrom(getenv func(string) string) (*Config, error) {
	l := loader{getenv: getenv}
	c := &Config{
		ListenAddr:  l.str("LISTEN_ADDR", ":8080"),
		MetricsAddr: l.str("METRICS_ADDR", ":9090"),
		PublicURL:   strings.TrimRight(l.required("PUBLIC_URL"), "/"),
		DatabaseURL: l.required("DATABASE_URL"),
		CAFile:      l.str("CA_FILE", ""),

		EncryptionKey: l.key("ENCRYPTION_KEY"),
		SessionKey:    l.key("SESSION_KEY"),

		KeycloakIssuer:          strings.TrimRight(l.required("KEYCLOAK_ISSUER"), "/"),
		KeycloakUserClients:     l.list("KEYCLOAK_USER_CLIENTS", "open-webui"),
		KeycloakWebClientID:     l.required("KEYCLOAK_WEB_CLIENT_ID"),
		KeycloakWebClientSecret: l.required("KEYCLOAK_WEB_CLIENT_SECRET"),
		UsernameClaim:           l.str("USERNAME_CLAIM", "preferred_username"),
		UsernameStripDomain:     strings.TrimPrefix(l.str("USERNAME_STRIP_DOMAIN", ""), "@"),

		Confluence: l.provider("CONFLUENCE"),
		Jira:       l.provider("JIRA"),

		APIKeyDefaultTTL: l.duration("API_KEY_DEFAULT_TTL", 90*24*time.Hour),
		APIKeyMaxTTL:     l.duration("API_KEY_MAX_TTL", 180*24*time.Hour),
		RateLimitPerMin:  l.int("RATE_LIMIT_PER_MINUTE", 60),
		HTTPTimeout:      l.duration("HTTP_TIMEOUT", 20*time.Second),
		MaxSearchResults: l.int("MAX_SEARCH_RESULTS", 20),
		MaxContentChars:  l.int("MAX_CONTENT_CHARS", 20000),
		SessionTTL:       l.duration("SESSION_TTL", 8*time.Hour),
		AuditRetention:   l.duration("AUDIT_RETENTION", 365*24*time.Hour),
	}
	if len(l.errs) > 0 {
		return nil, errors.Join(l.errs...)
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Config) validate() error {
	var errs []error
	for name, raw := range map[string]string{"PUBLIC_URL": c.PublicURL, "KEYCLOAK_ISSUER": c.KeycloakIssuer} {
		if u, err := url.Parse(raw); err != nil || u.Scheme == "" || u.Host == "" {
			errs = append(errs, fmt.Errorf("%s: invalid URL %q", name, raw))
		}
	}
	if !c.Confluence.Enabled() && !c.Jira.Enabled() {
		errs = append(errs, errors.New("at least one of CONFLUENCE_URL or JIRA_URL must be set"))
	}
	if c.APIKeyDefaultTTL > c.APIKeyMaxTTL {
		errs = append(errs, errors.New("API_KEY_DEFAULT_TTL must not exceed API_KEY_MAX_TTL"))
	}
	if c.RateLimitPerMin <= 0 || c.MaxSearchResults <= 0 || c.MaxContentChars <= 0 {
		errs = append(errs, errors.New("RATE_LIMIT_PER_MINUTE, MAX_SEARCH_RESULTS and MAX_CONTENT_CHARS must be positive"))
	}
	if c.AuditRetention < 0 {
		errs = append(errs, errors.New("AUDIT_RETENTION must not be negative"))
	}
	if len(c.KeycloakUserClients) == 0 {
		errs = append(errs, errors.New("KEYCLOAK_USER_CLIENTS must not be empty"))
	}
	return errors.Join(errs...)
}

type loader struct {
	getenv func(string) string
	errs   []error
}

func (l *loader) str(name, def string) string {
	if v := strings.TrimSpace(l.getenv(name)); v != "" {
		return v
	}
	return def
}

func (l *loader) required(name string) string {
	v := strings.TrimSpace(l.getenv(name))
	if v == "" {
		l.errs = append(l.errs, fmt.Errorf("%s is required", name))
	}
	return v
}

func (l *loader) key(name string) []byte {
	raw := l.required(name)
	if raw == "" {
		return nil
	}
	b, err := base64.StdEncoding.DecodeString(raw)
	if err != nil || len(b) != 32 {
		l.errs = append(l.errs, fmt.Errorf("%s must be 32 bytes encoded in base64 (openssl rand -base64 32)", name))
		return nil
	}
	return b
}

func (l *loader) list(name, def string) []string {
	var out []string
	for _, p := range strings.Split(l.str(name, def), ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (l *loader) int(name string, def int) int {
	raw := l.str(name, "")
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		l.errs = append(l.errs, fmt.Errorf("%s: %w", name, err))
	}
	return v
}

func (l *loader) duration(name string, def time.Duration) time.Duration {
	raw := l.str(name, "")
	if raw == "" {
		return def
	}
	v, err := time.ParseDuration(raw)
	if err != nil {
		l.errs = append(l.errs, fmt.Errorf("%s: %w", name, err))
	}
	return v
}

func (l *loader) provider(prefix string) Provider {
	base := strings.TrimRight(l.str(prefix+"_URL", ""), "/")
	if base == "" {
		return Provider{}
	}
	return Provider{
		BaseURL:      base,
		ClientID:     l.required(prefix + "_CLIENT_ID"),
		ClientSecret: l.required(prefix + "_CLIENT_SECRET"),
		Scopes:       l.list(prefix+"_SCOPES", "READ"),
	}
}
