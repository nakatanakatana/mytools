package litestreamvfs

import (
	"io"
	"log/slog"
	"testing"

	ncrucesvfs "github.com/ncruces/go-sqlite3/vfs"
	"github.com/stretchr/testify/require"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestOpenTempFileIsWritableAndDeletedOnClose(t *testing.T) {
	v := New(&replicaClientStub{}, testLogger())
	f, _, err := v.Open("", ncrucesvfs.OPEN_TEMP_DB|ncrucesvfs.OPEN_READWRITE|ncrucesvfs.OPEN_DELETEONCLOSE)
	require.NoError(t, err)
	_, err = f.WriteAt([]byte("x"), 0)
	require.NoError(t, err)
	require.NoError(t, f.Close())
}
