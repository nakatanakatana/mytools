package config

import (
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultHTTPAddr               = ":8080"
	DefaultSSHAddr                = ":2222"
	DefaultNamespace              = "sandbox"
	DefaultRouterURL              = "http://sandbox-router-svc.agent-sandbox-system.svc.cluster.local:8080"
	DefaultSnapshotClassName      = "rook-ceph-rbd-snapshot"
	DefaultEphemeralTemplate      = "opencode-ephemeral"
	DefaultEphemeralWarmPool      = "opencode-ephemeral-pool"
	DefaultPersistentTemplate     = "opencode-persistent"
	DefaultWorkspaceVolumeName    = "workspace"
	DefaultOpenCodePort           = 4096
	DefaultExecContainer          = "opencode"
	DefaultExecShell              = "/bin/sh"
	DefaultResumeTimeout          = 2 * time.Minute
	DefaultSSHGitLabBaseURL       = "https://gitlab.com"
	DefaultSSHKeysRefreshInterval = 15 * time.Minute
	DefaultEgressProxyAdminURL    = "http://egress-proxy-admin.proxy.svc.cluster.local:3129"
)

var (
	dns1123LabelRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	gitUsernameRe  = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)
)

// Config holds runtime configuration for sandbox-control.
type Config struct {
	HTTPAddr                    string
	SSHAddr                     string
	Namespace                   string
	RouterURL                   string
	SnapshotClassName           string
	EphemeralTemplate           string
	EphemeralWarmPool           string
	PersistentTemplate          string
	WorkspaceVolumeName         string
	OpenCodePort                int
	DefaultExecContainer        string
	DefaultExecShell            string
	ResumeTimeout               time.Duration
	SSHHostKeyPath              string
	SSHAuthorizedKeysPath       string
	SSHAuthorizedKeys           string
	SSHGitHubUsers              []string
	SSHGitLabUsers              []string
	SSHGitLabBaseURL            string
	SSHKeysRefreshInterval      time.Duration
	InsecureAllowAnySSHKey      bool
	AllowUnauthenticatedPreview bool
	GitHubAppClientID           string
	EgressProxyAdminURL         string
	Kubeconfig                  string
}

// Default returns Config populated with defaults and environment variable overrides.
func Default() Config {
	return Config{
		HTTPAddr:                    envOrDefault("HTTP_ADDR", DefaultHTTPAddr),
		SSHAddr:                     envOrDefault("SSH_ADDR", DefaultSSHAddr),
		Namespace:                   envOrDefault("SANDBOX_NAMESPACE", DefaultNamespace),
		RouterURL:                   envOrDefault("SANDBOX_ROUTER_URL", DefaultRouterURL),
		SnapshotClassName:           envOrDefault("SNAPSHOT_CLASS_NAME", DefaultSnapshotClassName),
		EphemeralTemplate:           envOrDefault("EPHEMERAL_TEMPLATE", DefaultEphemeralTemplate),
		EphemeralWarmPool:           envOrDefault("EPHEMERAL_WARMPOOL", DefaultEphemeralWarmPool),
		PersistentTemplate:          envOrDefault("PERSISTENT_TEMPLATE", DefaultPersistentTemplate),
		WorkspaceVolumeName:         envOrDefault("WORKSPACE_VOLUME_NAME", DefaultWorkspaceVolumeName),
		OpenCodePort:                envIntOrDefault("OPENCODE_PORT", DefaultOpenCodePort),
		DefaultExecContainer:        envOrDefault("DEFAULT_EXEC_CONTAINER", DefaultExecContainer),
		DefaultExecShell:            envOrDefault("DEFAULT_EXEC_SHELL", DefaultExecShell),
		ResumeTimeout:               envDurationOrDefault("RESUME_TIMEOUT", DefaultResumeTimeout),
		SSHHostKeyPath:              os.Getenv("SSH_HOST_KEY_PATH"),
		SSHAuthorizedKeysPath:       os.Getenv("SSH_AUTHORIZED_KEYS_PATH"),
		SSHAuthorizedKeys:           os.Getenv("SSH_AUTHORIZED_KEYS"),
		SSHGitHubUsers:              splitCSV(os.Getenv("SSH_GITHUB_USERS")),
		SSHGitLabUsers:              splitCSV(os.Getenv("SSH_GITLAB_USERS")),
		SSHGitLabBaseURL:            envOrDefault("SSH_GITLAB_BASE_URL", DefaultSSHGitLabBaseURL),
		SSHKeysRefreshInterval:      envDurationOrDefault("SSH_KEYS_REFRESH_INTERVAL", DefaultSSHKeysRefreshInterval),
		InsecureAllowAnySSHKey:      envBoolOrDefault("INSECURE_ALLOW_ANY_SSH_KEY", false),
		AllowUnauthenticatedPreview: envBoolOrDefault("ALLOW_UNAUTHENTICATED_PREVIEW", false),
		GitHubAppClientID:           os.Getenv("GITHUB_APP_CLIENT_ID"),
		EgressProxyAdminURL:         envOrDefault("EGRESS_PROXY_ADMIN_URL", DefaultEgressProxyAdminURL),
		Kubeconfig:                  os.Getenv("KUBECONFIG"),
	}
}

