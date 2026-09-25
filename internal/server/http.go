// Package server assembles the single HTTP listener: health endpoints, the
// Prometheus exposition, the REST API, and the MCP streamable-HTTP endpoint
// (the latter two optionally behind OAuth).
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	mcpserver "github.com/mark3labs/mcp-go/server"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/giantswarm/vm-manager/internal/api"
)

// Config configures the listener.
type Config struct {
	Addr    string
	MCPPath string
	// OAuth, when set, makes the server an OAuth 2.1 resource server: the MCP
	// endpoint and the REST API require a bearer token the platform IdP
	// issued (forwarded by muster / sent by the portal) or this server's own,
	// and every call carries the caller's identity. Off: anonymous — only for
	// a listener nothing but a trusted proxy or the local user can reach.
	OAuth *OAuthConfig
	// Metrics, when set, is served at GET /metrics outside the OAuth guard
	// like the probes: Prometheus scrapers do not do OAuth flows, and the
	// exposition holds no secrets (VM ids, names, states, counters).
	// Operators who need it private firewall the path or disable it with
	// --metrics-enabled=false.
	Metrics http.Handler
}

// Server is the assembled HTTP server.
type Server struct {
	http  *http.Server
	oauth *oauthRuntime
	log   *slog.Logger
}

// New builds the server.
func New(cfg Config, svc api.Services, mcpSrv *mcpserver.MCPServer, log *slog.Logger) (*Server, error) {
	if log == nil {
		log = slog.Default()
	}
	if cfg.MCPPath == "" {
		cfg.MCPPath = "/mcp"
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", ok)
	// Readiness is the listener itself: host prerequisites are reported by
	// GET /api/v1/host, so clients can render what is missing instead of
	// getting connection errors from an unready server.
	mux.HandleFunc("GET /readyz", ok)
	if cfg.Metrics != nil {
		mux.Handle("GET /metrics", cfg.Metrics)
	}

	s := &Server{log: log}
	if cfg.OAuth != nil {
		o, err := newOAuth(*cfg.OAuth, cfg.MCPPath, log)
		if err != nil {
			return nil, err
		}
		o.register(mux)
		s.oauth = o
	}

	// The REST API on its own mux so one guard covers every route; the
	// health endpoints above stay open for the probes.
	rest := http.NewServeMux()
	api.NewREST(svc, log).Register(rest)
	mux.Handle(api.Prefix+"/", s.guard(rest))

	mux.Handle(cfg.MCPPath, s.guard(mcpserver.NewStreamableHTTPServer(mcpSrv,
		mcpserver.WithEndpointPath(cfg.MCPPath),
	)))

	s.http = &http.Server{
		Addr:              cfg.Addr,
		Handler:           otelhttp.NewHandler(mux, "vm-manager", otelhttp.WithFilter(traced)),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: MCP streams and long VM operations outlive any
		// fixed value.
		IdleTimeout: 120 * time.Second,
	}
	return s, nil
}

// traced leaves the probes and the Prometheus exposition out of the traces:
// they are called every few seconds and carry no caller.
func traced(r *http.Request) bool {
	switch r.URL.Path {
	case "/healthz", "/readyz", "/metrics":
		return false
	}
	return true
}

func ok(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// guard requires an authenticated caller when OAuth is on.
func (s *Server) guard(next http.Handler) http.Handler {
	if s.oauth == nil {
		return next
	}
	return s.oauth.protect(next)
}

// Handler exposes the mux (tests).
func (s *Server) Handler() http.Handler { return s.http.Handler }

// Run serves until ctx is done, then shuts down gracefully.
func (s *Server) Run(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() {
		s.log.Info("listening", "addr", s.http.Addr)
		if err := s.http.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()
	select {
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("http server: %w", err)
		}
		return nil
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if s.oauth != nil {
		s.oauth.shutdown(shutdownCtx)
	}
	if err := s.http.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	s.log.Info("server stopped")
	return nil
}
