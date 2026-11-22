package sshgw

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"charm.land/ssh"
	"github.com/nakatanakatana/mytools/internal/sandboxcontrol/config"
	"github.com/nakatanakatana/mytools/internal/sandboxcontrol/githubauth"
	"github.com/nakatanakatana/mytools/internal/sandboxcontrol/k8s"
	"github.com/nakatanakatana/mytools/internal/sandboxcontrol/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gossh "golang.org/x/crypto/ssh"
)

type fakeSSHGHAuth struct {
	enabled      bool
	snap         githubauth.StatusSnapshot
	logoutCalled bool
}

func (f *fakeSSHGHAuth) Enabled() bool { return f.enabled }
func (f *fakeSSHGHAuth) Status(context.Context) githubauth.StatusSnapshot {
	return f.snap
}
func (f *fakeSSHGHAuth) StartDeviceFlow(context.Context) (githubauth.FlowStatus, <-chan githubauth.FlowStatus, error) {
	ch := make(chan githubauth.FlowStatus, 1)
	ch <- githubauth.FlowStatus{State: githubauth.FlowStateCompleted}
	close(ch)
	return githubauth.FlowStatus{
		State:           githubauth.FlowStatePending,
		UserCode:        "SSH1-2345",
		VerificationURI: "https://github.com/login/device",
	}, ch, nil
}
func (f *fakeSSHGHAuth) Logout(context.Context) (*githubauth.ProxyStatus, error) {
	f.logoutCalled = true
	return &githubauth.ProxyStatus{Mode: "app_installation", AppConfigured: true}, nil
}

func generateTestKeyPair(t *testing.T) (gossh.PublicKey, []byte) {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	sshPub, err := gossh.NewPublicKey(pub)
	require.NoError(t, err)
	return sshPub, gossh.MarshalAuthorizedKey(sshPub)
}

func TestKeyStore_GitHubAndGitLabFetchAndFallbackCache(t *testing.T) {
	ghKey, ghAuthorized := generateTestKeyPair(t)
	glKey, glAuthorized := generateTestKeyPair(t)
	otherKey, _ := generateTestKeyPair(t)

	failRemote := false
	ghSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failRemote {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		assert.Equal(t, "/alice.keys", r.URL.Path)
		_, _ = w.Write(ghAuthorized)
	}))
	defer ghSrv.Close()

	glSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failRemote {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		assert.Equal(t, "/bob.keys", r.URL.Path)
		_, _ = w.Write(glAuthorized)
	}))
	defer glSrv.Close()

	ks := NewKeyStore(KeyStoreOptions{
		GitHubUsers:   []string{"alice"},
		GitHubBaseURL: ghSrv.URL,
		GitLabUsers:   []string{"bob"},
		GitLabBaseURL: glSrv.URL,
	})

	require.NoError(t, ks.Refresh(t.Context()))
	assert.Equal(t, 2, ks.Count())
	assert.True(t, ks.Authorize(ghKey))
	assert.True(t, ks.Authorize(glKey))
	assert.False(t, ks.Authorize(otherKey))

	// When remote servers fail on refresh, cached keys must still be honored.
	failRemote = true
	require.Error(t, ks.Refresh(t.Context()))
	assert.Equal(t, 2, ks.Count())
	assert.True(t, ks.Authorize(ghKey))
	assert.True(t, ks.Authorize(glKey))
}

type fakeGWService struct {
	cfg           config.Config
	overview      *service.DashboardData
	createdReq    *service.CreateRequest
	suspendedName string
	resumedName   string
	waitedName    string
	deletedName   string
	cpSandbox     string
	execSandbox   string
	execCommand   []string
}

