package litestreamvfs

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/ncruces/go-sqlite3"
	"github.com/stretchr/testify/require"
	"github.com/superfly/ltx"
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

	return sqlitePagesFromFile(t, path)
}

// sqlitePagesAfterSQL writes pages to a temporary database, runs sql, and
// returns the resulting page map.
func sqlitePagesAfterSQL(t *testing.T, pages map[uint32][]byte, sql string) map[uint32][]byte {
	t.Helper()
	require.NotEmpty(t, pages)

	dir := t.TempDir()
	path := filepath.Join(dir, "mutate.db")
	require.NoError(t, os.WriteFile(path, pagesToBytes(t, pages), 0o644))

	db, err := sqlite3.Open(path)
	require.NoError(t, err)
	require.NoError(t, db.Exec(sql))
	require.NoError(t, db.Close())

	return sqlitePagesFromFile(t, path)
}

// sqliteLTXDiff encodes pages that differ between before and after. Commit is
// the after database page count.
func sqliteLTXDiff(t *testing.T, txid ltx.TXID, before, after map[uint32][]byte) ([]byte, *ltx.FileInfo) {
	t.Helper()
	require.NotEmpty(t, after)

	changed := make(map[uint32][]byte)
	for pgno, afterPage := range after {
		beforePage, ok := before[pgno]
		if !ok || !bytes.Equal(beforePage, afterPage) {
			page := make([]byte, len(afterPage))
			copy(page, afterPage)
			changed[pgno] = page
		}
	}
	require.NotEmpty(t, changed, "expected at least one changed page")

	commit := uint32(len(after))
	if max := maxPageNumber(after); max > commit {
		commit = max
	}
	return encodeTestLTXRange(t, 0, txid, txid, commit, changed)
}

func sqlitePagesFromFile(t *testing.T, path string) map[uint32][]byte {
	t.Helper()

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

func pagesToBytes(t *testing.T, pages map[uint32][]byte) []byte {
	t.Helper()
	n := maxPageNumber(pages)
	require.Positive(t, n)
	raw := make([]byte, int(n)*testPageSize)
	for pgno, page := range pages {
		require.Equal(t, testPageSize, len(page))
		copy(raw[int(pgno-1)*testPageSize:], page)
	}
	return raw
}
