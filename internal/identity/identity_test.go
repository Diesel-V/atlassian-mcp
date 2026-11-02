package identity

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"

	"atlassian-mcp/internal/store"
	"atlassian-mcp/internal/store/memory"
)

type fakeJWT map[string]*Claims

func (f fakeJWT) Verify(_ context.Context, raw string) (*Claims, error) {
	if c, ok := f[raw]; ok {
		return c, nil
	}
	return nil, errors.New("bad signature")
}

var now = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

func newResolver(t *testing.T) (*Resolver, *memory.Store) {
	t.Helper()
	st := memory.New()
	exp := now.Add(time.Hour)
	r := &Resolver{
		JWT: fakeJWT{
			"user":        {AuthorizedParty: "open-webui", Username: "I.Ivanov@Example.com", Expiry: exp},
			"svc-user":    {AuthorizedParty: "open-webui", Username: "service-account-open-webui", Expiry: exp},
			"no-username": {AuthorizedParty: "open-webui", Expiry: exp},
			"no-azp":      {Username: "alice", Expiry: exp},
			"agent":       {AuthorizedParty: "ticket-bot", Username: "service-account-ticket-bot", Expiry: exp},
			"disabled":    {AuthorizedParty: "old-bot", Expiry: exp},
			"unknown":     {AuthorizedParty: "random-client", Expiry: exp},
		},
		Keys:        st,
		Agents:      st,
		UserClients: []string{"open-webui"},
		StripDomain: "example.com",
		Now:         func() time.Time { return now },
	}
	ctx := context.Background()
	_ = st.PutAgent(ctx, &store.Agent{ClientID: "ticket-bot", Enabled: true})
	_ = st.PutAgent(ctx, &store.Agent{ClientID: "old-bot", Enabled: false})
	return r, st
}

func TestResolveJWT(t *testing.T) {
	r, _ := newResolver(t)
	ctx := context.Background()

	s, exp, err := r.Resolve(ctx, "user")
	if err != nil {
		t.Fatal(err)
	}
	if s != (Subject{Kind: KindUser, Username: "i.ivanov"}) || !exp.Equal(now.Add(time.Hour)) {
		t.Fatalf("user subject = %+v exp=%v", s, exp)
	}

	s, _, err = r.Resolve(ctx, "agent")
	if err != nil || s != (Subject{Kind: KindAgent, AgentID: "ticket-bot"}) {
		t.Fatalf("agent subject = %+v, %v", s, err)
	}
	if s.Principal() != "agent:ticket-bot" {
		t.Fatalf("principal = %q", s.Principal())
	}

	for raw, want := range map[string]error{
		"forged":      auth.ErrInvalidToken,
		"svc-user":    auth.ErrInvalidToken,
		"no-username": auth.ErrInvalidToken,
		"no-azp":      auth.ErrInvalidToken,
		"disabled":    ErrForbidden,
		"unknown":     ErrForbidden,
	} {
		if _, _, err := r.Resolve(ctx, raw); !errors.Is(err, want) {
			t.Errorf("%s: err = %v, want %v", raw, err, want)
		}
	}
}

