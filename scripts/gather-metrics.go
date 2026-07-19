package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
)

type OctocovMetric struct {
	Key   string  `json:"key"`
	Name  string  `json:"name"`
	Value float64 `json:"value"`
}

type OctocovReport struct {
	Key     string          `json:"key"`
	Name    string          `json:"name"`
	Metrics []OctocovMetric `json:"metrics"`
}

type complexityMetrics struct {
	Sum    float64 `json:"sum"`
	Max    float64 `json:"max"`
	Median float64 `json:"median"`
	P90    float64 `json:"p90"`
	P95    float64 `json:"p95"`
}

type ccccResult struct {
	Summary struct {
		Cognitive  complexityMetrics `json:"cognitive"`
		Cyclomatic complexityMetrics `json:"cyclomatic"`
	} `json:"summary"`
}

func buildCCCCMetrics(data []byte) (OctocovReport, error) {
	var result ccccResult
	if err := json.Unmarshal(data, &result); err != nil {
		return OctocovReport{}, fmt.Errorf("parse cccc output: %w", err)
	}

	return OctocovReport{
		Key:  "cccc_complexity",
		Name: "Code Complexity",
		Metrics: []OctocovMetric{
			{Key: "go_cognitive_sum", Name: "Go Cognitive Complexity (Sum)", Value: result.Summary.Cognitive.Sum},
			{Key: "go_cognitive_max", Name: "Go Cognitive Complexity (Max)", Value: result.Summary.Cognitive.Max},
			{Key: "go_cognitive_median", Name: "Go Cognitive Complexity (Median)", Value: result.Summary.Cognitive.Median},
			{Key: "go_cognitive_p90", Name: "Go Cognitive Complexity (p90)", Value: result.Summary.Cognitive.P90},
			{Key: "go_cognitive_p95", Name: "Go Cognitive Complexity (p95)", Value: result.Summary.Cognitive.P95},
			{Key: "go_cyclomatic_sum", Name: "Go Cyclomatic Complexity (Sum)", Value: result.Summary.Cyclomatic.Sum},
			{Key: "go_cyclomatic_max", Name: "Go Cyclomatic Complexity (Max)", Value: result.Summary.Cyclomatic.Max},
		},
	}, nil
}

func writeOctocovMetrics(path string, report OctocovReport) error {
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal octocov metrics: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write octocov metrics: %w", err)
	}
	return nil
}

func run() error {
	output, err := exec.Command("cccc", "--lang", "go", "cmd").Output()
	if err != nil {
		return fmt.Errorf("run cccc: %w", err)
	}
	report, err := buildCCCCMetrics(output)
	if err != nil {
		return err
	}
	return writeOctocovMetrics("cccc-metrics.json", report)
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
