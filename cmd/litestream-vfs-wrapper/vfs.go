package litestreamvfs

import (
	"context"
	"log/slog"

	"github.com/benbjohnson/litestream"
	"github.com/ncruces/go-sqlite3"
	ncrucesvfs "github.com/ncruces/go-sqlite3/vfs"
)

// VFS is a read-only ncruces SQLite VFS backed by a Litestream ReplicaClient.
type VFS struct {
	client    litestream.ReplicaClient
	logger    *slog.Logger
	CacheSize int
}

// New returns a VFS that serves database pages from client.
func New(client litestream.ReplicaClient, logger *slog.Logger) *VFS {
	if logger == nil {
		logger = slog.Default()
	}
	return &VFS{
		client:    client,
		logger:    logger,
		CacheSize: DefaultCacheSize,
	}
}

// DefaultCacheSize is the default page-cache capacity in bytes.
const DefaultCacheSize = 10 * 1024 * 1024

func (v *VFS) Open(name string, flags ncrucesvfs.OpenFlag) (ncrucesvfs.File, ncrucesvfs.OpenFlag, error) {
	if requiresTempFile(flags) {
		return openTempFile(name, flags)
	}
	if flags&ncrucesvfs.OPEN_MAIN_DB == 0 {
		return nil, flags, sqlite3.CANTOPEN
	}

	f, err := openReplicaFile(context.Background(), v.client, name, v.logger, v.CacheSize)
	if err != nil {
		v.logger.Error("open replica file", "name", name, "error", err)
		return nil, flags, sqlite3.CANTOPEN
	}
	// Preserve an explicit OPEN_READONLY request and always expose read-only.
	flags |= ncrucesvfs.OPEN_READONLY
	return f, flags, nil
}

func (v *VFS) Delete(name string, syncDir bool) error {
	_ = name
	_ = syncDir
	return nil
}

func (v *VFS) Access(name string, flags ncrucesvfs.AccessFlag) (bool, error) {
	_ = name
	_ = flags
	return false, nil
}

func (v *VFS) FullPathname(name string) (string, error) {
	return name, nil
}
