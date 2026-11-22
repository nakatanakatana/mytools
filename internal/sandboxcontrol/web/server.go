package web

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/nakatanakatana/mytools/internal/sandboxcontrol/config"
	"github.com/nakatanakatana/mytools/internal/sandboxcontrol/proxy"
	"github.com/nakatanakatana/mytools/internal/sandboxcontrol/service"
)

//go:embed templates/index.html
var templateFS embed.FS

// ControlService is the subset of service.Service needed by the HTTP layer.
type ControlService interface {
	proxy.SandboxEnsurer
	Config() config.Config
	Ping(ctx context.Context) error
	Overview(ctx context.Context) (*service.DashboardData, error)
	Create(ctx context.Context, req service.CreateRequest) (*service.CreateResult, error)
	Suspend(ctx context.Context, name string) error
	Resume(ctx context.Context, name string) error
	Delete(ctx context.Context, name string) error
	CreateCheckpoint(ctx context.Context, sandboxName, checkpointName string) (string, error)
	DeleteCheckpoint(ctx context.Context, name string) error
	GetLogs(ctx context.Context, sandboxName string, tailLines int64) (string, error)
}

// Server serves the Web UI, health probes, JSON API, and preview reverse proxy.
type Server struct {
	svc     ControlService
	preview http.Handler
	tmpl    *template.Template
	mux     *http.ServeMux
}

type dashboardView struct {
	Data               *service.DashboardData
	UserLogin          string
	EphemeralTemplate  string
	PersistentTemplate string
	OpenCodePort       int
	Message            string
	Error              string
}

