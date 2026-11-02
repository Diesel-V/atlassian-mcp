// Package postgres is the production store.Store implementation.
// Secrets (OAuth tokens, agent PATs) are encrypted before they reach the database.
package postgres

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"atlassian-mcp/internal/crypto"
	"atlassian-mcp/internal/store"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// migrationLockID is an arbitrary constant for pg_advisory_lock.
const migrationLockID = 727_135_001

// Store is a PostgreSQL-backed store.Store.
type Store struct {
	pool   *pgxpool.Pool
	sealer *crypto.Sealer
}

var _ store.Store = (*Store)(nil)

// Open connects to the database. Call Migrate before first use.
func Open(ctx context.Context, dsn string, sealer *crypto.Sealer) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres: connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: ping: %w", err)
	}
	return &Store{pool: pool, sealer: sealer}, nil
}

// Migrate applies pending embedded migrations under an advisory lock,
// so concurrent replicas don't race.
func (s *Store) Migrate(ctx context.Context) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLockID); err != nil {
		return fmt.Errorf("postgres: lock: %w", err)
	}
	defer conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, migrationLockID) //nolint:errcheck

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	var current int
	if err := conn.QueryRow(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return err
	}

	files, err := fs.Glob(migrationsFS, "migrations/*.sql")
	if err != nil {
		return err
	}
	slices.Sort(files)
	for _, f := range files {
		version, err := strconv.Atoi(strings.SplitN(strings.TrimPrefix(f, "migrations/"), "_", 2)[0])
		if err != nil {
			return fmt.Errorf("postgres: bad migration name %q", f)
		}
		if version <= current {
			continue
		}
		sql, err := migrationsFS.ReadFile(f)
		if err != nil {
			return err
		}
		err = pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, string(sql)); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, version)
			return err
		})
		if err != nil {
			return fmt.Errorf("postgres: migration %s: %w", f, err)
		}
	}
	return nil
}

func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }
func (s *Store) Close()                         { s.pool.Close() }

// Associated data binds each ciphertext to its owner and purpose.
func tokenAD(user string, p store.Provider, field string) string {
	return "oauth|" + user + "|" + string(p) + "|" + field
}
func agentAD(clientID string) string { return "agent|" + clientID }

func (s *Store) GetToken(ctx context.Context, user string, p store.Provider) (*store.OAuthToken, error) {
	var t store.OAuthToken
	var access, refresh string
	err := s.pool.QueryRow(ctx,
		`SELECT access_token, refresh_token, expiry, updated_at FROM oauth_tokens WHERE username=$1 AND provider=$2`,
		user, string(p)).Scan(&access, &refresh, &t.Expiry, &t.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if t.AccessToken, err = s.sealer.OpenString(access, tokenAD(user, p, "access")); err != nil {
		return nil, fmt.Errorf("postgres: decrypt access token: %w", err)
	}
	if t.RefreshToken, err = s.sealer.OpenString(refresh, tokenAD(user, p, "refresh")); err != nil {
		return nil, fmt.Errorf("postgres: decrypt refresh token: %w", err)
	}
	t.Username, t.Provider = user, p
	return &t, nil
}

func (s *Store) PutToken(ctx context.Context, t *store.OAuthToken) error {
	access, err := s.sealer.SealString(t.AccessToken, tokenAD(t.Username, t.Provider, "access"))
	if err != nil {
		return err
	}
	refresh, err := s.sealer.SealString(t.RefreshToken, tokenAD(t.Username, t.Provider, "refresh"))
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO oauth_tokens (username, provider, access_token, refresh_token, expiry, updated_at)
		VALUES ($1, $2, $3, $4, $5, now())
		ON CONFLICT (username, provider) DO UPDATE
		SET access_token=EXCLUDED.access_token, refresh_token=EXCLUDED.refresh_token,
		    expiry=EXCLUDED.expiry, updated_at=now()`,
		t.Username, string(t.Provider), access, refresh, t.Expiry)
	return err
}

func (s *Store) DeleteToken(ctx context.Context, user string, p store.Provider) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM oauth_tokens WHERE username=$1 AND provider=$2`, user, string(p))
	return err
}

func (s *Store) ConnectedProviders(ctx context.Context, user string) ([]store.Provider, error) {
	rows, err := s.pool.Query(ctx, `SELECT provider FROM oauth_tokens WHERE username=$1 ORDER BY provider`, user)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (store.Provider, error) {
		var p string
		err := r.Scan(&p)
		return store.Provider(p), err
	})
}

