// Command atlassian-mcp is an MCP server giving LLM clients and agents access to
// Confluence and Jira Data Center under each caller's own permissions.
//
//	atlassian-mcp serve      run the server (default)
//	atlassian-mcp migrate    apply database migrations and exit
//	atlassian-mcp agent ...  manage autonomous agents (see "agent -h")
//	atlassian-mcp user ...   withdraw a person's access (see "user -h")
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"atlassian-mcp/internal/atlassian"
	"atlassian-mcp/internal/atlassian/confluence"
	"atlassian-mcp/internal/atlassian/jira"
	"atlassian-mcp/internal/audit"
	"atlassian-mcp/internal/config"
	"atlassian-mcp/internal/crypto"
	"atlassian-mcp/internal/identity"
	"atlassian-mcp/internal/metrics"
	"atlassian-mcp/internal/offboard"
	"atlassian-mcp/internal/ratelimit"
	"atlassian-mcp/internal/server"
	"atlassian-mcp/internal/store"
	"atlassian-mcp/internal/store/postgres"
	"atlassian-mcp/internal/tools"
	"atlassian-mcp/internal/web"
)

// version is set at build time: -ldflags "-X main.version=..."
var version = "dev"

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel()}))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmd := "serve"
	args := os.Args[1:]
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve(ctx, log)
	case "migrate":
		err = migrate(ctx)
	case "agent":
		err = agentCmd(ctx, args, os.Stdin, os.Stdout)
	case "user":
		err = userCmd(ctx, args, os.Stdout)
	case "version":
		fmt.Println(version)
	default:
		err = fmt.Errorf("unknown command %q (serve, migrate, agent, user, version)", cmd)
	}
	if err != nil {
		log.Error("fatal", "cmd", cmd, "err", err)
		os.Exit(1)
	}
}

func logLevel() slog.Level {
	var l slog.Level
	if err := l.UnmarshalText([]byte(os.Getenv("LOG_LEVEL"))); err != nil {
		return slog.LevelInfo
	}
	return l
}

func openStore(ctx context.Context, cfg *config.Config) (*postgres.Store, error) {
	sealer, err := crypto.NewSealer(cfg.EncryptionKey)
	if err != nil {
		return nil, err
	}
	return postgres.Open(ctx, cfg.DatabaseURL, sealer)
}

func migrate(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	st, err := openStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer st.Close()
	return st.Migrate(ctx)
}

// httpClient builds the outbound client, optionally trusting a corporate CA.
func httpClient(cfg *config.Config) (*http.Client, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if cfg.CAFile != "" {
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read CA_FILE: %w", err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("CA_FILE contains no certificates")
		}
		tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return &http.Client{Transport: tr, Timeout: cfg.HTTPTimeout}, nil
}

