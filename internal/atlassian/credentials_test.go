package atlassian

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"atlassian-mcp/internal/identity"
	"atlassian-mcp/internal/store"
	"atlassian-mcp/internal/store/memory"
)

var t0 = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

// fakeTokenServer emulates the Data Center token endpoint with rotating refresh tokens.
type fakeTokenServer struct {
	srv      *httptest.Server
	calls    atomic.Int32
	mu       sync.Mutex
	validRT  string
	delay    time.Duration
	failWith int
}

func newFakeTokenServer(t *testing.T) *fakeTokenServer {
	f := &fakeTokenServer{validRT: "rt-1"}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rest/oauth2/latest/token" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = r.ParseForm()
		n := f.calls.Add(1)
		time.Sleep(f.delay)
		if f.failWith != 0 {
			w.WriteHeader(f.failWith)
			return
		}
		if r.Form.Get("client_id") != "cid" || r.Form.Get("client_secret") != "csec" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.Form.Get("grant_type") {
		case "refresh_token":
			f.mu.Lock()
			defer f.mu.Unlock()
			if r.Form.Get("refresh_token") != f.validRT {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":"invalid_grant"}`)
				return
			}
			f.validRT = fmt.Sprintf("rt-%d", n+1)
			fmt.Fprintf(w, `{"access_token":"at-%d","refresh_token":%q,"token_type":"bearer","expires_in":3600}`, n, f.validRT)
		case "authorization_code":
			if r.Form.Get("code") != "good-code" || r.Form.Get("code_verifier") == "" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":"invalid_grant"}`)
				return
			}
			_, _ = io.WriteString(w, `{"access_token":"at-code","refresh_token":"rt-1","token_type":"bearer","expires_in":3600}`)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func setup(t *testing.T) (*Credentials, *memory.Store, *fakeTokenServer) {
	f := newFakeTokenServer(t)
	st := memory.New()
	op := NewOAuthProvider(store.Confluence, f.srv.URL, "cid", "csec", "https://mcp/cb", []string{"READ"}, f.srv.Client(), nil)
	c := &Credentials{Tokens: st, Agents: st, OAuth: map[store.Provider]*OAuthProvider{store.Confluence: op}, Now: func() time.Time { return t0 }}
	return c, st, f
}

func authHeader(t *testing.T, a Auth) string {
	t.Helper()
	r, _ := http.NewRequest(http.MethodGet, "http://x", nil)
	a(r)
	return r.Header.Get("Authorization")
}

var alice = identity.Subject{Kind: identity.KindUser, Username: "alice"}

func TestUserNotConnected(t *testing.T) {
	c, _, _ := setup(t)
	if _, err := c.Auth(context.Background(), alice, store.Confluence); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("err = %v", err)
	}
}

func TestUserFreshToken(t *testing.T) {
	c, st, f := setup(t)
	ctx := context.Background()
	_ = st.PutToken(ctx, &store.OAuthToken{Username: "alice", Provider: store.Confluence, AccessToken: "at-0", RefreshToken: "rt-1", Expiry: t0.Add(time.Hour)})
	a, err := c.Auth(ctx, alice, store.Confluence)
	if err != nil || authHeader(t, a) != "Bearer at-0" {
		t.Fatalf("auth = %v", err)
	}
	if f.calls.Load() != 0 {
		t.Fatal("fresh token must not be refreshed")
	}
	// An API-key subject uses its owner's grant.
	a, err = c.Auth(ctx, identity.Subject{Kind: identity.KindAPIKey, Username: "alice", KeyID: "k"}, store.Confluence)
	if err != nil || authHeader(t, a) != "Bearer at-0" {
		t.Fatalf("api key auth = %v", err)
	}
}

func TestUserRefreshRotatesAndCollapses(t *testing.T) {
	c, st, f := setup(t)
	f.delay = 50 * time.Millisecond
	ctx := context.Background()
	_ = st.PutToken(ctx, &store.OAuthToken{Username: "alice", Provider: store.Confluence, AccessToken: "old", RefreshToken: "rt-1", Expiry: t0.Add(30 * time.Second)})

	var wg sync.WaitGroup
	results := make([]string, 8)
	errs := make([]error, 8)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			a, err := c.Auth(ctx, alice, store.Confluence)
			errs[i] = err
			if err == nil {
				results[i] = authHeader(t, a)
			}
		}(i)
	}
	wg.Wait()
	for i := range results {
		if errs[i] != nil || results[i] != "Bearer at-1" {
			t.Fatalf("caller %d: %q %v", i, results[i], errs[i])
		}
	}
	if n := f.calls.Load(); n != 1 {
		t.Fatalf("token endpoint called %d times, want 1", n)
	}
	saved, _ := st.GetToken(ctx, "alice", store.Confluence)
	if saved.AccessToken != "at-1" || saved.RefreshToken != "rt-2" || !saved.Expiry.After(t0) {
		t.Fatalf("saved = %+v", saved)
	}
}

