package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/arl/gitstatus"
)

func ptrString(v string) *string {
	return &v
}

func TestProcessDisplayUsesDirectChildOfShell(t *testing.T) {
	result := paneProcessInfoResult{ProcessInfo: paneProcessInfo{ForegroundProcesses: []foregroundProcess{
		{Name: "zsh"},
		{Name: "go", Cmdline: ptrString("go test ./...")},
		{Name: "tabinfo.test", Cmdline: ptrString("/tmp/tabinfo.test")},
	}}}
	if got := processNameFromProcessInfo(result, "zsh"); got != "go" {
		t.Fatalf("processNameFromProcessInfo() = %q, want go", got)
	}
	if got := processFullFromProcessInfo(result, "zsh"); got != "go test ./..." {
		t.Fatalf("processFullFromProcessInfo() = %q, want go test ./...", got)
	}
}

func TestProcessDisplayUsesNonShellProcessWhenOnlyOneExists(t *testing.T) {
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

func TestProcessDisplayOmitsShellOnlyPane(t *testing.T) {
	result := paneProcessInfoResult{ProcessInfo: paneProcessInfo{ForegroundProcesses: []foregroundProcess{{Name: "bash"}}}}
	if got := processNameFromProcessInfo(result, "bash"); got != "" {
		t.Fatalf("processNameFromProcessInfo() = %q, want empty", got)
	}
	if got := processFullFromProcessInfo(result, "bash"); got != "" {
		t.Fatalf("processFullFromProcessInfo() = %q, want empty", got)
	}
}

func TestShellProcessNameUsesBaseName(t *testing.T) {
	if got := shellProcessName("/bin/zsh"); got != "zsh" {
		t.Fatalf("shellProcessName() = %q, want %q", got, "zsh")
	}
}

func TestBuildTabLabelUsesConfiguredOrderAndSeparator(t *testing.T) {
	cwd := "/repo/project"
	display := tabDisplayConfig{
		Items:       []displayItem{displayItemGit, displayItemDirectory, displayItemProcess, displayItemTabNumber, displayItemEnvironment},
		Separator:   " | ",
		Environment: []environmentDisplayConfig{{Icon: "⎈", Variable: "KUBECONFIG_NAME"}},
	}
	got := buildTabLabel(
		tabInfo{TabID: "t1", Number: 2},
		[]paneInfo{{PaneID: "p1", ForegroundCWD: &cwd}},
		paneLayout{FocusedPaneID: "p1"},
		tabDynamicInfo{Process: "go", Git: "⎇ main", Environment: map[string]string{"KUBECONFIG_NAME": "production"}},
		display,
	)
	if want := " ⎇ main |  project | go | 2 | ⎈ production"; got != want {
		t.Fatalf("buildTabLabel() = %q, want %q", got, want)
	}
}

func TestBuildTabLabelOmitsMissingItemsWithoutExtraSeparators(t *testing.T) {
	display := tabDisplayConfig{
		Items:       []displayItem{displayItemProcess, displayItemGit, displayItemEnvironment, displayItemTabNumber},
		Separator:   "",
		Environment: []environmentDisplayConfig{{Icon: "⎈", Variable: "KUBECONFIG_NAME"}},
	}
	got := buildTabLabel(tabInfo{TabID: "t1", Number: 2}, nil, paneLayout{}, tabDynamicInfo{Process: "go"}, display)
	if want := "go2"; got != want {
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
	}, tabDisplayConfig{Items: []displayItem{displayItemTabNumber, displayItemProcessFull}, Separator: " "})
	want := "3 go test ./..."
	if got != want {
		t.Fatalf("buildTabLabel() = %q, want %q", got, want)
	}
}

func TestBuildTabLabelRespectsDisplayConfig(t *testing.T) {
	cwd := "/repo/project"
	tab := tabInfo{TabID: "t1", Number: 2, Focused: true}
	panes := []paneInfo{{PaneID: "p1", TabID: "t1", Focused: true, ForegroundCWD: &cwd}}
	display := tabDisplayConfig{Items: []displayItem{displayItemTabNumber, displayItemGit}, Separator: " "}

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

func TestParseTabInfoConfigReadsItemsAndSeparator(t *testing.T) {
	config, err := parseTabInfoConfig([]byte(`
display:
  active:
    items: [directory, tab_number, environment]
    separator: " | "
    environment:
      - icon: "◆"
        variable: PROJECT
  inactive:
    items: [tab_number, process]
`))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := config.Display.Active.Items, []displayItem{displayItemDirectory, displayItemTabNumber, displayItemEnvironment}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Active.Items = %#v, want %#v", got, want)
	}
	if got := config.Display.Active.Separator; got != " | " {
		t.Fatalf("Active.Separator = %q, want %q", got, " | ")
	}
	if got := config.Display.Inactive.Separator; got != " " {
		t.Fatalf("Inactive.Separator = %q, want one space", got)
	}
}

func TestParseTabInfoConfigRejectsInvalidItems(t *testing.T) {
	for _, tc := range []struct {
		name string
		yaml string
	}{
		{name: "missing active", yaml: "display:\n  active: {}\n  inactive:\n    items: [tab_number]\n"},
		{name: "empty inactive", yaml: "display:\n  active:\n    items: [tab_number]\n  inactive:\n    items: []\n"},
		{name: "unknown", yaml: "display:\n  active:\n    items: [tab_number, owner]\n  inactive:\n    items: [tab_number]\n"},
		{name: "duplicate", yaml: "display:\n  active:\n    items: [tab_number, tab_number]\n  inactive:\n    items: [tab_number]\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseTabInfoConfig([]byte(tc.yaml)); err == nil {
				t.Fatal("parseTabInfoConfig() error = nil")
			}
		})
	}
}

