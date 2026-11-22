package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/nakatanakatana/mytools/internal/sandboxcontrol/config"
	"github.com/nakatanakatana/mytools/internal/sandboxcontrol/k8s"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// DashboardData aggregates all resources displayed in the Web UI and CLI.
type DashboardData struct {
	Namespace   string               `json:"namespace"`
	WarmPools   []k8s.WarmPoolInfo   `json:"warmPools"`
	Sandboxes   []k8s.SandboxInfo    `json:"sandboxes"`
	Claims      []k8s.ClaimInfo      `json:"claims"`
	Checkpoints []k8s.CheckpointInfo `json:"checkpoints"`
	Templates   []k8s.TemplateInfo   `json:"templates"`
}

// CreateRequest specifies parameters to create or fork a sandbox.
type CreateRequest struct {
	Name       string
	Template   string
	TTL        time.Duration
	Checkpoint string
}

// CreateResult describes the created resource.
type CreateResult struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"` // "SandboxClaim" or "Sandbox"
	Template string `json:"template"`
}

// Service coordinates sandbox lifecycle operations across Web UI, Preview Proxy, and SSH Gateway.
type Service struct {
	cfg          config.Config
	client       k8s.Client
	now          func() time.Time
	pollInterval time.Duration
}

// New constructs a Service with production defaults.
func New(cfg config.Config, client k8s.Client) *Service {
	return &Service{
		cfg:          cfg,
		client:       client,
		now:          time.Now,
		pollInterval: 500 * time.Millisecond,
	}
}

// Config returns the active configuration.
func (s *Service) Config() config.Config {
	return s.cfg
}

// Ping checks Kubernetes API reachability in the sandbox namespace.
func (s *Service) Ping(ctx context.Context) error {
	return s.client.Ping(ctx)
}

// Overview collects warm pools, sandboxes, claims, checkpoints, and templates.
func (s *Service) Overview(ctx context.Context) (*DashboardData, error) {
	pools, err := s.client.ListWarmPools(ctx)
	if err != nil {
		return nil, err
	}
	sandboxes, err := s.client.ListSandboxes(ctx)
	if err != nil {
		return nil, err
	}
	claims, err := s.client.ListClaims(ctx)
	if err != nil {
		return nil, err
	}
	checkpoints, err := s.client.ListCheckpoints(ctx)
	if err != nil {
		return nil, err
	}
	templates, err := s.client.ListTemplates(ctx)
	if err != nil {
		return nil, err
	}

	claimBySandbox := make(map[string]string, len(claims))
	for _, c := range claims {
		if c.SandboxName != "" {
			claimBySandbox[c.SandboxName] = c.Name
		}
	}
	activeSandboxes := make([]k8s.SandboxInfo, 0, len(sandboxes))
	for i := range sandboxes {
		if cn, ok := claimBySandbox[sandboxes[i].Name]; ok {
			sandboxes[i].ClaimName = cn
		}
		if sandboxes[i].WarmPoolStandby && sandboxes[i].ClaimName == "" {
			continue
		}
		activeSandboxes = append(activeSandboxes, sandboxes[i])
	}
	sandboxes = activeSandboxes

	slices.SortFunc(pools, func(a, b k8s.WarmPoolInfo) int { return strings.Compare(a.Name, b.Name) })
	slices.SortFunc(sandboxes, func(a, b k8s.SandboxInfo) int { return strings.Compare(a.Name, b.Name) })
	slices.SortFunc(claims, func(a, b k8s.ClaimInfo) int { return strings.Compare(a.Name, b.Name) })
	slices.SortFunc(checkpoints, func(a, b k8s.CheckpointInfo) int { return strings.Compare(a.Name, b.Name) })
	slices.SortFunc(templates, func(a, b k8s.TemplateInfo) int { return strings.Compare(a.Name, b.Name) })

	return &DashboardData{
		Namespace:   s.cfg.Namespace,
		WarmPools:   pools,
		Sandboxes:   sandboxes,
		Claims:      claims,
		Checkpoints: checkpoints,
		Templates:   templates,
	}, nil
}