func (f *fakeGWService) Config() config.Config { return f.cfg }
func (f *fakeGWService) Overview(context.Context) (*service.DashboardData, error) {
	return f.overview, nil
}
func (f *fakeGWService) Create(_ context.Context, req service.CreateRequest) (*service.CreateResult, error) {
	f.createdReq = &req
	return &service.CreateResult{Name: req.Name, Kind: "Sandbox", Template: req.Template}, nil
}
func (f *fakeGWService) Suspend(_ context.Context, name string) error {
	f.suspendedName = name
	return nil
}
func (f *fakeGWService) Resume(_ context.Context, name string) error {
	f.resumedName = name
	return nil
}
func (f *fakeGWService) EnsureRunningAndReady(_ context.Context, name string) (*k8s.SandboxInfo, error) {
	f.waitedName = name
	return &k8s.SandboxInfo{Name: name, Ready: true}, nil
}
func (f *fakeGWService) CreateCheckpoint(_ context.Context, sb, cp string) (string, error) {
	f.cpSandbox = sb
	if cp == "" {
		return sb + "-cp", nil
	}
	return cp, nil
}
func (f *fakeGWService) DeleteCheckpoint(context.Context, string) error { return nil }
func (f *fakeGWService) Delete(_ context.Context, name string) error {
	f.deletedName = name
	return nil
}
func (f *fakeGWService) Exec(_ context.Context, sb string, opts k8s.ExecOptions) error {
	f.execSandbox = sb
	f.execCommand = opts.Command
	if opts.Stdout != nil {
		_, _ = opts.Stdout.Write([]byte("attached:" + sb + "\n"))
	}
	return nil
}

func TestServer_ManagementCommands(t *testing.T) {
	cfg := config.Default()
	cfg.InsecureAllowAnySSHKey = true
	fsvc := &fakeGWService{
		cfg: cfg,
		overview: &service.DashboardData{
			WarmPools:   []k8s.WarmPoolInfo{{Name: "opencode-ephemeral-pool", TemplateName: "opencode-ephemeral", DesiredReplicas: 1, ReadyReplicas: 1}},
			Sandboxes:   []k8s.SandboxInfo{{Name: "dev-1", Phase: "Running", TemplateName: "opencode-persistent", Persistent: true}},
			Checkpoints: []k8s.CheckpointInfo{{Name: "cp-1", SourcePVC: "workspace-dev-1", ReadyToUse: true}},
		},
	}
	gh := &fakeSSHGHAuth{
		enabled: true,
		snap: githubauth.StatusSnapshot{
			DeviceFlowEnabled: true,
			Proxy:             &githubauth.ProxyStatus{Mode: "app_installation", AppConfigured: true},
		},
	}
	ks := NewKeyStore(KeyStoreOptions{InsecureAllowAny: true})
	srv, err := NewServer(fsvc, ks, gh, nil)
	require.NoError(t, err)

	var buf bytes.Buffer
	assert.Equal(t, 0, srv.runManagementCommand(t.Context(), []string{"ls"}, &buf))
	assert.Contains(t, buf.String(), "opencode-ephemeral-pool")
	assert.Contains(t, buf.String(), "dev-1")
	assert.Contains(t, buf.String(), "cp-1")

	buf.Reset()
	assert.Equal(t, 0, srv.runManagementCommand(t.Context(), []string{"new", "--template=opencode-persistent", "--ttl=2h", "--from=cp-1", "fork-1"}, &buf))
	require.NotNil(t, fsvc.createdReq)
	assert.Equal(t, "fork-1", fsvc.createdReq.Name)
	assert.Equal(t, "cp-1", fsvc.createdReq.Checkpoint)

	buf.Reset()
	assert.Equal(t, 0, srv.runManagementCommand(t.Context(), []string{"checkpoint", "dev-1", "cp-2"}, &buf))
	assert.Equal(t, "dev-1", fsvc.cpSandbox)

	buf.Reset()
	assert.Equal(t, 0, srv.runManagementCommand(t.Context(), []string{"suspend", "dev-1"}, &buf))
	assert.Equal(t, "dev-1", fsvc.suspendedName)

	buf.Reset()
	assert.Equal(t, 0, srv.runManagementCommand(t.Context(), []string{"resume", "--wait", "dev-1"}, &buf))
	assert.Equal(t, "dev-1", fsvc.waitedName)

	buf.Reset()
	assert.Equal(t, 0, srv.runManagementCommand(t.Context(), []string{"rm", "dev-1"}, &buf))
	assert.Equal(t, "dev-1", fsvc.deletedName)

	buf.Reset()
	assert.Equal(t, 0, srv.runManagementCommand(t.Context(), []string{"github", "status"}, &buf))
	assert.Contains(t, buf.String(), "mode: app_installation")

	buf.Reset()
	assert.Equal(t, 0, srv.runManagementCommand(t.Context(), []string{"github", "login"}, &buf))
	assert.Contains(t, buf.String(), "SSH1-2345")
	assert.Contains(t, buf.String(), "GitHub authentication completed")

	buf.Reset()
	assert.Equal(t, 0, srv.runManagementCommand(t.Context(), []string{"github", "logout"}, &buf))
	assert.True(t, gh.logoutCalled)
	assert.Contains(t, buf.String(), "mode: app_installation")
}

