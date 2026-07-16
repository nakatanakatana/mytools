# Herdr Tab Info Plugin

`cmd/herdr-plugin-tabinfo` contains a Herdr plugin that rewrites tab labels with live tab information through the Herdr Socket API.

Configure each tab label from its workspace-local tab number, foreground process, directory, Git information, and environment-variable values. The `items` list selects and orders the displayed values independently for active and inactive tabs.

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

Use `config.yaml` under `HERDR_PLUGIN_CONFIG_DIR` to choose displayed fields. When it is not present, the plugin also reads `$XDG_CONFIG_HOME/herdr-plugin-tabinfo/config.yaml` (or `~/.config/herdr-plugin-tabinfo/config.yaml` when `XDG_CONFIG_HOME` is unset). For local development, set `HERDR_TABINFO_CONFIG` to an explicit file path.

```yaml
display:
  active:
    items:
      - tab_number
      - process
      - directory
      - git
      - environment
    separator: " "
    environment:
      - icon: '⎈'
        variable: KUBECONFIG_NAME
  inactive:
    items:
      - tab_number
      - process
    separator: " "
    environment: []
```

The active and inactive tab settings are independent, and each requires an `items` list. Valid values are `tab_number`, `process`, `process_full`, `directory`, `git`, and `environment`; the list controls inclusion and order. `separator` defaults to one space and can be empty. Omitted dynamic values do not leave an extra separator. `environment` expands its entries in YAML order; use `environment: []` to disable them. The retired boolean fields are not supported. Both `process` and `process_full` select the first non-shell member of the shell-to-descendant foreground-process chain, which is the direct child of `$SHELL`; `process` shows only its name, while `process_full` includes its arguments. Including Git or environment variables for inactive tabs runs the corresponding lookup for every tab.

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
