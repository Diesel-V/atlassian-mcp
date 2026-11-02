// Package offboard cuts off a person's access: it revokes their agent keys and
// deletes their Confluence/Jira grants. It is run by an administrator when an
// employee leaves or when access must be withdrawn immediately.
package offboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"atlassian-mcp/internal/store"
)

// Store is what offboarding needs from persistence.
type Store interface {
	store.Tokens
	store.APIKeys
	store.Agents
	store.Audit
}

// Report lists what was withdrawn and what needs a human decision.
type Report struct {
	Username     string
	RevokedKeys  []string         // IDs of keys that were active and are now revoked
	Disconnected []store.Provider // grants that were deleted
	OwnedAgents  []string         // autonomous agents this person owns: reassign or disable them
}

// User withdraws all access held by username. It is idempotent: running it
// again revokes nothing new. The action is written to the audit log.
func User(ctx context.Context, st Store, username, operator string, now time.Time) (*Report, error) {
	username = strings.ToLower(strings.TrimSpace(username))
	if username == "" {
		return nil, errors.New("offboard: username is required")
	}
	rep := &Report{Username: username}

	keys, err := st.ListAPIKeys(ctx, username)
	if err != nil {
		return nil, fmt.Errorf("list keys: %w", err)
	}
	for _, k := range keys {
		if !k.Active(now) {
			continue
		}
		if err := st.RevokeAPIKey(ctx, username, k.ID, now); err != nil {
			return rep, fmt.Errorf("revoke key %s: %w", k.ID, err)
		}
		rep.RevokedKeys = append(rep.RevokedKeys, k.ID)
	}

	providers, err := st.ConnectedProviders(ctx, username)
	if err != nil {
		return rep, fmt.Errorf("list grants: %w", err)
	}
	for _, p := range providers {
		if err := st.DeleteToken(ctx, username, p); err != nil {
			return rep, fmt.Errorf("delete %s grant: %w", p, err)
		}
		rep.Disconnected = append(rep.Disconnected, p)
	}

	agents, err := st.ListAgents(ctx)
	if err != nil {
		return rep, fmt.Errorf("list agents: %w", err)
	}
	for _, a := range agents {
		if strings.EqualFold(a.Owner, username) {
			rep.OwnedAgents = append(rep.OwnedAgents, a.ClientID)
		}
	}

	params, _ := json.Marshal(map[string]any{
		"username": username, "revoked_keys": len(rep.RevokedKeys), "disconnected": rep.Disconnected,
	})
	if err := st.InsertAudit(ctx, &store.AuditEntry{
		Time: now, SubjectKind: "admin", Principal: operator, Tool: "offboard",
		Params: params, Results: len(rep.RevokedKeys) + len(rep.Disconnected), Status: "ok",
	}); err != nil {
		return rep, fmt.Errorf("audit: %w", err)
	}
	return rep, nil
}
