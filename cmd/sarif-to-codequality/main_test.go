package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"gotest.tools/v3/assert"
)

func TestParseArgs(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	args, err := ParseArgs(fs, []string{"-out", "output.json", "input1.sarif", "input2.sarif"})
	assert.NilError(t, err)
	assert.Equal(t, args.OutFile, "output.json")
	assert.DeepEqual(t, args.InFiles, []string{"input1.sarif", "input2.sarif"})
}

func TestLoadConfig(t *testing.T) {
	_ = os.Setenv("BASE_DIR", "/root")
	defer func() { _ = os.Unsetenv("BASE_DIR") }()

	cfg, err := LoadConfig()
	assert.NilError(t, err)
	assert.Equal(t, cfg.BaseDir, "/root")
}

func TestRun_Stdin(t *testing.T) {
	sarifJSON := `{
		"version": "2.1.0",
		"runs": [
			{
				"tool": {
					"driver": {
						"name": "TestTool"
					}
				},
				"results": [
					{
						"ruleId": "TEST01",
						"message": {
							"text": "test error"
						},
						"locations": [
							{
								"physicalLocation": {
									"artifactLocation": {
										"uri": "main.go"
									},
									"region": {
										"startLine": 42
									}
								}
							}
						]
					}
				]
			}
		]
	}`

	in := bytes.NewBufferString(sarifJSON)
	var out bytes.Buffer

	err := Run(nil, in, ".", &out)
	assert.NilError(t, err)

	var issues []Issue
	err = json.Unmarshal(out.Bytes(), &issues)
	assert.NilError(t, err)

	assert.Equal(t, len(issues), 1)
	assert.Equal(t, issues[0].CheckName, "TEST01")
	assert.Equal(t, issues[0].Description, "test error")
	assert.Equal(t, issues[0].Location.Path, "main.go")
	assert.Equal(t, issues[0].Location.Lines.Begin, 42)
}

func TestRun_MultipleFiles(t *testing.T) {
	tmpDir := t.TempDir()
	f1 := filepath.Join(tmpDir, "1.sarif")
	f2 := filepath.Join(tmpDir, "2.sarif")

	sarif1 := `{
		"version": "2.1.0",
		"runs": [{
			"tool": {"driver": {"name": "T1"}},
			"results": [{
				"ruleId": "R1",
				"message": {"text": "m1"},
				"locations": [{"physicalLocation": {"artifactLocation": {"uri": "a.go"}, "region": {"startLine": 1}}}]
			}]
		}]
	}`
	sarif2 := `{
		"version": "2.1.0",
		"runs": [{
			"tool": {"driver": {"name": "T2"}},
			"results": [{
				"ruleId": "R2",
				"message": {"text": "m2"},
				"locations": [{"physicalLocation": {"artifactLocation": {"uri": "b.go"}, "region": {"startLine": 2}}}]
			}]
		}]
	}`

	assert.NilError(t, os.WriteFile(f1, []byte(sarif1), 0644))
	assert.NilError(t, os.WriteFile(f2, []byte(sarif2), 0644))

	var out bytes.Buffer
	err := Run([]string{f1, f2}, nil, ".", &out)
	assert.NilError(t, err)

	var issues []Issue
	err = json.Unmarshal(out.Bytes(), &issues)
	assert.NilError(t, err)

	assert.Equal(t, len(issues), 2)
	assert.Equal(t, issues[0].CheckName, "R1")
	assert.Equal(t, issues[1].CheckName, "R2")
}

func TestIntegration_SARIFToCodeQuality(t *testing.T) {
	tests := []struct {
		name         string
		sarifFiles   []string
		expectedJSON string
	}{
		{
			name:         "simple",
			sarifFiles:   []string{"testdata/simple.sarif"},
			expectedJSON: "testdata/simple.json",
		},
		{
			name:         "multiple",
			sarifFiles:   []string{"testdata/simple.sarif", "testdata/extra.sarif"},
			expectedJSON: "testdata/multiple.json",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			err := Run(tt.sarifFiles, nil, "/path/to", &out)
			assert.NilError(t, err)

			expected, err := os.ReadFile(tt.expectedJSON)
			assert.NilError(t, err)

			var actualIssues, expectedIssues []Issue
			err = json.Unmarshal(out.Bytes(), &actualIssues)
			assert.NilError(t, err)

			err = json.Unmarshal(expected, &expectedIssues)
			assert.NilError(t, err)

			assert.DeepEqual(t, actualIssues, expectedIssues)
		})
	}
}
