package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/arl/gitstatus"
)

func ptrString(v string) *string {
	return &v
}

func TestProcessNameFromProcessInfoUsesProcessNameOnly(t *testing.T) {
	got := processNameFromProcessInfo(paneProcessInfoResult{
		ProcessInfo: paneProcessInfo{
			ForegroundProcesses: []foregroundProcess{
				{
					Name:    "nvim",
					Cmdline: ptrString("nvim main.go"),
					Argv:    []string{"nvim", "main.go"},
					Argv0:   ptrString("nvim"),
				},
			},
		},
	}, "")
	if got != "nvim" {
		t.Fatalf("processNameFromProcessInfo() = %q, want %q", got, "nvim")
	}
}

func TestProcessNameFromProcessInfoSkipsShellProcessName(t *testing.T) {
	got := processNameFromProcessInfo(paneProcessInfoResult{
		ProcessInfo: paneProcessInfo{
			ForegroundProcesses: []foregroundProcess{
				{Name: "bash"},
				{Name: "python"},
			},
		},
	}, "bash")
	if got != "python" {
		t.Fatalf("processNameFromProcessInfo() = %q, want %q", got, "python")
	}
}

func TestShellProcessNameUsesBaseName(t *testing.T) {
	if got := shellProcessName("/bin/zsh"); got != "zsh" {
		t.Fatalf("shellProcessName() = %q, want %q", got, "zsh")
	}
}

func TestBuildTabLabel(t *testing.T) {
	cwd := "/repo/project"
	tab := tabInfo{
		TabID:       "t1",
		WorkspaceID: "w1",
		Number:      2,
		Label:       "dev",
		Focused:     true,
	}
	panes := []paneInfo{{
		PaneID:        "p1",
		TabID:         "t1",
		Focused:       true,
		ForegroundCWD: &cwd,
	}}
	layout := paneLayout{TabID: "t1", FocusedPaneID: "p1"}

	got := buildTabLabel(tab, panes, layout, tabDynamicInfo{}, defaultTabInfoConfig().Display.Active)
	want := "2  project"
	if got != want {
		t.Fatalf("buildTabLabel() = %q, want %q", got, want)
	}
}

func TestBuildTabLabelIncludesGitStatus(t *testing.T) {
	tab := tabInfo{TabID: "t1", Number: 2, Focused: true}
	got := buildTabLabel(tab, nil, paneLayout{}, tabDynamicInfo{Git: "⎇ main ✚ 2"}, defaultTabInfoConfig().Display.Active)
	want := "2  ⎇ main ✚ 2"
	if got != want {
		t.Fatalf("buildTabLabel() = %q, want %q", got, want)
	}
}

func TestBuildTabLabelIncludesConfiguredEnvironmentVariable(t *testing.T) {
	tab := tabInfo{TabID: "t1", Number: 2, Focused: true}
	got := buildTabLabel(tab, nil, paneLayout{}, tabDynamicInfo{Environment: map[string]string{"KUBECONFIG_NAME": "production"}}, defaultTabInfoConfig().Display.Active)
	want := "2 ⎈ production"
	if got != want {
		t.Fatalf("buildTabLabel() = %q, want %q", got, want)
	}
}

func TestBuildTabLabelShowsProcessForInactiveTab(t *testing.T) {
	tab := tabInfo{TabID: "t1", Number: 3, Focused: false}
	got := buildTabLabel(tab, nil, paneLayout{}, tabDynamicInfo{Process: "go"}, defaultTabInfoConfig().Display.Inactive)
	want := "3 go"
	if got != want {
		t.Fatalf("buildTabLabel() = %q, want %q", got, want)
	}
}

func TestBuildTabLabelShowsFullProcessWhenEnabled(t *testing.T) {
	tab := tabInfo{TabID: "t1", Number: 3, Focused: false}
	got := buildTabLabel(tab, nil, paneLayout{}, tabDynamicInfo{
		ProcessFull: "go test ./...",
	}, tabDisplayConfig{TabNumber: true, ProcessFull: true})
	want := "3 go test ./..."
	if got != want {
		t.Fatalf("buildTabLabel() = %q, want %q", got, want)
	}
}