// Create creates either a SandboxClaim (for ephemeral warm pool checkout) or a direct Sandbox
// (for persistent templates or checkpoint forks).
func (s *Service) Create(ctx context.Context, req CreateRequest) (*CreateResult, error) {
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = fmt.Sprintf("sb-%s", s.now().UTC().Format("20060102-150405"))
	}
	if !config.IsValidDNS1123Label(name) {
		return nil, fmt.Errorf("invalid sandbox name %q: must be a DNS-1123 label", name)
	}
	if req.TTL < 0 {
		return nil, errors.New("ttl must not be negative")
	}

	var shutdownTime *time.Time
	if req.TTL > 0 {
		t := s.now().UTC().Add(req.TTL)
		shutdownTime = &t
	}

	checkpoint := strings.TrimSpace(req.Checkpoint)
	if checkpoint != "" && !config.IsValidDNS1123Label(checkpoint) {
		return nil, fmt.Errorf("invalid checkpoint name %q", checkpoint)
	}

	tmpl := strings.TrimSpace(req.Template)
	if checkpoint != "" {
		if tmpl == "" || tmpl == s.cfg.EphemeralTemplate {
			tmpl = s.cfg.PersistentTemplate
		}
	} else if tmpl == "" {
		tmpl = s.cfg.EphemeralTemplate
	}

	if checkpoint == "" && tmpl == s.cfg.EphemeralTemplate {
		if err := s.client.CreateClaim(ctx, name, s.cfg.EphemeralWarmPool, shutdownTime); err != nil {
			return nil, err
		}
		return &CreateResult{
			Name:     name,
			Kind:     "SandboxClaim",
			Template: tmpl,
		}, nil
	}

	if err := s.client.CreateSandboxFromTemplate(ctx, k8s.CreateSandboxOptions{
		Name:                name,
		TemplateName:        tmpl,
		WorkspaceVolumeName: s.cfg.WorkspaceVolumeName,
		CheckpointName:      checkpoint,
		ShutdownTime:        shutdownTime,
	}); err != nil {
		return nil, err
	}
	return &CreateResult{
		Name:     name,
		Kind:     "Sandbox",
		Template: tmpl,
	}, nil
}

// Suspend transitions a Sandbox into Suspended operatingMode.
func (s *Service) Suspend(ctx context.Context, name string) error {
	target, err := s.resolveSandboxName(ctx, name)
	if err != nil {
		return err
	}
	return s.client.SetSandboxOperatingMode(ctx, target, k8s.OperatingModeSuspended)
}

// Resume transitions a Sandbox into Running operatingMode without waiting for Ready.
func (s *Service) Resume(ctx context.Context, name string) error {
	target, err := s.resolveSandboxName(ctx, name)
	if err != nil {
		return err
	}
	return s.client.SetSandboxOperatingMode(ctx, target, k8s.OperatingModeRunning)
}

// EnsureRunningAndReady resumes the sandbox if it is suspended and waits until Ready=True.
// Returns the resolved SandboxInfo.
func (s *Service) EnsureRunningAndReady(ctx context.Context, name string) (*k8s.SandboxInfo, error) {
	target, err := s.resolveSandboxName(ctx, name)
	if err != nil {
		return nil, err
	}
	sb, err := s.client.GetSandbox(ctx, target)
	if err != nil {
		return nil, err
	}
	if sb.Ready && !sb.Suspended {
		return sb, nil
	}
	if sb.Suspended || sb.OperatingMode == k8s.OperatingModeSuspended {
		if err := s.client.SetSandboxOperatingMode(ctx, target, k8s.OperatingModeRunning); err != nil {
			return nil, err
		}
	}

	waitCtx, cancel := context.WithTimeout(ctx, s.cfg.ResumeTimeout)
	defer cancel()

	ticker := time.NewTicker(s.pollInterval)
	defer ticker.Stop()

	for {
		cur, err := s.client.GetSandbox(waitCtx, target)
		if err != nil {
			return nil, err
		}
		if cur.Ready && !cur.Suspended {
			return cur, nil
		}
		select {
		case <-waitCtx.Done():
			return nil, fmt.Errorf("timed out waiting for sandbox %q to become Ready (last phase=%s, reason=%s): %w", target, cur.Phase, cur.ReadyReason, waitCtx.Err())
		case <-ticker.C:
		}
	}
}