func serve(ctx context.Context, log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	hc, err := httpClient(cfg)
	if err != nil {
		return err
	}
	st, err := openStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		return err
	}

	jwt, err := identity.NewKeycloakVerifier(ctx, cfg.KeycloakIssuer, cfg.UsernameClaim, hc)
	if err != nil {
		return err
	}
	login, err := web.NewKeycloakLogin(ctx, cfg.KeycloakIssuer, cfg.KeycloakWebClientID, cfg.KeycloakWebClientSecret,
		cfg.PublicURL+"/login/callback", cfg.UsernameClaim, cfg.UsernameStripDomain, hc)
	if err != nil {
		return err
	}

	oauth := map[store.Provider]*atlassian.OAuthProvider{}
	connectors := map[store.Provider]web.Connector{}
	svc := &tools.Service{PublicURL: cfg.PublicURL}
	if p := cfg.Confluence; p.Enabled() {
		c, err := confluence.NewClient(p.BaseURL, hc, cfg.MaxSearchResults, cfg.MaxContentChars)
		if err != nil {
			return err
		}
		svc.Confluence = c
		oauth[store.Confluence] = atlassian.NewOAuthProvider(store.Confluence, p.BaseURL, p.ClientID, p.ClientSecret,
			cfg.PublicURL+"/oauth/confluence/callback", p.Scopes, hc, c)
		connectors[store.Confluence] = oauth[store.Confluence]
	}
	if p := cfg.Jira; p.Enabled() {
		c := jira.NewClient(p.BaseURL, hc, cfg.MaxSearchResults, cfg.MaxContentChars)
		svc.Jira = c
		oauth[store.Jira] = atlassian.NewOAuthProvider(store.Jira, p.BaseURL, p.ClientID, p.ClientSecret,
			cfg.PublicURL+"/oauth/jira/callback", p.Scopes, hc, c)
		connectors[store.Jira] = oauth[store.Jira]
	}

	creds := &atlassian.Credentials{Tokens: st, Agents: st, OAuth: oauth, Log: log}
	reg := metrics.NewRegistry()
	svc.Creds = creds
	svc.Audit = &audit.Recorder{Store: st, Log: log}
	svc.Limiter = ratelimit.New(cfg.RateLimitPerMin)
	svc.Metrics = tools.NewMetrics(reg)
	reg.NewGaugeFunc("amcp_connected_users", "Users holding a Confluence/Jira grant.", "provider",
		func() (map[string]float64, error) {
			cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			n, err := st.CountConnected(cctx)
			out := map[string]float64{}
			for p, c := range n {
				out[string(p)] = float64(c)
			}
			return out, err
		},
		func(err error) { log.Warn("connected users metric", "err", err) })
	if cfg.AuditRetention > 0 {
		go (&audit.Retention{Store: st, Keep: cfg.AuditRetention, Log: log}).Run(ctx, 24*time.Hour)
	}

	resolver := &identity.Resolver{
		JWT: jwt, Keys: st, Agents: st,
		UserClients: cfg.KeycloakUserClients, StripDomain: cfg.UsernameStripDomain, Log: log,
	}
	webHandler, err := web.New(web.Config{
		PublicURL: cfg.PublicURL, SessionKey: cfg.SessionKey, SessionTTL: cfg.SessionTTL,
		APIKeyDefault: cfg.APIKeyDefaultTTL, APIKeyMax: cfg.APIKeyMaxTTL,
		Login: login, Connectors: connectors, Grants: creds, Tokens: st, Keys: st, Log: log,
	})
	if err != nil {
		return err
	}

	app := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           server.Router(server.MCPHandler(svc, resolver, version, log), webHandler, st, log),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      2*cfg.HTTPTimeout + 10*time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	metricsSrv := &http.Server{Addr: cfg.MetricsAddr, Handler: reg, ReadHeaderTimeout: 5 * time.Second}

	errc := make(chan error, 2)
	go func() { errc <- listen(app) }()
	go func() { errc <- listen(metricsSrv) }()
	log.Info("started", "version", version, "listen", cfg.ListenAddr, "metrics", cfg.MetricsAddr,
		"confluence", cfg.Confluence.Enabled(), "jira", cfg.Jira.Enabled())

	select {
	case <-ctx.Done():
	case err := <-errc:
		return err
	}
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return errors.Join(app.Shutdown(shutdownCtx), metricsSrv.Shutdown(shutdownCtx))
}

func listen(s *http.Server) error {
	if err := s.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// --- user offboarding CLI ---

const userUsage = `usage:
  atlassian-mcp user offboard -username USER [-operator NAME]
      revokes all agent keys of USER and deletes their Confluence/Jira grants`

func userCmd(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New(userUsage)
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	st, err := openStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		return err
	}
	return runUserCmd(ctx, st, args, stdout, time.Now())
}

