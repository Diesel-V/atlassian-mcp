// Package web serves the "My access" pages: browser login via Keycloak,
// connecting Confluence/Jira with OAuth 2.0 and managing personal agent keys.
package web

import (
	"context"
	"crypto/subtle"
	"embed"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/oauth2"

	"atlassian-mcp/internal/atlassian"
	"atlassian-mcp/internal/crypto"
	"atlassian-mcp/internal/identity"
	"atlassian-mcp/internal/store"
)

//go:embed templates/*.html
var templatesFS embed.FS

var pageTmpl = template.Must(template.New("index.html").Funcs(template.FuncMap{
	"date": func(t time.Time) string { return t.Local().Format("02.01.2006") },
	"datetimep": func(t *time.Time) string {
		if t == nil {
			return "—"
		}
		return t.Local().Format("02.01.2006 15:04")
	},
}).ParseFS(templatesFS, "templates/index.html"))

// Connector is an Atlassian product the user can grant access to.
type Connector interface {
	AuthCodeURL(state, verifier string) string
	Exchange(ctx context.Context, code, verifier string) (*oauth2.Token, error)
	WhoAmI(ctx context.Context, auth atlassian.Auth) (string, error)
}

// GrantSaver persists an OAuth grant.
type GrantSaver interface {
	SaveGrant(ctx context.Context, user string, p store.Provider, t *oauth2.Token) error
}

// Config wires the handler.
type Config struct {
	PublicURL     string
	SessionKey    []byte
	SessionTTL    time.Duration
	APIKeyDefault time.Duration
	APIKeyMax     time.Duration
	Login         LoginProvider
	Connectors    map[store.Provider]Connector
	Grants        GrantSaver
	Tokens        store.Tokens
	Keys          store.APIKeys
	Log           *slog.Logger
	Now           func() time.Time
}

// Handler serves the web UI.
type Handler struct {
	cfg    Config
	jar    *cookieJar
	origin string
	mux    *http.ServeMux
}

const (
	sessionCookie = "amcp_session"
	loginCookie   = "amcp_login"
	connectCookie = "amcp_connect"
	flowTTL       = 10 * time.Minute
)

type session struct {
	User string `json:"u"`
	CSRF string `json:"c"`
}

type loginFlow struct {
	State, Nonce, Verifier, Next string
}

type connectFlow struct {
	State, Verifier, User string
	Provider              store.Provider
}

// New builds the handler.
func New(cfg Config) (*Handler, error) {
	sealer, err := crypto.NewSealer(cfg.SessionKey)
	if err != nil {
		return nil, err
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	u, err := url.Parse(cfg.PublicURL)
	if err != nil {
		return nil, err
	}
	h := &Handler{
		cfg:    cfg,
		jar:    &cookieJar{sealer: sealer, secure: u.Scheme == "https", now: cfg.Now},
		origin: u.Scheme + "://" + u.Host,
		mux:    http.NewServeMux(),
	}
	h.mux.HandleFunc("GET /{$}", h.index)
	h.mux.HandleFunc("GET /login", h.login)
	h.mux.HandleFunc("GET /login/callback", h.loginCallback)
	h.mux.HandleFunc("POST /logout", h.withSession(h.logout))
	h.mux.HandleFunc("GET /connect/{provider}", h.connect)
	h.mux.HandleFunc("GET /oauth/{provider}/callback", h.connectCallback)
	h.mux.HandleFunc("POST /disconnect/{provider}", h.withSession(h.disconnect))
	h.mux.HandleFunc("POST /keys", h.withSession(h.createKey))
	h.mux.HandleFunc("POST /keys/{id}/revoke", h.withSession(h.revokeKey))
	return h, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	hdr := w.Header()
	hdr.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
	hdr.Set("X-Frame-Options", "DENY")
	hdr.Set("X-Content-Type-Options", "nosniff")
	hdr.Set("Referrer-Policy", "no-referrer") // callback URLs carry authorization codes
	hdr.Set("Cache-Control", "no-store")
	h.mux.ServeHTTP(w, r)
}

func (h *Handler) currentSession(r *http.Request) (session, bool) {
	var s session
	ok := h.jar.get(r, sessionCookie, &s)
	return s, ok && s.User != ""
}

// withSession requires a session and a valid CSRF token for state-changing requests.
func (h *Handler) withSession(next func(http.ResponseWriter, *http.Request, session)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s, ok := h.currentSession(r)
		if !ok {
			http.Error(w, "Сессия истекла, обновите страницу", http.StatusUnauthorized)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && origin != h.origin {
			http.Error(w, "Запрос с чужого сайта отклонён", http.StatusForbidden)
			return
		}
		if subtle.ConstantTimeCompare([]byte(r.PostFormValue("csrf")), []byte(s.CSRF)) != 1 {
			http.Error(w, "Недействительный токен формы, обновите страницу", http.StatusForbidden)
			return
		}
		next(w, r, s)
	}
}

func (h *Handler) redirectToLogin(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
}

// --- login ---

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	f := loginFlow{State: randomToken(), Nonce: randomToken(), Verifier: oauth2.GenerateVerifier(), Next: safeNext(r.URL.Query().Get("next"))}
	if err := h.jar.set(w, loginCookie, f, flowTTL); err != nil {
		h.fail(w, "set login cookie", err)
		return
	}
	http.Redirect(w, r, h.cfg.Login.AuthCodeURL(f.State, f.Nonce, f.Verifier), http.StatusFound)
}

