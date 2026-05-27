package converter

import (
	"testing"

	"gotest.tools/v3/assert"
)

func TestNormalizePath(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		baseDir  string
		expected string
	}{
		{"Already relative", "file.go", "/root", "file.go"},
		{"Absolute path in baseDir", "/root/src/file.go", "/root", "src/file.go"},
		{"File scheme", "file:///root/src/file.go", "/root", "src/file.go"},
		{"File scheme with relative", "file://src/file.go", "/root", "src/file.go"},
		{"Out of baseDir", "/other/file.go", "/root", "/other/file.go"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			actual := NormalizePath(tt.input, tt.baseDir)
			assert.Equal(t, actual, tt.expected)
		})
	}
}
