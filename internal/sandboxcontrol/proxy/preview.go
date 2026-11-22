package proxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"

	"github.com/nakatanakatana/mytools/internal/sandboxcontrol/config"
	"github.com/nakatanakatana/mytools/internal/sandboxcontrol/k8s"
)

const (
	HeaderTailscaleUserLogin = "Tailscale-User-Login"
	HeaderSandboxID          = "X-Sandbox-ID"
	HeaderSandboxNamespace   = "X-Sandbox-Namespace"
	HeaderSandboxPort        = "X-Sandbox-Port"
	HeaderSandboxUserEmail   = "X-Sandbox-User-Email"

	PreviewTargetCookieName = "sandbox_preview_target"
	ControlBasePath         = "/_control"

	MinPreviewPort = 3000
	MaxPreviewPort = 9999
)

// SandboxEnsurer ensures a sandbox is running and ready before proxying traffic.
type SandboxEnsurer interface {
	EnsureRunningAndReady(ctx context.Context, name string) (*k8s.SandboxInfo, error)
}

// PreviewHandler reverse-proxies /preview/<sandbox>/<port>/... and Cookie-backed
// root-relative SPA/dev-server paths to sandbox-router.
type PreviewHandler struct {
	routerURL            *url.URL
	namespace            string
	allowUnauthenticated bool
	ensurer              SandboxEnsurer
	transport            http.RoundTripper
}

// NewPreviewHandler constructs a PreviewHandler targeting routerRawURL.
func NewPreviewHandler(routerRawURL, namespace string, allowUnauthenticated bool, ensurer SandboxEnsurer) (*PreviewHandler, error) {
	u, err := url.Parse(routerRawURL)
	if err != nil {
		return nil, fmt.Errorf("parse router url: %w", err)
	}
	return &PreviewHandler{
		routerURL:            u,
		namespace:            namespace,
		allowUnauthenticated: allowUnauthenticated,
		ensurer:              ensurer,
	}, nil
}

// ParsePreviewPath extracts sandbox name, port (3000-9999), and remaining upstream path from /preview/<sandbox>/<port>/...
func ParsePreviewPath(p string) (sandbox string, port int, rest string, err error) {
	trimmed := strings.TrimPrefix(p, "/preview/")
	if trimmed == p || trimmed == "" {
		return "", 0, "", fmt.Errorf("invalid preview path %q", p)
	}
	parts := strings.SplitN(trimmed, "/", 3)
	if len(parts) < 2 {
		return "", 0, "", fmt.Errorf("preview path must be /preview/<sandbox>/<port>/..., got %q", p)
	}
	sandbox, port, err = validateSandboxAndPort(parts[0], parts[1])
	if err != nil {
		return "", 0, "", err
	}
	if len(parts) == 3 {
		rest = "/" + parts[2]
	} else {
		rest = "/"
	}
	return sandbox, port, rest, nil
}

// ParsePreviewTargetCookie parses and validates a "<sandbox>:<port>" cookie value.
func ParsePreviewTargetCookie(val string) (sandbox string, port int, err error) {
	rawSandbox, rawPort, ok := strings.Cut(strings.TrimSpace(val), ":")
	if !ok {
		return "", 0, fmt.Errorf("invalid %s cookie format", PreviewTargetCookieName)
	}
	return validateSandboxAndPort(rawSandbox, rawPort)
}

func validateSandboxAndPort(sandbox, rawPort string) (string, int, error) {
	if !config.IsValidDNS1123Label(sandbox) {
		return "", 0, fmt.Errorf("invalid sandbox name %q", sandbox)
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil || port < MinPreviewPort || port > MaxPreviewPort {
		return "", 0, fmt.Errorf("invalid port %q: must be between %d and %d", rawPort, MinPreviewPort, MaxPreviewPort)
	}
	return sandbox, port, nil
}

func (h *PreviewHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	userLogin, ok := h.authenticate(w, r)
	if !ok {
		return
	}

	sandboxName, port, restPath, err := ParsePreviewPath(r.URL.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	targetSandbox, ok := h.ensureReady(w, r, sandboxName)
	if !ok {
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     PreviewTargetCookieName,
		Value:    fmt.Sprintf("%s:%d", targetSandbox, port),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   isHTTPSRequest(r),
	})

	// WebSocket upgrades and non-GET/HEAD requests proxy directly; browser GET/HEAD navigations
	// redirect to root-relative paths so SPAs (e.g. OpenCode SolidJS Router) see window.location.pathname at "/".
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
		location := restPath
		if r.URL.RawQuery != "" {
			location += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, location, http.StatusFound)
		return
	}

	h.proxyToSandbox(w, r, targetSandbox, port, restPath, userLogin)
}

