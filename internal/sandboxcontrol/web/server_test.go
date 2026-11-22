package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/nakatanakatana/mytools/internal/sandboxcontrol/config"
	"github.com/nakatanakatana/mytools/internal/sandboxcontrol/k8s"
	"github.com/nakatanakatana/mytools/internal/sandboxcontrol/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeService struct {
	cfg           config.Config
	pingErr       error
	overview      *service.DashboardData
	createdReq    *service.CreateRequest
	suspendedName string
	resumedName   string
	deletedName   string
	cpSandbox     string
	deletedCP     string
	logs          string
}

func (f *fakeService) Config() config.Config     { return f.cfg }
func (f *fakeService) Ping(context.Context) error { return f.pingErr }
func (f *fakeService) Overview(context.Context) (*service.DashboardData, error) {
	return f.overview, nil
}
func (f *fakeService) Create(_ context.Context, req service.CreateRequest) (*service.CreateResult, error) {
	f.createdReq = &req
	return &service.CreateResult{Name: req.Name, Kind: "SandboxClaim", Template: req.Template}, nil
}
func (f *fakeService) Suspend(_ context.Context, name string) error {
	f.suspendedName = name
	return nil
}
func (f *fakeService) Resume(_ context.Context, name string) error {
	f.resumedName = name
	return nil
}
func (f *fakeService) Delete(_ context.Context, name string) error {
	f.deletedName = name
	return nil
}
func (f *fakeService) CreateCheckpoint(_ context.Context, sb, cp string) (string, error) {
	f.cpSandbox = sb
	if cp == "" {
		return sb + "-cp", nil
	}
	return cp, nil
}
func (f *fakeService) DeleteCheckpoint(_ context.Context, name string) error {
	f.deletedCP = name
	return nil
}
func (f *fakeService) GetLogs(context.Context, string, int64) (string, error) {
	return f.logs, nil
}
func (f *fakeService) EnsureRunningAndReady(_ context.Context, name string) (*k8s.SandboxInfo, error) {
	return &k8s.SandboxInfo{Name: name, Ready: true}, nil
}

func TestServer_HealthDashboardAndActions(t *testing.T) {
	cfg := config.Default()
	cfg.InsecureAllowAnySSHKey = true
	fs := &fakeService{
		cfg: cfg,
		overview: &service.DashboardData{
			Namespace: "sandbox",
			WarmPools: []k8s.WarmPoolInfo{{Name: "opencode-ephemeral-pool", TemplateName: "opencode-ephemeral", DesiredReplicas: 1, ReadyReplicas: 1}},
			Sandboxes: []k8s.SandboxInfo{{Name: "dev-1", Phase: "Running", Ready: true, Persistent: true, TemplateName: "opencode-persistent"}},
			Checkpoints: []k8s.CheckpointInfo{{Name: "cp-1", SourcePVC: "workspace-dev-1", SandboxName: "dev-1", ReadyToUse: true}},
		},
		logs: "line1\nline2\n",
	}

	srv, err := NewServer(fs)
	require.NoError(t, err)

	// /healthz
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	assert.Equal(t, http.StatusOK, rec.Code)

	// /readyz ok and failure
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	assert.Equal(t, http.StatusOK, rec.Code)

	fs.pingErr = errors.New("apiserver down")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	fs.pingErr = nil

	// Dashboard HTML
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "opencode-ephemeral-pool")
	assert.Contains(t, rec.Body.String(), "dev-1")
	assert.Contains(t, rec.Body.String(), "/preview/dev-1/4096/")
	assert.Contains(t, rec.Body.String(), "cp-1")

	// Create form POST
	form := url.Values{
		"name":     {"my-sb"},
		"template": {"opencode-ephemeral"},
		"ttl":      {"2h"},
	}
	req := httptest.NewRequest(http.MethodPost, "/sandboxes", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusSeeOther, rec.Code)
	require.NotNil(t, fs.createdReq)
	assert.Equal(t, "my-sb", fs.createdReq.Name)

	// Suspend / Resume / Checkpoint / Delete
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sandboxes/dev-1/suspend", nil))
	assert.Equal(t, http.StatusSeeOther, rec.Code)
	assert.Equal(t, "dev-1", fs.suspendedName)

	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sandboxes/dev-1/resume", nil))
	assert.Equal(t, http.StatusSeeOther, rec.Code)
	assert.Equal(t, "dev-1", fs.resumedName)

	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sandboxes/dev-1/checkpoint", nil))
	assert.Equal(t, http.StatusSeeOther, rec.Code)
	assert.Equal(t, "dev-1", fs.cpSandbox)

	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sandboxes/dev-1/delete", nil))
	assert.Equal(t, http.StatusSeeOther, rec.Code)
	assert.Equal(t, "dev-1", fs.deletedName)

	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/checkpoints/cp-1/delete", nil))
	assert.Equal(t, http.StatusSeeOther, rec.Code)
	assert.Equal(t, "cp-1", fs.deletedCP)

	// Logs raw text
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sandboxes/dev-1/logs?format=text", nil))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "line1\nline2\n", rec.Body.String())
}
