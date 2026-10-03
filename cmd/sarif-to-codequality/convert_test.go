package main

import (
	"encoding/json"
	"testing"

	"gotest.tools/v3/assert"
)

func TestConvert(t *testing.T) {
	report := Report{
		Runs: []SARIFRun{
			{
				Results: []Result{
					{
						RuleID: "rule1",
						Message: Message{
							Text: "message1",
						},
						Level: "error",
						Locations: []SARIFLocation{
							{
								PhysicalLocation: PhysicalLocation{
									ArtifactLocation: ArtifactLocation{
										URI: "file:///path/to/file1.go",
									},
									Region: Region{
										StartLine: 10,
									},
								},
							},
						},
					},
					{
						RuleID: "rule2",
						Message: Message{
							Text: "message2",
						},
						Level: "warning",
						Locations: []SARIFLocation{
							{
								PhysicalLocation: PhysicalLocation{
									ArtifactLocation: ArtifactLocation{
										URI: "path/to/file2.go",
									},
									Region: Region{
										StartLine: 20,
									},
								},
							},
						},
					},
					{
						RuleID: "rule3",
						Message: Message{
							Text: "message3",
						},
						Level:     "note",
						Locations: []SARIFLocation{},
					},
				},
			},
		},
	}

	issues := Convert(report, "/path/to")

	assert.Equal(t, len(issues), 2)

	assert.Equal(t, issues[0].CheckName, "rule1")
	assert.Equal(t, issues[0].Description, "message1")
	assert.Equal(t, issues[0].Severity, "critical")
	assert.Equal(t, issues[0].Location.Path, "file1.go")
	assert.Equal(t, issues[0].Location.Lines.Begin, 10)
	assert.Equal(t, issues[0].Fingerprint, GenerateFingerprint("rule1", "file:///path/to/file1.go", "message1"))

	assert.Equal(t, issues[1].CheckName, "rule2")
	assert.Equal(t, issues[1].Description, "message2")
	assert.Equal(t, issues[1].Severity, "major")
	assert.Equal(t, issues[1].Location.Path, "path/to/file2.go")
	assert.Equal(t, issues[1].Location.Lines.Begin, 20)
	assert.Equal(t, issues[1].Fingerprint, GenerateFingerprint("rule2", "path/to/file2.go", "message2"))
}

func TestGenerateFingerprint(t *testing.T) {
	ruleID := "rule-1"
	path := "path/to/file.go"
	message := "This is a test message."

	fp1 := GenerateFingerprint(ruleID, path, message)
	fp2 := GenerateFingerprint(ruleID, path, message)

	assert.Equal(t, fp1, fp2)

	fp3 := GenerateFingerprint("rule-2", path, message)
	assert.Assert(t, fp1 != fp3)

	fp4 := GenerateFingerprint(ruleID, "other/path.go", message)
	assert.Assert(t, fp1 != fp4)

	fp5 := GenerateFingerprint(ruleID, path, "Other message.")
	assert.Assert(t, fp1 != fp5)
}

func TestConvertPath(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		baseDir  string
		expected string
	}{
		{
			name:     "Already relative",
			path:     "pkg/foo/bar.go",
			baseDir:  "/app",
			expected: "pkg/foo/bar.go",
		},
		{
			name:     "Absolute path in baseDir",
			path:     "/app/pkg/foo/bar.go",
			baseDir:  "/app",
			expected: "pkg/foo/bar.go",
		},
		{
			name:     "File scheme",
			path:     "file:///app/pkg/foo/bar.go",
			baseDir:  "/app",
			expected: "pkg/foo/bar.go",
		},
		{
			name:     "File scheme with relative",
			path:     "file://pkg/foo/bar.go",
			baseDir:  "/app",
			expected: "pkg/foo/bar.go",
		},
		{
			name:     "Out of baseDir",
			path:     "/other/pkg/foo/bar.go",
			baseDir:  "/app",
			expected: "/other/pkg/foo/bar.go",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			actual := ConvertPath(tt.path, tt.baseDir)
			assert.Equal(t, actual, tt.expected)
		})
	}
}

func TestConvertSeverity(t *testing.T) {
	tests := []struct {
		name     string
		level    string
		expected string
	}{
		{
			name:     "SARIF error to GitLab critical",
			level:    "error",
			expected: "critical",
		},
		{
			name:     "SARIF warning to GitLab major",
			level:    "warning",
			expected: "major",
		},
		{
			name:     "SARIF note to GitLab minor",
			level:    "note",
			expected: "minor",
		},
		{
			name:     "SARIF none to GitLab info",
			level:    "none",
			expected: "info",
		},
		{
			name:     "Unknown level to GitLab info",
			level:    "unknown",
			expected: "info",
		},
		{
			name:     "Empty level to GitLab info",
			level:    "",
			expected: "info",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			actual := ConvertSeverity(tt.level)
			assert.Equal(t, actual, tt.expected)
		})
	}
}

func TestSARIFUnmarshal(t *testing.T) {
	data := `{
		"$schema": "https://schemastore.azurewebsites.net/schemas/json/sarif-2.1.0-rtm.5.json",
		"version": "2.1.0",
		"runs": [
			{
				"tool": {
					"driver": {
						"name": "ExampleTool"
					}
				},
				"results": [
					{
						"ruleId": "Rule001",
						"message": {
							"text": "Example message"
						},
						"level": "error",
						"locations": [
							{
								"physicalLocation": {
									"artifactLocation": {
										"uri": "file.go"
									},
									"region": {
										"startLine": 10
									}
								}
							}
						]
					}
				]
			}
		]
	}`

	var report Report
	err := json.Unmarshal([]byte(data), &report)
	assert.NilError(t, err)
	assert.Equal(t, report.Version, "2.1.0")
	assert.Equal(t, len(report.Runs), 1)
	assert.Equal(t, report.Runs[0].Tool.Driver.Name, "ExampleTool")
	assert.Equal(t, len(report.Runs[0].Results), 1)
	assert.Equal(t, report.Runs[0].Results[0].RuleID, "Rule001")
	assert.Equal(t, report.Runs[0].Results[0].Level, "error")
	assert.Equal(t, report.Runs[0].Results[0].Locations[0].PhysicalLocation.ArtifactLocation.URI, "file.go")
	assert.Equal(t, report.Runs[0].Results[0].Locations[0].PhysicalLocation.Region.StartLine, 10)
}

func TestGitLabMarshal(t *testing.T) {
	issue := Issue{
		Description: "Example issue",
		Fingerprint: "fingerprint-123",
		Severity:    "major",
		Location: Location{
			Path: "file.go",
			Lines: Lines{
				Begin: 10,
			},
		},
	}

	data, err := json.Marshal(issue)
	assert.NilError(t, err)

	var result map[string]any
	err = json.Unmarshal(data, &result)
	assert.NilError(t, err)

	assert.Equal(t, result["description"], "Example issue")
	assert.Equal(t, result["fingerprint"], "fingerprint-123")
	assert.Equal(t, result["severity"], "major")

	location := result["location"].(map[string]any)
	assert.Equal(t, location["path"], "file.go")

	lines := location["lines"].(map[string]any)
	assert.Equal(t, lines["begin"], float64(10))
}