// Parse parses command-line flags on top of environment defaults and validates the result.
func Parse(args []string) (Config, error) {
	cfg := Default()
	githubUsers := strings.Join(cfg.SSHGitHubUsers, ",")
	gitlabUsers := strings.Join(cfg.SSHGitLabUsers, ",")

	fs := flag.NewFlagSet("sandbox-control", flag.ContinueOnError)
	fs.StringVar(&cfg.HTTPAddr, "http-addr", cfg.HTTPAddr, "HTTP server listen address.")
	fs.StringVar(&cfg.SSHAddr, "ssh-addr", cfg.SSHAddr, "Embedded SSH server listen address.")
	fs.StringVar(&cfg.Namespace, "namespace", cfg.Namespace, "Target Kubernetes namespace for sandboxes.")
	fs.StringVar(&cfg.RouterURL, "router-url", cfg.RouterURL, "Base URL of sandbox-router service.")
	fs.StringVar(&cfg.SnapshotClassName, "snapshot-class", cfg.SnapshotClassName, "VolumeSnapshotClass name for checkpoints.")
	fs.StringVar(&cfg.EphemeralTemplate, "ephemeral-template", cfg.EphemeralTemplate, "SandboxTemplate name for ephemeral sandboxes.")
	fs.StringVar(&cfg.EphemeralWarmPool, "ephemeral-warmpool", cfg.EphemeralWarmPool, "SandboxWarmPool name for ephemeral sandboxes.")
	fs.StringVar(&cfg.PersistentTemplate, "persistent-template", cfg.PersistentTemplate, "SandboxTemplate name for persistent sandboxes.")
	fs.StringVar(&cfg.WorkspaceVolumeName, "workspace-volume-name", cfg.WorkspaceVolumeName, "PVC template name used for sandbox workspaces.")
	fs.IntVar(&cfg.OpenCodePort, "opencode-port", cfg.OpenCodePort, "Default OpenCode HTTP port inside sandboxes.")
	fs.StringVar(&cfg.DefaultExecContainer, "exec-container", cfg.DefaultExecContainer, "Default container name for pods/exec and logs.")
	fs.StringVar(&cfg.DefaultExecShell, "exec-shell", cfg.DefaultExecShell, "Default shell binary for SSH direct attach.")
	fs.DurationVar(&cfg.ResumeTimeout, "resume-timeout", cfg.ResumeTimeout, "Maximum duration to wait for a resumed sandbox to become Ready.")
	fs.StringVar(&cfg.SSHHostKeyPath, "ssh-host-key-path", cfg.SSHHostKeyPath, "Path to PEM-encoded SSH host private key (generated in memory if empty).")
	fs.StringVar(&cfg.SSHAuthorizedKeysPath, "ssh-authorized-keys-path", cfg.SSHAuthorizedKeysPath, "Path to authorized_keys file.")
	fs.StringVar(&cfg.SSHAuthorizedKeys, "ssh-authorized-keys", cfg.SSHAuthorizedKeys, "Inline authorized_keys content.")
	fs.StringVar(&githubUsers, "ssh-github-users", githubUsers, "Comma-separated GitHub usernames whose public keys are authorized.")
	fs.StringVar(&gitlabUsers, "ssh-gitlab-users", gitlabUsers, "Comma-separated GitLab usernames whose public keys are authorized.")
	fs.StringVar(&cfg.SSHGitLabBaseURL, "ssh-gitlab-base-url", cfg.SSHGitLabBaseURL, "Base URL for GitLab public key lookups.")
	fs.DurationVar(&cfg.SSHKeysRefreshInterval, "ssh-keys-refresh-interval", cfg.SSHKeysRefreshInterval, "Interval for refreshing remote SSH public keys.")
	fs.BoolVar(&cfg.InsecureAllowAnySSHKey, "insecure-allow-any-ssh-key", cfg.InsecureAllowAnySSHKey, "Allow any SSH public key (development only).")
	fs.BoolVar(&cfg.AllowUnauthenticatedPreview, "allow-unauthenticated-preview", cfg.AllowUnauthenticatedPreview, "Allow preview proxy requests without Tailscale-User-Login header (development only).")
	fs.StringVar(&cfg.GitHubAppClientID, "github-app-client-id", cfg.GitHubAppClientID, "GitHub App Client ID for Device Flow authentication (disabled when empty).")
	fs.StringVar(&cfg.EgressProxyAdminURL, "egress-proxy-admin-url", cfg.EgressProxyAdminURL, "Base URL of the egress-proxy internal admin API.")
	fs.StringVar(&cfg.Kubeconfig, "kubeconfig", cfg.Kubeconfig, "Path to kubeconfig file (uses in-cluster config if empty).")

	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	cfg.SSHGitHubUsers = splitCSV(githubUsers)
	cfg.SSHGitLabUsers = splitCSV(gitlabUsers)

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate checks that the configuration is safe and internally consistent.
func (c Config) Validate() error {
	if strings.TrimSpace(c.HTTPAddr) == "" {
		return errors.New("http-addr must not be empty")
	}
	if strings.TrimSpace(c.SSHAddr) == "" {
		return errors.New("ssh-addr must not be empty")
	}
	if !IsValidDNS1123Label(c.Namespace) {
		return fmt.Errorf("invalid namespace %q: must be a valid DNS-1123 label", c.Namespace)
	}
	if err := validateHTTPURL("router-url", c.RouterURL); err != nil {
		return err
	}
	if err := validateHTTPURL("egress-proxy-admin-url", c.EgressProxyAdminURL); err != nil {
		return err
	}
	if strings.TrimSpace(c.SnapshotClassName) == "" {
		return errors.New("snapshot-class must not be empty")
	}
	if c.OpenCodePort < 1 || c.OpenCodePort > 65535 {
		return fmt.Errorf("opencode-port must be between 1 and 65535, got %d", c.OpenCodePort)
	}
	if c.ResumeTimeout <= 0 {
		return errors.New("resume-timeout must be positive")
	}
	if c.SSHKeysRefreshInterval <= 0 {
		return errors.New("ssh-keys-refresh-interval must be positive")
	}
	if err := validateHTTPURL("ssh-gitlab-base-url", c.SSHGitLabBaseURL); err != nil {
		return err
	}
	for _, u := range c.SSHGitHubUsers {
		if !IsValidGitUsername(u) {
			return fmt.Errorf("invalid GitHub username %q", u)
		}
	}
	for _, u := range c.SSHGitLabUsers {
		if !IsValidGitUsername(u) {
			return fmt.Errorf("invalid GitLab username %q", u)
		}
	}
	if !c.InsecureAllowAnySSHKey && !c.HasSSHKeySources() {
		return errors.New("at least one SSH authorized key source (--ssh-github-users, --ssh-gitlab-users, --ssh-authorized-keys-path, or --ssh-authorized-keys) is required unless --insecure-allow-any-ssh-key is set")
	}
	return nil
}

// HasSSHKeySources reports whether any static or remote SSH key source is configured.
func (c Config) HasSSHKeySources() bool {
	return strings.TrimSpace(c.SSHAuthorizedKeysPath) != "" ||
		strings.TrimSpace(c.SSHAuthorizedKeys) != "" ||
		len(c.SSHGitHubUsers) > 0 ||
		len(c.SSHGitLabUsers) > 0
}

// IsValidDNS1123Label reports whether s is a valid Kubernetes DNS-1123 label.
func IsValidDNS1123Label(s string) bool {
	return dns1123LabelRe.MatchString(s)
}

// IsValidGitUsername reports whether u is a safe GitHub/GitLab username segment.
func IsValidGitUsername(u string) bool {
	return gitUsernameRe.MatchString(u) && u != "." && u != ".."
}

func validateHTTPURL(name, raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("invalid %s %q: must be an http or https URL", name, raw)
	}
	return nil
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envIntOrDefault(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func envDurationOrDefault(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}

func envBoolOrDefault(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func splitCSV(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var out []string
	for part := range strings.SplitSeq(raw, ",") {
		p := strings.TrimSpace(part)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
