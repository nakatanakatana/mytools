package litestreamvfs

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/benbjohnson/litestream"
	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/ncruces/go-sqlite3"
	ncrucesvfs "github.com/ncruces/go-sqlite3/vfs"
	"github.com/superfly/ltx"
)

type replicaFile struct {
	client litestream.ReplicaClient
	name   string
	logger *slog.Logger
	ctx    context.Context
	cancel context.CancelFunc

	wg        sync.WaitGroup
	closeOnce sync.Once

	mu       sync.Mutex
	pageSize uint32
	commit   uint32
	index    map[uint32]ltx.PageIndexElem
	pos      ltx.Pos
	maxTXID1 ltx.TXID

	pollPos      ltx.Pos
	pollMaxTXID1 ltx.TXID
	pollCommit   uint32
	cache        *lru.Cache[uint32, []byte]
	lock         ncrucesvfs.LockLevel
}

type replicaSnapshot struct {
	index    map[uint32]ltx.PageIndexElem
	commit   uint32
	pos      ltx.Pos
	maxTXID1 ltx.TXID
}

type replicaUpdate struct {
	index    map[uint32]ltx.PageIndexElem
	replace  bool
	commit   uint32
	pos      ltx.Pos
	maxTXID1 ltx.TXID
}

func openReplicaFile(ctx context.Context, client litestream.ReplicaClient, name string, logger *slog.Logger, cacheSize int, pollInterval time.Duration) (*replicaFile, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if cacheSize <= 0 {
		cacheSize = DefaultCacheSize
	}

	infos, err := litestream.CalcRestorePlan(ctx, client, 0, time.Time{}, logger)
	if err != nil {
		return nil, fmt.Errorf("restore plan: %w", err)
	}
	if len(infos) == 0 {
		return nil, fmt.Errorf("restore plan: empty")
	}

	pageSize, err := detectPageSize(ctx, client, infos)
	if err != nil {
		return nil, err
	}

	entries := cacheSize / int(pageSize)
	if entries < 1 {
		entries = 1
	}
	cache, err := lru.New[uint32, []byte](entries)
	if err != nil {
		return nil, fmt.Errorf("create page cache: %w", err)
	}

	fileCtx, cancel := context.WithCancel(ctx)
	f := &replicaFile{
		client:   client,
		name:     name,
		logger:   logger.With("name", name),
		ctx:      fileCtx,
		cancel:   cancel,
		pageSize: pageSize,
		index:    make(map[uint32]ltx.PageIndexElem),
		cache:    cache,
	}
	snapshot, err := f.buildSnapshot(fileCtx, infos, pageSize)
	if err != nil {
		cancel()
		return nil, err
	}
	f.index = snapshot.index
	f.commit = snapshot.commit
	f.pos = snapshot.pos
	f.maxTXID1 = snapshot.maxTXID1
	f.pollPos = snapshot.pos
	f.pollMaxTXID1 = snapshot.maxTXID1
	f.pollCommit = snapshot.commit
	if pollInterval > 0 {
		f.wg.Add(1)
		go func() {
			defer f.wg.Done()
			f.monitorReplicaClient(fileCtx, pollInterval)
		}()
	}
	return f, nil
}

func detectPageSize(ctx context.Context, client litestream.ReplicaClient, infos []*ltx.FileInfo) (uint32, error) {
	var lastErr error
	for i := len(infos) - 1; i >= 0; i-- {
		hdr, err := litestream.FetchLTXHeader(ctx, client, infos[i])
		if err != nil {
			lastErr = err
			continue
		}
		if !isSupportedPageSize(hdr.PageSize) {
			return 0, fmt.Errorf("unsupported page size: %d", hdr.PageSize)
		}
		return hdr.PageSize, nil
	}
	if lastErr != nil {
		return 0, fmt.Errorf("fetch ltx header: %w", lastErr)
	}
	return 0, fmt.Errorf("no ltx file available to determine page size")
}

func isSupportedPageSize(pageSize uint32) bool {
	switch pageSize {
	case 512, 1024, 2048, 4096, 8192, 16384, 32768, 65536:
		return true
	default:
		return false
	}
}

