package converter

import (
	"path/filepath"
	"strings"
)

// NormalizePath normalizes a file path to be relative to the baseDir.
func NormalizePath(path, baseDir string) string {
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
