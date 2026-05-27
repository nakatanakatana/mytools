package cli

import (
	"flag"
	"testing"

	"gotest.tools/v3/assert"
)

func TestParseArgs(t *testing.T) {
	args := []string{"-out", "report.json", "input1.sarif", "input2.sarif"}
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	
	parsed, err := Parse(fs, args)
	assert.NilError(t, err)
	assert.Equal(t, parsed.OutFile, "report.json")
	assert.Equal(t, len(parsed.InFiles), 2)
	assert.Equal(t, parsed.InFiles[0], "input1.sarif")
}
