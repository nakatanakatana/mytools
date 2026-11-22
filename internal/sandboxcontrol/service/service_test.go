package service

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/nakatanakatana/mytools/internal/sandboxcontrol/config"
	"github.com/nakatanakatana/mytools/internal/sandboxcontrol/k8s"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

type fakeClient struct {
	mu          sync.Mutex
	warmPools   []k8s.WarmPoolInfo
	templates   []k8s.TemplateInfo
	claims      map[string]k8s.ClaimInfo
	sandboxes   map[string]k8s.SandboxInfo
	checkpoints map[string]k8s.CheckpointInfo
	execCalled  *k8s.ExecOptions
	readyOnPoll int
	pollCount   int
}

func newFakeClient() *fakeClient {
	return &fakeClient{
		claims:      map[string]k8s.ClaimInfo{},
		sandboxes:   map[string]k8s.SandboxInfo{},
		checkpoints: map[string]k8s.CheckpointInfo{},
	}
}

func (f *fakeClient) Namespace() string          { return "sandbox" }
func (f *fakeClient) Ping(context.Context) error { return nil }
func (f *fakeClient) ListWarmPools(context.Context) ([]k8s.WarmPoolInfo, error) {
	return f.warmPools, nil
}
func (f *fakeClient) ListTemplates(context.Context) ([]k8s.TemplateInfo, error) {
	return f.templates, nil
}
func (f *fakeClient) GetTemplate(_ context.Context, name string) (*k8s.TemplateInfo, error) {
	for _, t := range f.templates {
		if t.Name == name {
			return &t, nil
		}
	}
	return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "sandboxtemplates"}, name)
}
func (f *fakeClient) ListClaims(context.Context) ([]k8s.ClaimInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]k8s.ClaimInfo, 0, len(f.claims))
	for _, c := range f.claims {
		out = append(out, c)
	}
	return out, nil
}
func (f *fakeClient) CreateClaim(_ context.Context, name, warmPoolName string, shutdownTime *time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claims[name] = k8s.ClaimInfo{Name: name, WarmPoolName: warmPoolName, ShutdownTime: shutdownTime}
	return nil
}
func (f *fakeClient) DeleteClaim(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.claims, name)
	return nil
}
func (f *fakeClient) ListSandboxes(context.Context) ([]k8s.SandboxInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]k8s.SandboxInfo, 0, len(f.sandboxes))
	for _, s := range f.sandboxes {
		out = append(out, s)
	}
	return out, nil
}
func (f *fakeClient) GetSandbox(_ context.Context, name string) (*k8s.SandboxInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sb, ok := f.sandboxes[name]
	if !ok {
		return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "sandboxes"}, name)
	}
	if sb.OperatingMode == k8s.OperatingModeRunning && !sb.Ready && f.readyOnPoll > 0 {
		f.pollCount++
		if f.pollCount >= f.readyOnPoll {
			sb.Ready = true
			sb.Suspended = false
			sb.Phase = "Running"
			f.sandboxes[name] = sb
		}
	}
	return &sb, nil
}
func (f *fakeClient) CreateSandboxFromTemplate(_ context.Context, opts k8s.CreateSandboxOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sandboxes[opts.Name] = k8s.SandboxInfo{
		Name:                opts.Name,
		TemplateName:        opts.TemplateName,
		OperatingMode:       k8s.OperatingModeRunning,
		Phase:               "Running",
		Ready:               true,
		Persistent:          true,
		WorkspaceVolumeName: opts.WorkspaceVolumeName,
		ShutdownTime:        opts.ShutdownTime,
	}
	return nil
}
func (f *fakeClient) SetSandboxOperatingMode(_ context.Context, name, mode string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	sb, ok := f.sandboxes[name]
	if !ok {
		return apierrors.NewNotFound(schema.GroupResource{Resource: "sandboxes"}, name)
	}
	sb.OperatingMode = mode
	if mode == k8s.OperatingModeSuspended {
		sb.Suspended = true
		sb.Ready = false
		sb.Phase = "Suspended"
	} else {
		sb.Suspended = false
		sb.Phase = "Starting"
	}
	f.sandboxes[name] = sb
	return nil
}
func (f *fakeClient) DeleteSandbox(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.sandboxes, name)
	return nil
}
func (f *fakeClient) ListCheckpoints(context.Context) ([]k8s.CheckpointInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]k8s.CheckpointInfo, 0, len(f.checkpoints))
	for _, cp := range f.checkpoints {
		out = append(out, cp)
	}
	return out, nil
}
func (f *fakeClient) CreateCheckpoint(_ context.Context, name, pvcName, snapshotClass, sandboxName string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checkpoints[name] = k8s.CheckpointInfo{
		Name:              name,
		SourcePVC:         pvcName,
		SandboxName:       sandboxName,
		SnapshotClassName: snapshotClass,
		ReadyToUse:        true,
	}
	return nil
}
func (f *fakeClient) DeleteCheckpoint(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.checkpoints, name)
	return nil
}
func (f *fakeClient) ResolvePodName(_ context.Context, sb *k8s.SandboxInfo) (string, error) {
	return sb.Name, nil
}
func (f *fakeClient) GetPodLogs(context.Context, string, string, int64) (string, error) {
	return "hello log\n", nil
}
func (f *fakeClient) StreamPodLogs(context.Context, string, string, int64, bool) (io.ReadCloser, error) {
	return io.NopCloser(nil), nil
}
func (f *fakeClient) ExecPod(_ context.Context, opts k8s.ExecOptions) error {
	f.execCalled = &opts
	return nil
}

