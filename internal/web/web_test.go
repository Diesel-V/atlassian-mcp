package web

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"atlassian-mcp/internal/atlassian"
	"atlassian-mcp/internal/identity"
	"atlassian-mcp/internal/store"
	"atlassian-mcp/internal/store/memory"
)

var now = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

// fakeLogin emulates Keycloak: it remembers the parameters of the last authorize call.
type fakeLogin struct {
	user                   string
	state, nonce, verifier string
}

func (f *fakeLogin) AuthCodeURL(state, nonce, verifier string) string {
	f.state, f.nonce, f.verifier = state, nonce, verifier
	return "https://kc.example/auth?state=" + url.QueryEscape(state)
}

func (f *fakeLogin) Exchange(_ context.Context, code, verifier, nonce string) (string, error) {
	if code != "kc-code" || verifier != f.verifier || nonce != f.nonce {
		return "", errors.New("bad exchange")
	}
	return f.user, nil
}

// fakeConnector emulates the Confluence authorization server and API.
type fakeConnector struct {
	verifier  string
	atlassian string // username the issued token belongs to
}

func (f *fakeConnector) AuthCodeURL(state, verifier string) string {
	f.verifier = verifier
	return "https://confluence.example/rest/oauth2/latest/authorize?state=" + url.QueryEscape(state)
}

func (f *fakeConnector) Exchange(_ context.Context, code, verifier string) (*oauth2.Token, error) {
	if code != "c-code" || verifier != f.verifier {
		return nil, errors.New("bad code")
	}
	return &oauth2.Token{AccessToken: "at-" + f.atlassian, RefreshToken: "rt", Expiry: now.Add(time.Hour)}, nil
}

func (f *fakeConnector) WhoAmI(_ context.Context, auth atlassian.Auth) (string, error) {
	r, _ := http.NewRequest(http.MethodGet, "http://x", nil)
	auth(r)
	if r.Header.Get("Authorization") != "Bearer at-"+f.atlassian {
		return "", atlassian.ErrUnauthorized
	}
	return f.atlassian, nil
}

type grantSaver struct{ st *memory.Store }

func (g grantSaver) SaveGrant(ctx context.Context, user string, p store.Provider, t *oauth2.Token) error {
	return g.st.PutToken(ctx, &store.OAuthToken{Username: user, Provider: p, AccessToken: t.AccessToken, RefreshToken: t.RefreshToken, Expiry: t.Expiry})
}