// CreateCheckpoint creates a VolumeSnapshot from a persistent sandbox's workspace PVC.
func (s *Service) CreateCheckpoint(ctx context.Context, sandboxName, checkpointName string) (string, error) {
	target, err := s.resolveSandboxName(ctx, sandboxName)
	if err != nil {
		return "", err
	}
	sb, err := s.client.GetSandbox(ctx, target)
	if err != nil {
		return "", err
	}
	if !sb.Persistent {
		return "", fmt.Errorf("sandbox %q is not persistent (no volumeClaimTemplates); checkpoints require a persistent sandbox", target)
	}

	cpName := strings.TrimSpace(checkpointName)
	if cpName == "" {
		cpName = fmt.Sprintf("%s-%s", target, s.now().UTC().Format("20060102-150405"))
	}
	if !config.IsValidDNS1123Label(cpName) {
		return "", fmt.Errorf("invalid checkpoint name %q: must be a DNS-1123 label", cpName)
	}

	wsVol := sb.WorkspaceVolumeName
	if wsVol == "" {
		wsVol = s.cfg.WorkspaceVolumeName
	}
	pvcName := fmt.Sprintf("%s-%s", wsVol, target)

	if err := s.client.CreateCheckpoint(ctx, cpName, pvcName, s.cfg.SnapshotClassName, target); err != nil {
		return "", err
	}
	return cpName, nil
}

// DeleteCheckpoint deletes a VolumeSnapshot by name.
func (s *Service) DeleteCheckpoint(ctx context.Context, name string) error {
	if !config.IsValidDNS1123Label(name) {
		return fmt.Errorf("invalid checkpoint name %q", name)
	}
	return s.client.DeleteCheckpoint(ctx, name)
}

// Delete removes a SandboxClaim (if matched by claim name or bound sandbox name) or a Sandbox.
func (s *Service) Delete(ctx context.Context, name string) error {
	if !config.IsValidDNS1123Label(name) {
		return fmt.Errorf("invalid sandbox name %q", name)
	}
	claims, err := s.client.ListClaims(ctx)
	if err == nil {
		for _, c := range claims {
			if c.Name == name || c.SandboxName == name {
				return s.client.DeleteClaim(ctx, c.Name)
			}
		}
	}
	return s.client.DeleteSandbox(ctx, name)
}

// GetLogs fetches recent logs from the sandbox's backing pod.
func (s *Service) GetLogs(ctx context.Context, sandboxName string, tailLines int64) (string, error) {
	target, err := s.resolveSandboxName(ctx, sandboxName)
	if err != nil {
		return "", err
	}
	sb, err := s.client.GetSandbox(ctx, target)
	if err != nil {
		return "", err
	}
	podName, err := s.client.ResolvePodName(ctx, sb)
	if err != nil {
		return "", err
	}
	return s.client.GetPodLogs(ctx, podName, s.cfg.DefaultExecContainer, tailLines)
}

// StreamLogs streams logs from the sandbox's backing pod.
func (s *Service) StreamLogs(ctx context.Context, sandboxName string, tailLines int64, follow bool) (io.ReadCloser, error) {
	target, err := s.resolveSandboxName(ctx, sandboxName)
	if err != nil {
		return nil, err
	}
	sb, err := s.client.GetSandbox(ctx, target)
	if err != nil {
		return nil, err
	}
	podName, err := s.client.ResolvePodName(ctx, sb)
	if err != nil {
		return nil, err
	}
	return s.client.StreamPodLogs(ctx, podName, s.cfg.DefaultExecContainer, tailLines, follow)
}

// Exec ensures the sandbox is running and ready, resolves its backing pod, and attaches via pods/exec.
func (s *Service) Exec(ctx context.Context, sandboxName string, opts k8s.ExecOptions) error {
	sb, err := s.EnsureRunningAndReady(ctx, sandboxName)
	if err != nil {
		return err
	}
	podName, err := s.client.ResolvePodName(ctx, sb)
	if err != nil {
		return err
	}
	opts.PodName = podName
	if opts.ContainerName == "" {
		opts.ContainerName = s.cfg.DefaultExecContainer
	}
	if len(opts.Command) == 0 {
		opts.Command = []string{s.cfg.DefaultExecShell}
	}
	return s.client.ExecPod(ctx, opts)
}

func (s *Service) resolveSandboxName(ctx context.Context, name string) (string, error) {
	if !config.IsValidDNS1123Label(name) {
		return "", fmt.Errorf("invalid sandbox name %q", name)
	}
	if _, err := s.client.GetSandbox(ctx, name); err == nil {
		return name, nil
	} else if !apierrors.IsNotFound(err) {
		return "", err
	}
	// Fallback: allow targeting by SandboxClaim name.
	claims, err := s.client.ListClaims(ctx)
	if err != nil {
		return "", err
	}
	for _, c := range claims {
		if c.Name == name && c.SandboxName != "" {
			return c.SandboxName, nil
		}
	}
	return "", fmt.Errorf("sandbox %q not found", name)
}
