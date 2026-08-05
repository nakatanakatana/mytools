package litestreamvfs

import (
	"io"
	"log/slog"
	"testing"

	ncrucesvfs "github.com/ncruces/go-sqlite3/vfs"
)

func TestNewImplementsNcrucesVFS(t *testing.T) {
	var _ ncrucesvfs.VFS = New(&replicaClientStub{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}
