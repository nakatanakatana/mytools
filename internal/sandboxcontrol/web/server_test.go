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
	"github.com/nakatanakatana/mytools/internal/sandboxcontrol/githubauth"
	"github.com/nakatanakatana/mytools/internal/sandboxcontrol/k8s"
	"github.com/nakatanakatana/mytools/internal/sandboxcontrol/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeGHAuth struct {
	enabled      bool
	snap         githubauth.StatusSnapshot
	loginFlow    githubauth.FlowStatus
	logoutCalled bool
}

func (f *fakeGHAuth) Enabled() bool { return f.enabled }
func (f *fakeGHAuth) Status(context.Context) githubauth.StatusSnapshot {
	return f.snap
}
func (f *fakeGHAuth) StartDeviceFlow(context.Context) (githubauth.FlowStatus, <-chan githubauth.FlowStatus, error) {
	ch := make(chan githubauth.FlowStatus, 1)
	ch <- githubauth.FlowStatus{State: githubauth.FlowStateCompleted}
	close(ch)
	return f.loginFlow, ch, nil
}
func (f *fakeGHAuth) Logout(context.Context) (*githubauth.ProxyStatus, error) {
	f.logoutCalled = true
	return &githubauth.ProxyStatus{Mode: "app_installation", AppConfigured: true}, nil
}

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

	exp := int64(1763810000)
	gh := &fakeGHAuth{
		enabled: true,
		snap: githubauth.StatusSnapshot{
			DeviceFlowEnabled: true,
			Proxy: &githubauth.ProxyStatus{
				Mode:               "user_oauth",
				UserTokenExpiresAt: &exp,
				AppConfigured:      true,
			},
			Flow: githubauth.FlowStatus{State: githubauth.FlowStateIdle},
		},
		loginFlow: githubauth.FlowStatus{
			State:           githubauth.FlowStatePending,
			UserCode:        "WXYZ-9999",
			VerificationURI: "https://github.com/login/device",
		},
	}

	srv, err := NewServer(fs, gh)
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

	// GitHub Auth status JSON, Device Flow login JSON, and logout
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/github/status", nil))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"user_oauth"`)

	loginReq := httptest.NewRequest(http.MethodPost, "/github/login", nil)
	loginReq.Header.Set("Accept", "application/json")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, loginReq)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"WXYZ-9999"`)

	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/github/logout", nil))
	assert.Equal(t, http.StatusSeeOther, rec.Code)
	assert.True(t, gh.logoutCalled)
}
