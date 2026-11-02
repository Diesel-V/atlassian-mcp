// Package server assembles the HTTP surface: MCP endpoint, web UI, health checks.
package server

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"atlassian-mcp/internal/identity"
	"atlassian-mcp/internal/tools"
)

// Instructions are sent to MCP clients and guide the model.
const Instructions = `Access to the company's Confluence and Jira Data Center.
All calls run with the permissions of the person (or agent) using this connection:
results never include anything they could not open themselves.
Search first, then read pages or issues you need. Always cite the returned links.
When searching for a person, use their name exactly as written; do not add or guess patronymics.
If a tool says access is not connected, show the user the link from the message and ask them to retry afterwards.`

// MCPHandler serves the MCP Streamable HTTP endpoint behind bearer authentication.
func MCPHandler(svc *tools.Service, resolver *identity.Resolver, version string, log *slog.Logger) http.Handler {
	srv := mcp.NewServer(&mcp.Implementation{Name: "atlassian-mcp", Version: version},
		&mcp.ServerOptions{Instructions: Instructions})
	svc.Register(srv)
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, Logger: log})
	return auth.RequireBearerToken(resolver.TokenVerifier(),
		&auth.RequireBearerTokenOptions{Scopes: []string{identity.ScopeAccess}})(h)
}

// Pinger reports backend readiness.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Router combines the endpoints.
func Router(mcpHandler, web http.Handler, db Pinger, log *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/mcp", http.MaxBytesHandler(mcpHandler, 1<<20))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := db.Ping(ctx); err != nil {
			log.Warn("readiness check failed", "err", err)
			http.Error(w, "database unavailable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.Handle("/", http.MaxBytesHandler(web, 64<<10))
	return accessLog(mux, log)
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// accessLog logs method, path and status. Query strings are omitted:
// OAuth callbacks carry authorization codes.
func accessLog(next http.Handler, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			return
		}
		log.Debug("http", "method", r.Method, "path", r.URL.Path, "status", rec.status, "duration_ms", time.Since(start).Milliseconds())
	})
}