func TestResolveAPIKey(t *testing.T) {
	r, st := newResolver(t)
	ctx := context.Background()

	full, id, secret, err := NewAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateAPIKey(ctx, &store.APIKey{ID: id, Username: "alice", SecretHash: HashSecret(secret),
		CreatedAt: now, ExpiresAt: now.Add(24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}

	s, exp, err := r.Resolve(ctx, full)
	if err != nil {
		t.Fatal(err)
	}
	if s != (Subject{Kind: KindAPIKey, Username: "alice", KeyID: id}) || !exp.Equal(now.Add(24*time.Hour)) {
		t.Fatalf("subject = %+v, exp = %v", s, exp)
	}
	if k, _ := st.GetAPIKey(ctx, id); k.LastUsedAt == nil {
		t.Error("last_used_at not updated")
	}

	wrongSecret := full[:len(full)-1] + flip(full[len(full)-1])
	if _, _, err := r.Resolve(ctx, wrongSecret); !errors.Is(err, auth.ErrInvalidToken) {
		t.Errorf("wrong secret: err = %v", err)
	}
	if _, _, err := r.Resolve(ctx, "amcp_garbage"); !errors.Is(err, auth.ErrInvalidToken) {
		t.Errorf("malformed: err = %v", err)
	}
	otherFull, _, _, _ := NewAPIKey()
	if _, _, err := r.Resolve(ctx, otherFull); !errors.Is(err, auth.ErrInvalidToken) {
		t.Errorf("unknown id: err = %v", err)
	}

	r.Now = func() time.Time { return now.Add(25 * time.Hour) }
	if _, _, err := r.Resolve(ctx, full); !errors.Is(err, auth.ErrInvalidToken) {
		t.Errorf("expired: err = %v", err)
	}
	r.Now = func() time.Time { return now }
	_ = st.RevokeAPIKey(ctx, "alice", id, now)
	if _, _, err := r.Resolve(ctx, full); !errors.Is(err, auth.ErrInvalidToken) {
		t.Errorf("revoked: err = %v", err)
	}
}

func TestTokenVerifierAdapter(t *testing.T) {
	r, _ := newResolver(t)
	v := r.TokenVerifier()
	ti, err := v(context.Background(), "user", nil)
	if err != nil {
		t.Fatal(err)
	}
	s, ok := SubjectFromTokenInfo(ti)
	if !ok || s.Username != "i.ivanov" || ti.UserID != "i.ivanov" {
		t.Fatalf("token info = %+v", ti)
	}
	if !slices.Contains(ti.Scopes, ScopeAccess) {
		t.Fatalf("access scope missing: %+v", ti)
	}
	// Unregistered client: valid token, no subject, no access scope (the middleware answers 403).
	denied, err := v(context.Background(), "unknown", nil)
	if err != nil || slices.Contains(denied.Scopes, ScopeAccess) || denied.Expiration.IsZero() {
		t.Fatalf("unregistered client: info=%+v err=%v", denied, err)
	}
	if _, ok := SubjectFromTokenInfo(denied); ok {
		t.Fatal("unregistered client yielded a subject")
	}
	if _, err := v(context.Background(), "forged", nil); err == nil || strings.Contains(err.Error(), "signature") {
		t.Fatalf("verification details leaked or missing: %v", err)
	}
	if _, ok := SubjectFromTokenInfo(nil); ok {
		t.Fatal("nil token info yielded a subject")
	}
}

func TestAPIKeyFormat(t *testing.T) {
	full, id, secret, err := NewAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	gotID, gotSecret, err := ParseAPIKey(full)
	if err != nil || gotID != id || gotSecret != secret {
		t.Fatalf("parse = %q %q %v", gotID, gotSecret, err)
	}
	if !SecretMatches(secret, HashSecret(secret)) || SecretMatches(secret+"x", HashSecret(secret)) {
		t.Fatal("SecretMatches")
	}
	for _, bad := range []string{"", "amcp_", "amcp_zz_" + secret, "xxx_" + id + "_" + secret, "amcp_" + id + secret} {
		if _, _, err := ParseAPIKey(bad); !errors.Is(err, ErrMalformedKey) {
			t.Errorf("ParseAPIKey(%q) err = %v", bad, err)
		}
	}
}

func TestNormalizeUsername(t *testing.T) {
	cases := map[[2]string]string{
		{" Alice@Example.com ", "example.com"}: "alice",
		{"alice@other.com", "example.com"}:     "alice@other.com",
		{"Bob", ""}:                            "bob",
	}
	for in, want := range cases {
		if got := NormalizeUsername(in[0], in[1]); got != want {
			t.Errorf("NormalizeUsername(%q, %q) = %q, want %q", in[0], in[1], got, want)
		}
	}
}

func TestSubjectContext(t *testing.T) {
	s := Subject{Kind: KindUser, Username: "alice"}
	got, ok := FromContext(WithSubject(context.Background(), s))
	if !ok || got != s || !got.ActsAsUser() {
		t.Fatalf("got %+v %v", got, ok)
	}
	if _, ok := FromContext(context.Background()); ok {
		t.Fatal("empty context has subject")
	}
}

func flip(c byte) string {
	if c == 'A' {
		return "B"
	}
	return "A"
}
