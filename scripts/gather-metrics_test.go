package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const ccccOutput = `{
  "summary": {
    "cognitive": {"sum": 10, "max": 5, "median": 1, "p90": 3, "p95": 4},
    "cyclomatic": {"sum": 20, "max": 8, "median": 2, "p90": 5, "p95": 6}
  }
}`

func TestBuildCCCCMetrics(t *testing.T) {
	report, err := buildCCCCMetrics([]byte(ccccOutput))
	if err != nil {
		t.Fatalf("buildCCCCMetrics() error = %v", err)
	}

	if report.Key != "cccc_complexity" {
		t.Errorf("Key = %q, want %q", report.Key, "cccc_complexity")
	}
	if report.Name != "Code Complexity" {
		t.Errorf("Name = %q, want %q", report.Name, "Code Complexity")
	}

	want := []OctocovMetric{
		{Key: "go_cognitive_sum", Name: "Go Cognitive Complexity (Sum)", Value: 10},
		{Key: "go_cognitive_max", Name: "Go Cognitive Complexity (Max)", Value: 5},
		{Key: "go_cognitive_median", Name: "Go Cognitive Complexity (Median)", Value: 1},
		{Key: "go_cognitive_p90", Name: "Go Cognitive Complexity (p90)", Value: 3},
		{Key: "go_cognitive_p95", Name: "Go Cognitive Complexity (p95)", Value: 4},
		{Key: "go_cyclomatic_sum", Name: "Go Cyclomatic Complexity (Sum)", Value: 20},
		{Key: "go_cyclomatic_max", Name: "Go Cyclomatic Complexity (Max)", Value: 8},
	}
	if len(report.Metrics) != len(want) {
		t.Fatalf("len(Metrics) = %d, want %d", len(report.Metrics), len(want))
	}
	for i := range want {
		if report.Metrics[i] != want[i] {
			t.Errorf("Metrics[%d] = %#v, want %#v", i, report.Metrics[i], want[i])
		}
	}
}

func TestBuildCCCCMetricsRejectsInvalidJSON(t *testing.T) {
	if _, err := buildCCCCMetrics([]byte(`{"summary":`)); err == nil {
		t.Fatal("buildCCCCMetrics() error = nil, want an error")
	}
}

func TestWriteOctocovMetrics(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cccc-metrics.json")
	report := OctocovReport{
		Key:  "cccc_complexity",
		Name: "Code Complexity",
		Metrics: []OctocovMetric{
			{Key: "go_cognitive_sum", Name: "Go Cognitive Complexity (Sum)", Value: 10},
		},
	}

	if err := writeOctocovMetrics(path, report); err != nil {
		t.Fatalf("writeOctocovMetrics() error = %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("generated file is not JSON: %v", err)
	}
	for _, key := range []string{"key", "name", "metrics"} {
		if _, ok := got[key]; !ok {
			t.Errorf("generated file does not contain %q", key)
		}
	}
}

func TestCIWorkflowReportsCCCCMetrics(t *testing.T) {
	type step struct {
		Uses string            `yaml:"uses"`
		Run  string            `yaml:"run"`
		With map[string]string `yaml:"with"`
		Env  map[string]string `yaml:"env"`
	}
	type workflow struct {
		Env  map[string]string `yaml:"env"`
		Jobs map[string]struct {
			Steps []step `yaml:"steps"`
		} `yaml:"jobs"`
	}

	data, err := os.ReadFile("../.github/workflows/ci.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var ci workflow
	if err := yaml.Unmarshal(data, &ci); err != nil {
		t.Fatalf("parse CI workflow: %v", err)
	}
	if !strings.HasPrefix(ci.Env["CCCC_VERSION"], "v1.") {
		t.Fatalf("CCCC_VERSION = %q, want a v1 release", ci.Env["CCCC_VERSION"])
	}

	testJob, ok := ci.Jobs["test"]
	if !ok {
		t.Fatal("test job is missing")
	}
	var foundAction, foundGather, foundOctocovMetrics bool
	for _, step := range testJob.Steps {
		if step.Uses == "moznion/cccc-action@8fa5a4b13bf907676709cece09147a047b7be7b0" && step.With["version"] == "${{ env.CCCC_VERSION }}" {
			foundAction = true
		}
		if step.Run == "go run ./scripts/gather-metrics.go" {
			foundGather = true
		}
		if strings.HasPrefix(step.Uses, "k1LoW/octocov-action@") && step.Env["OCTOCOV_CUSTOM_METRICS_CCCC"] == "cccc-metrics.json" {
			foundOctocovMetrics = true
		}
	}
	if !foundAction {
		t.Error("test job does not install CCCC_VERSION with the pinned moznion/cccc-action v1")
	}
	if !foundGather {
		t.Error("test job does not gather cccc metrics")
	}
	if !foundOctocovMetrics {
		t.Error("test job does not pass cccc-metrics.json to octocov")
	}
}
