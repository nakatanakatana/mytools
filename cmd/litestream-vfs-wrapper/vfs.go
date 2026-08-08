package litestreamvfs

import (
	"context"
	"log/slog"
	"time"

	"github.com/benbjohnson/litestream"
	"github.com/ncruces/go-sqlite3"
	ncrucesvfs "github.com/ncruces/go-sqlite3/vfs"
)

// VFS is a read-only ncruces SQLite VFS backed by a Litestream ReplicaClient.
type VFS struct {
	client    litestream.ReplicaClient
	logger    *slog.Logger
	CacheSize int
	// PollInterval is the interval between replica polls for each open main-database file.
	// New sets it to DefaultPollInterval. PollInterval <= 0 disables live-follow after open.
	PollInterval time.Duration
}

// New returns a VFS that serves database pages from client.
func New(client litestream.ReplicaClient, logger *slog.Logger) *VFS {
	if logger == nil {
		logger = slog.Default()
	}
	return &VFS{
		client:       client,
		logger:       logger,
		CacheSize:    DefaultCacheSize,
		PollInterval: DefaultPollInterval,
	}
}

const (
	// DefaultCacheSize is the default page-cache capacity in bytes.
	DefaultCacheSize = 10 * 1024 * 1024
	// DefaultPollInterval is the default interval between replica polls.
	DefaultPollInterval = time.Second
)

func (v *VFS) Open(name string, flags ncrucesvfs.OpenFlag) (ncrucesvfs.File, ncrucesvfs.OpenFlag, error) {
	if requiresTempFile(flags) {
		return openTempFile(name, flags)
	}
	if flags&ncrucesvfs.OPEN_MAIN_DB == 0 {
		return nil, flags, sqlite3.CANTOPEN
	}

	f, err := openReplicaFile(context.Background(), v.client, name, v.logger, v.CacheSize, v.PollInterval)
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
