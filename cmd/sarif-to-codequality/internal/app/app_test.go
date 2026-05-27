package app

import (
	"bytes"
	"os"
	"testing"

	"gotest.tools/v3/assert"
)

func TestRun_MultipleFiles(t *testing.T) {
	content1 := `{
		"version": "2.1.0",
		"runs": [{"tool": {"driver": {"name": "test1"}}, "results": [{"ruleId": "R1", "message": {"text": "msg1"}, "locations": [{"physicalLocation": {"artifactLocation": {"uri": "f1.go"}, "region": {"startLine": 1}}}]}]}]
	}`
	content2 := `{
		"version": "2.1.0",
		"runs": [{"tool": {"driver": {"name": "test2"}}, "results": [{"ruleId": "R2", "message": {"text": "msg2"}, "locations": [{"physicalLocation": {"artifactLocation": {"uri": "f2.go"}, "region": {"startLine": 2}}}]}]}]
	}`

	tmpfile1, err := os.CreateTemp("", "test1*.sarif")
	assert.NilError(t, err)
	defer func() { _ = os.Remove(tmpfile1.Name()) }()
	_, err = tmpfile1.Write([]byte(content1))
	assert.NilError(t, err)
	_ = tmpfile1.Close()

	tmpfile2, err := os.CreateTemp("", "test2*.sarif")
	assert.NilError(t, err)
	defer func() { _ = os.Remove(tmpfile2.Name()) }()
	_, err = tmpfile2.Write([]byte(content2))
	assert.NilError(t, err)
	_ = tmpfile2.Close()

	var stdout bytes.Buffer
	err = Run([]string{tmpfile1.Name(), tmpfile2.Name()}, nil, "", &stdout)
	assert.NilError(t, err)

	assert.Assert(t, len(stdout.Bytes()) > 0)
	// Check if both issues are present in the output
	assert.Assert(t, bytes.Contains(stdout.Bytes(), []byte("msg1")))
	assert.Assert(t, bytes.Contains(stdout.Bytes(), []byte("msg2")))
}

func TestRun_Stdin(t *testing.T) {
	sarifContent := `{
		"version": "2.1.0",
		"runs": [{"tool": {"driver": {"name": "test"}}, "results": [{"ruleId": "R1", "message": {"text": "msg-stdin"}, "locations": [{"physicalLocation": {"artifactLocation": {"uri": "f.go"}, "region": {"startLine": 1}}}]}]}]
	}`
	stdin := bytes.NewReader([]byte(sarifContent))
	var stdout bytes.Buffer

	err := Run([]string{}, stdin, "", &stdout)
	assert.NilError(t, err)

	assert.Assert(t, bytes.Contains(stdout.Bytes(), []byte("msg-stdin")))
}
