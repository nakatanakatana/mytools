package app_test

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/nakatanakatana/mytools/cmd/sarif-to-codequality/internal/app"
	"gotest.tools/v3/assert"
	"gotest.tools/v3/golden"
)

func TestIntegration_SARIFToCodeQuality(t *testing.T) {
	testCases := []struct {
		name     string
		inFiles  []string
		golden   string
	}{
		{
			name:    "simple",
			inFiles: []string{"testdata/simple.sarif"},
			golden:  "simple.json",
		},
		{
			name:    "multiple",
			inFiles: []string{"testdata/simple.sarif", "testdata/extra.sarif"},
			golden:  "multiple.json",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var inPaths []string
			for _, f := range tc.inFiles {
				inPaths = append(inPaths, filepath.Join("testdata", filepath.Base(f)))
			}

			var stdout bytes.Buffer
			err := app.Run(inPaths, nil, "", &stdout)
			assert.NilError(t, err)

			golden.Assert(t, stdout.String(), tc.golden)
		})
	}
}