type env struct {
	srv   *httptest.Server
	login *fakeLogin
	conf  *fakeConnector
	st    *memory.Store
	c     *http.Client
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{login: &fakeLogin{user: "alice"}, conf: &fakeConnector{atlassian: "Alice"}, st: memory.New()}
	h, err := New(Config{
		PublicURL:     "http://mcp.example",
		SessionKey:    bytes.Repeat([]byte{9}, 32),
		SessionTTL:    8 * time.Hour,
		APIKeyDefault: 90 * 24 * time.Hour,
		APIKeyMax:     180 * 24 * time.Hour,
		Login:         e.login,
		Connectors:    map[store.Provider]Connector{store.Confluence: e.conf},
		Grants:        grantSaver{e.st},
		Tokens:        e.st,
		Keys:          e.st,
		Log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:           func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	e.srv = httptest.NewServer(h)
	t.Cleanup(e.srv.Close)
	e.c = newBrowser()
	return e
}

func newBrowser() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func (e *env) get(t *testing.T, c *http.Client, path string) *http.Response {
	t.Helper()
	resp, err := c.Get(e.srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func (e *env) post(t *testing.T, c *http.Client, path string, form url.Values) *http.Response {
	t.Helper()
	resp, err := c.PostForm(e.srv.URL+path, form)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func body(t *testing.T, r *http.Response) string {
	t.Helper()
	b, _ := io.ReadAll(r.Body)
	return string(b)
}

// signIn walks the browser through the Keycloak login.
func (e *env) signIn(t *testing.T, c *http.Client) {
	t.Helper()
	r := e.get(t, c, "/")
	if r.StatusCode != http.StatusFound || !strings.HasPrefix(r.Header.Get("Location"), "/login") {
		t.Fatalf("anonymous index: %d %s", r.StatusCode, r.Header.Get("Location"))
	}
	r = e.get(t, c, r.Header.Get("Location"))
	if !strings.HasPrefix(r.Header.Get("Location"), "https://kc.example/auth") {
		t.Fatalf("login redirect: %s", r.Header.Get("Location"))
	}
	r = e.get(t, c, "/login/callback?code=kc-code&state="+url.QueryEscape(e.login.state))
	if r.StatusCode != http.StatusFound || r.Header.Get("Location") != "/" {
		t.Fatalf("callback: %d %s %s", r.StatusCode, r.Header.Get("Location"), body(t, r))
	}
}

var csrfRe = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

func (e *env) csrf(t *testing.T, c *http.Client) string {
	t.Helper()
	m := csrfRe.FindStringSubmatch(body(t, e.get(t, c, "/")))
	if m == nil {
		t.Fatal("no csrf token on page")
	}
	return m[1]
}

func TestLoginRequiresMatchingState(t *testing.T) {
	e := newEnv(t)
	c := newBrowser()
	e.get(t, c, "/login")
	if r := e.get(t, c, "/login/callback?code=kc-code&state=forged"); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("forged state: %d", r.StatusCode)
	}
	// A callback without the login cookie (e.g. CSRF login) is rejected.
	if r := e.get(t, newBrowser(), "/login/callback?code=kc-code&state="+e.login.state); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing cookie: %d", r.StatusCode)
	}
}

func TestOpenRedirectBlocked(t *testing.T) {
	for in, want := range map[string]string{"/connect/jira": "/connect/jira", "//evil.com": "/", "https://evil.com": "/", `/\evil.com`: "/", "": "/"} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q", in, got)
		}
	}
}

func TestConnectFlow(t *testing.T) {
	e := newEnv(t)
	e.signIn(t, e.c)

	page := body(t, e.get(t, e.c, "/"))
	if !strings.Contains(page, "alice") || !strings.Contains(page, "Не подключено") || !strings.Contains(page, `href="/connect/confluence"`) {
		t.Fatalf("index:\n%s", page)
	}
	if strings.Contains(page, "/connect/jira") {
		t.Error("disabled provider shown")
	}

	r := e.get(t, e.c, "/connect/confluence")
	loc, _ := url.Parse(r.Header.Get("Location"))
	if loc.Host != "confluence.example" {
		t.Fatalf("connect redirect: %s", loc)
	}
	state := loc.Query().Get("state")

	r = e.get(t, e.c, "/oauth/confluence/callback?code=c-code&state="+url.QueryEscape(state))
	if r.Header.Get("Location") != "/?msg=connected" {
		t.Fatalf("callback: %d %s", r.StatusCode, r.Header.Get("Location"))
	}
	tok, err := e.st.GetToken(context.Background(), "alice", store.Confluence)
	if err != nil || tok.AccessToken != "at-Alice" {
		t.Fatalf("grant not saved: %+v %v", tok, err)
	}
	// The state is single-use.
	if r := e.get(t, e.c, "/oauth/confluence/callback?code=c-code&state="+url.QueryEscape(state)); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("replayed callback: %d", r.StatusCode)
	}

	if !strings.Contains(body(t, e.get(t, e.c, "/")), "Подключено") {
		t.Fatal("connected state not shown")
	}

	r = e.post(t, e.c, "/disconnect/confluence", url.Values{"csrf": {e.csrf(t, e.c)}})
	if r.Header.Get("Location") != "/?msg=disconnected" {
		t.Fatalf("disconnect: %d", r.StatusCode)
	}
	if _, err := e.st.GetToken(context.Background(), "alice", store.Confluence); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("grant not removed")
	}
}

func TestConnectRejectsOtherAtlassianAccount(t *testing.T) {
	e := newEnv(t)
	e.conf.atlassian = "bob" // someone else is logged into Confluence in this browser
	e.signIn(t, e.c)
	loc, _ := url.Parse(e.get(t, e.c, "/connect/confluence").Header.Get("Location"))
	r := e.get(t, e.c, "/oauth/confluence/callback?code=c-code&state="+url.QueryEscape(loc.Query().Get("state")))
	if r.Header.Get("Location") != "/?msg=mismatch" {
		t.Fatalf("callback: %s", r.Header.Get("Location"))
	}
	if _, err := e.st.GetToken(context.Background(), "alice", store.Confluence); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("foreign grant saved")
	}
}

func TestConnectCallbackBoundToUser(t *testing.T) {
	e := newEnv(t)
	e.signIn(t, e.c)
	loc, _ := url.Parse(e.get(t, e.c, "/connect/confluence").Header.Get("Location"))

	// Another signed-in user cannot complete alice's flow with a stolen callback URL.
	other := newBrowser()
	e.login.user = "mallory"
	e.signIn(t, other)
	r := e.get(t, other, "/oauth/confluence/callback?code=c-code&state="+url.QueryEscape(loc.Query().Get("state")))
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("stolen callback: %d", r.StatusCode)
	}
}

func TestConnectDenied(t *testing.T) {
	e := newEnv(t)
	e.signIn(t, e.c)
	loc, _ := url.Parse(e.get(t, e.c, "/connect/confluence").Header.Get("Location"))
	r := e.get(t, e.c, "/oauth/confluence/callback?error=access_denied&state="+url.QueryEscape(loc.Query().Get("state")))
	if r.Header.Get("Location") != "/?msg=denied" {
		t.Fatalf("denied: %s", r.Header.Get("Location"))
	}
}