func (h *Handler) loginCallback(w http.ResponseWriter, r *http.Request) {
	var f loginFlow
	if !h.jar.get(r, loginCookie, &f) || r.URL.Query().Get("state") != f.State || f.State == "" {
		http.Error(w, "Вход устарел, начните заново", http.StatusBadRequest)
		return
	}
	h.jar.clear(w, loginCookie)
	if r.URL.Query().Get("error") != "" {
		http.Error(w, "Вход отменён", http.StatusUnauthorized)
		return
	}
	user, err := h.cfg.Login.Exchange(r.Context(), r.URL.Query().Get("code"), f.Verifier, f.Nonce)
	if err != nil {
		h.cfg.Log.Warn("login exchange failed", "err", err)
		http.Error(w, "Не удалось выполнить вход", http.StatusUnauthorized)
		return
	}
	if err := h.jar.set(w, sessionCookie, session{User: user, CSRF: randomToken()}, h.cfg.SessionTTL); err != nil {
		h.fail(w, "set session", err)
		return
	}
	h.cfg.Log.Info("web login", "user", user)
	http.Redirect(w, r, f.Next, http.StatusFound)
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request, _ session) {
	h.jar.clear(w, sessionCookie)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// safeNext allows only same-site absolute paths, preventing open redirects.
func safeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, `/\`) {
		return "/"
	}
	return next
}

// --- connecting products ---

func (h *Handler) provider(r *http.Request) (store.Provider, Connector, bool) {
	p := store.Provider(r.PathValue("provider"))
	c, ok := h.cfg.Connectors[p]
	return p, c, ok && p.Valid()
}

func (h *Handler) connect(w http.ResponseWriter, r *http.Request) {
	s, ok := h.currentSession(r)
	if !ok {
		h.redirectToLogin(w, r)
		return
	}
	p, c, ok := h.provider(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	f := connectFlow{State: randomToken(), Verifier: oauth2.GenerateVerifier(), User: s.User, Provider: p}
	if err := h.jar.set(w, connectCookie, f, flowTTL); err != nil {
		h.fail(w, "set connect cookie", err)
		return
	}
	http.Redirect(w, r, c.AuthCodeURL(f.State, f.Verifier), http.StatusFound)
}

func (h *Handler) connectCallback(w http.ResponseWriter, r *http.Request) {
	s, ok := h.currentSession(r)
	if !ok {
		http.Error(w, "Сессия истекла, откройте страницу доступа заново", http.StatusUnauthorized)
		return
	}
	p, c, ok := h.provider(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	var f connectFlow
	q := r.URL.Query()
	if !h.jar.get(r, connectCookie, &f) || f.State == "" || q.Get("state") != f.State || f.Provider != p || f.User != s.User {
		http.Error(w, "Подключение устарело, начните заново", http.StatusBadRequest)
		return
	}
	h.jar.clear(w, connectCookie)
	if q.Get("error") != "" {
		h.back(w, r, "denied")
		return
	}

	tok, err := c.Exchange(r.Context(), q.Get("code"), f.Verifier)
	if err != nil {
		h.cfg.Log.Warn("oauth exchange failed", "user", s.User, "provider", p, "err", err)
		h.back(w, r, "failed")
		return
	}
	// The grant must belong to the same person who is logged in here,
	// otherwise a shared browser could attach someone else's access to this user.
	who, err := c.WhoAmI(r.Context(), atlassian.Bearer(tok.AccessToken))
	if err != nil {
		h.cfg.Log.Warn("whoami failed", "user", s.User, "provider", p, "err", err)
		h.back(w, r, "failed")
		return
	}
	if identity.NormalizeUsername(who, "") != s.User {
		h.cfg.Log.Warn("oauth identity mismatch", "user", s.User, "provider", p, "atlassian_user", who)
		h.back(w, r, "mismatch")
		return
	}
	if err := h.cfg.Grants.SaveGrant(r.Context(), s.User, p, tok); err != nil {
		h.fail(w, "save grant", err)
		return
	}
	h.cfg.Log.Info("provider connected", "user", s.User, "provider", p)
	h.back(w, r, "connected")
}

func (h *Handler) disconnect(w http.ResponseWriter, r *http.Request, s session) {
	p, _, ok := h.provider(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := h.cfg.Tokens.DeleteToken(r.Context(), s.User, p); err != nil {
		h.fail(w, "delete token", err)
		return
	}
	h.cfg.Log.Info("provider disconnected", "user", s.User, "provider", p)
	h.back(w, r, "disconnected")
}

// --- personal keys ---

func (h *Handler) createKey(w http.ResponseWriter, r *http.Request, s session) {
	name := strings.TrimSpace(r.PostFormValue("name"))
	if name == "" || utf8.RuneCountInString(name) > 64 {
		h.back(w, r, "badname")
		return
	}
	ttl := h.cfg.APIKeyDefault
	if days, err := strconv.Atoi(r.PostFormValue("days")); err == nil && days > 0 {
		ttl = time.Duration(days) * 24 * time.Hour
	}
	if ttl > h.cfg.APIKeyMax {
		ttl = h.cfg.APIKeyMax
	}
	full, id, secret, err := identity.NewAPIKey()
	if err != nil {
		h.fail(w, "generate key", err)
		return
	}
	now := h.cfg.Now()
	k := &store.APIKey{ID: id, Username: s.User, Name: name, SecretHash: identity.HashSecret(secret), CreatedAt: now, ExpiresAt: now.Add(ttl)}
	if err := h.cfg.Keys.CreateAPIKey(r.Context(), k); err != nil {
		h.fail(w, "create key", err)
		return
	}
	h.cfg.Log.Info("api key created", "user", s.User, "key_id", id, "expires", k.ExpiresAt)
	// Rendered directly (not redirected) so the secret is never stored anywhere.
	h.render(w, r, s, "", full)
}

func (h *Handler) revokeKey(w http.ResponseWriter, r *http.Request, s session) {
	id := r.PathValue("id")
	err := h.cfg.Keys.RevokeAPIKey(r.Context(), s.User, id, h.cfg.Now())
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.fail(w, "revoke key", err)
		return
	}
	h.cfg.Log.Info("api key revoked", "user", s.User, "key_id", id)
	h.back(w, r, "revoked")
}

// --- rendering ---

var messages = map[string]string{
	"connected":    "Доступ подключён.",
	"disconnected": "Доступ отключён: сервис удалил ваши токены и больше не обращается к Confluence/Jira от вашего имени. Чтобы отозвать и само разрешение, удалите приложение в профиле Confluence/Jira (раздел авторизованных приложений).",
	"denied":       "Вы отклонили запрос доступа.",
	"failed":       "Не удалось подключить доступ. Попробуйте ещё раз.",
	"mismatch":     "Учётная запись в Confluence/Jira не совпадает с текущим пользователем. Доступ не сохранён.",
	"revoked":      "Ключ отозван.",
	"badname":      "Укажите название ключа (до 64 символов).",
}

func (h *Handler) back(w http.ResponseWriter, r *http.Request, msg string) {
	http.Redirect(w, r, "/?msg="+msg, http.StatusSeeOther)
}

func (h *Handler) index(w http.ResponseWriter, r *http.Request) {
	s, ok := h.currentSession(r)
	if !ok {
		h.redirectToLogin(w, r)
		return
	}
	h.render(w, r, s, messages[r.URL.Query().Get("msg")], "")
}

type providerView struct {
	ID        store.Provider
	Title     string
	Connected bool
}

type keyView struct {
	*store.APIKey
	Active bool
}

type pageData struct {
	User       string
	CSRF       string
	Message    string
	NewKey     string
	MCPURL     string
	Providers  []providerView
	Keys       []keyView
	DefaultTTL int
	MaxTTL     int
}

func (h *Handler) render(w http.ResponseWriter, r *http.Request, s session, msg, newKey string) {
	ctx := r.Context()
	connected, err := h.cfg.Tokens.ConnectedProviders(ctx, s.User)
	if err != nil {
		h.fail(w, "list providers", err)
		return
	}
	keys, err := h.cfg.Keys.ListAPIKeys(ctx, s.User)
	if err != nil {
		h.fail(w, "list keys", err)
		return
	}
	data := pageData{
		User: s.User, CSRF: s.CSRF, Message: msg, NewKey: newKey, MCPURL: h.cfg.PublicURL + "/mcp",
		DefaultTTL: int(h.cfg.APIKeyDefault.Hours() / 24), MaxTTL: int(h.cfg.APIKeyMax.Hours() / 24),
	}
	for _, p := range []store.Provider{store.Confluence, store.Jira} {
		if _, ok := h.cfg.Connectors[p]; !ok {
			continue
		}
		v := providerView{ID: p, Title: p.Title()}
		for _, c := range connected {
			v.Connected = v.Connected || c == p
		}
		data.Providers = append(data.Providers, v)
	}
	now := h.cfg.Now()
	for _, k := range keys {
		data.Keys = append(data.Keys, keyView{APIKey: k, Active: k.Active(now)})
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pageTmpl.Execute(w, data); err != nil {
		h.cfg.Log.Error("render", "err", err)
	}
}

func (h *Handler) fail(w http.ResponseWriter, op string, err error) {
	h.cfg.Log.Error("web: "+op, "err", err)
	http.Error(w, "Внутренняя ошибка", http.StatusInternalServerError)
}
