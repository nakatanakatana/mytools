package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParse_DefaultsWithGitHubUser(t *testing.T) {
	cfg, err := Parse([]string{"--ssh-github-users=nakatanakatana"})
	require.NoError(t, err)

	assert.Equal(t, DefaultHTTPAddr, cfg.HTTPAddr)
	assert.Equal(t, DefaultSSHAddr, cfg.SSHAddr)
	assert.Equal(t, DefaultNamespace, cfg.Namespace)
	assert.Equal(t, DefaultRouterURL, cfg.RouterURL)
	assert.Equal(t, DefaultSnapshotClassName, cfg.SnapshotClassName)
	assert.Equal(t, DefaultOpenCodePort, cfg.OpenCodePort)
	assert.Equal(t, DefaultResumeTimeout, cfg.ResumeTimeout)
	assert.Equal(t, []string{"nakatanakatana"}, cfg.SSHGitHubUsers)
	assert.Empty(t, cfg.SSHGitLabUsers)
	assert.False(t, cfg.InsecureAllowAnySSHKey)
}

func TestParse_EnvOverrides(t *testing.T) {
	t.Setenv("HTTP_ADDR", ":9090")
	t.Setenv("SSH_ADDR", ":2200")
	t.Setenv("SANDBOX_NAMESPACE", "dev-sandbox")
	t.Setenv("SANDBOX_ROUTER_URL", "http://sandbox-router.agent-sandbox-system.svc:8080")
	t.Setenv("SSH_GITHUB_USERS", "alice, bob")
	t.Setenv("SSH_GITLAB_USERS", "charlie")
	t.Setenv("RESUME_TIMEOUT", "45s")

	cfg, err := Parse(nil)
	require.NoError(t, err)

	assert.Equal(t, ":9090", cfg.HTTPAddr)
	assert.Equal(t, ":2200", cfg.SSHAddr)
	assert.Equal(t, "dev-sandbox", cfg.Namespace)
	assert.Equal(t, "http://sandbox-router.agent-sandbox-system.svc:8080", cfg.RouterURL)
	assert.Equal(t, []string{"alice", "bob"}, cfg.SSHGitHubUsers)
	assert.Equal(t, []string{"charlie"}, cfg.SSHGitLabUsers)
	assert.Equal(t, 45*time.Second, cfg.ResumeTimeout)
}

func TestParse_ValidationErrors(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{
			name: "missing ssh key source",
			args: nil,
		},
		{
			name: "invalid namespace",
			args: []string{"--namespace=INVALID_NS", "--insecure-allow-any-ssh-key"},
		},
		{
			name: "invalid router url",
			args: []string{"--router-url=ftp://bad", "--insecure-allow-any-ssh-key"},
		},
		{
			name: "invalid github username",
			args: []string{"--ssh-github-users=alice/../bob"},
		},
		{
			name: "invalid gitlab username dotdot",
			args: []string{"--ssh-gitlab-users=.."},
		},
		{
			name: "invalid opencode port",
			args: []string{"--opencode-port=70000", "--insecure-allow-any-ssh-key"},
		},
		{
			name: "non-positive resume timeout",
			args: []string{"--resume-timeout=0s", "--insecure-allow-any-ssh-key"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(tc.args)
			require.Error(t, err)
		})
	}
}
