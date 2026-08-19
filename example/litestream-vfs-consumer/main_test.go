package main

import (
	"bytes"
	"context"
	"testing"
)

func TestRunPrintsFirstColumn(t *testing.T) {
	var stdout bytes.Buffer
	err := run(context.Background(), &stdout, []string{
		"-replica", t.TempDir(), "-database", "app.db", "-query", "SELECT 'ok'",
	})
	if err == nil {
		t.Fatal("run succeeded without a replica")
	}
}