func (s *Store) CountConnected(ctx context.Context) (map[store.Provider]int, error) {
	rows, err := s.pool.Query(ctx, `SELECT provider, count(*) FROM oauth_tokens GROUP BY provider`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	n := map[store.Provider]int{}
	for rows.Next() {
		var p string
		var c int
		if err := rows.Scan(&p, &c); err != nil {
			return nil, err
		}
		n[store.Provider(p)] = c
	}
	return n, rows.Err()
}

const apiKeyCols = `id, username, name, secret_hash, created_at, expires_at, revoked_at, last_used_at`

func scanKey(r pgx.CollectableRow) (*store.APIKey, error) {
	var k store.APIKey
	err := r.Scan(&k.ID, &k.Username, &k.Name, &k.SecretHash, &k.CreatedAt, &k.ExpiresAt, &k.RevokedAt, &k.LastUsedAt)
	return &k, err
}

func (s *Store) CreateAPIKey(ctx context.Context, k *store.APIKey) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO api_keys (`+apiKeyCols+`) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		k.ID, k.Username, k.Name, k.SecretHash, k.CreatedAt, k.ExpiresAt, k.RevokedAt, k.LastUsedAt)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return store.ErrConflict
	}
	return err
}

func (s *Store) GetAPIKey(ctx context.Context, id string) (*store.APIKey, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+apiKeyCols+` FROM api_keys WHERE id=$1`, id)
	if err != nil {
		return nil, err
	}
	k, err := pgx.CollectExactlyOneRow(rows, scanKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	return k, err
}

func (s *Store) ListAPIKeys(ctx context.Context, user string) ([]*store.APIKey, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+apiKeyCols+` FROM api_keys WHERE username=$1 ORDER BY created_at DESC`, user)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanKey)
}

func (s *Store) RevokeAPIKey(ctx context.Context, user, id string, at time.Time) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE api_keys SET revoked_at=COALESCE(revoked_at, $3) WHERE id=$1 AND username=$2`, id, user, at)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *Store) TouchAPIKey(ctx context.Context, id string, at time.Time) error {
	tag, err := s.pool.Exec(ctx, `UPDATE api_keys SET last_used_at=$2 WHERE id=$1`, id, at)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *Store) scanAgent(r pgx.CollectableRow) (*store.Agent, error) {
	var a store.Agent
	var sealed string
	if err := r.Scan(&a.ClientID, &a.Name, &a.Owner, &a.Enabled, &sealed, &a.CreatedAt); err != nil {
		return nil, err
	}
	raw, err := s.sealer.OpenString(sealed, agentAD(a.ClientID))
	if err != nil {
		return nil, fmt.Errorf("postgres: decrypt agent %s: %w", a.ClientID, err)
	}
	if err := json.Unmarshal([]byte(raw), &a.Credentials); err != nil {
		return nil, err
	}
	return &a, nil
}

func (s *Store) GetAgent(ctx context.Context, clientID string) (*store.Agent, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT client_id, name, owner, enabled, credentials, created_at FROM agents WHERE client_id=$1`, clientID)
	if err != nil {
		return nil, err
	}
	a, err := pgx.CollectExactlyOneRow(rows, s.scanAgent)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	return a, err
}

func (s *Store) PutAgent(ctx context.Context, a *store.Agent) error {
	creds := a.Credentials
	if creds == nil {
		creds = map[store.Provider]string{}
	}
	raw, err := json.Marshal(creds)
	if err != nil {
		return err
	}
	sealed, err := s.sealer.SealString(string(raw), agentAD(a.ClientID))
	if err != nil {
		return err
	}
	created := a.CreatedAt
	if created.IsZero() {
		created = time.Now()
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO agents (client_id, name, owner, enabled, credentials, created_at)
		VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (client_id) DO UPDATE
		SET name=EXCLUDED.name, owner=EXCLUDED.owner, enabled=EXCLUDED.enabled, credentials=EXCLUDED.credentials`,
		a.ClientID, a.Name, a.Owner, a.Enabled, sealed, created)
	return err
}

func (s *Store) ListAgents(ctx context.Context) ([]*store.Agent, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT client_id, name, owner, enabled, credentials, created_at FROM agents ORDER BY client_id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, s.scanAgent)
}

func (s *Store) InsertAudit(ctx context.Context, e *store.AuditEntry) error {
	params := e.Params
	if len(params) == 0 {
		params = json.RawMessage(`{}`)
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO audit_log (ts, subject_kind, principal, key_id, tool, params, results, status, duration_ms, error)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		e.Time, e.SubjectKind, e.Principal, e.KeyID, e.Tool, []byte(params), e.Results, e.Status, e.DurationMS, e.Error)
	return err
}

func (s *Store) PurgeAudit(ctx context.Context, before time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM audit_log WHERE ts < $1`, before)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
