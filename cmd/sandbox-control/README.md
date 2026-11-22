# sandbox-control

A single-binary Kubernetes control plane, Tailscale-authenticated dev-server preview proxy, and embedded SSH gateway for [`kubernetes-sigs/agent-sandbox`](https://github.com/kubernetes-sigs/agent-sandbox).

## Features

1. **Web UI (`:8080`)**
   - Displays `SandboxWarmPool` readiness, active/suspended `Sandbox` and `SandboxClaim` resources, and `VolumeSnapshot` checkpoints.
   - Creates ephemeral sandboxes via `SandboxClaim` (`opencode-ephemeral-pool`), persistent sandboxes from `SandboxTemplate/opencode-persistent`, or forks a new persistent sandbox from a `VolumeSnapshot` checkpoint.
   - Links to OpenCode Web UI (`/preview/<sandbox>/4096/`) and arbitrary dev-server preview ports (`/preview/<sandbox>/<port>/`), copies `opencode attach` commands, creates/deletes checkpoints, suspends/resumes sandboxes, deletes resources, and views Pod logs.
   - Health probes at `/healthz` and `/readyz`.
2. **Tailscale Preview Reverse Proxy (`:8080`)**
   - Handles `/preview/<sandbox>/<port>/...` (HTTP and WebSocket).
   - Validates `Tailscale-User-Login` and propagates it as `X-Sandbox-User-Email`.
   - Automatically resumes `Suspended` sandboxes and waits until `Ready=True`.
   - Proxies to `sandbox-router` with `X-Sandbox-ID`, `X-Sandbox-Namespace`, and `X-Sandbox-Port` headers.
3. **Embedded SSH Gateway (`:2222`, mapped from `Service/sandbox-control-ssh` port `22 -> targetPort: ssh`)**
   - Authorizes SSH public keys fetched from GitHub (`https://github.com/<user>.keys`), GitLab (`<base>/<user>.keys`), and/or static `authorized_keys`.
   - Management commands: `ssh sandbox-ctl ls | new | checkpoint | suspend | resume | rm | github status | github login | github logout`.
   - Direct shell: `ssh <sandbox-name>@sandbox-ctl` auto-resumes suspended sandboxes and attaches via Kubernetes `pods/exec` with TTY resize support.
4. **GitHub App Device Flow Authentication**
   - Displays `egress-proxy-admin` GitHub auth mode (`user_oauth`, `app_installation`, `unconfigured`) and user token expiry in the Web UI and via `ssh sandbox-ctl github status`.
   - When `--github-app-client-id` (`GITHUB_APP_CLIENT_ID`) is set, enables Device Flow login/logout from both the Web UI and SSH gateway, registering tokens via `egress-proxy-admin` without writing secrets to disk or logs.

## Configuration

| Flag | Environment Variable | Default | Description |
| --- | --- | --- | --- |
| `--http-addr` | `HTTP_ADDR` | `:8080` | HTTP listen address |
| `--ssh-addr` | `SSH_ADDR` | `:2222` | SSH gateway listen address |
| `--namespace` | `SANDBOX_NAMESPACE` | `sandbox` | Target Kubernetes namespace |
| `--router-url` | `SANDBOX_ROUTER_URL` | `http://sandbox-router-svc.agent-sandbox-system.svc.cluster.local:8080` | `sandbox-router` base URL |
| `--snapshot-class` | `SNAPSHOT_CLASS_NAME` | `rook-ceph-rbd-snapshot` | `VolumeSnapshotClass` name |
| `--ssh-github-users` | `SSH_GITHUB_USERS` | `""` | Comma-separated GitHub usernames for SSH public key auth |
| `--ssh-gitlab-users` | `SSH_GITLAB_USERS` | `""` | Comma-separated GitLab usernames for SSH public key auth |
| `--ssh-gitlab-base-url` | `SSH_GITLAB_BASE_URL` | `https://gitlab.com` | GitLab base URL for `.keys` lookup |
| `--ssh-authorized-keys-path` | `SSH_AUTHORIZED_KEYS_PATH` | `""` | Optional path to `authorized_keys` file |
| `--ssh-authorized-keys` | `SSH_AUTHORIZED_KEYS` | `""` | Optional inline `authorized_keys` content |
| `--ssh-keys-refresh-interval` | `SSH_KEYS_REFRESH_INTERVAL` | `15m` | Periodic refresh interval for remote SSH keys |
| `--ssh-host-key-path` | `SSH_HOST_KEY_PATH` | `""` | Optional PEM host key path (in-memory Ed25519 if omitted) |
| `--github-app-client-id` | `GITHUB_APP_CLIENT_ID` | `""` | GitHub App Client ID for Device Flow auth (disabled when empty) |
| `--egress-proxy-admin-url` | `EGRESS_PROXY_ADMIN_URL` | `http://egress-proxy-admin.proxy.svc.cluster.local:3129` | Base URL of the `egress-proxy` internal admin API |

## Container Hardening

The `sandbox-control` Docker target runs as non-root (`USER 1000:1000`) on `gcr.io/distroless/static` and requires no writable filesystem (`readOnlyRootFilesystem: true`, `allowPrivilegeEscalation: false`, `capabilities.drop: ["ALL"]`).