func TestServer_EndToEndSSHAuthAndRouting(t *testing.T) {
	allowedPub, allowedPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	allowedSSHPub, err := gossh.NewPublicKey(allowedPub)
	require.NoError(t, err)
	allowedSigner, err := gossh.NewSignerFromKey(allowedPriv)
	require.NoError(t, err)

	_, deniedPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	deniedSigner, err := gossh.NewSignerFromKey(deniedPriv)
	require.NoError(t, err)

	ks := NewKeyStore(KeyStoreOptions{
		AuthorizedKeysInline: string(gossh.MarshalAuthorizedKey(allowedSSHPub)),
	})
	require.NoError(t, ks.Refresh(t.Context()))

	cfg := config.Default()
	cfg.SSHAddr = "127.0.0.1:0"
	cfg.SSHAuthorizedKeys = string(gossh.MarshalAuthorizedKey(allowedSSHPub))

	fsvc := &fakeGWService{
		cfg: cfg,
		overview: &service.DashboardData{
			Sandboxes: []k8s.SandboxInfo{{Name: "dev-1", Phase: "Running"}},
		},
	}

	srv, err := NewServer(fsvc, ks, nil, nil)
	require.NoError(t, err)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = ln.Close() }()

	go func() {
		if serveErr := srv.wishServer.Serve(ln); serveErr != nil && serveErr != ssh.ErrServerClosed {
			t.Logf("ssh serve error: %v", serveErr)
		}
	}()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	// Unauthorized key must be rejected.
	_, err = gossh.Dial("tcp", ln.Addr().String(), &gossh.ClientConfig{
		User:            "admin",
		Auth:            []gossh.AuthMethod{gossh.PublicKeys(deniedSigner)},
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
		Timeout:         2 * time.Second,
	})
	require.Error(t, err)

	// Authorized key + management command: ssh admin@sandbox-ctl ls
	client, err := gossh.Dial("tcp", ln.Addr().String(), &gossh.ClientConfig{
		User:            "admin",
		Auth:            []gossh.AuthMethod{gossh.PublicKeys(allowedSigner)},
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
		Timeout:         2 * time.Second,
	})
	require.NoError(t, err)
	sess, err := client.NewSession()
	require.NoError(t, err)
	out, err := sess.CombinedOutput("ls")
	require.NoError(t, err)
	assert.Contains(t, string(out), "dev-1")
	_ = sess.Close()
	_ = client.Close()

	// Authorized key + direct shell attach: ssh dev-1@sandbox-ctl
	client2, err := gossh.Dial("tcp", ln.Addr().String(), &gossh.ClientConfig{
		User:            "dev-1",
		Auth:            []gossh.AuthMethod{gossh.PublicKeys(allowedSigner)},
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
		Timeout:         2 * time.Second,
	})
	require.NoError(t, err)
	sess2, err := client2.NewSession()
	require.NoError(t, err)
	out2, err := sess2.CombinedOutput("")
	require.NoError(t, err)
	assert.Equal(t, "attached:dev-1\n", string(out2))
	assert.Equal(t, "dev-1", fsvc.execSandbox)
	_ = sess2.Close()
	_ = client2.Close()
}
