package litestreamvfs

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ncruces/go-sqlite3"
	"github.com/stretchr/testify/require"
)

func newReplicaClientStubFromSQLite(t *testing.T, ddl string) *replicaClientStub {
	t.Helper()
	pages := sqlitePagesFromDDL(t, ddl)
	data, info := encodeTestLTX(t, 1, pages)
	client := newReplicaClientStub()
	client.addFile(info, data)
	return client
}

func sqlitePagesFromDDL(t *testing.T, ddl string) map[uint32][]byte {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "source.db")
	db, err := sqlite3.Open(path)
	require.NoError(t, err)
	require.NoError(t, db.Exec(ddl))
	require.NoError(t, db.Exec("PRAGMA journal_mode=DELETE;"))
	require.NoError(t, db.Close())

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(raw), testPageSize)
	require.Equal(t, 0, len(raw)%testPageSize, "db size must be page-aligned")

	pages := make(map[uint32][]byte)
	pageCount := uint32(len(raw) / testPageSize)
	for i := uint32(0); i < pageCount; i++ {
		pgno := i + 1
		start := int(i) * testPageSize
		page := make([]byte, testPageSize)
		copy(page, raw[start:start+testPageSize])
		pages[pgno] = page
	}
	return pages
}