func TestLoadTabInfoConfigReadsXDGConfigFile(t *testing.T) {
	configHome := t.TempDir()
	configPath := filepath.Join(configHome, "herdr-plugin-tabinfo", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte("display:\n  active:\n    items: [git]\n  inactive:\n    items: [tab_number]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_TABINFO_CONFIG", "")
	t.Setenv("HERDR_PLUGIN_CONFIG_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", configHome)

	config, err := loadTabInfoConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := config.Display.Active.Items, []displayItem{displayItemGit}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Active.Items = %#v, want %#v", got, want)
	}
}

func TestLoadTabInfoConfigPrefersExplicitConfig(t *testing.T) {
	configHome := t.TempDir()
	globalConfigPath := filepath.Join(configHome, "herdr-plugin-tabinfo", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(globalConfigPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(globalConfigPath, []byte("display:\n  active:\n    items: [git]\n  inactive:\n    items: [tab_number]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	explicitConfigPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(explicitConfigPath, []byte("display:\n  active:\n    items: [directory]\n  inactive:\n    items: [tab_number]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_TABINFO_CONFIG", explicitConfigPath)
	t.Setenv("HERDR_PLUGIN_CONFIG_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", configHome)

	config, err := loadTabInfoConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := config.Display.Active.Items, []displayItem{displayItemDirectory}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Active.Items = %#v, want %#v", got, want)
	}
}

func TestLoadTabInfoConfigPrefersPluginConfigDir(t *testing.T) {
	pluginConfigDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(pluginConfigDir, "config.yaml"), []byte("display:\n  active:\n    items: [directory]\n  inactive:\n    items: [tab_number]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	configHome := t.TempDir()
	globalConfigPath := filepath.Join(configHome, "herdr-plugin-tabinfo", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(globalConfigPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(globalConfigPath, []byte("display:\n  active:\n    items: [git]\n  inactive:\n    items: [tab_number]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_TABINFO_CONFIG", "")
	t.Setenv("HERDR_PLUGIN_CONFIG_DIR", pluginConfigDir)
	t.Setenv("XDG_CONFIG_HOME", configHome)

	config, err := loadTabInfoConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := config.Display.Active.Items, []displayItem{displayItemDirectory}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Active.Items = %#v, want %#v", got, want)
	}
}

func TestBuildTabLabelUsesDifferentActiveAndInactiveSettings(t *testing.T) {
	config := tabInfoConfig{Display: displayConfig{
		Active:   tabDisplayConfig{Items: []displayItem{displayItemTabNumber, displayItemDirectory}, Separator: " "},
		Inactive: tabDisplayConfig{Items: []displayItem{displayItemProcessFull, displayItemGit}, Separator: " "},
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

func TestProcessFullFromProcessInfoUsesFallbacks(t *testing.T) {
	got := processFullFromProcessInfo(paneProcessInfoResult{
		ProcessInfo: paneProcessInfo{
			ForegroundProcesses: []foregroundProcess{
				{Name: "go", Cmdline: ptrString("go test ./...")},
			},
		},
	}, "")
	if got != "go test ./..." {
		t.Fatalf("processFullFromProcessInfo() = %q, want %q", got, "go test ./...")
	}
}

func TestReadEnvironmentVariableWithoutDirenvUsesInheritedValue(t *testing.T) {
	if got := readEnvironmentVariable("/repo", "", "KUBECONFIG_NAME", "production"); got != "production" {
		t.Fatalf("readEnvironmentVariable() = %q, want production", got)
	}
}

func TestBuildTabLabelOmitsEmptyEnvironmentValue(t *testing.T) {
	tab := tabInfo{TabID: "t1", Number: 2, Focused: true}
	display := tabDisplayConfig{Items: []displayItem{displayItemTabNumber, displayItemEnvironment}, Separator: " ", Environment: []environmentDisplayConfig{{Icon: "◆", Variable: "PROJECT"}, {Icon: "●", Variable: "TEAM"}}}
	got := buildTabLabel(tab, nil, paneLayout{}, tabDynamicInfo{}, display)
	if got != "2" {
		t.Fatalf("buildTabLabel() = %q, want 2", got)
	}
}

func TestParseTabInfoConfigOverridesEnvironmentDisplays(t *testing.T) {
	config, err := parseTabInfoConfig([]byte("display:\n  active:\n    items: [environment]\n    environment:\n      - icon: '◆'\n        variable: PROJECT\n      - icon: '●'\n        variable: TEAM\n  inactive:\n    items: [tab_number]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := config.Display.Active.Environment, []environmentDisplayConfig{{Icon: "◆", Variable: "PROJECT"}, {Icon: "●", Variable: "TEAM"}}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("Active.Environment = %#v, want %#v", got, want)
	}
}

func TestBuildTabLabelIncludesMultipleEnvironmentVariables(t *testing.T) {
	tab := tabInfo{TabID: "t1", Number: 2, Focused: true}
	display := tabDisplayConfig{Items: []displayItem{displayItemTabNumber, displayItemEnvironment}, Separator: " ", Environment: []environmentDisplayConfig{{Icon: "◆", Variable: "PROJECT"}, {Icon: "●", Variable: "TEAM"}}}
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