func TestService_CreateModesAndCheckpoint(t *testing.T) {
	fc := newFakeClient()
	cfg := config.Default()
	cfg.InsecureAllowAnySSHKey = true
	svc := New(cfg, fc)
	svc.pollInterval = 5 * time.Millisecond

	ctx := t.Context()

	// Ephemeral -> SandboxClaim
	res, err := svc.Create(ctx, CreateRequest{
		Name:     "eph-a",
		Template: cfg.EphemeralTemplate,
		TTL:      time.Hour,
	})
	require.NoError(t, err)
	assert.Equal(t, "SandboxClaim", res.Kind)
	assert.Contains(t, fc.claims, "eph-a")

	// Persistent -> Sandbox
	res, err = svc.Create(ctx, CreateRequest{
		Name:     "pers-a",
		Template: cfg.PersistentTemplate,
	})
	require.NoError(t, err)
	assert.Equal(t, "Sandbox", res.Kind)
	assert.Contains(t, fc.sandboxes, "pers-a")

	// Checkpoint from persistent sandbox
	cpName, err := svc.CreateCheckpoint(ctx, "pers-a", "cp-1")
	require.NoError(t, err)
	assert.Equal(t, "cp-1", cpName)
	assert.Equal(t, "workspace-pers-a", fc.checkpoints["cp-1"].SourcePVC)

	// Fork from checkpoint even if ephemeral was passed -> switches to persistent
	res, err = svc.Create(ctx, CreateRequest{
		Name:       "fork-a",
		Template:   cfg.EphemeralTemplate,
		Checkpoint: "cp-1",
	})
	require.NoError(t, err)
	assert.Equal(t, "Sandbox", res.Kind)
	assert.Equal(t, cfg.PersistentTemplate, res.Template)
}

func TestService_AutoResumeAndExec(t *testing.T) {
	fc := newFakeClient()
	fc.sandboxes["dev-1"] = k8s.SandboxInfo{
		Name:          "dev-1",
		OperatingMode: k8s.OperatingModeSuspended,
		Suspended:     true,
		Ready:         false,
		Phase:         "Suspended",
	}
	fc.readyOnPoll = 2

	cfg := config.Default()
	cfg.InsecureAllowAnySSHKey = true
	cfg.ResumeTimeout = 2 * time.Second
	svc := New(cfg, fc)
	svc.pollInterval = 5 * time.Millisecond

	require.NoError(t, svc.Exec(t.Context(), "dev-1", k8s.ExecOptions{TTY: true}))
	require.NotNil(t, fc.execCalled)
	assert.Equal(t, "dev-1", fc.execCalled.PodName)
	assert.Equal(t, "opencode", fc.execCalled.ContainerName)
	assert.Equal(t, []string{"/bin/sh"}, fc.execCalled.Command)
	assert.True(t, fc.sandboxes["dev-1"].Ready)
}

func TestService_OverviewExcludesUnclaimedWarmPoolStandbySandboxes(t *testing.T) {
	fc := newFakeClient()
	fc.sandboxes["pool-standby-1"] = k8s.SandboxInfo{
		Name:            "pool-standby-1",
		Phase:           "Running",
		Ready:           true,
		WarmPoolStandby: true,
	}
	fc.sandboxes["active-1"] = k8s.SandboxInfo{
		Name:  "active-1",
		Phase: "Running",
		Ready: true,
	}

	cfg := config.Default()
	cfg.InsecureAllowAnySSHKey = true
	svc := New(cfg, fc)

	data, err := svc.Overview(t.Context())
	require.NoError(t, err)
	require.Len(t, data.Sandboxes, 1)
	assert.Equal(t, "active-1", data.Sandboxes[0].Name)
}