func runUserCmd(ctx context.Context, st offboard.Store, args []string, stdout io.Writer, now time.Time) error {
	sub, rest := args[0], args[1:]
	if sub != "offboard" {
		return fmt.Errorf("unknown user command %q\n%s", sub, userUsage)
	}
	fs := flag.NewFlagSet("user offboard", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	username := fs.String("username", "", "login of the person whose access is withdrawn")
	operator := fs.String("operator", os.Getenv("USER"), "who runs the command (written to the audit log)")
	if err := fs.Parse(rest); err != nil {
		return fmt.Errorf("%v\n%s", err, userUsage)
	}
	if *username == "" {
		return errors.New("-username is required\n" + userUsage)
	}
	if *operator == "" {
		*operator = "cli"
	}
	rep, err := offboard.User(ctx, st, *username, *operator, now)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "user %s: revoked %d key(s), disconnected %v\n", rep.Username, len(rep.RevokedKeys), rep.Disconnected)
	if len(rep.OwnedAgents) > 0 {
		fmt.Fprintf(stdout, "owns agents %s: reassign the owner or disable them (atlassian-mcp agent disable -client-id ...)\n",
			strings.Join(rep.OwnedAgents, ", "))
	}
	return nil
}

// --- agent registry CLI ---

const agentUsage = `usage:
  atlassian-mcp agent add -client-id ID -name NAME -owner USER
  atlassian-mcp agent set-token -client-id ID -provider confluence|jira   (reads the PAT from stdin)
  atlassian-mcp agent remove-token -client-id ID -provider confluence|jira
  atlassian-mcp agent enable|disable -client-id ID
  atlassian-mcp agent list`

func agentCmd(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New(agentUsage)
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	st, err := openStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		return err
	}
	return runAgentCmd(ctx, st, args, stdin, stdout)
}

func runAgentCmd(ctx context.Context, st store.Agents, args []string, stdin io.Reader, stdout io.Writer) error {
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("agent "+sub, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	clientID := fs.String("client-id", "", "Keycloak client ID of the agent")
	name := fs.String("name", "", "human-readable name")
	owner := fs.String("owner", "", "username of the responsible person")
	provider := fs.String("provider", "", "confluence or jira")
	if err := fs.Parse(rest); err != nil {
		return fmt.Errorf("%v\n%s", err, agentUsage)
	}

	if sub == "list" {
		agents, err := st.ListAgents(ctx)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "CLIENT_ID\tNAME\tOWNER\tENABLED\tPRODUCTS")
		for _, a := range agents {
			var ps []string
			for _, p := range []store.Provider{store.Confluence, store.Jira} {
				if a.Credentials[p] != "" {
					ps = append(ps, string(p))
				}
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%v\t%s\n", a.ClientID, a.Name, a.Owner, a.Enabled, strings.Join(ps, ","))
		}
		return tw.Flush()
	}

	if *clientID == "" {
		return errors.New("-client-id is required\n" + agentUsage)
	}
	if sub == "add" {
		if *name == "" || *owner == "" {
			return errors.New("-name and -owner are required")
		}
		if _, err := st.GetAgent(ctx, *clientID); err == nil {
			return fmt.Errorf("agent %s already exists", *clientID)
		}
		return st.PutAgent(ctx, &store.Agent{ClientID: *clientID, Name: *name, Owner: *owner, Enabled: true,
			Credentials: map[store.Provider]string{}})
	}

	a, err := st.GetAgent(ctx, *clientID)
	if err != nil {
		return fmt.Errorf("agent %s: %w", *clientID, err)
	}
	switch sub {
	case "enable", "disable":
		a.Enabled = sub == "enable"
	case "set-token", "remove-token":
		p := store.Provider(*provider)
		if !p.Valid() {
			return errors.New("-provider must be confluence or jira")
		}
		if a.Credentials == nil {
			a.Credentials = map[store.Provider]string{}
		}
		if sub == "remove-token" {
			delete(a.Credentials, p)
			break
		}
		raw, err := io.ReadAll(io.LimitReader(stdin, 4096))
		if err != nil {
			return err
		}
		pat := strings.TrimSpace(string(raw))
		if pat == "" {
			return errors.New("empty token on stdin")
		}
		a.Credentials[p] = pat
	default:
		return fmt.Errorf("unknown agent command %q\n%s", sub, agentUsage)
	}
	return st.PutAgent(ctx, a)
}
