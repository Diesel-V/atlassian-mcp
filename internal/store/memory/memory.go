// Package memory is an in-process Store implementation for tests and local runs.
package memory

import (
	"context"
	"maps"
	"slices"
	"sync"
	"time"

	"atlassian-mcp/internal/store"
)

type tokenKey struct {
	user string
	p    store.Provider
}

// Store keeps everything in memory. Returned values are copies.
type Store struct {
	mu     sync.Mutex
	tokens map[tokenKey]store.OAuthToken
	keys   map[string]store.APIKey
	agents map[string]store.Agent
	audit  []store.AuditEntry
}

var _ store.Store = (*Store)(nil)

// New returns an empty Store.
func New() *Store {
	return &Store{
		tokens: map[tokenKey]store.OAuthToken{},
		keys:   map[string]store.APIKey{},
		agents: map[string]store.Agent{},
	}
}

func (s *Store) GetToken(_ context.Context, user string, p store.Provider) (*store.OAuthToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tokens[tokenKey{user, p}]
	if !ok {
		return nil, store.ErrNotFound
	}
	return &t, nil
}

func (s *Store) PutToken(_ context.Context, t *store.OAuthToken) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *t
	if cp.UpdatedAt.IsZero() {
		cp.UpdatedAt = time.Now()
	}
	s.tokens[tokenKey{t.Username, t.Provider}] = cp
	return nil
}

func (s *Store) DeleteToken(_ context.Context, user string, p store.Provider) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tokens, tokenKey{user, p})
	return nil
}

func (s *Store) ConnectedProviders(_ context.Context, user string) ([]store.Provider, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []store.Provider
	for k := range s.tokens {
		if k.user == user {
			out = append(out, k.p)
		}
	}
	slices.Sort(out)
	return out, nil
}

func (s *Store) CountConnected(context.Context) (map[store.Provider]int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := map[store.Provider]int{}
	for k := range s.tokens {
		n[k.p]++
	}
	return n, nil
}

func (s *Store) CreateAPIKey(_ context.Context, k *store.APIKey) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.keys[k.ID]; exists {
		return store.ErrConflict
	}
	s.keys[k.ID] = copyKey(*k)
	return nil
}

func (s *Store) GetAPIKey(_ context.Context, id string) (*store.APIKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.keys[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	cp := copyKey(k)
	return &cp, nil
}

func (s *Store) ListAPIKeys(_ context.Context, user string) ([]*store.APIKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*store.APIKey
	for _, k := range s.keys {
		if k.Username == user {
			cp := copyKey(k)
			out = append(out, &cp)
		}
	}
	slices.SortFunc(out, func(a, b *store.APIKey) int { return b.CreatedAt.Compare(a.CreatedAt) })
	return out, nil
}

func (s *Store) RevokeAPIKey(_ context.Context, user, id string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.keys[id]
	if !ok || k.Username != user {
		return store.ErrNotFound
	}
	if k.RevokedAt == nil {
		k.RevokedAt = &at
		s.keys[id] = k
	}
	return nil
}

func (s *Store) TouchAPIKey(_ context.Context, id string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.keys[id]
	if !ok {
		return store.ErrNotFound
	}
	k.LastUsedAt = &at
	s.keys[id] = k
	return nil
}

func (s *Store) GetAgent(_ context.Context, clientID string) (*store.Agent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.agents[clientID]
	if !ok {
		return nil, store.ErrNotFound
	}
	cp := copyAgent(a)
	return &cp, nil
}

func (s *Store) PutAgent(_ context.Context, a *store.Agent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := copyAgent(*a)
	if old, ok := s.agents[a.ClientID]; ok {
		cp.CreatedAt = old.CreatedAt
	} else if cp.CreatedAt.IsZero() {
		cp.CreatedAt = time.Now()
	}
	s.agents[a.ClientID] = cp
	return nil
}

func (s *Store) ListAgents(_ context.Context) ([]*store.Agent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*store.Agent, 0, len(s.agents))
	for _, a := range s.agents {
		cp := copyAgent(a)
		out = append(out, &cp)
	}
	slices.SortFunc(out, func(a, b *store.Agent) int {
		if a.ClientID < b.ClientID {
			return -1
		}
		if a.ClientID > b.ClientID {
			return 1
		}
		return 0
	})
	return out, nil
}

func (s *Store) InsertAudit(_ context.Context, e *store.AuditEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.audit = append(s.audit, *e)
	return nil
}

func (s *Store) PurgeAudit(_ context.Context, before time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.audit[:0]
	for _, e := range s.audit {
		if !e.Time.Before(before) {
			kept = append(kept, e)
		}
	}
	n := int64(len(s.audit) - len(kept))
	s.audit = kept
	return n, nil
}

// AuditEntries returns a copy of the audit log (test helper).
func (s *Store) AuditEntries() []store.AuditEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.audit)
}

func (s *Store) Ping(context.Context) error { return nil }
func (s *Store) Close()                     {}

func copyKey(k store.APIKey) store.APIKey {
	if k.RevokedAt != nil {
		t := *k.RevokedAt
		k.RevokedAt = &t
	}
	if k.LastUsedAt != nil {
		t := *k.LastUsedAt
		k.LastUsedAt = &t
	}
	return k
}

func copyAgent(a store.Agent) store.Agent {
	a.Credentials = maps.Clone(a.Credentials)
	return a
}