// NewServer creates the HTTP handler tree for sandbox-control.
func NewServer(svc ControlService) (*Server, error) {
	cfg := svc.Config()
	previewHandler, err := proxy.NewPreviewHandler(
		cfg.RouterURL,
		cfg.Namespace,
		cfg.AllowUnauthenticatedPreview,
		svc,
	)
	if err != nil {
		return nil, err
	}
	tmpl, err := template.ParseFS(templateFS, "templates/index.html")
	if err != nil {
		return nil, fmt.Errorf("parse dashboard template: %w", err)
	}

	s := &Server{
		svc:     svc,
		preview: previewHandler,
		tmpl:    tmpl,
		mux:     http.NewServeMux(),
	}
	s.routes()
	return s, nil
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.HandleFunc("GET /readyz", s.handleReadyz)
	s.mux.HandleFunc("GET /{$}", s.handleDashboard)
	s.mux.HandleFunc("GET /api/overview", s.handleOverviewJSON)
	s.mux.HandleFunc("POST /sandboxes", s.handleCreateSandbox)
	s.mux.HandleFunc("POST /sandboxes/{name}/suspend", s.handleSuspend)
	s.mux.HandleFunc("POST /sandboxes/{name}/resume", s.handleResume)
	s.mux.HandleFunc("POST /sandboxes/{name}/delete", s.handleDeleteSandbox)
	s.mux.HandleFunc("POST /sandboxes/{name}/checkpoint", s.handleCreateCheckpoint)
	s.mux.HandleFunc("GET /sandboxes/{name}/logs", s.handleLogs)
	s.mux.HandleFunc("POST /checkpoints/{name}/delete", s.handleDeleteCheckpoint)
	s.mux.Handle("/preview/", s.preview)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := s.svc.Ping(ctx); err != nil {
		http.Error(w, fmt.Sprintf("not ready: %v", err), http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ready"}`))
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	data, err := s.svc.Overview(r.Context())
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to load overview: %v", err), http.StatusInternalServerError)
		return
	}
	cfg := s.svc.Config()
	view := dashboardView{
		Data:               data,
		UserLogin:          r.Header.Get(proxy.HeaderTailscaleUserLogin),
		EphemeralTemplate:  cfg.EphemeralTemplate,
		PersistentTemplate: cfg.PersistentTemplate,
		OpenCodePort:       cfg.OpenCodePort,
		Message:            r.URL.Query().Get("msg"),
		Error:              r.URL.Query().Get("err"),
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.Execute(w, view); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) handleOverviewJSON(w http.ResponseWriter, r *http.Request) {
	data, err := s.svc.Overview(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(data)
}

func (s *Server) handleCreateSandbox(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		redirectWithError(w, r, fmt.Sprintf("invalid form: %v", err))
		return
	}
	var ttl time.Duration
	if rawTTL := strings.TrimSpace(r.FormValue("ttl")); rawTTL != "" {
		d, err := time.ParseDuration(rawTTL)
		if err != nil {
			redirectWithError(w, r, fmt.Sprintf("invalid ttl %q: %v", rawTTL, err))
			return
		}
		ttl = d
	}

	res, err := s.svc.Create(r.Context(), service.CreateRequest{
		Name:       r.FormValue("name"),
		Template:   r.FormValue("template"),
		TTL:        ttl,
		Checkpoint: r.FormValue("checkpoint"),
	})
	if err != nil {
		redirectWithError(w, r, err.Error())
		return
	}
	redirectWithMessage(w, r, fmt.Sprintf("Created %s %q (%s)", res.Kind, res.Name, res.Template))
}

func (s *Server) handleSuspend(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.svc.Suspend(r.Context(), name); err != nil {
		redirectWithError(w, r, err.Error())
		return
	}
	redirectWithMessage(w, r, fmt.Sprintf("Suspended sandbox %q", name))
}

func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.svc.Resume(r.Context(), name); err != nil {
		redirectWithError(w, r, err.Error())
		return
	}
	redirectWithMessage(w, r, fmt.Sprintf("Resuming sandbox %q", name))
}

func (s *Server) handleDeleteSandbox(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.svc.Delete(r.Context(), name); err != nil {
		redirectWithError(w, r, err.Error())
		return
	}
	redirectWithMessage(w, r, fmt.Sprintf("Deleted %q", name))
}

func (s *Server) handleCreateCheckpoint(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	_ = r.ParseForm()
	cpName, err := s.svc.CreateCheckpoint(r.Context(), name, r.FormValue("checkpoint"))
	if err != nil {
		redirectWithError(w, r, err.Error())
		return
	}
	redirectWithMessage(w, r, fmt.Sprintf("Created checkpoint %q from %q", cpName, name))
}

func (s *Server) handleDeleteCheckpoint(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.svc.DeleteCheckpoint(r.Context(), name); err != nil {
		redirectWithError(w, r, err.Error())
		return
	}
	redirectWithMessage(w, r, fmt.Sprintf("Deleted checkpoint %q", name))
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	tail := int64(200)
	if rawTail := r.URL.Query().Get("tail"); rawTail != "" {
		if n, err := strconv.ParseInt(rawTail, 10, 64); err == nil && n > 0 {
			tail = n
		}
	}
	logs, err := s.svc.GetLogs(r.Context(), name, tail)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to fetch logs: %v", err), http.StatusBadGateway)
		return
	}
	if r.URL.Query().Get("format") == "text" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(logs))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = fmt.Fprintf(w, `<!DOCTYPE html><html><head><meta charset="utf-8"><title>Logs: %s</title><style>body{margin:0;padding:1rem;background:#0f172a;color:#e2e8f0;font-family:ui-monospace,monospace}pre{white-space:pre-wrap;word-break:break-word}a{color:#38bdf8}</style></head><body><p><a href="/">Back</a> · <a href="/sandboxes/%s/logs?tail=%d">Refresh</a> · <a href="/sandboxes/%s/logs?tail=%d&format=text">Raw</a></p><pre>%s</pre></body></html>`,
		template.HTMLEscapeString(name),
		url.PathEscape(name), tail,
		url.PathEscape(name), tail,
		template.HTMLEscapeString(logs),
	)
}

func redirectWithMessage(w http.ResponseWriter, r *http.Request, msg string) {
	http.Redirect(w, r, "/?msg="+url.QueryEscape(msg), http.StatusSeeOther)
}

func redirectWithError(w http.ResponseWriter, r *http.Request, msg string) {
	http.Redirect(w, r, "/?err="+url.QueryEscape(msg), http.StatusSeeOther)
}
