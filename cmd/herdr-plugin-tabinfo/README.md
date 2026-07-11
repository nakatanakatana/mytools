# Herdr Tab Info Plugin

`cmd/herdr-plugin-tabinfo` contains a Herdr plugin that rewrites tab labels with live tab information through the Herdr Socket API.

The label format starts with the Herdr workspace-local tab number and the foreground process name. When the tab is active, the plugin can also append directory, Git, and an environment-variable value.

Example:

```text
2 nvim  repo  ⎇ main ✚ 2 … 1 ⎈ production
```

- `2`: Herdr's workspace-local tab number.
- `nvim`: foreground process name of the tab's focused pane.
- ` repo`: focused pane's current directory basename.
- ` ⎇ main ✚ 2 … 1`: Git branch plus modified and untracked file counts.
- `⎈ production`: a configured environment variable from the pane directory's direnv environment.

The plugin replaces user-provided tab labels and does not store or restore original labels.

## Install

```bash
herdr plugin install nakatanakatana/mytools/cmd/herdr-plugin-tabinfo
```

Use `herdr plugin config-dir nakatanakatana.tabinfo` to locate the plugin config directory after installation.

## Local Development

```bash
go build -o cmd/herdr-plugin-tabinfo/bin/herdr-plugin-tabinfo ./cmd/herdr-plugin-tabinfo
herdr plugin link /home/tanaka/.wt/mytools/herdr-plugin-tabinfo/cmd/herdr-plugin-tabinfo
```

## Event Hooks

The plugin refreshes labels when Herdr emits these events:

- `tab.focused`: active tab changed.
- `tab.created`, `tab.closed`, `tab.moved`, `tab.renamed`: tab list, ordering, or user-visible labels changed.
- `pane.created`, `pane.closed`, `pane.moved`, `pane.exited`: pane count or pane ownership changed.
- `pane.focused`: focused pane changed.
- `pane.agent_detected`, `pane.agent_status_changed`: detected agent or agent status changed.
- `layout.updated`: focused pane or layout snapshot changed.
- `workspace.created`, `workspace.updated`, `workspace.closed`, `workspace.focused`, `workspace.renamed`, `workspace.moved`: workspace-level focus, membership, or metadata changed.

## Configuration

Set `HERDR_TABINFO_MAX_LABEL` to change the maximum generated label length. The default is `80`.

Use `config.yaml` under `HERDR_PLUGIN_CONFIG_DIR` to choose displayed fields. When it is not present, the plugin also reads `$XDG_CONFIG_HOME/herdr-plugin-tabinfo/config.yaml` (or `~/.config/herdr-plugin-tabinfo/config.yaml` when `XDG_CONFIG_HOME` is unset). For local development, set `HERDR_TABINFO_CONFIG` to an explicit file path. Fields default to `true` when no config is present.

```yaml
display:
  active:
    tab_number: true
    process: true
    process_full: false
    directory: true
    git: true
    environment:
      - icon: '⎈'
        variable: KUBECONFIG_NAME
  inactive:
    tab_number: true
    process: true
    process_full: false
    directory: false
    git: false
    environment: []
```

The active and inactive tab settings are independent. Each state must keep either `tab_number`, `process`, or `process_full` enabled so tab labels are never empty. `process` shows only the process name and skips it when it matches `$SHELL`. `process_full` shows the process with arguments. `environment` accepts multiple entries, each with an `icon` and `variable`, and displays them in that order. Use `environment: []` to disable them. Enabling Git or environment variables for inactive tabs runs the corresponding lookup for every tab.

Git information is read from the focused pane's current directory with `github.com/arl/gitstatus`. If the directory is outside a Git working tree or Git does not answer within 700ms, only the Git item is omitted.

Environment-variable values use `direnv exec <pane-directory> printenv <variable>` when `direnv` is available. Without `direnv`, they use the plugin process's value. An empty value or a failed command omits the item.

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
