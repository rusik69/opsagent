package web

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/server"
	"github.com/rusik69/opsagent/internal/agent"
	"github.com/rusik69/opsagent/internal/config"
	"github.com/rusik69/opsagent/internal/correlate"
	"github.com/rusik69/opsagent/internal/incidents"
	"github.com/rusik69/opsagent/internal/mcp"
	"github.com/rusik69/opsagent/internal/repos"
	"github.com/rusik69/opsagent/internal/review"
	"github.com/rusik69/opsagent/internal/sshx"
	"github.com/rusik69/opsagent/internal/store"
)

//go:embed templates/*.html static/*
var assets embed.FS

// Server is the HTTP server for both the web UI and the JSON API.
type Server struct {
	cfg       *config.Config
	store     *store.Store
	executor  *sshx.Executor
	resolver  sshx.HostResolver
	repos     *repos.Manager
	mcpSrv    *mcp.Server
	agent     *agent.Agent
	pool      *agent.Pool
	incidents *incidents.Service
	correlate *correlate.Engine
	reviewer  *review.Reviewer
	tmpl      *template.Template
	mux       *http.ServeMux
	// diagSem bounds the number of simultaneous diagnoses server-wide.
	diagSem chan struct{}
	// httpSrv is the running HTTP server (for graceful shutdown).
	httpSrv *http.Server
}

// SetReviewer registers the self-improvement reviewer.
func (s *Server) SetReviewer(rv *review.Reviewer) { s.reviewer = rv }

func New(cfg *config.Config, st *store.Store, ex *sshx.Executor, resolver sshx.HostResolver, reposMgr *repos.Manager, mcpSrv *mcp.Server, ag *agent.Agent, inc *incidents.Service, corr *correlate.Engine) (*Server, error) {
	maxConcurrent := cfg.Agent.MaxConcurrent
	if maxConcurrent <= 0 {
		maxConcurrent = 4
	}
	tmpl, err := template.New("").Funcs(template.FuncMap{
		"split":         strings.Fields,
		"sliceSolution": sliceSolution,
	}).ParseFS(assets, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	s := &Server{
		cfg:       cfg,
		store:     st,
		executor:  ex,
		resolver:  resolver,
		repos:     reposMgr,
		mcpSrv:    mcpSrv,
		agent:     ag,
		pool:      agent.NewPool(),
		incidents: inc,
		correlate: corr,
		tmpl:      tmpl,
		mux:       http.NewServeMux(),
		diagSem:   make(chan struct{}, maxConcurrent),
	}
	s.routes()
	return s, nil
}

func (s *Server) routes() {
	// MCP streamable HTTP transport (optional via config).
	if s.cfg.MCP.ExposeHTTP {
		mcpHTTP := server.NewStreamableHTTPServer(s.mcpSrv.MCPServer())
		s.mux.Handle("GET /mcp", mcpHTTP)
		s.mux.Handle("POST /mcp", mcpHTTP)
		s.mux.Handle("DELETE /mcp", mcpHTTP)
		s.mux.Handle("OPTIONS /mcp", mcpHTTP)
	}

	// Static assets.
	s.mux.Handle("GET /static/", http.FileServer(http.FS(assets)))

	// Health.
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	// Pages.
	s.mux.HandleFunc("GET /", s.handleDashboard)
	s.mux.HandleFunc("GET /incidents", s.handleIncidentsPage)
	s.mux.HandleFunc("GET /incidents/{id}", s.handleIncidentPage)
	s.mux.HandleFunc("GET /hosts", s.handleHostsPage)
	s.mux.HandleFunc("GET /repos", s.handleReposPage)
	s.mux.HandleFunc("GET /memory", s.handleMemoryPage)
	s.mux.HandleFunc("GET /groups", s.handleGroupsPage)
	s.mux.HandleFunc("GET /history", s.handleHistoryPage)

	// Partial (HTMX) endpoints.
	s.mux.HandleFunc("GET /partials/incident/{id}", s.handleIncidentPartial)
	s.mux.HandleFunc("GET /partials/diagnosis/{id}", s.handleDiagnosisPartial)
	s.mux.HandleFunc("GET /partials/memory", s.handleMemoryPartial)
	s.mux.HandleFunc("GET /partials/instructions", s.handleInstructionsPartial)

	// API.
	s.mux.HandleFunc("POST /api/v1/incidents", s.handleAPICreateIncident)
	s.mux.HandleFunc("POST /api/v1/incidents/alertmanager", s.handleAPIAlertmanager)
	s.mux.HandleFunc("GET /api/v1/incidents", s.handleAPIListIncidents)
	s.mux.HandleFunc("GET /api/v1/incidents/{id}", s.handleAPIGetIncident)
	s.mux.HandleFunc("POST /api/v1/incidents/{id}/diagnose", s.handleAPIDiagnose)
	s.mux.HandleFunc("POST /api/v1/incidents/{id}/cancel", s.handleAPICancelDiagnosis)
	s.mux.HandleFunc("POST /api/v1/incidents/{id}/status", s.handleAPIUpdateStatus)
	s.mux.HandleFunc("GET /api/v1/incidents/{id}/diagnosis", s.handleAPIGetDiagnosis)
	s.mux.HandleFunc("GET /api/v1/incidents/{id}/related", s.handleAPIRelated)
	s.mux.HandleFunc("GET /api/v1/incidents/{id}/events", s.handleAPIIncidentEvents)

	s.mux.HandleFunc("GET /api/v1/groups", s.handleAPIGroups)
	s.mux.HandleFunc("GET /api/v1/groups/{id}", s.handleAPIGroupDetail)
	s.mux.HandleFunc("POST /api/v1/correlate/run", s.handleAPIRunCorrelation)

	s.mux.HandleFunc("GET /api/v1/retrospectives", s.handleAPIRetrospectives)
	s.mux.HandleFunc("POST /api/v1/review/run", s.handleAPIRunReview)

	s.mux.HandleFunc("GET /api/v1/hosts", s.handleAPIHosts)
	s.mux.HandleFunc("GET /api/v1/hosts/{host}/commands", s.handleAPIHostCommands)
	s.mux.HandleFunc("POST /api/v1/hosts/{host}/commands/run", s.handleAPIHostRunCommand)

	s.mux.HandleFunc("GET /api/v1/repos", s.handleAPIRepos)
	s.mux.HandleFunc("POST /api/v1/repos/sync", s.handleAPIReposSync)
	s.mux.HandleFunc("GET /api/v1/repos/search", s.handleAPIReposSearch)

	s.mux.HandleFunc("GET /api/v1/memory", s.handleAPIMemories)
	s.mux.HandleFunc("POST /api/v1/memory", s.handleAPICreateMemory)
	s.mux.HandleFunc("GET /api/v1/instructions", s.handleAPIInstructions)
	s.mux.HandleFunc("POST /api/v1/instructions", s.handleAPICreateInstruction)
	s.mux.HandleFunc("POST /api/v1/instructions/{id}/apply", s.handleAPIApplyInstruction)
}

// Handler returns the root http.Handler with middleware applied.
func (s *Server) Handler() http.Handler {
	return s.apiKeyMiddleware(s.logMiddleware(s.mux))
}

func (s *Server) logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/static/") {
			next.ServeHTTP(w, r)
			return
		}
		log.Printf("%s %s", r.Method, r.URL.Path)
		next.ServeHTTP(w, r)
	})
}

