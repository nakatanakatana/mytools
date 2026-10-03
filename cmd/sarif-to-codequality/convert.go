package main

import (
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"strings"
)

// Report represents a SARIF report.
type Report struct {
	Version string     `json:"version"`
	Runs    []SARIFRun `json:"runs"`
}

// SARIFRun represents a run in a SARIF report.
type SARIFRun struct {
	Tool    Tool     `json:"tool"`
	Results []Result `json:"results"`
}

// Tool represents the tool that generated the SARIF report.
type Tool struct {
	Driver Driver `json:"driver"`
}

// Driver represents the driver of the tool.
type Driver struct {
	Name string `json:"name"`
}

// Result represents a single result in a SARIF report.
type Result struct {
	RuleID    string          `json:"ruleId"`
	Message   Message         `json:"message"`
	Level     string          `json:"level,omitempty"`
	Locations []SARIFLocation `json:"locations,omitempty"`
}

// Message represents a message in a SARIF report.
type Message struct {
	Text string `json:"text"`
}

// SARIFLocation represents a physical location in a SARIF report.
type SARIFLocation struct {
	PhysicalLocation PhysicalLocation `json:"physicalLocation"`
}

// PhysicalLocation represents a physical location in a SARIF report.
type PhysicalLocation struct {
	ArtifactLocation ArtifactLocation `json:"artifactLocation"`
	Region           Region           `json:"region"`
}

// ArtifactLocation represents an artifact location in a SARIF report.
type ArtifactLocation struct {
	URI string `json:"uri"`
}

// Region represents a region in a SARIF report.
type Region struct {
	StartLine   int `json:"startLine"`
	StartColumn int `json:"startColumn,omitempty"`
}

// Issue represents a GitLab Code Quality issue.
type Issue struct {
	CheckName   string   `json:"check_name"`
	Description string   `json:"description"`
	Fingerprint string   `json:"fingerprint"`
	Severity    string   `json:"severity"`
	Location    Location `json:"location"`
}

// Location represents the location of a Code Quality issue.
type Location struct {
	Path  string `json:"path"`
	Lines Lines  `json:"lines"`
}

// Lines represents the line numbers of a Code Quality issue.
type Lines struct {
	Begin int `json:"begin"`
}

// Convert converts a SARIF report to a slice of GitLab Code Quality issues.
func Convert(report Report, baseDir string) []Issue {
	var issues []Issue

	for _, run := range report.Runs {
		for _, res := range run.Results {
			if len(res.Locations) == 0 {
				continue
			}

			loc := res.Locations[0]
			path := loc.PhysicalLocation.ArtifactLocation.URI
			normalizedPath := ConvertPath(path, baseDir)
			line := loc.PhysicalLocation.Region.StartLine

			issues = append(issues, Issue{
				CheckName:   res.RuleID,
				Description: res.Message.Text,
				Fingerprint: GenerateFingerprint(res.RuleID, path, res.Message.Text),
				Severity:    ConvertSeverity(res.Level),
				Location: Location{
					Path: normalizedPath,
					Lines: Lines{
						Begin: line,
					},
				},
			})
		}
	}

	return issues
}

// GenerateFingerprint generates a unique and stable fingerprint for an issue.
func GenerateFingerprint(ruleID, path, message string) string {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%s:%s:%s", ruleID, path, message)
	return fmt.Sprintf("%x", h.Sum(nil))
}

// ConvertPath normalizes a file path to be relative to the baseDir.
func ConvertPath(path, baseDir string) string {
	// Remove file:// prefix
	path = strings.TrimPrefix(path, "file://")

	if !filepath.IsAbs(path) {
		return path
	}

	rel, err := filepath.Rel(baseDir, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return path
	}

	return rel
}

// ConvertSeverity maps a SARIF level to a GitLab severity level.
func ConvertSeverity(level string) string {
	switch level {
	case "error":
		return "critical"
	case "warning":
		return "major"
	case "note":
		return "minor"
	case "none":
		return "info"
	default:
		return "info"
	}
}
