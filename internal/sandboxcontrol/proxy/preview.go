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
)

// SandboxEnsurer ensures a sandbox is running and ready before proxying traffic.
type SandboxEnsurer interface {
	EnsureRunningAndReady(ctx context.Context, name string) (*k8s.SandboxInfo, error)
}

// PreviewHandler reverse-proxies /preview/<sandbox>/<port>/... to sandbox-router.
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

// ParsePreviewPath extracts sandbox name, port, and remaining upstream path from /preview/<sandbox>/<port>/...
func ParsePreviewPath(p string) (sandbox string, port int, rest string, err error) {
	trimmed := strings.TrimPrefix(p, "/preview/")
	if trimmed == p || trimmed == "" {
		return "", 0, "", fmt.Errorf("invalid preview path %q", p)
	}
	parts := strings.SplitN(trimmed, "/", 3)
	if len(parts) < 2 {
		return "", 0, "", fmt.Errorf("preview path must be /preview/<sandbox>/<port>/..., got %q", p)
	}
	sandbox = parts[0]
	if !config.IsValidDNS1123Label(sandbox) {
		return "", 0, "", fmt.Errorf("invalid sandbox name %q", sandbox)
	}
	port, err = strconv.Atoi(parts[1])
	if err != nil || port < 1 || port > 65535 {
		return "", 0, "", fmt.Errorf("invalid port %q", parts[1])
	}
	if len(parts) == 3 {
		rest = "/" + parts[2]
	} else {
		rest = "/"
	}
	return sandbox, port, rest, nil
}

func (h *PreviewHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	userLogin := strings.TrimSpace(r.Header.Get(HeaderTailscaleUserLogin))
	if userLogin == "" && !h.allowUnauthenticated {
		http.Error(w, "unauthorized: missing Tailscale-User-Login header", http.StatusUnauthorized)
		return
	}

	sandboxName, port, restPath, err := ParsePreviewPath(r.URL.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	sb, err := h.ensurer.EnsureRunningAndReady(r.Context(), sandboxName)
	if err != nil {
		http.Error(w, fmt.Sprintf("sandbox %q not ready: %v", sandboxName, err), http.StatusBadGateway)
		return
	}

	targetSandbox := sandboxName
	if sb != nil && sb.Name != "" {
		targetSandbox = sb.Name
	}

	rp := &httputil.ReverseProxy{
		Transport: h.transport,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(h.routerURL)
			pr.Out.URL.Path = restPath
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
