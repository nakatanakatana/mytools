package converter

import (
	"testing"

	"gotest.tools/v3/assert"
)

func TestMapSeverity(t *testing.T) {
	tests := []struct {
		name     string
		level    string
		expected string
	}{
		{"SARIF error to GitLab critical", "error", "critical"},
		{"SARIF warning to GitLab major", "warning", "major"},
		{"SARIF note to GitLab minor", "note", "minor"},
		{"SARIF none to GitLab info", "none", "info"},
		{"Unknown level to GitLab info", "unknown", "info"},
		{"Empty level to GitLab info", "", "info"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			actual := MapSeverity(tt.level)
			assert.Equal(t, actual, tt.expected)
		})
	}
}
