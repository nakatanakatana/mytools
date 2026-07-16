package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/arl/gitstatus"
	"gopkg.in/yaml.v3"
)

const (
	source          = "herdr-tabinfo"
	defaultMaxLabel = 80
	iconDirectory   = ""
	iconGit         = ""
	gitTimeout      = 700 * time.Millisecond
)

var workingDirMu sync.Mutex

// gitmuxConfig intentionally omits tmux.styles. Tab labels must remain plain text.
type gitmuxConfig struct {
	Tmux gitmuxTmuxConfig `yaml:"tmux"`
}

type gitmuxTmuxConfig struct {
	Symbols gitmuxSymbols `yaml:"symbols"`
	Layout  []string      `yaml:"layout"`
	Options gitmuxOptions `yaml:"options"`
}

type gitmuxSymbols struct {
	Branch     string `yaml:"branch"`
	HashPrefix string `yaml:"hashprefix"`
	Ahead      string `yaml:"ahead"`
	Behind     string `yaml:"behind"`
	Staged     string `yaml:"staged"`
	Conflict   string `yaml:"conflict"`
	Modified   string `yaml:"modified"`
	Untracked  string `yaml:"untracked"`
	Stashed    string `yaml:"stashed"`
	Clean      string `yaml:"clean"`
	Insertions string `yaml:"insertions"`
	Deletions  string `yaml:"deletions"`
}

type gitmuxOptions struct {
	BranchMaxLen      int    `yaml:"branch_max_len"`
	BranchTrim        string `yaml:"branch_trim"`
	Ellipsis          string `yaml:"ellipsis"`
	HideClean         bool   `yaml:"hide_clean"`
	DivergenceSpace   bool   `yaml:"divergence_space"`
	SwapDivergence    bool   `yaml:"swap_divergence"`
	FlagsWithoutCount bool   `yaml:"flags_without_count"`
}

type tabInfoConfig struct {
	Display displayConfig `yaml:"display"`
}

type displayConfig struct {
	Active   tabDisplayConfig `yaml:"active"`
	Inactive tabDisplayConfig `yaml:"inactive"`
}

type displayItem string

const (
	displayItemTabNumber   displayItem = "tab_number"
	displayItemProcess     displayItem = "process"
	displayItemProcessFull displayItem = "process_full"
	displayItemDirectory   displayItem = "directory"
	displayItemGit         displayItem = "git"
	displayItemEnvironment displayItem = "environment"
)

type tabDisplayConfig struct {
	Items       []displayItem              `yaml:"items"`
	Separator   string                     `yaml:"separator"`
	Environment []environmentDisplayConfig `yaml:"environment"`
}

type rawTabDisplayConfig struct {
	Items       []displayItem              `yaml:"items"`
	Separator   *string                    `yaml:"separator"`
	Environment []environmentDisplayConfig `yaml:"environment"`
}

type rawTabInfoConfig struct {
	Display struct {
		Active   rawTabDisplayConfig `yaml:"active"`
		Inactive rawTabDisplayConfig `yaml:"inactive"`
	} `yaml:"display"`
}

func (config rawTabDisplayConfig) resolved() tabDisplayConfig {
	separator := " "
	if config.Separator != nil {
		separator = *config.Separator
	}
	return tabDisplayConfig{Items: config.Items, Separator: separator, Environment: config.Environment}
}

func (config tabDisplayConfig) hasItem(item displayItem) bool {
	for _, configured := range config.Items {
		if configured == item {
			return true
		}
	}
	return false
}

func validateTabDisplayConfig(name string, config tabDisplayConfig) error {
	if len(config.Items) == 0 {
		return fmt.Errorf("tabinfo config display.%s.items must not be empty", name)
	}
	valid := map[displayItem]bool{
		displayItemTabNumber: true, displayItemProcess: true, displayItemProcessFull: true,
		displayItemDirectory: true, displayItemGit: true, displayItemEnvironment: true,
	}
	seen := make(map[displayItem]bool, len(config.Items))
	for _, item := range config.Items {
		if !valid[item] {
			return fmt.Errorf("tabinfo config display.%s.items contains unknown item %q", name, item)
		}
		if seen[item] {
			return fmt.Errorf("tabinfo config display.%s.items contains duplicate item %q", name, item)
		}
		seen[item] = true
	}
	return nil
}