func TestBuildTabLabelRespectsDisplayConfig(t *testing.T) {
	cwd := "/repo/project"
	tab := tabInfo{TabID: "t1", Number: 2, Focused: true}
	panes := []paneInfo{{PaneID: "p1", TabID: "t1", Focused: true, ForegroundCWD: &cwd}}
	display := tabDisplayConfig{TabNumber: true, Git: true}

	got := buildTabLabel(tab, panes, paneLayout{FocusedPaneID: "p1"}, tabDynamicInfo{
		Process:     "nvim",
		Git:         "⎇ main",
		Environment: map[string]string{"KUBECONFIG_NAME": "production"},
	}, display)
	want := "2  ⎇ main"
	if got != want {
		t.Fatalf("buildTabLabel() = %q, want %q", got, want)
	}
}

func TestLoadTabInfoConfigRequiresGlobalTabContent(t *testing.T) {
	config, err := parseTabInfoConfig([]byte("display:\n  active:\n    tab_number: false\n    process: false\n    process_full: false\n"))
	if err == nil {
		t.Fatalf("parseTabInfoConfig() = %#v, want error", config)
	}
}

func TestLoadTabInfoConfigReadsXDGConfigFile(t *testing.T) {
	configHome := t.TempDir()
	configPath := filepath.Join(configHome, "herdr-plugin-tabinfo", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte("display:\n  active:\n    git: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_TABINFO_CONFIG", "")
	t.Setenv("HERDR_PLUGIN_CONFIG_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", configHome)

	config, err := loadTabInfoConfig()
	if err != nil {
		t.Fatal(err)
	}
	if config.Display.Active.Git {
		t.Fatalf("Active.Git = true, want false")
	}
}

func TestLoadTabInfoConfigPrefersExplicitConfig(t *testing.T) {
	configHome := t.TempDir()
	globalConfigPath := filepath.Join(configHome, "herdr-plugin-tabinfo", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(globalConfigPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(globalConfigPath, []byte("display:\n  active:\n    git: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	explicitConfigPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(explicitConfigPath, []byte("display:\n  active:\n    directory: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_TABINFO_CONFIG", explicitConfigPath)
	t.Setenv("HERDR_PLUGIN_CONFIG_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", configHome)

	config, err := loadTabInfoConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !config.Display.Active.Git || config.Display.Active.Directory {
		t.Fatalf("Active = %#v", config.Display.Active)
	}
}

func TestLoadTabInfoConfigPrefersPluginConfigDir(t *testing.T) {
	pluginConfigDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(pluginConfigDir, "config.yaml"), []byte("display:\n  active:\n    directory: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	configHome := t.TempDir()
	globalConfigPath := filepath.Join(configHome, "herdr-plugin-tabinfo", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(globalConfigPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(globalConfigPath, []byte("display:\n  active:\n    git: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_TABINFO_CONFIG", "")
	t.Setenv("HERDR_PLUGIN_CONFIG_DIR", pluginConfigDir)
	t.Setenv("XDG_CONFIG_HOME", configHome)

	config, err := loadTabInfoConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !config.Display.Active.Git || config.Display.Active.Directory {
		t.Fatalf("Active = %#v", config.Display.Active)
	}
}

func TestParseTabInfoConfigOverridesDefaults(t *testing.T) {
	config, err := parseTabInfoConfig([]byte("display:\n  active:\n    process: false\n    git: false\n  inactive:\n    process_full: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !config.Display.Active.TabNumber || config.Display.Active.Process || config.Display.Active.ProcessFull || config.Display.Active.Git || !config.Display.Active.Directory || len(config.Display.Active.Environment) != 1 || config.Display.Active.Environment[0].Variable != "KUBECONFIG_NAME" {
		t.Fatalf("Active = %#v", config.Display.Active)
	}
	if !config.Display.Inactive.TabNumber || !config.Display.Inactive.Process || !config.Display.Inactive.ProcessFull {
		t.Fatalf("Inactive = %#v", config.Display.Inactive)
	}
	if config.Display.Inactive.Directory || config.Display.Inactive.Git || len(config.Display.Inactive.Environment) != 0 {
		t.Fatalf("Inactive = %#v", config.Display.Inactive)
	}
}

func TestBuildTabLabelUsesDifferentActiveAndInactiveSettings(t *testing.T) {
	config := tabInfoConfig{Display: displayConfig{
		Active:   tabDisplayConfig{TabNumber: true, Directory: true},
		Inactive: tabDisplayConfig{ProcessFull: true, Git: true},
	}}
	cwd := "/repo/project"
	active := buildTabLabel(
		tabInfo{TabID: "t1", Number: 2, Focused: true},
		[]paneInfo{{PaneID: "p1", ForegroundCWD: &cwd}},
		paneLayout{FocusedPaneID: "p1"},
		tabDynamicInfo{Process: "nvim"},
		config.Display.forTab(true),
	)
	inactive := buildTabLabel(
		tabInfo{TabID: "t2", Number: 3},
		nil,
		paneLayout{},
		tabDynamicInfo{ProcessFull: "go test ./...", Git: "⎇ main"},
		config.Display.forTab(false),
	)
	if active != "2  project" {
		t.Fatalf("active = %q", active)
	}
	if inactive != "go test ./...  ⎇ main" {
		t.Fatalf("inactive = %q", inactive)
	}
}

func TestProcessFullFromProcessInfoPrefersLastNamedForegroundProcess(t *testing.T) {
	got := processFullFromProcessInfo(paneProcessInfoResult{
		ProcessInfo: paneProcessInfo{
			ForegroundProcesses: []foregroundProcess{
				{Name: "bash"},
				{Name: "zsh", Cmdline: ptrString("zsh -l")},
			},
		},
	})
	if got != "zsh -l" {
		t.Fatalf("processFullFromProcessInfo() = %q, want %q", got, "zsh -l")
	}
}

func TestReadEnvironmentVariableWithoutDirenvUsesInheritedValue(t *testing.T) {
	if got := readEnvironmentVariable("/repo", "", "KUBECONFIG_NAME", "production"); got != "production" {
		t.Fatalf("readEnvironmentVariable() = %q, want production", got)
	}
}

func TestBuildTabLabelOmitsEmptyEnvironmentValue(t *testing.T) {
	tab := tabInfo{TabID: "t1", Number: 2, Focused: true}
	display := tabDisplayConfig{TabNumber: true, Environment: []environmentDisplayConfig{{Icon: "◆", Variable: "PROJECT"}, {Icon: "●", Variable: "TEAM"}}}
	got := buildTabLabel(tab, nil, paneLayout{}, tabDynamicInfo{}, display)
	if got != "2" {
		t.Fatalf("buildTabLabel() = %q, want 2", got)
	}
}

func TestParseTabInfoConfigOverridesEnvironmentDisplays(t *testing.T) {
	config, err := parseTabInfoConfig([]byte("display:\n  active:\n    environment:\n      - icon: '◆'\n        variable: PROJECT\n      - icon: '●'\n        variable: TEAM\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := config.Display.Active.Environment, []environmentDisplayConfig{{Icon: "◆", Variable: "PROJECT"}, {Icon: "●", Variable: "TEAM"}}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("Active.Environment = %#v, want %#v", got, want)
	}
}

func TestBuildTabLabelIncludesMultipleEnvironmentVariables(t *testing.T) {
	tab := tabInfo{TabID: "t1", Number: 2, Focused: true}
	display := tabDisplayConfig{TabNumber: true, Environment: []environmentDisplayConfig{{Icon: "◆", Variable: "PROJECT"}, {Icon: "●", Variable: "TEAM"}}}
	got := buildTabLabel(tab, nil, paneLayout{}, tabDynamicInfo{Environment: map[string]string{"PROJECT": "tabinfo", "TEAM": "platform"}}, display)
	if got != "2 ◆ tabinfo ● platform" {
		t.Fatalf("buildTabLabel() = %q", got)
	}
}

func TestFormatGitStatusUsesGitmuxLayoutWithoutStyles(t *testing.T) {
	config, err := parseGitmuxConfig([]byte(`
tmux:
  symbols:
    branch: "BR "
    modified: "M "
    clean: "C"
  styles:
    branch: "#[fg=red]"
    modified: "#[fg=yellow]"
  layout: [branch, " | ", flags]
`))
	if err != nil {
		t.Fatal(err)
	}

	got := formatGitStatus(&gitstatus.Status{
		Porcelain: gitstatus.Porcelain{LocalBranch: "main", NumModified: 2},
	}, config.Tmux)
	want := "BR main | M 2"
	if got != want {
		t.Fatalf("formatGitStatus() = %q, want %q", got, want)
	}
}

func TestFormatGitStatusSupportsGitmuxOptions(t *testing.T) {
	config, err := parseGitmuxConfig([]byte(`
tmux:
  symbols:
    branch: ""
    ahead: "+"
    behind: "-"
    modified: "m"
  layout: [branch, divergence, flags]
  options:
    branch_max_len: 7
    branch_trim: center
    ellipsis: "~"
    divergence_space: true
    swap_divergence: true
    flags_without_count: true
`))
	if err != nil {
		t.Fatal(err)
	}

	got := formatGitStatus(&gitstatus.Status{
		Porcelain: gitstatus.Porcelain{
			LocalBranch: "feature/tabinfo",
			AheadCount:  2,
			BehindCount: 1,
			NumModified: 3,
		},
	}, config.Tmux)
	want := "fea~nfo +2 -1 m"
	if got != want {
		t.Fatalf("formatGitStatus() = %q, want %q", got, want)
	}
}

func TestPlanLabelUpdatesRewritesEveryTab(t *testing.T) {
	cwd := "/repo/project"
	snapshot := sessionSnapshot{
		Tabs: []tabInfo{
			{
				TabID:       "t1",
				WorkspaceID: "w1",
				Number:      1,
				Label:       "dev",
				Focused:     true,
			},
			{
				TabID:       "t2",
				WorkspaceID: "w1",
				Number:      2,
				Label:       "tests",
				Focused:     false,
			},
		},
		Panes: []paneInfo{{
			PaneID:        "p1",
			TabID:         "t1",
			Focused:       true,
			ForegroundCWD: &cwd,
		}},
		Layouts: []paneLayout{{TabID: "t1", FocusedPaneID: "p1"}},
	}

	renames := planLabelUpdates(snapshot)

	if len(renames) != 2 {
		t.Fatalf("len(renames) = %d, want 2: %#v", len(renames), renames)
	}
	if renames[0] != (tabRename{TabID: "t1", Label: "1  project"}) {
		t.Fatalf("rename = %#v", renames[0])
	}
	if renames[1] != (tabRename{TabID: "t2", Label: "2"}) {
		t.Fatalf("rename = %#v", renames[1])
	}
}

func TestPlanLabelUpdatesIncludesProcessLabelsForInactiveTabs(t *testing.T) {
	snapshot := sessionSnapshot{
		Tabs: []tabInfo{
			{TabID: "t1", WorkspaceID: "w1", Number: 1, Label: "dev", Focused: true},
			{TabID: "t2", WorkspaceID: "w1", Number: 2, Label: "tests", Focused: false},
		},
		Panes: []paneInfo{
			{PaneID: "p1", TabID: "t1", Focused: true},
			{PaneID: "p2", TabID: "t2", Focused: true},
		},
		Layouts: []paneLayout{
			{TabID: "t1", FocusedPaneID: "p1"},
			{TabID: "t2", FocusedPaneID: "p2"},
		},
	}

	renames := planLabelUpdatesWithInfo(snapshot, defaultTabInfoConfig().Display, func(_ tabInfo, pane *paneInfo) tabDynamicInfo {
		if pane.PaneID == "p1" {
			return tabDynamicInfo{Process: "nvim"}
		}
		return tabDynamicInfo{Process: "go"}
	})

	if len(renames) != 2 {
		t.Fatalf("len(renames) = %d, want 2: %#v", len(renames), renames)
	}
	if renames[0] != (tabRename{TabID: "t1", Label: "1 nvim"}) {
		t.Fatalf("active rename = %#v", renames[0])
	}
	if renames[1] != (tabRename{TabID: "t2", Label: "2 go"}) {
		t.Fatalf("inactive rename = %#v", renames[1])
	}
}

func TestPlanLabelUpdatesPrioritizesActiveTabAndWorkspace(t *testing.T) {
	snapshot := sessionSnapshot{
		Tabs: []tabInfo{
			{TabID: "t1", WorkspaceID: "w1", Number: 1, Label: "one"},
			{TabID: "t2", WorkspaceID: "w2", Number: 2, Label: "two"},
			{TabID: "t3", WorkspaceID: "w1", Number: 3, Label: "three", Focused: true},
			{TabID: "t4", WorkspaceID: "w1", Number: 4, Label: "four"},
			{TabID: "t5", WorkspaceID: "w3", Number: 5, Label: "five"},
		},
	}

	renames := planLabelUpdates(snapshot)
	want := []tabRename{
		{TabID: "t3", Label: "3"},
		{TabID: "t1", Label: "1"},
		{TabID: "t4", Label: "4"},
		{TabID: "t2", Label: "2"},
		{TabID: "t5", Label: "5"},
	}
	if len(renames) != len(want) {
		t.Fatalf("len(renames) = %d, want %d: %#v", len(renames), len(want), renames)
	}
	for i := range want {
		if renames[i] != want[i] {
			t.Fatalf("renames[%d] = %#v, want %#v", i, renames[i], want[i])
		}
	}
}

func TestPrioritizedTabsKeepsSnapshotOrderWithoutActiveTab(t *testing.T) {
	tabs := []tabInfo{
		{TabID: "t1", WorkspaceID: "w1"},
		{TabID: "t2", WorkspaceID: "w2"},
	}

	got := prioritizedTabs(tabs)
	for i := range tabs {
		if got[i] != tabs[i] {
			t.Fatalf("tabs[%d] = %#v, want %#v", i, got[i], tabs[i])
		}
	}
}

func TestHerdrClientOpensConnectionPerRequest(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "herdr.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			t.Skipf("unix socket listen is not permitted in this environment: %v", err)
		}
		t.Fatal(err)
	}
	defer func() {
		_ = listener.Close()
	}()

	requests := make(chan apiRequest, 2)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 2; i++ {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			func() {
				defer func() {
					_ = conn.Close()
				}()
				var request apiRequest
				if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&request); err != nil {
					t.Errorf("decode request: %v", err)
					return
				}
				requests <- request
				_, _ = conn.Write([]byte(`{"id":"` + request.ID + `","result":{"type":"ok"}}` + "\n"))
			}()
		}
	}()

	client, err := dialHerdr(socketPath)
	if err != nil {
		t.Fatal(err)
	}
	var first json.RawMessage
	if err := client.request("ping", map[string]any{}, &first); err != nil {
		t.Fatalf("first request failed: %v", err)
	}
	var second json.RawMessage
	if err := client.request("tab.rename", map[string]string{"tab_id": "t1", "label": "dev"}, &second); err != nil {
		t.Fatalf("second request failed: %v", err)
	}
	<-done

	if got := (<-requests).Method; got != "ping" {
		t.Fatalf("first method = %q, want ping", got)
	}
	if got := (<-requests).Method; got != "tab.rename" {
		t.Fatalf("second method = %q, want tab.rename", got)
	}
}
