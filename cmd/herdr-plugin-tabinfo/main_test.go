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
	})
	if got != "nvim" {
		t.Fatalf("processNameFromProcessInfo() = %q, want %q", got, "nvim")
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

func TestBuildTabLabelIncludesKubernetesConfig(t *testing.T) {
	tab := tabInfo{TabID: "t1", Number: 2, Focused: true}
	got := buildTabLabel(tab, nil, paneLayout{}, tabDynamicInfo{Kubernetes: "production"}, defaultTabInfoConfig().Display.Active)
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
		Process:    "nvim",
		Git:        "⎇ main",
		Kubernetes: "production",
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

func TestParseTabInfoConfigOverridesDefaults(t *testing.T) {
	config, err := parseTabInfoConfig([]byte("display:\n  active:\n    process: false\n    git: false\n  inactive:\n    process_full: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !config.Display.Active.TabNumber || config.Display.Active.Process || config.Display.Active.ProcessFull || config.Display.Active.Git || !config.Display.Active.Directory || !config.Display.Active.Kubernetes {
		t.Fatalf("Active = %#v", config.Display.Active)
	}
	if !config.Display.Inactive.TabNumber || !config.Display.Inactive.Process || !config.Display.Inactive.ProcessFull {
		t.Fatalf("Inactive = %#v", config.Display.Inactive)
	}
	if config.Display.Inactive.Directory || config.Display.Inactive.Git || config.Display.Inactive.Kubernetes {
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

func TestReadKubernetesConfigWithoutDirenvUsesInheritedValue(t *testing.T) {
	if got := readKubernetesConfig("/repo", "", "production"); got != "production" {
		t.Fatalf("readKubernetesConfig() = %q, want production", got)
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