func TestConnectRequiresLogin(t *testing.T) {
	e := newEnv(t)
	r := e.get(t, newBrowser(), "/connect/confluence")
	if r.Header.Get("Location") != "/login?next=%2Fconnect%2Fconfluence" {
		t.Fatalf("redirect: %s", r.Header.Get("Location"))
	}
	if r := e.get(t, newBrowser(), "/connect/unknown"); r.StatusCode != http.StatusFound {
		t.Fatalf("anonymous unknown provider must redirect to login first, got %d", r.StatusCode)
	}
	e.signIn(t, e.c)
	if r := e.get(t, e.c, "/connect/jira"); r.StatusCode != http.StatusNotFound {
		t.Fatalf("disabled provider: %d", r.StatusCode)
	}
}

func TestAPIKeys(t *testing.T) {
	e := newEnv(t)
	e.signIn(t, e.c)
	token := e.csrf(t, e.c)

	r := e.post(t, e.c, "/keys", url.Values{"csrf": {token}, "name": {"OpenCode"}, "days": {"9999"}})
	page := body(t, r)
	m := regexp.MustCompile(`amcp_[0-9a-f]{16}_[A-Za-z0-9_-]{43}`).FindString(page)
	if r.StatusCode != http.StatusOK || m == "" {
		t.Fatalf("create: %d\n%s", r.StatusCode, page)
	}
	id, secret, err := identity.ParseAPIKey(m)
	if err != nil {
		t.Fatal(err)
	}
	k, err := e.st.GetAPIKey(context.Background(), id)
	if err != nil || k.Username != "alice" || k.Name != "OpenCode" || !identity.SecretMatches(secret, k.SecretHash) {
		t.Fatalf("stored key: %+v %v", k, err)
	}
	if got := k.ExpiresAt.Sub(now); got != 180*24*time.Hour {
		t.Errorf("ttl not capped: %v", got)
	}
	if strings.Contains(body(t, e.get(t, e.c, "/")), secret) {
		t.Fatal("secret shown again after creation")
	}

	if r := e.post(t, e.c, "/keys", url.Values{"csrf": {token}, "name": {" "}}); r.Header.Get("Location") != "/?msg=badname" {
		t.Errorf("empty name: %s", r.Header.Get("Location"))
	}

	// Another user cannot revoke alice's key.
	other := newBrowser()
	e.login.user = "mallory"
	e.signIn(t, other)
	if r := e.post(t, other, "/keys/"+id+"/revoke", url.Values{"csrf": {e.csrf(t, other)}}); r.StatusCode != http.StatusNotFound {
		t.Fatalf("foreign revoke: %d", r.StatusCode)
	}

	if r := e.post(t, e.c, "/keys/"+id+"/revoke", url.Values{"csrf": {token}}); r.Header.Get("Location") != "/?msg=revoked" {
		t.Fatalf("revoke: %d", r.StatusCode)
	}
	k, _ = e.st.GetAPIKey(context.Background(), id)
	if k.RevokedAt == nil {
		t.Fatal("not revoked")
	}
}

func TestCSRFProtection(t *testing.T) {
	e := newEnv(t)
	e.signIn(t, e.c)
	if r := e.post(t, e.c, "/keys", url.Values{"name": {"x"}}); r.StatusCode != http.StatusForbidden {
		t.Fatalf("missing csrf: %d", r.StatusCode)
	}
	if r := e.post(t, e.c, "/keys", url.Values{"csrf": {"forged"}, "name": {"x"}}); r.StatusCode != http.StatusForbidden {
		t.Fatalf("forged csrf: %d", r.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/keys",
		strings.NewReader(url.Values{"csrf": {e.csrf(t, e.c)}, "name": {"x"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://evil.example")
	r, err := e.c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin post: %d", r.StatusCode)
	}
	if r := e.post(t, newBrowser(), "/keys", url.Values{"csrf": {"x"}, "name": {"x"}}); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no session: %d", r.StatusCode)
	}
}

func TestSecurityHeadersAndCookies(t *testing.T) {
	e := newEnv(t)
	r := e.get(t, e.c, "/login")
	for h, want := range map[string]string{
		"X-Frame-Options": "DENY", "Referrer-Policy": "no-referrer", "Cache-Control": "no-store", "X-Content-Type-Options": "nosniff",
	} {
		if got := r.Header.Get(h); got != want {
			t.Errorf("%s = %q", h, got)
		}
	}
	if !strings.Contains(r.Header.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Errorf("CSP = %q", r.Header.Get("Content-Security-Policy"))
	}
	for _, c := range r.Cookies() {
		if !c.HttpOnly || c.SameSite != http.SameSiteLaxMode {
			t.Errorf("cookie %s flags: %+v", c.Name, c)
		}
	}
}

func TestTamperedSessionRejected(t *testing.T) {
	e := newEnv(t)
	c := newBrowser()
	u, _ := url.Parse(e.srv.URL)
	c.Jar.SetCookies(u, []*http.Cookie{{Name: sessionCookie, Value: "forged"}})
	if r := e.get(t, c, "/"); r.StatusCode != http.StatusFound {
		t.Fatalf("forged session accepted: %d", r.StatusCode)
	}
}
