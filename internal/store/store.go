// Package store defines persistence contracts. Implementations live in subpackages.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// ErrNotFound is returned when the requested record does not exist.
var ErrNotFound = errors.New("store: not found")

// ErrConflict is returned when a record with the same identity already exists.
var ErrConflict = errors.New("store: conflict")

// Provider identifies an Atlassian product.
type Provider string

const (
	Confluence Provider = "confluence"
	Jira       Provider = "jira"
)

// Valid reports whether p is a known provider.
func (p Provider) Valid() bool { return p == Confluence || p == Jira }

// Title is a human-readable provider name.
func (p Provider) Title() string {
	switch p {
	case Confluence:
		return "Confluence"
	case Jira:
		return "Jira"
	}
	return string(p)
}

// OAuthToken is a user's OAuth 2.0 grant for one provider.
type OAuthToken struct {
	Username     string
	Provider     Provider
	AccessToken  string
	RefreshToken string
	Expiry       time.Time
	UpdatedAt    time.Time
}

// APIKey is a personal key that lets an agent act on behalf of its owner.
// Only the hash of the secret part is stored.
type APIKey struct {
	ID         string
	Username   string
	Name       string
	SecretHash string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	RevokedAt  *time.Time
	LastUsedAt *time.Time
}

// Active reports whether the key can be used at time now.
func (k *APIKey) Active(now time.Time) bool {
	return k.RevokedAt == nil && now.Before(k.ExpiresAt)
}

// Agent is an autonomous agent registered by its Keycloak client ID.
// Credentials hold per-provider personal access tokens of the agent's
// dedicated Atlassian service account.
type Agent struct {
	ClientID    string
	Name        string
	Owner       string
	Enabled     bool
	Credentials map[Provider]string
	CreatedAt   time.Time
}

// AuditEntry is one tool invocation. It never contains document content.
type AuditEntry struct {
	Time        time.Time
	SubjectKind string
	Principal   string
	KeyID       string
	Tool        string
	Params      json.RawMessage
	Results     int
	Status      string
	DurationMS  int64
	Error       string
}

// Tokens persists users' OAuth grants.
type Tokens interface {
	GetToken(ctx context.Context, username string, p Provider) (*OAuthToken, error)
	PutToken(ctx context.Context, t *OAuthToken) error
	DeleteToken(ctx context.Context, username string, p Provider) error
	ConnectedProviders(ctx context.Context, username string) ([]Provider, error)
	// CountConnected returns the number of users holding a grant, per provider.
	CountConnected(ctx context.Context) (map[Provider]int, error)
}

// APIKeys persists personal agent keys.
type APIKeys interface {
	CreateAPIKey(ctx context.Context, k *APIKey) error
	GetAPIKey(ctx context.Context, id string) (*APIKey, error)
	ListAPIKeys(ctx context.Context, username string) ([]*APIKey, error)
	// RevokeAPIKey revokes the key only if it belongs to username.
	RevokeAPIKey(ctx context.Context, username, id string, at time.Time) error
	TouchAPIKey(ctx context.Context, id string, at time.Time) error
}

// Agents persists the registry of autonomous agents.
type Agents interface {
	GetAgent(ctx context.Context, clientID string) (*Agent, error)
	PutAgent(ctx context.Context, a *Agent) error
	ListAgents(ctx context.Context) ([]*Agent, error)
}

// Audit persists the audit log.
type Audit interface {
	InsertAudit(ctx context.Context, e *AuditEntry) error
	// PurgeAudit deletes entries older than before and returns how many were removed.
	PurgeAudit(ctx context.Context, before time.Time) (int64, error)
}

// Store aggregates all persistence contracts.
type Store interface {
	Tokens
	APIKeys
	Agents
	Audit
	Ping(ctx context.Context) error
	Close()
}