func (f *replicaFile) buildSnapshot(ctx context.Context, infos []*ltx.FileInfo, pageSize uint32) (replicaSnapshot, error) {
	snapshot := replicaSnapshot{index: make(map[uint32]ltx.PageIndexElem)}
	for _, info := range infos {
		idx, err := litestream.FetchPageIndex(ctx, f.client, info)
		if err != nil {
			return replicaSnapshot{}, fmt.Errorf("fetch page index: %w", err)
		}
		hdr, err := litestream.FetchLTXHeader(ctx, f.client, info)
		if err != nil {
			return replicaSnapshot{}, fmt.Errorf("fetch header: %w", err)
		}
		if hdr.PageSize != pageSize {
			return replicaSnapshot{}, fmt.Errorf("page size mismatch: want %d got %d", pageSize, hdr.PageSize)
		}
		if hdr.MinTXID != info.MinTXID || hdr.MaxTXID != info.MaxTXID {
			return replicaSnapshot{}, fmt.Errorf("transaction range mismatch: file info %s-%s header %s-%s", info.MinTXID, info.MaxTXID, hdr.MinTXID, hdr.MaxTXID)
		}
		for pgno, elem := range idx {
			snapshot.index[pgno] = elem
		}
		snapshot.commit = hdr.Commit
		if info.MaxTXID > snapshot.pos.TXID {
			snapshot.pos = info.Pos()
		}
		if info.Level == 1 && info.MaxTXID > snapshot.maxTXID1 {
			snapshot.maxTXID1 = info.MaxTXID
		}
	}
	return snapshot, nil
}

func (f *replicaFile) Close() error {
	f.closeOnce.Do(func() {
		f.cancel()
		f.wg.Wait()
	})
	return nil
}

func (f *replicaFile) Size() (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return int64(f.commit) * int64(f.pageSize), nil
}

func (f *replicaFile) ReadAt(p []byte, off int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	size, err := f.Size()
	if err != nil {
		return 0, err
	}
	if off >= size {
		return 0, io.EOF
	}

	pageSize := int64(f.pageSize)
	total := 0
	for total < len(p) && off < size {
		pgno := uint32(off/pageSize) + 1
		pageOff := int(off % pageSize)
		page, err := f.page(pgno)
		if err != nil {
			if total > 0 {
				return total, err
			}
			return 0, err
		}

		n := copy(p[total:], page[pageOff:])
		// Pretend to be in rollback-journal mode for SQLite readers.
		if off == 0 && total == 0 && n >= 28 {
			p[18], p[19] = 0x01, 0x01
			_, _ = rand.Read(p[24:28])
		}

		total += n
		off += int64(n)
	}
	if total < len(p) {
		return total, io.EOF
	}
	return total, nil
}

func (f *replicaFile) page(pgno uint32) ([]byte, error) {
	f.mu.Lock()
	if data, ok := f.cache.Get(pgno); ok {
		f.mu.Unlock()
		return data, nil
	}
	elem, ok := f.index[pgno]
	f.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("page not found: %d", pgno)
	}

	hdr, data, err := litestream.FetchPage(context.Background(), f.client, elem.Level, elem.MinTXID, elem.MaxTXID, elem.Offset, elem.Size)
	if err != nil {
		return nil, fmt.Errorf("fetch page %d: %w", pgno, err)
	}
	if hdr.Pgno != pgno {
		return nil, fmt.Errorf("page number mismatch: want %d got %d", pgno, hdr.Pgno)
	}
	if uint32(len(data)) != f.pageSize {
		return nil, fmt.Errorf("page size mismatch: want %d got %d", f.pageSize, len(data))
	}

	f.mu.Lock()
	if cached, ok := f.cache.Get(pgno); ok {
		f.mu.Unlock()
		return cached, nil
	}
	f.cache.Add(pgno, data)
	f.mu.Unlock()
	return data, nil
}

func (f *replicaFile) WriteAt(p []byte, off int64) (int, error) {
	_ = p
	_ = off
	return 0, sqlite3.READONLY
}

func (f *replicaFile) Truncate(size int64) error {
	_ = size
	return sqlite3.READONLY
}

func (f *replicaFile) Sync(flags ncrucesvfs.SyncFlag) error {
	_ = flags
	return nil
}

func (f *replicaFile) Lock(lock ncrucesvfs.LockLevel) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if lock < f.lock {
		return fmt.Errorf("invalid lock downgrade")
	}
	if lock >= ncrucesvfs.LOCK_RESERVED {
		return sqlite3.READONLY
	}
	f.lock = lock
	return nil
}

func (f *replicaFile) Unlock(lock ncrucesvfs.LockLevel) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if lock != ncrucesvfs.LOCK_NONE && lock != ncrucesvfs.LOCK_SHARED {
		return fmt.Errorf("invalid unlock target")
	}
	f.lock = lock
	return nil
}

func (f *replicaFile) CheckReservedLock() (bool, error) {
	return false, nil
}

func (f *replicaFile) SectorSize() int {
	return 4096
}

func (f *replicaFile) DeviceCharacteristics() ncrucesvfs.DeviceCharacteristic {
	return 0
}

var _ ncrucesvfs.File = (*replicaFile)(nil)