// ClearPreviewCookie expires the sandbox_preview_target cookie.
func ClearPreviewCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     PreviewTargetCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   isHTTPSRequest(r),
	})
}

// ServeFallback proxies unmatched root-relative SPA/dev-server requests using the
// sandbox_preview_target cookie set by /preview/{sandbox}/{port}/...
func (h *PreviewHandler) ServeFallback(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(PreviewTargetCookieName)
	if err != nil || strings.TrimSpace(cookie.Value) == "" {
		if r.Method == http.MethodGet && r.URL.Path == "/" {
			http.Redirect(w, r, ControlBasePath, http.StatusFound)
			return
		}
		http.NotFound(w, r)
		return
	}

	sandboxName, port, err := ParsePreviewTargetCookie(cookie.Value)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	userLogin, ok := h.authenticate(w, r)
	if !ok {
		return
	}

	targetSandbox, ok := h.ensureReady(w, r, sandboxName)
	if !ok {
		return
	}

	upstreamPath := r.URL.Path
	if upstreamPath == "" {
		upstreamPath = "/"
	}
	h.proxyToSandbox(w, r, targetSandbox, port, upstreamPath, userLogin)
}

func (h *PreviewHandler) authenticate(w http.ResponseWriter, r *http.Request) (string, bool) {
	userLogin := strings.TrimSpace(r.Header.Get(HeaderTailscaleUserLogin))
	if userLogin == "" && !h.allowUnauthenticated {
		http.Error(w, "unauthorized: missing Tailscale-User-Login header", http.StatusUnauthorized)
		return "", false
	}
	return userLogin, true
}

func (h *PreviewHandler) ensureReady(w http.ResponseWriter, r *http.Request, sandboxName string) (string, bool) {
	sb, err := h.ensurer.EnsureRunningAndReady(r.Context(), sandboxName)
	if err != nil {
		http.Error(w, fmt.Sprintf("sandbox %q not ready: %v", sandboxName, err), http.StatusBadGateway)
		return "", false
	}
	if sb != nil && sb.Name != "" {
		return sb.Name, true
	}
	return sandboxName, true
}

func (h *PreviewHandler) proxyToSandbox(w http.ResponseWriter, r *http.Request, targetSandbox string, port int, upstreamPath, userLogin string) {
	rp := &httputil.ReverseProxy{
		Transport:     h.transport,
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(h.routerURL)
			pr.Out.URL.Path = upstreamPath
			pr.Out.URL.RawPath = ""
			pr.Out.URL.RawQuery = pr.In.URL.RawQuery
			pr.Out.Host = h.routerURL.Host

			pr.Out.Header.Set(HeaderSandboxID, targetSandbox)
			pr.Out.Header.Set(HeaderSandboxNamespace, h.namespace)
			pr.Out.Header.Set(HeaderSandboxPort, strconv.Itoa(port))
			if userLogin != "" {
				pr.Out.Header.Set(HeaderSandboxUserEmail, userLogin)
			} else {
				pr.Out.Header.Del(HeaderSandboxUserEmail)
			}
			// Preserve WebSocket upgrade headers explicitly if present.
			if strings.EqualFold(pr.In.Header.Get("Upgrade"), "websocket") {
				pr.Out.Header.Set("Connection", "Upgrade")
				pr.Out.Header.Set("Upgrade", "websocket")
			}
		},
		ErrorHandler: func(rw http.ResponseWriter, _ *http.Request, proxyErr error) {
			http.Error(rw, fmt.Sprintf("preview proxy error: %v", proxyErr), http.StatusBadGateway)
		},
	}

	rp.ServeHTTP(w, r)
}

func isHTTPSRequest(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https")
}
