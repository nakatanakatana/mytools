package litestreamvfs

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/benbjohnson/litestream"
	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/ncruces/go-sqlite3"
	ncrucesvfs "github.com/ncruces/go-sqlite3/vfs"
	"github.com/superfly/ltx"
)

const (
	pageFetchMaxAttempts    = 3
	pageFetchRetryBaseDelay = 10 * time.Millisecond
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

	pollMu            sync.Mutex
	pollPos           ltx.Pos
	pollMaxTXID1      ltx.TXID
	pollCommit        uint32
	lastPollSuccess   time.Time
	lastPollErr       error
	pending           *replicaUpdate
	visibleGeneration uint64

	cache *lru.Cache[uint32, []byte]
	lock  ncrucesvfs.LockLevel
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

	f.mu.Lock()
	generation := f.visibleGeneration
	pageSize := int64(f.pageSize)
	size := int64(f.commit) * pageSize
	f.mu.Unlock()

	if off >= size {
		return 0, io.EOF
	}

	total := 0
	for total < len(p) && off < size {
		pgno := uint32(off/pageSize) + 1
		pageOff := int(off % pageSize)
		page, err := f.page(pgno, generation)
		if err != nil {
			if total > 0 {
				return total, err
			}
			return 0, err
		}

		f.mu.Lock()
		changed := f.visibleGeneration != generation
		f.mu.Unlock()
		if changed {
			err := ncrucesvfs.SystemError(fmt.Errorf("visible snapshot changed during read"), sqlite3.BUSY)
			if total > 0 {
				return total, err
			}
			return 0, err
		}

		n := copy(p[total:], page[pageOff:])
		// Pretend to be in rollback-journal mode for SQLite readers.
		if off == 0 && total == 0 && n >= 28 {
			p[18], p[19] = 0x01, 0x01
			if _, randErr := rand.Read(p[24:28]); randErr != nil {
				return 0, fmt.Errorf("read change counter rand bytes: %w", randErr)
			}
		}

		total += n
		off += int64(n)
	}
	if total < len(p) {
		return total, io.EOF
	}
	return total, nil
}

func (f *replicaFile) page(pgno uint32, generation uint64) ([]byte, error) {
	f.mu.Lock()
	if generation != f.visibleGeneration {
		f.mu.Unlock()
		return nil, ncrucesvfs.SystemError(fmt.Errorf("visible snapshot changed"), sqlite3.BUSY)
	}
	if data, ok := f.cache.Get(pgno); ok {
		f.mu.Unlock()
		return data, nil
	}
	elem, ok := f.index[pgno]
	f.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("page not found: %d", pgno)
	}

	data, err := f.fetchPageWithRetry(f.ctx, pgno, elem)
	if err != nil {
		return nil, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if generation != f.visibleGeneration {
		return nil, ncrucesvfs.SystemError(fmt.Errorf("visible snapshot changed during page fetch"), sqlite3.BUSY)
	}
	if current, ok := f.index[pgno]; !ok || current != elem {
		return nil, ncrucesvfs.SystemError(fmt.Errorf("page index changed during page fetch"), sqlite3.BUSY)
	}
	if cached, ok := f.cache.Get(pgno); ok {
		return cached, nil
	}
	f.cache.Add(pgno, data)
	return data, nil
}

func (f *replicaFile) fetchPageWithRetry(ctx context.Context, pgno uint32, elem ltx.PageIndexElem) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < pageFetchMaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if attempt > 0 {
			delay := pageFetchRetryBaseDelay << (attempt - 1)
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
			}
		}

		hdr, data, err := litestream.FetchPage(ctx, f.client, elem.Level, elem.MinTXID, elem.MaxTXID, elem.Offset, elem.Size)
		if err != nil {
			lastErr = err
			if !isRetryablePageError(err) {
				return nil, fmt.Errorf("fetch page %d: %w", pgno, err)
			}
			continue
		}
		if hdr.Pgno != pgno {
			return nil, fmt.Errorf("page number mismatch: want %d got %d", pgno, hdr.Pgno)
		}
		if uint32(len(data)) != f.pageSize {
			return nil, fmt.Errorf("page size mismatch: want %d got %d", f.pageSize, len(data))
		}
		return data, nil
	}
	return nil, ncrucesvfs.SystemError(fmt.Errorf("fetch page %d: %w", pgno, lastErr), sqlite3.BUSY)
}

func isRetryablePageError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrNotExist) {
		return true
	}
	if strings.Contains(err.Error(), "unexpected EOF") {
		return true
	}
	var temporary interface{ Temporary() bool }
	return errors.As(err, &temporary) && temporary.Temporary()
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
	if lock == ncrucesvfs.LOCK_NONE && f.pending != nil {
		f.applyUpdateLocked(*f.pending)
		f.pending = nil
	}
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