// apiKeyMiddleware enforces the configured API keys. When no key is set at
// all, access is unrestricted. Three keys are supported, in decreasing scope:
//   - api_key: full access to every endpoint.
//   - api_key_readonly: GET/HEAD/OPTIONS requests only.
//   - api_key_webhook: incident intake only (POST /api/v1/incidents and
//     /api/v1/incidents/alertmanager), for alerting systems.
//
// /healthz is always exempt so liveness probes work without credentials.
func (s *Server) apiKeyMiddleware(next http.Handler) http.Handler {
	cfg := s.cfg.Server
	if cfg.APIKey == "" && cfg.APIKeyReadOnly == "" && cfg.APIKeyWebhook == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		key := apiKey(r)
		if key == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch {
		case cfg.APIKey != "" && key == cfg.APIKey:
			next.ServeHTTP(w, r)
			return
		case cfg.APIKeyReadOnly != "" && key == cfg.APIKeyReadOnly && isReadOnly(r):
			next.ServeHTTP(w, r)
			return
		case cfg.APIKeyWebhook != "" && key == cfg.APIKeyWebhook && isWebhook(r):
			next.ServeHTTP(w, r)
			return
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
}

func apiKey(r *http.Request) string {
	if k := r.Header.Get("X-API-Key"); k != "" {
		return k
	}
	if c, err := r.Cookie("api_key"); err == nil {
		return c.Value
	}
	return ""
}

func isReadOnly(r *http.Request) bool {
	return r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions
}

func isWebhook(r *http.Request) bool {
	if r.Method != http.MethodPost {
		return false
	}
	switch r.URL.Path {
	case "/api/v1/incidents", "/api/v1/incidents/alertmanager":
		return true
	}
	return false
}

// ListenAndServe serves the API and UI with sensible connection timeouts.
// ReadTimeout/WriteTimeout are intentionally left unset so the MCP streamable
// HTTP transport (SSE) can hold long-lived connections.
func (s *Server) ListenAndServe() error {
	srv := &http.Server{
		Addr:              s.cfg.Server.Listen,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	s.httpSrv = srv
	log.Printf("opsagent listening on %s", s.cfg.Server.Listen)
	return srv.ListenAndServe()
}

// Shutdown gracefully stops the HTTP server.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.httpSrv == nil {
		return nil
	}
	return s.httpSrv.Shutdown(ctx)
}

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("render %s: %v", name, err)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func sliceSolution(s string) string {
	if len(s) > 80 {
		return s[:80] + "…"
	}
	return s
}

func readJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	return dec.Decode(v)
}
