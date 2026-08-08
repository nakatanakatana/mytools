package litestreamvfs

import (
	"context"
	"testing"
	"time"

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

func TestSQLiteLiveFollowWithoutReopen(t *testing.T) {
	before := sqlitePagesFromDDL(t, "CREATE TABLE t (v TEXT); INSERT INTO t VALUES ('ok');")
	data, info := encodeTestLTX(t, 1, before)
	client := newReplicaClientStub()
	client.addFile(info, data)

	v := New(client, testLogger())
	v.PollInterval = 10 * time.Millisecond
	vfsName := uniqueVFSName(t)
	ncrucesvfs.Register(vfsName, v)
	t.Cleanup(func() { ncrucesvfs.Unregister(vfsName) })

	db, err := sqlite3.OpenContext(context.Background(), "file:replica.db?vfs="+vfsName+"&mode=ro")
	require.NoError(t, err)

	stmt, _, err := db.Prepare("SELECT v FROM t")
	require.NoError(t, err)
	require.True(t, stmt.Step())
	require.Equal(t, "ok", stmt.ColumnText(0))
	require.NoError(t, stmt.Close())

	after := sqlitePagesAfterSQL(t, before, "UPDATE t SET v = 'updated';")
	diff, diffInfo := sqliteLTXDiff(t, 2, before, after)
	client.addLTX(diffInfo, diff)

	require.Eventually(t, func() bool {
		s, _, prepErr := db.Prepare("SELECT v FROM t")
		require.NoError(t, prepErr)
		defer s.Close()
		return s.Step() && s.ColumnText(0) == "updated"
	}, time.Second, 10*time.Millisecond)

	require.NoError(t, db.Close())
	require.Eventually(t, func() bool { return client.activeRequests() == 0 }, time.Second, 5*time.Millisecond)
	baseline := client.requestCount()
	var stableSamples int
	require.Eventually(t, func() bool {
		if client.requestCount() != baseline || client.activeRequests() != 0 {
			stableSamples = 0
			baseline = client.requestCount()
			return false
		}
		stableSamples++
		return stableSamples >= 3
	}, time.Second, v.PollInterval)
}

func TestSQLiteTransactionSnapshotIsStable(t *testing.T) {
	before := sqlitePagesFromDDL(t, "CREATE TABLE t (v TEXT); INSERT INTO t VALUES ('ok');")
	data, info := encodeTestLTX(t, 1, before)
	client := newReplicaClientStub()
	client.addFile(info, data)

	v := New(client, testLogger())
	v.PollInterval = 10 * time.Millisecond
	vfsName := uniqueVFSName(t)
	ncrucesvfs.Register(vfsName, v)
	t.Cleanup(func() { ncrucesvfs.Unregister(vfsName) })

	db, err := sqlite3.OpenContext(context.Background(), "file:replica.db?vfs="+vfsName+"&mode=ro")
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()

	tx := db.Begin()
	stmt, _, err := db.Prepare("SELECT v FROM t")
	require.NoError(t, err)
	require.True(t, stmt.Step())
	require.Equal(t, "ok", stmt.ColumnText(0))

	listsBefore := client.listCount(0, 2)
	after := sqlitePagesAfterSQL(t, before, "UPDATE t SET v = 'updated';")
	diff, diffInfo := sqliteLTXDiff(t, 2, before, after)
	client.addLTX(diffInfo, diff)

	require.Eventually(t, func() bool {
		return client.listCount(0, 2) > listsBefore
	}, time.Second, 5*time.Millisecond)

	// Second statement on the same open transaction re-reads through the VFS
	// while SHARED is still held (ColumnText on the first stepped row would not).
	stmt2, _, err := db.Prepare("SELECT v FROM t")
	require.NoError(t, err)
	require.True(t, stmt2.Step())
	require.Equal(t, "ok", stmt2.ColumnText(0))
	require.NoError(t, stmt2.Close())

	require.NoError(t, stmt.Close())
	require.NoError(t, tx.Commit())

	require.Eventually(t, func() bool {
		s, _, prepErr := db.Prepare("SELECT v FROM t")
		require.NoError(t, prepErr)
		defer s.Close()
		return s.Step() && s.ColumnText(0) == "updated"
	}, time.Second, 10*time.Millisecond)
}

func uniqueVFSName(t *testing.T) string {
	t.Helper()
	return "litestream-" + t.Name()
}