type environmentDisplayConfig struct {
	Icon     string `yaml:"icon"`
	Variable string `yaml:"variable"`
}

type apiRequest struct {
	ID     string      `json:"id"`
	Method string      `json:"method"`
	Params interface{} `json:"params"`
}

type apiResponse struct {
	ID     string          `json:"id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *apiError       `json:"error,omitempty"`
}

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type sessionSnapshotResult struct {
	Type     string          `json:"type"`
	Snapshot sessionSnapshot `json:"snapshot"`
}

type sessionSnapshot struct {
	Tabs    []tabInfo        `json:"tabs"`
	Panes   []paneInfo       `json:"panes"`
	Layouts []paneLayout     `json:"layouts"`
	Extra   json.RawMessage  `json:"-"`
	Raw     *json.RawMessage `json:"-"`
	Unknown map[string]any   `json:"-"`
}

type tabInfo struct {
	TabID       string `json:"tab_id"`
	WorkspaceID string `json:"workspace_id"`
	Number      int    `json:"number"`
	Label       string `json:"label"`
	Focused     bool   `json:"focused"`
	PaneCount   int    `json:"pane_count"`
	AgentStatus string `json:"agent_status"`
}

type paneInfo struct {
	PaneID        string  `json:"pane_id"`
	WorkspaceID   string  `json:"workspace_id"`
	TabID         string  `json:"tab_id"`
	Focused       bool    `json:"focused"`
	AgentStatus   string  `json:"agent_status"`
	CWD           *string `json:"cwd"`
	ForegroundCWD *string `json:"foreground_cwd"`
	Agent         *string `json:"agent"`
	DisplayAgent  *string `json:"display_agent"`
}

type paneLayout struct {
	WorkspaceID   string `json:"workspace_id"`
	TabID         string `json:"tab_id"`
	FocusedPaneID string `json:"focused_pane_id"`
}

type paneProcessInfoResult struct {
	Type        string          `json:"type"`
	ProcessInfo paneProcessInfo `json:"process_info"`
}

type paneProcessInfo struct {
	PaneID              string              `json:"pane_id"`
	ForegroundProcesses []foregroundProcess `json:"foreground_processes"`
}

type foregroundProcess struct {
	Name    string   `json:"name"`
	Argv    []string `json:"argv"`
	Argv0   *string  `json:"argv0"`
	Cmdline *string  `json:"cmdline"`
}

type herdrClient struct {
	socketPath string
	nextID     int
}

type tabRename struct {
	TabID string
	Label string
}

type tabDynamicInfo struct {
	Process     string
	ProcessFull string
	Git         string
	Environment map[string]string
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "[%s] %v\n", source, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	socketPath := os.Getenv("HERDR_SOCKET_PATH")
	if socketPath == "" {
		return errors.New("HERDR_SOCKET_PATH is not set. Run this from Herdr as a plugin action or event")
	}
	shellProcessName := shellProcessName(os.Getenv("SHELL"))

	gitmux, err := loadGitmuxConfig()
	if err != nil {
		return err
	}
	config, err := loadTabInfoConfig()
	if err != nil {
		return err
	}

	client, err := dialHerdr(socketPath)
	if err != nil {
		return err
	}

	var result sessionSnapshotResult
	if err := client.request("session.snapshot", map[string]any{}, &result); err != nil {
		return err
	}

	updated, err := refreshLabels(client, result.Snapshot, gitmux, config, shellProcessName)
	if err != nil {
		return err
	}

	if len(args) > 0 && args[0] == "refresh" {
		suffix := "s"
		if updated == 1 {
			suffix = ""
		}
		fmt.Printf("updated %d tab label%s\n", updated, suffix)
	}

	return nil
}

func dialHerdr(socketPath string) (*herdrClient, error) {
	return &herdrClient{
		socketPath: socketPath,
	}, nil
}

func (c *herdrClient) request(method string, params interface{}, result interface{}) error {
	c.nextID++
	id := fmt.Sprintf("%s-%d-%d", source, os.Getpid(), c.nextID)
	request := apiRequest{
		ID:     id,
		Method: method,
		Params: params,
	}

	conn, err := net.Dial("unix", c.socketPath)
	if err != nil {
		return fmt.Errorf("connect to Herdr socket: %w", err)
	}
	defer func() {
		_ = conn.Close()
	}()

	if err := json.NewEncoder(conn).Encode(request); err != nil {
		return fmt.Errorf("%s request failed: %w", method, err)
	}

	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return fmt.Errorf("%s response failed: %w", method, err)
	}

	var response apiResponse
	if err := json.Unmarshal(line, &response); err != nil {
		return fmt.Errorf("%s response is invalid JSON: %w", method, err)
	}
	if response.ID != id {
		return fmt.Errorf("%s response id mismatch: got %q, want %q", method, response.ID, id)
	}
	if response.Error != nil {
		return fmt.Errorf("%s: %s", response.Error.Code, response.Error.Message)
	}
	if result == nil {
		return nil
	}
	if err := json.Unmarshal(response.Result, result); err != nil {
		return fmt.Errorf("%s result decode failed: %w", method, err)
	}
	return nil
}

func refreshLabels(client *herdrClient, snapshot sessionSnapshot, gitmux gitmuxConfig, config tabInfoConfig, shellProcessName string) (int, error) {
	renames := planLabelUpdatesWithInfo(snapshot, config.Display, func(tab tabInfo, pane *paneInfo) tabDynamicInfo {
		cwd := paneCWD(pane)
		info := tabDynamicInfo{}
		display := config.Display.forTab(tab.Focused)
		if display.hasItem(displayItemProcess) || display.hasItem(displayItemProcessFull) {
			processInfo, ok := fetchPaneProcessInfo(client, pane)
			if ok {
				if display.hasItem(displayItemProcess) {
					info.Process = processNameFromProcessInfo(processInfo, shellProcessName)
				}
				if display.hasItem(displayItemProcessFull) {
					info.ProcessFull = processFullFromProcessInfo(processInfo)
				}
			}
		}
		if display.hasItem(displayItemGit) {
			info.Git = displayGitStatus(cwd, gitmux)
		}
		if display.hasItem(displayItemEnvironment) && len(display.Environment) > 0 {
			info.Environment = make(map[string]string, len(display.Environment))
			for _, environment := range display.Environment {
				if environment.Variable != "" {
					info.Environment[environment.Variable] = displayEnvironmentVariable(cwd, environment.Variable)
				}
			}
		}
		return info
	})
	for i, rename := range renames {
		var renameResult json.RawMessage
		if err := client.request("tab.rename", map[string]string{
			"tab_id": rename.TabID,
			"label":  rename.Label,
		}, &renameResult); err != nil {
			return i, err
		}
	}
	return len(renames), nil
}

func planLabelUpdates(snapshot sessionSnapshot) []tabRename {
	return planLabelUpdatesWithInfo(snapshot, defaultTabInfoConfig().Display, nil)
}

func planLabelUpdatesWithInfo(snapshot sessionSnapshot, display displayConfig, infoForPane func(tabInfo, *paneInfo) tabDynamicInfo) []tabRename {
	panesByTab := map[string][]paneInfo{}
	for _, pane := range snapshot.Panes {
		panesByTab[pane.TabID] = append(panesByTab[pane.TabID], pane)
	}
	layoutsByTab := map[string]paneLayout{}
	for _, layout := range snapshot.Layouts {
		layoutsByTab[layout.TabID] = layout
	}

	var renames []tabRename

	for _, tab := range prioritizedTabs(snapshot.Tabs) {
		displayForTab := display.forTab(tab.Focused)
		focusedPane := findFocusedPane(panesByTab[tab.TabID], layoutsByTab[tab.TabID])
		info := tabDynamicInfo{}
		if infoForPane != nil {
			info = infoForPane(tab, focusedPane)
		}
		label := buildTabLabel(tab, panesByTab[tab.TabID], layoutsByTab[tab.TabID], info, displayForTab)

		if tab.Label == label {
			continue
		}
		renames = append(renames, tabRename{
			TabID: tab.TabID,
			Label: label,
		})
	}

	return renames
}

// prioritizedTabs keeps the snapshot order within each priority group.
func prioritizedTabs(tabs []tabInfo) []tabInfo {
	var active *tabInfo
	for i := range tabs {
		if tabs[i].Focused {
			active = &tabs[i]
			break
		}
	}
	if active == nil {
		return tabs
	}

	ordered := make([]tabInfo, 0, len(tabs))
	ordered = append(ordered, *active)
	for _, tab := range tabs {
		if tab.TabID != active.TabID && tab.WorkspaceID == active.WorkspaceID {
			ordered = append(ordered, tab)
		}
	}
	for _, tab := range tabs {
		if tab.TabID != active.TabID && tab.WorkspaceID != active.WorkspaceID {
			ordered = append(ordered, tab)
		}
	}
	return ordered
}

func buildTabLabel(tab tabInfo, panes []paneInfo, layout paneLayout, info tabDynamicInfo, display tabDisplayConfig) string {
	focusedPane := findFocusedPane(panes, layout)
	parts := []string{}
	if display.hasItem(displayItemTabNumber) {
		parts = append(parts, strconv.Itoa(tab.Number))
	}
	if display.hasItem(displayItemProcess) && info.Process != "" {
		parts = append(parts, info.Process)
	}
	if display.hasItem(displayItemProcessFull) && info.ProcessFull != "" {
		parts = append(parts, info.ProcessFull)
	}
	if display.hasItem(displayItemDirectory) {
		if cwd := displayCWD(focusedPane); cwd != "" {
			parts = append(parts, iconDirectory+" "+cwd)
		}
	}
	if display.hasItem(displayItemGit) && info.Git != "" {
		parts = append(parts, iconGit+" "+info.Git)
	}
	if display.hasItem(displayItemEnvironment) {
		for _, environment := range display.Environment {
			if value := info.Environment[environment.Variable]; environment.Variable != "" && value != "" {
				parts = append(parts, formatEnvironmentValue(environment.Icon, value))
			}
		}
	}
	return normalizeLabel(strings.Join(parts, " "))
}

func fetchPaneProcessInfo(client *herdrClient, pane *paneInfo) (paneProcessInfoResult, bool) {
	if pane == nil {
		return paneProcessInfoResult{}, false
	}

	var result paneProcessInfoResult
	if err := client.request("pane.process_info", map[string]string{"pane_id": pane.PaneID}, &result); err != nil {
		return paneProcessInfoResult{}, false
	}
	return result, true
}

func processNameFromProcessInfo(result paneProcessInfoResult, shellProcessName string) string {
	for index := len(result.ProcessInfo.ForegroundProcesses) - 1; index >= 0; index-- {
		process := result.ProcessInfo.ForegroundProcesses[index]
		if process.Name != "" && process.Name != shellProcessName {
			return process.Name
		}
	}
	return ""
}

func shellProcessName(shell string) string {
	if shell == "" {
		return ""
	}
	return filepath.Base(shell)
}

func processFullFromProcessInfo(result paneProcessInfoResult) string {
	for index := len(result.ProcessInfo.ForegroundProcesses) - 1; index >= 0; index-- {
		process := result.ProcessInfo.ForegroundProcesses[index]
		if process.Cmdline != nil && *process.Cmdline != "" {
			return *process.Cmdline
		}
		if len(process.Argv) > 0 {
			return strings.Join(process.Argv, " ")
		}
		if process.Argv0 != nil && *process.Argv0 != "" {
			return *process.Argv0
		}
		if process.Name != "" {
			return process.Name
		}
	}
	return ""
}

func loadGitmuxConfig() (gitmuxConfig, error) {
	path := os.Getenv("HERDR_TABINFO_GITMUX_CONFIG")
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return gitmuxConfig{}, err
		}
		path = filepath.Join(home, ".gitmux.conf")
	}

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return defaultGitmuxConfig(), nil
	}
	if err != nil {
		return gitmuxConfig{}, fmt.Errorf("read gitmux config %q: %w", path, err)
	}
	return parseGitmuxConfig(data)
}

func parseGitmuxConfig(data []byte) (gitmuxConfig, error) {
	config := defaultGitmuxConfig()
	if err := yaml.Unmarshal(data, &config); err != nil {
		return gitmuxConfig{}, fmt.Errorf("parse gitmux config: %w", err)
	}
	if len(config.Tmux.Layout) == 0 {
		config.Tmux.Layout = defaultGitmuxConfig().Tmux.Layout
	}
	return config, nil
}

func defaultGitmuxConfig() gitmuxConfig {
	return gitmuxConfig{Tmux: gitmuxTmuxConfig{
		Symbols: gitmuxSymbols{
			Branch:     "⎇ ",
			HashPrefix: ":",
			Ahead:      "↑·",
			Behind:     "↓·",
			Staged:     "● ",
			Conflict:   "✖ ",
			Modified:   "✚ ",
			Untracked:  "… ",
			Stashed:    "⚑ ",
			Clean:      "✔",
			Insertions: "Σ",
			Deletions:  "Δ",
		},
		Layout: []string{"branch", "remote-branch", "divergence", " - ", "flags"},
		Options: gitmuxOptions{
			BranchTrim: "right",
			Ellipsis:   "…",
		},
	}}
}

func loadTabInfoConfig() (tabInfoConfig, error) {
	paths, err := tabInfoConfigPaths()
	if err != nil {
		return tabInfoConfig{}, err
	}

	for _, path := range paths {
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return tabInfoConfig{}, fmt.Errorf("read tabinfo config %q: %w", path, err)
		}
		return parseTabInfoConfig(data)
	}

	return defaultTabInfoConfig(), nil
}

func tabInfoConfigPaths() ([]string, error) {
	if path := os.Getenv("HERDR_TABINFO_CONFIG"); path != "" {
		return []string{path}, nil
	}

	paths := make([]string, 0, 2)
	if configDir := os.Getenv("HERDR_PLUGIN_CONFIG_DIR"); configDir != "" {
		paths = append(paths, filepath.Join(configDir, "config.yaml"))
	}

	configHome := os.Getenv("XDG_CONFIG_HOME")
	if configHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("find home directory for tabinfo config: %w", err)
		}
		configHome = filepath.Join(home, ".config")
	}
	return append(paths, filepath.Join(configHome, "herdr-plugin-tabinfo", "config.yaml")), nil
}

func parseTabInfoConfig(data []byte) (tabInfoConfig, error) {
	var raw rawTabInfoConfig
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return tabInfoConfig{}, fmt.Errorf("parse tabinfo config: %w", err)
	}
	config := tabInfoConfig{Display: displayConfig{
		Active:   raw.Display.Active.resolved(),
		Inactive: raw.Display.Inactive.resolved(),
	}}
	if err := validateTabDisplayConfig("active", config.Display.Active); err != nil {
		return tabInfoConfig{}, err
	}
	if err := validateTabDisplayConfig("inactive", config.Display.Inactive); err != nil {
		return tabInfoConfig{}, err
	}
	return config, nil
}

func defaultTabInfoConfig() tabInfoConfig {
	return tabInfoConfig{Display: displayConfig{
		Active: tabDisplayConfig{
			Items:     []displayItem{displayItemTabNumber, displayItemProcess, displayItemDirectory, displayItemGit, displayItemEnvironment},
			Separator: " ",
			Environment: []environmentDisplayConfig{{
				Icon:     "⎈",
				Variable: "KUBECONFIG_NAME",
			}},
		},
		Inactive: tabDisplayConfig{
			Items:     []displayItem{displayItemTabNumber, displayItemProcess},
			Separator: " ",
		},
	}}
}

func (config displayConfig) forTab(active bool) tabDisplayConfig {
	if active {
		return config.Active
	}
	return config.Inactive
}

func displayGitStatus(cwd string, config gitmuxConfig) string {
	if cwd == "" {
		return ""
	}

	status, err := readGitStatus(cwd)
	if err != nil {
		return ""
	}
	return formatGitStatus(status, config.Tmux)
}

func displayEnvironmentVariable(cwd, variable string) string {
	if variable == "" {
		return ""
	}
	direnvPath, err := exec.LookPath("direnv")
	if err != nil {
		direnvPath = ""
	}
	return readEnvironmentVariable(cwd, direnvPath, variable, os.Getenv(variable))
}

func readEnvironmentVariable(cwd, direnvPath, variable, inherited string) string {
	if direnvPath == "" {
		return inherited
	}
	if cwd == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, direnvPath, "exec", cwd, "printenv", variable)
	command.Dir = cwd
	output, err := command.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

func formatEnvironmentValue(icon, value string) string {
	if icon == "" {
		return value
	}
	return icon + " " + value
}

func readGitStatus(dir string) (*gitstatus.Status, error) {
	workingDirMu.Lock()
	defer workingDirMu.Unlock()

	previousDir, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	if err := os.Chdir(dir); err != nil {
		return nil, err
	}
	defer func() {
		_ = os.Chdir(previousDir)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	return gitstatus.NewWithContext(ctx)
}

func formatGitStatus(status *gitstatus.Status, config gitmuxTmuxConfig) string {
	var pending []string
	var output strings.Builder

	flush := func() {
		items := pending[:0]
		for _, item := range pending {
			if item != "" {
				items = append(items, item)
			}
		}
		output.WriteString(strings.Join(items, " "))
		pending = pending[:0]
	}

	for _, item := range config.Layout {
		switch item {
		case "branch":
			pending = append(pending, formatGitBranch(status, config))
		case "remote":
			pending = append(pending, truncateGitBranch(status.RemoteBranch, config.Options), formatGitDivergence(status, config))
		case "remote-branch":
			pending = append(pending, truncateGitBranch(status.RemoteBranch, config.Options))
		case "divergence":
			pending = append(pending, formatGitDivergence(status, config))
		case "flags":
			pending = append(pending, formatGitFlags(status, config))
		case "stats":
			pending = append(pending, formatGitStats(status, config))
		default:
			flush()
			output.WriteString(item)
		}
	}
	flush()
	return strings.TrimSpace(output.String())
}

func formatGitBranch(status *gitstatus.Status, config gitmuxTmuxConfig) string {
	state := ""
	switch status.State {
	case gitstatus.Rebasing:
		state = "[rebase] "
	case gitstatus.AM:
		state = "[am] "
	case gitstatus.AMRebase:
		state = "[am-rebase] "
	case gitstatus.Merging:
		state = "[merge] "
	case gitstatus.CherryPicking:
		state = "[cherry-pick] "
	case gitstatus.Reverting:
		state = "[revert] "
	case gitstatus.Bisecting:
		state = "[bisect] "
	}

	if status.IsDetached {
		return state + config.Symbols.HashPrefix + status.HEAD
	}
	branch := truncateGitBranch(status.LocalBranch, config.Options)
	if state != "" {
		return state + branch
	}
	return config.Symbols.Branch + branch
}

func formatGitDivergence(status *gitstatus.Status, config gitmuxTmuxConfig) string {
	var values []string
	if !config.Options.SwapDivergence {
		if status.BehindCount > 0 {
			values = append(values, config.Symbols.Behind+strconv.Itoa(status.BehindCount))
		}
		if status.AheadCount > 0 {
			values = append(values, config.Symbols.Ahead+strconv.Itoa(status.AheadCount))
		}
	} else {
		if status.AheadCount > 0 {
			values = append(values, config.Symbols.Ahead+strconv.Itoa(status.AheadCount))
		}
		if status.BehindCount > 0 {
			values = append(values, config.Symbols.Behind+strconv.Itoa(status.BehindCount))
		}
	}
	separator := ""
	if config.Options.DivergenceSpace {
		separator = " "
	}
	return strings.Join(values, separator)
}

func formatGitFlags(status *gitstatus.Status, config gitmuxTmuxConfig) string {
	format := func(symbol string, count int) string {
		if config.Options.FlagsWithoutCount {
			return symbol
		}
		return symbol + strconv.Itoa(count)
	}

	var flags []string
	if status.IsClean {
		if status.NumStashed > 0 {
			flags = append(flags, format(config.Symbols.Stashed, status.NumStashed))
		}
		if !config.Options.HideClean {
			flags = append(flags, config.Symbols.Clean)
		}
		return strings.Join(flags, " ")
	}
	if status.NumStaged > 0 {
		flags = append(flags, format(config.Symbols.Staged, status.NumStaged))
	}
	if status.NumConflicts > 0 {
		flags = append(flags, format(config.Symbols.Conflict, status.NumConflicts))
	}
	if status.NumModified > 0 {
		flags = append(flags, format(config.Symbols.Modified, status.NumModified))
	}
	if status.NumStashed > 0 {
		flags = append(flags, format(config.Symbols.Stashed, status.NumStashed))
	}
	if status.NumUntracked > 0 {
		flags = append(flags, format(config.Symbols.Untracked, status.NumUntracked))
	}
	return strings.Join(flags, " ")
}

func formatGitStats(status *gitstatus.Status, config gitmuxTmuxConfig) string {
	var stats []string
	if status.Insertions > 0 {
		stats = append(stats, config.Symbols.Insertions+strconv.Itoa(status.Insertions))
	}
	if status.Deletions > 0 {
		stats = append(stats, config.Symbols.Deletions+strconv.Itoa(status.Deletions))
	}
	return strings.Join(stats, " ")
}

func truncateGitBranch(branch string, options gitmuxOptions) string {
	if options.BranchMaxLen <= 0 || len([]rune(branch)) <= options.BranchMaxLen {
		return branch
	}
	runes := []rune(branch)
	ellipsis := []rune(options.Ellipsis)
	if len(ellipsis) > options.BranchMaxLen {
		ellipsis = nil
	}
	remaining := options.BranchMaxLen - len(ellipsis)
	switch options.BranchTrim {
	case "left":
		return string(ellipsis) + string(runes[len(runes)-remaining:])
	case "center":
		left := remaining / 2
		right := remaining - left
		return string(runes[:left]) + string(ellipsis) + string(runes[len(runes)-right:])
	default:
		return string(runes[:remaining]) + string(ellipsis)
	}
}

func findFocusedPane(panes []paneInfo, layout paneLayout) *paneInfo {
	if layout.FocusedPaneID != "" {
		for i := range panes {
			if panes[i].PaneID == layout.FocusedPaneID {
				return &panes[i]
			}
		}
	}
	for i := range panes {
		if panes[i].Focused {
			return &panes[i]
		}
	}
	if len(panes) > 0 {
		return &panes[0]
	}
	return nil
}

func displayCWD(pane *paneInfo) string {
	if pane == nil {
		return ""
	}
	cwd := paneCWD(pane)
	if cwd == "" {
		return ""
	}
	home, err := os.UserHomeDir()
	if err == nil {
		if cwd == home {
			cwd = "~"
		} else if strings.HasPrefix(cwd, home+string(os.PathSeparator)) {
			cwd = "~" + strings.TrimPrefix(cwd, home)
		}
	}
	base := filepath.Base(cwd)
	if base != "." && base != string(os.PathSeparator) {
		return base
	}
	return cwd
}

func paneCWD(pane *paneInfo) string {
	if pane == nil {
		return ""
	}
	return firstNonEmpty(pane.ForegroundCWD, pane.CWD)
}

func firstNonEmpty(values ...*string) string {
	for _, value := range values {
		if value != nil && *value != "" {
			return *value
		}
	}
	return ""
}

func normalizeLabel(label string) string {
	maxLabel := defaultMaxLabel
	if value := os.Getenv("HERDR_TABINFO_MAX_LABEL"); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil && parsed > 0 {
			maxLabel = parsed
		}
	}

	cleaned := strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, label)), " ")

	if len([]rune(cleaned)) <= maxLabel {
		return cleaned
	}

	runes := []rune(cleaned)
	suffixStart := strings.LastIndex(cleaned, " [")
	if suffixStart == -1 || !strings.HasSuffix(cleaned, "]") {
		return strings.TrimSpace(string(runes[:maxLabel]))
	}

	suffix := cleaned[suffixStart:]
	suffixRunes := []rune(suffix)
	baseMax := maxLabel - len(suffixRunes) - 1
	if baseMax < 1 {
		baseMax = 1
	}
	baseRunes := []rune(cleaned[:suffixStart])
	if len(baseRunes) > baseMax {
		baseRunes = baseRunes[:baseMax]
	}
	return strings.TrimSpace(string(baseRunes)) + suffix
}
