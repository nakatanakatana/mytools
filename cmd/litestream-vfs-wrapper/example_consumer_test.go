package litestreamvfs

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/benbjohnson/litestream/file"
	"github.com/stretchr/testify/require"
	"github.com/superfly/ltx"
)

func TestConsumerBuildsWithoutCGOAndReadsFileReplica(t *testing.T) {
	replica := t.TempDir()
	writeFileReplica(t, replica, "CREATE TABLE t (v TEXT); INSERT INTO t VALUES ('from replica');")

	bin := filepath.Join(t.TempDir(), "consumer")
	build := exec.Command("go", "build", "-buildvcs=false", "-o", bin, ".")
	build.Dir = filepath.Join("..", "..", "example", "litestream-vfs-consumer")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	out, err := build.CombinedOutput()
	require.NoError(t, err, "consumer build failed: %s", out)

	output, err := exec.Command(bin, "-replica", replica, "-database", "app.db", "-query", "SELECT v FROM t").CombinedOutput()
	require.NoError(t, err, "consumer run failed: %s", output)
	require.Equal(t, "from replica\n", string(output))
}

func writeFileReplica(t *testing.T, replicaPath, ddl string) {
	t.Helper()
	pages := sqlitePagesFromDDL(t, ddl)
	ltxBytes, info := encodeTestLTX(t, 1, pages)
	_ = info

	client := file.NewReplicaClient(replicaPath)
	require.NoError(t, client.Init(context.Background()))
	_, err := client.WriteLTXFile(context.Background(), 0, ltx.TXID(1), ltx.TXID(1), bytes.NewReader(ltxBytes))
	require.NoError(t, err)
}
