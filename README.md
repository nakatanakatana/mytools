# mytools

![publish-docker-image](https://github.com/nakatanakatana/mytools/actions/workflows/publish-docker-image.yaml/badge.svg)
![CI](https://github.com/nakatanakatana/mytools/actions/workflows/ci.yaml/badge.svg)
![Coverage](https://github.com/nakatanakatana/octocov-central/blob/main/badges/nakatanakatana/mytools/coverage.svg?raw=true)
![Code to Test Ratio](https://github.com/nakatanakatana/octocov-central/blob/main/badges/nakatanakatana/mytools/ratio.svg?raw=true)
![Test Execution Time](https://github.com/nakatanakatana/octocov-central/blob/main/badges/nakatanakatana/mytools/time.svg?raw=true)
[![Ask DeepWiki](https://deepwiki.com/badge.svg)](https://deepwiki.com/nakatanakatana/mytools)

A monorepo containing various utility tools written in Go.

## Included Tools

### 1. [sarif-to-codequality](file:///home/tanaka/repos/github.com/nakatanakatana/mytools/cmd/sarif-to-codequality/README.md)
A CLI tool that converts SARIF (Static Analysis Results Interchange Format) files into GitLab Code Quality format.
It helps you merge security and analysis results into GitLab's code quality UI within CI pipelines.

### 2. [nip05](file:///home/tanaka/repos/github.com/nakatanakatana/mytools/cmd/nip05/README.md)
A standalone server for managing, generating, and serving `.well-known/nostr.json` files for Nostr's NIP-05 (user identifier and domain verification).

### 3. [wsl-keyring](file:///home/tanaka/repos/github.com/nakatanakatana/mytools/cmd/wsl-keyring/README.md)
A D-Bus Secret Service provider daemon that exposes the `org.freedesktop.secrets` interface and integrates with 1Password (`op.exe`) as the storage backend. Designed for WSL environments.

### 4. [nostr-relay](file:///home/tanaka/repos/github.com/nakatanakatana/mytools/cmd/nostr-relay/README.md)
A minimal Nostr relay built on `fiatjaf.com/nostr/khatru`, supporting NIP-01 relay flow and NIP-11 relay metadata with in-memory storage.

---

## Development and Build

This repository uses [aqua](https://aquaproj.github.io/) to manage development tools (Go, GolangCI-Lint, GoReleaser, etc.).

### Setup Dependencies
```bash
aqua i
```

### Build
Build all tools and output binaries under the `dist/` directory.
```bash
make build
# or
go build -o ./dist/ ./cmd/...
```

### Run Tests
```bash
make test
# or
go test ./...
```

### Run Linter
```bash
make lint
# or
golangci-lint run ./...
```

---

## Docker Integration

You can build Docker images for individual tools or for the entire monorepo.

### Build Tool-Specific Image
```bash
# For sarif-to-codequality only
docker build --target sarif-to-codequality -t sarif-to-codequality .

# For nip05 only
docker build --target nip05 -t nip05 .

# For wsl-keyring only
docker build --target wsl-keyring -t wsl-keyring .

# For nostr-relay only
docker build --target nostr-relay -t nostr-relay .
```

### Build All Tools Image
```bash
docker build --target mytools -t mytools .
```

---

## Herdr Tab Info Plugin

`cmd/herdr-plugin-tabinfo` contains a Herdr plugin that rewrites tab labels with live tab information through the Herdr Socket API.

The label format starts with the Herdr workspace-local tab number and the foreground command. When the tab is active, the plugin can also append directory, Git, and Kubernetes information.

Example:

```text
2 nvim  repo  ⎇ main ✚ 2 … 1 ⎈ production
```

- `2`: Herdr's workspace-local tab number.
- `nvim`: foreground command of the tab's focused pane.
- ` repo`: focused pane's current directory basename.
- ` ⎇ main ✚ 2 … 1`: Git branch plus modified and untracked file counts.
- `⎈ production`: `KUBECONFIG_NAME` from the pane directory's direnv environment.

The plugin replaces user-provided tab labels and does not store or restore original labels.

### Local Development

```bash
go build -o cmd/herdr-plugin-tabinfo/bin/herdr-plugin-tabinfo ./cmd/herdr-plugin-tabinfo
herdr plugin link /home/tanaka/.wt/mytools/herdr-plugin-tabinfo/cmd/herdr-plugin-tabinfo
```

### Event Hooks

The plugin refreshes labels when Herdr emits these events:

- `tab.focused`: active tab changed.
- `tab.created`, `tab.closed`, `tab.moved`, `tab.renamed`: tab list, ordering, or user-visible labels changed.
- `pane.created`, `pane.closed`, `pane.moved`, `pane.exited`: pane count or pane ownership changed.
- `pane.focused`: focused pane changed.
- `pane.agent_detected`, `pane.agent_status_changed`: detected agent or agent status changed.
- `layout.updated`: focused pane or layout snapshot changed.
- `workspace.created`, `workspace.updated`, `workspace.closed`, `workspace.focused`, `workspace.renamed`, `workspace.moved`: workspace-level focus, membership, or metadata changed.

### Configuration

Set `HERDR_TABINFO_MAX_LABEL` to change the maximum generated label length. The default is `80`.

Use `config.yaml` under `HERDR_PLUGIN_CONFIG_DIR` to choose displayed fields. For local development, set `HERDR_TABINFO_CONFIG` to an explicit file path. Fields default to `true` when no config is present.

```yaml
display:
  active:
    tab_number: true
    command: true
    directory: true
    git: true
    kubernetes: true
  inactive:
    tab_number: true
    command: true
    directory: false
    git: false
    kubernetes: false
```

The active and inactive tab settings are independent. Each state must keep either `tab_number` or `command` enabled so tab labels are never empty. Enabling Git or Kubernetes for inactive tabs runs the corresponding lookup for every tab.

Git information is read from the focused pane's current directory with `github.com/arl/gitstatus`. If the directory is outside a Git working tree or Git does not answer within 700ms, only the Git item is omitted.

Kubernetes information uses `direnv exec <pane-directory> printenv KUBECONFIG_NAME` when `direnv` is available. Without `direnv`, it uses the plugin process's `KUBECONFIG_NAME`. An empty value or a failed command omits the Kubernetes item.

The Git display reads the existing gitmux configuration format. Set `HERDR_TABINFO_GITMUX_CONFIG` to an explicit configuration path. Otherwise, the plugin uses `~/.gitmux.conf` when it exists, or gitmux's default symbols and layout when it does not.

```yaml
tmux:
  symbols:
    branch: '⎇ '
    modified: '✚ '
    untracked: '… '
  layout: [branch, ' - ', flags]
  options:
    branch_max_len: 24
    branch_trim: right
    hide_clean: false
```

The plugin supports gitmux's `symbols`, `layout`, and `options` (`branch_max_len`, `branch_trim`, `ellipsis`, `hide_clean`, `swap_divergence`, `divergence_space`, and `flags_without_count`). It deliberately ignores `tmux.styles`, so tmux color and attribute codes are never included in tab labels.
