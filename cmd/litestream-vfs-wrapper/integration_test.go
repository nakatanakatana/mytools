package litestreamvfs

import (
	"context"
	"testing"

	"github.com/ncruces/go-sqlite3"
	ncrucesvfs "github.com/ncruces/go-sqlite3/vfs"
	"github.com/stretchr/testify/require"
)

func TestSQLiteReadsReplicaThroughRegisteredVFS(t *testing.T) {
	v := New(newReplicaClientStubFromSQLite(t, "CREATE TABLE t (v TEXT); INSERT INTO t VALUES ('ok');"), testLogger())
	const vfsName = "litestream-test"
	ncrucesvfs.Register(vfsName, v)
	t.Cleanup(func() { ncrucesvfs.Unregister(vfsName) })

	db, err := sqlite3.OpenContext(context.Background(), "file:replica.db?vfs="+vfsName+"&mode=ro")
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()

	stmt, _, err := db.Prepare("SELECT v FROM t")
	require.NoError(t, err)
	defer func() { require.NoError(t, stmt.Close()) }()
	require.True(t, stmt.Step())
	require.Equal(t, "ok", stmt.ColumnText(0))
}