func TestUserRefreshRejectedRemovesGrant(t *testing.T) {
	c, st, _ := setup(t)
	ctx := context.Background()
	_ = st.PutToken(ctx, &store.OAuthToken{Username: "alice", Provider: store.Confluence, AccessToken: "old", RefreshToken: "stolen-and-used", Expiry: t0.Add(-time.Hour)})
	if _, err := c.Auth(ctx, alice, store.Confluence); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("err = %v", err)
	}
	if _, err := st.GetToken(ctx, "alice", store.Confluence); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("rejected grant still stored")
	}
}

func TestUserRefreshUpstreamFailureKeepsGrant(t *testing.T) {
	c, st, f := setup(t)
	f.failWith = http.StatusBadGateway
	ctx := context.Background()
	_ = st.PutToken(ctx, &store.OAuthToken{Username: "alice", Provider: store.Confluence, AccessToken: "old", RefreshToken: "rt-1", Expiry: t0.Add(-time.Hour)})
	if _, err := c.Auth(ctx, alice, store.Confluence); !errors.Is(err, ErrUpstream) {
		t.Fatalf("err = %v", err)
	}
	if _, err := st.GetToken(ctx, "alice", store.Confluence); err != nil {
		t.Fatal("grant removed on transient failure")
	}
}

func TestAgentCredentials(t *testing.T) {
	c, st, _ := setup(t)
	ctx := context.Background()
	_ = st.PutAgent(ctx, &store.Agent{ClientID: "bot", Enabled: true, Credentials: map[store.Provider]string{store.Jira: "pat-j"}})
	_ = st.PutAgent(ctx, &store.Agent{ClientID: "off", Enabled: false, Credentials: map[store.Provider]string{store.Jira: "pat"}})
	bot := identity.Subject{Kind: identity.KindAgent, AgentID: "bot"}

	a, err := c.Auth(ctx, bot, store.Jira)
	if err != nil || authHeader(t, a) != "Bearer pat-j" {
		t.Fatalf("agent jira: %v", err)
	}
	for name, s := range map[string]identity.Subject{
		"no confluence creds": bot,
		"disabled":            {Kind: identity.KindAgent, AgentID: "off"},
		"unknown":             {Kind: identity.KindAgent, AgentID: "ghost"},
	} {
		p := store.Confluence
		if name == "disabled" {
			p = store.Jira
		}
		if _, err := c.Auth(ctx, s, p); !errors.Is(err, ErrNoAgentAccess) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	// Agents never fall back to anybody's user grant.
	_ = st.PutToken(ctx, &store.OAuthToken{Username: "bot", Provider: store.Confluence, AccessToken: "x", Expiry: t0.Add(time.Hour)})
	if _, err := c.Auth(ctx, bot, store.Confluence); !errors.Is(err, ErrNoAgentAccess) {
		t.Fatalf("agent used a user grant: %v", err)
	}
}

func TestInvalidate(t *testing.T) {
	c, st, _ := setup(t)
	ctx := context.Background()
	_ = st.PutToken(ctx, &store.OAuthToken{Username: "alice", Provider: store.Confluence, AccessToken: "a", Expiry: t0.Add(time.Hour)})
	if err := c.Invalidate(ctx, alice, store.Confluence); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Auth(ctx, alice, store.Confluence); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("err = %v", err)
	}
	if err := c.Invalidate(ctx, identity.Subject{Kind: identity.KindAgent, AgentID: "bot"}, store.Confluence); err != nil {
		t.Fatal(err)
	}
}

func TestAuthorizationCodeFlow(t *testing.T) {
	c, st, f := setup(t)
	op := c.OAuth[store.Confluence]
	ctx := context.Background()

	u, err := url.Parse(op.AuthCodeURL("state-123", "verifier-verifier-verifier-verifier-verifier-1"))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if !strings.HasPrefix(u.String(), f.srv.URL+"/rest/oauth2/latest/authorize") || q.Get("state") != "state-123" ||
		q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || q.Get("client_id") != "cid" ||
		q.Get("redirect_uri") != "https://mcp/cb" || q.Get("scope") != "READ" {
		t.Fatalf("auth url = %s", u)
	}

	if _, err := op.Exchange(ctx, "bad-code", "v"); err == nil {
		t.Fatal("bad code accepted")
	}
	tok, err := op.Exchange(ctx, "good-code", "verifier")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SaveGrant(ctx, "alice", store.Confluence, tok); err != nil {
		t.Fatal(err)
	}
	saved, _ := st.GetToken(ctx, "alice", store.Confluence)
	if saved.AccessToken != "at-code" || saved.RefreshToken != "rt-1" {
		t.Fatalf("saved = %+v", saved)
	}
}
