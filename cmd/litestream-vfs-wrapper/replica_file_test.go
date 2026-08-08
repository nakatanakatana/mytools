package litestreamvfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ncruces/go-sqlite3"
	ncrucesvfs "github.com/ncruces/go-sqlite3/vfs"
	"github.com/stretchr/testify/require"
	"github.com/superfly/ltx"
)

type temporaryPageError struct {
	msg string
}

func (e temporaryPageError) Error() string   { return e.msg }
func (e temporaryPageError) Temporary() bool { return true }

const testPageSize = 4096

func TestReplicaFileReadsTheLatestLTXPage(t *testing.T) {
	client := newReplicaClientStubFromPages(t, map[uint32][]byte{
		1: sqliteHeaderPage(),
		2: []byte("updated page"),
	})
	f, err := openReplicaFile(context.Background(), client, "replica.db", testLogger(), DefaultCacheSize, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })

	buf := make([]byte, len("updated page"))
	_, err = f.ReadAt(buf, int64(f.pageSize))
	require.NoError(t, err)
	require.Equal(t, "updated page", string(buf))
}

func TestReplicaFileRejectsWritesAndReservedLocks(t *testing.T) {
	f := openTestReplicaFile(t)
	_, err := f.WriteAt([]byte("x"), 0)
	require.ErrorIs(t, err, sqlite3.READONLY)
	require.ErrorIs(t, f.Lock(ncrucesvfs.LOCK_RESERVED), sqlite3.READONLY)
}

func TestReplicaFileConcurrentReadAt(t *testing.T) {
	f := openTestReplicaFile(t)
	const goroutines = 32
	var wg sync.WaitGroup
	errCh := make(chan error, goroutines)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 200; n++ {
				buf := make([]byte, len("updated page"))
				if _, err := f.ReadAt(buf, int64(f.pageSize)); err != nil {
					errCh <- err
					return
				}
				if string(buf) != "updated page" {
					errCh <- fmt.Errorf("unexpected page data: %q", buf)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		require.NoError(t, err)
	}
}

func TestReplicaFileConcurrentLockReadUnlockWithPoll(t *testing.T) {
	const (
		readers  = 8
		versions = 20
		iters    = 100
	)

	page1 := sqliteHeaderPage()
	page1[28], page1[29], page1[30], page1[31] = 0, 0, 0, 3
	page2 := bytes.Repeat([]byte{1}, testPageSize)
	page3 := bytes.Repeat([]byte{1}, testPageSize)
	client := newReplicaClientStubFromPages(t, map[uint32][]byte{
		1: page1,
		2: page2,
		3: page3,
	})
	f, err := openReplicaFile(context.Background(), client, "replica.db", testLogger(), DefaultCacheSize, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })

	updates := make([]struct {
		data []byte
		info *ltx.FileInfo
	}, 0, versions-1)
	for ver := 2; ver <= versions; ver++ {
		fill := bytes.Repeat([]byte{byte(ver)}, testPageSize)
		data, info := encodeTestLTX(t, ltx.TXID(ver), map[uint32][]byte{
			2: fill,
			3: bytes.Repeat([]byte{byte(ver)}, testPageSize),
		})
		updates = append(updates, struct {
			data []byte
			info *ltx.FileInfo
		}{data: data, info: info})
	}

	errCh := make(chan error, readers+1)
	var wg sync.WaitGroup
	var inShared atomic.Int32
	var pollsOverlapped atomic.Int32
	var seenOld, seenNew atomic.Bool

	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf2 := make([]byte, testPageSize)
			buf3 := make([]byte, testPageSize)
			for n := 0; n < iters; n++ {
				if lockErr := f.Lock(ncrucesvfs.LOCK_SHARED); lockErr != nil {
					errCh <- lockErr
					return
				}
				inShared.Add(1)
				_, readErr2 := f.ReadAt(buf2, int64(f.pageSize))
				_, readErr3 := f.ReadAt(buf3, int64(2)*int64(f.pageSize))
				unlockErr := f.Unlock(ncrucesvfs.LOCK_NONE)
				inShared.Add(-1)
				if unlockErr != nil {
					errCh <- unlockErr
					return
				}
				if readErr2 != nil {
					if isBusySystemError(readErr2) {
						continue
					}
					errCh <- readErr2
					return
				}
				if readErr3 != nil {
					if isBusySystemError(readErr3) {
						continue
					}
					errCh <- readErr3
					return
				}
				v2, v3 := buf2[0], buf3[0]
				if v2 < 1 || int(v2) > versions || v3 < 1 || int(v3) > versions {
					errCh <- fmt.Errorf("unexpected version bytes %d/%d", v2, v3)
					return
				}
				if err := uniformPage(buf2, v2); err != nil {
					errCh <- err
					return
				}
				if err := uniformPage(buf3, v3); err != nil {
					errCh <- err
					return
				}
				// lock is a level, not a refcount: another Unlock(LOCK_NONE) may
				// publish between the two ReadAts. Retry that iteration.
				if v2 != v3 {
					continue
				}
				if v2 == 1 {
					seenOld.Store(true)
				} else {
					seenNew.Store(true)
				}
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for _, u := range updates {
			client.addLTX(u.info, u.data)
			overlapped := false
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				if inShared.Load() > 0 {
					overlapped = true
					break
				}
				time.Sleep(50 * time.Microsecond)
			}
			if pollErr := f.pollReplicaClient(context.Background()); pollErr != nil {
				errCh <- pollErr
				return
			}
			if overlapped {
				pollsOverlapped.Add(1)
			}
		}
	}()

	wg.Wait()
	close(errCh)
	for err := range errCh {
		require.NoError(t, err)
	}
	require.True(t, seenOld.Load(), "expected at least one consistent read of version 1")
	require.True(t, seenNew.Load(), "expected at least one consistent read of a post-update version")
	require.Greater(t, pollsOverlapped.Load(), int32(0), "expected pollReplicaClient to overlap a SHARED hold")

	// Deterministic multi-page snapshot under one SHARED hold while poll runs.
	const finalVer = versions + 1
	require.NoError(t, f.Lock(ncrucesvfs.LOCK_SHARED))
	assertUniformVersionPages(t, f, byte(versions), 2, 3)
	finalFill := bytes.Repeat([]byte{byte(finalVer)}, testPageSize)
	finalData, finalInfo := encodeTestLTX(t, ltx.TXID(finalVer), map[uint32][]byte{
		2: finalFill,
		3: bytes.Repeat([]byte{byte(finalVer)}, testPageSize),
	})
	client.addLTX(finalInfo, finalData)
	require.NoError(t, f.pollReplicaClient(context.Background()))
	assertUniformVersionPages(t, f, byte(versions), 2, 3)
	require.NoError(t, f.Unlock(ncrucesvfs.LOCK_NONE))
	assertUniformVersionPages(t, f, byte(finalVer), 2, 3)
}

func assertUniformVersionPages(t *testing.T, f *replicaFile, want byte, pageA, pageB uint32) {
	t.Helper()
	bufA := make([]byte, testPageSize)
	bufB := make([]byte, testPageSize)
	_, err := f.ReadAt(bufA, int64(pageA-1)*int64(f.pageSize))
	require.NoError(t, err)
	_, err = f.ReadAt(bufB, int64(pageB-1)*int64(f.pageSize))
	require.NoError(t, err)
	require.NoError(t, uniformPage(bufA, want))
	require.NoError(t, uniformPage(bufB, want))
	require.Equal(t, want, bufA[0])
	require.Equal(t, want, bufB[0])
}

func uniformPage(buf []byte, want byte) error {
	for _, b := range buf {
		if b != want {
			return fmt.Errorf("mixed page bytes: want all %d, saw %d", want, b)
		}
	}
	return nil
}

func TestBuildSnapshot(t *testing.T) {
	tests := []struct {
		name       string
		addFiles   func(t *testing.T, client *replicaClientStub) []*ltx.FileInfo
		wantPos    ltx.TXID
		wantMaxL1  ltx.TXID
		wantCommit uint32
		wantPages  int
	}{
		{
			name: "mixed level restore plan",
			addFiles: func(t *testing.T, client *replicaClientStub) []*ltx.FileInfo {
				baseData, baseInfo := encodeTestLTXRange(t, 1, 1, 2, 3, map[uint32][]byte{
					1: sqliteHeaderPage(),
					2: []byte("base page two"),
					3: []byte("base page three"),
				})
				latestData, latestInfo := encodeTestLTXRange(t, 0, 3, 3, 4, map[uint32][]byte{
					2: []byte("latest page two"),
					4: []byte("latest page four"),
				})
				client.addFile(baseInfo, baseData)
				client.addFile(latestInfo, latestData)
				return []*ltx.FileInfo{baseInfo, latestInfo}
			},
			wantPos:    3,
			wantMaxL1:  2,
			wantCommit: 4,
			wantPages:  4,
		},
		{
			name: "level zero only leaves level one cursor empty",
			addFiles: func(t *testing.T, client *replicaClientStub) []*ltx.FileInfo {
				data, info := encodeTestLTXRange(t, 0, 1, 1, 2, map[uint32][]byte{
					1: sqliteHeaderPage(),
					2: []byte("page two"),
				})
				client.addFile(info, data)
				return []*ltx.FileInfo{info}
			},
			wantPos:    1,
			wantMaxL1:  0,
			wantCommit: 2,
			wantPages:  2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newReplicaClientStub()
			infos := tt.addFiles(t, client)

			f := &replicaFile{client: client}
			snapshot, err := f.buildSnapshot(context.Background(), infos, testPageSize)
			require.NoError(t, err)
			require.Equal(t, tt.wantPos, snapshot.pos.TXID)
			require.Equal(t, tt.wantMaxL1, snapshot.maxTXID1)
			require.Equal(t, tt.wantCommit, snapshot.commit)
			require.Len(t, snapshot.index, tt.wantPages)
		})
	}
}

func TestBuildSnapshotRejectsMismatchedHeader(t *testing.T) {
	tests := []struct {
		name      string
		pageSize  uint32
		mutate    func(*ltx.FileInfo)
		wantError string
	}{
		{
			name:      "page size",
			pageSize:  1024,
			wantError: "page size mismatch",
		},
		{
			name:     "transaction range",
			pageSize: testPageSize,
			mutate: func(info *ltx.FileInfo) {
				info.MaxTXID = 2
			},
			wantError: "transaction range mismatch",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, info := encodeTestLTXWithPageSize(t, tt.pageSize, 0, 1, 1, 1, map[uint32][]byte{
				1: sqliteHeaderPageForSize(tt.pageSize),
			})
			requestedInfo := *info
			if tt.mutate != nil {
				tt.mutate(&requestedInfo)
			}
			client := newReplicaClientStub()
			client.addFile(&requestedInfo, data)

			originalIndex := map[uint32]ltx.PageIndexElem{99: {Level: 9}}
			f := &replicaFile{client: client, index: originalIndex, commit: 99}
			_, err := f.buildSnapshot(context.Background(), []*ltx.FileInfo{&requestedInfo}, testPageSize)
			require.ErrorContains(t, err, tt.wantError)
			require.Equal(t, originalIndex, f.index)
			require.Equal(t, uint32(99), f.commit)
		})
	}
}

func TestOpenReplicaFileInstallsSnapshotState(t *testing.T) {
	client := newReplicaClientStubFromPages(t, map[uint32][]byte{
		1: sqliteHeaderPage(),
		2: []byte("page two"),
	})
	f, err := openReplicaFile(context.Background(), client, "replica.db", testLogger(), DefaultCacheSize, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })

	require.Equal(t, ltx.TXID(1), f.pos.TXID)
	require.Equal(t, ltx.TXID(0), f.maxTXID1)
	require.Equal(t, uint32(2), f.commit)
	require.Equal(t, f.pos, f.pollPos)
	require.Equal(t, f.maxTXID1, f.pollMaxTXID1)
	require.Equal(t, f.commit, f.pollCommit)
}

func TestReplicaFileDefersPollWhileShared(t *testing.T) {
	client := newReplicaClientStubFromPages(t, map[uint32][]byte{
		1: sqliteHeaderPage(),
		2: []byte("page two v1"),
	})
	f, err := openReplicaFile(context.Background(), client, "replica.db", testLogger(), DefaultCacheSize, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })

	oldSize, err := f.Size()
	require.NoError(t, err)
	buf := make([]byte, len("page two v1"))
	_, err = f.ReadAt(buf, int64(f.pageSize))
	require.NoError(t, err)
	require.Equal(t, "page two v1", string(buf))

	require.NoError(t, f.Lock(ncrucesvfs.LOCK_SHARED))

	data, info := encodeTestLTXRange(t, 0, 2, 2, 3, map[uint32][]byte{
		2: []byte("page two v2"),
		3: []byte("page three"),
	})
	client.addLTX(info, data)
	require.NoError(t, f.pollReplicaClient(context.Background()))

	_, err = f.ReadAt(buf, int64(f.pageSize))
	require.NoError(t, err)
	require.Equal(t, "page two v1", string(buf))
	size, err := f.Size()
	require.NoError(t, err)
	require.Equal(t, oldSize, size)
	require.Equal(t, ltx.TXID(1), f.pos.TXID)
	require.NotNil(t, f.pending)

	require.NoError(t, f.Unlock(ncrucesvfs.LOCK_NONE))
	require.Nil(t, f.pending)
	require.Equal(t, ltx.TXID(2), f.pos.TXID)

	buf = make([]byte, len("page two v2"))
	_, err = f.ReadAt(buf, int64(f.pageSize))
	require.NoError(t, err)
	require.Equal(t, "page two v2", string(buf))
	size, err = f.Size()
	require.NoError(t, err)
	require.Equal(t, int64(3)*int64(f.pageSize), size)
}

func TestReplicaFileAccumulatesMultiplePolls(t *testing.T) {
	client := newReplicaClientStubFromPages(t, map[uint32][]byte{
		1: sqliteHeaderPage(),
		2: []byte("page two v1"),
	})
	f, err := openReplicaFile(context.Background(), client, "replica.db", testLogger(), DefaultCacheSize, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })

	require.NoError(t, f.Lock(ncrucesvfs.LOCK_SHARED))

	data2, info2 := encodeTestLTX(t, 2, map[uint32][]byte{
		2: []byte("page two v2"),
	})
	client.addLTX(info2, data2)
	require.NoError(t, f.pollReplicaClient(context.Background()))

	data3, info3 := encodeTestLTX(t, 3, map[uint32][]byte{
		2: []byte("page two v3"),
	})
	client.addLTX(info3, data3)
	require.NoError(t, f.pollReplicaClient(context.Background()))

	require.NotNil(t, f.pending)
	require.Equal(t, ltx.TXID(3), f.pending.pos.TXID)
	require.Equal(t, ltx.TXID(1), f.pos.TXID)

	require.NoError(t, f.Unlock(ncrucesvfs.LOCK_NONE))
	require.Nil(t, f.pending)
	require.Equal(t, ltx.TXID(3), f.pos.TXID)

	buf := make([]byte, len("page two v3"))
	_, err = f.ReadAt(buf, int64(f.pageSize))
	require.NoError(t, err)
	require.Equal(t, "page two v3", string(buf))
}

func TestReplicaFileInvalidatesUpdatedPageOnly(t *testing.T) {
	client := newReplicaClientStubFromPages(t, map[uint32][]byte{
		1: sqliteHeaderPage(),
		2: []byte("page two v1"),
	})
	f, err := openReplicaFile(context.Background(), client, "replica.db", testLogger(), DefaultCacheSize, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })

	hdr := make([]byte, 16)
	_, err = f.ReadAt(hdr, 0)
	require.NoError(t, err)
	page2 := make([]byte, len("page two v1"))
	_, err = f.ReadAt(page2, int64(f.pageSize))
	require.NoError(t, err)
	require.True(t, f.cache.Contains(1))
	require.True(t, f.cache.Contains(2))

	require.NoError(t, f.Lock(ncrucesvfs.LOCK_SHARED))
	data, info := encodeTestLTX(t, 2, map[uint32][]byte{
		2: []byte("page two v2"),
	})
	client.addLTX(info, data)
	require.NoError(t, f.pollReplicaClient(context.Background()))
	require.True(t, f.cache.Contains(1))
	require.True(t, f.cache.Contains(2))

	require.NoError(t, f.Unlock(ncrucesvfs.LOCK_NONE))
	require.True(t, f.cache.Contains(1))
	require.False(t, f.cache.Contains(2))

	before := client.requestCount()
	_, err = f.ReadAt(hdr, 0)
	require.NoError(t, err)
	require.Equal(t, before, client.requestCount())

	page2 = make([]byte, len("page two v2"))
	_, err = f.ReadAt(page2, int64(f.pageSize))
	require.NoError(t, err)
	require.Equal(t, "page two v2", string(page2))
	require.Greater(t, client.requestCount(), before)
}

func TestReplicaFileReplacementPurgesCacheAndShrinks(t *testing.T) {
	client := newReplicaClientStubFromPages(t, map[uint32][]byte{
		1: sqliteHeaderPage(),
		2: []byte("page two"),
		3: []byte("page three"),
	})
	f, err := openReplicaFile(context.Background(), client, "replica.db", testLogger(), DefaultCacheSize, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })
	require.Equal(t, uint32(3), f.commit)

	for _, pgno := range []uint32{1, 2, 3} {
		_, err = f.page(pgno, f.visibleGeneration)
		require.NoError(t, err)
	}
	require.Equal(t, 3, f.cache.Len())

	require.NoError(t, f.Lock(ncrucesvfs.LOCK_SHARED))
	oldSize, err := f.Size()
	require.NoError(t, err)
	require.Equal(t, int64(3)*int64(f.pageSize), oldSize)

	data, info := encodeTestLTXRange(t, 0, 2, 2, 1, map[uint32][]byte{
		1: sqliteHeaderPage(),
	})
	client.addLTX(info, data)
	require.NoError(t, f.pollReplicaClient(context.Background()))

	size, err := f.Size()
	require.NoError(t, err)
	require.Equal(t, oldSize, size)
	require.Equal(t, 3, f.cache.Len())
	require.NotNil(t, f.pending)
	require.True(t, f.pending.replace)

	require.NoError(t, f.Unlock(ncrucesvfs.LOCK_NONE))
	require.Nil(t, f.pending)
	size, err = f.Size()
	require.NoError(t, err)
	require.Equal(t, int64(f.pageSize), size)
	require.Equal(t, uint32(1), f.commit)
	require.Equal(t, 0, f.cache.Len())
	for pgno := range f.index {
		require.LessOrEqual(t, pgno, f.commit)
	}
	_, ok := f.index[2]
	require.False(t, ok)
	_, ok = f.index[3]
	require.False(t, ok)
}

func TestReplicaFileUnlockSharedDoesNotPublish(t *testing.T) {
	client := newReplicaClientStubFromPages(t, map[uint32][]byte{
		1: sqliteHeaderPage(),
		2: []byte("page two v1"),
	})
	f, err := openReplicaFile(context.Background(), client, "replica.db", testLogger(), DefaultCacheSize, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })

	require.NoError(t, f.Lock(ncrucesvfs.LOCK_SHARED))
	data, info := encodeTestLTX(t, 2, map[uint32][]byte{
		2: []byte("page two v2"),
	})
	client.addLTX(info, data)
	require.NoError(t, f.pollReplicaClient(context.Background()))
	require.NotNil(t, f.pending)
	require.Equal(t, ltx.TXID(1), f.pos.TXID)

	require.NoError(t, f.Unlock(ncrucesvfs.LOCK_SHARED))
	require.NotNil(t, f.pending)
	require.Equal(t, ltx.TXID(1), f.pos.TXID)
	require.Equal(t, ncrucesvfs.LOCK_SHARED, f.lock)

	buf := make([]byte, len("page two v1"))
	_, err = f.ReadAt(buf, int64(f.pageSize))
	require.NoError(t, err)
	require.Equal(t, "page two v1", string(buf))

	require.NoError(t, f.Unlock(ncrucesvfs.LOCK_NONE))
	require.Nil(t, f.pending)
	require.Equal(t, ltx.TXID(2), f.pos.TXID)
	buf = make([]byte, len("page two v2"))
	_, err = f.ReadAt(buf, int64(f.pageSize))
	require.NoError(t, err)
	require.Equal(t, "page two v2", string(buf))
}

func TestReplicaFileRetriesPageFetch(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		failures  int
		wantCalls int
		wantOK    bool
	}{
		{name: "unexpected EOF once", err: io.ErrUnexpectedEOF, failures: 1, wantCalls: 2, wantOK: true},
		{name: "deadline exceeded twice", err: context.DeadlineExceeded, failures: 2, wantCalls: 3, wantOK: true},
		{name: "wrapped not exist once", err: fmt.Errorf("backend missing: %w", os.ErrNotExist), failures: 1, wantCalls: 2, wantOK: true},
		{name: "temporary once", err: temporaryPageError{msg: "backend busy"}, failures: 1, wantCalls: 2, wantOK: true},
		{name: "unexpected EOF message once", err: fmt.Errorf("s3: unexpected EOF"), failures: 1, wantCalls: 2, wantOK: true},
		{name: "exhausts unexpected EOF", err: io.ErrUnexpectedEOF, failures: 10, wantCalls: 3, wantOK: false},
		{name: "exhausts deadline", err: context.DeadlineExceeded, failures: 10, wantCalls: 3, wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newReplicaClientStubFromPages(t, map[uint32][]byte{
				1: sqliteHeaderPage(),
				2: []byte("updated page"),
			})
			f, err := openReplicaFile(context.Background(), client, "replica.db", testLogger(), DefaultCacheSize, 0)
			require.NoError(t, err)
			t.Cleanup(func() { _ = f.Close() })

			var opens atomic.Int32
			var armFailingOpen func()
			armFailingOpen = func() {
				client.OpenLTXFileFunc = func(ctx context.Context, level int, minTXID, maxTXID ltx.TXID, offset, size int64) (io.ReadCloser, error) {
					n := int(opens.Add(1))
					if n <= tt.failures {
						client.mu.Lock()
						armFailingOpen()
						client.mu.Unlock()
						return nil, tt.err
					}
					return client.openLTXFileData(level, minTXID, maxTXID, offset, size)
				}
			}
			armFailingOpen()

			buf := make([]byte, len("updated page"))
			_, err = f.ReadAt(buf, int64(f.pageSize))
			require.Equal(t, tt.wantCalls, int(opens.Load()))
			if tt.wantOK {
				require.NoError(t, err)
				require.Equal(t, "updated page", string(buf))
				return
			}
			require.Error(t, err)
			require.True(t, isBusySystemError(err), "want temporary/BUSY error, got %T %v", err, err)
			require.False(t, f.cache.Contains(2))
		})
	}
}

func TestReplicaFileStopsRetryingPermanentPageError(t *testing.T) {
	client := newReplicaClientStubFromPages(t, map[uint32][]byte{
		1: sqliteHeaderPage(),
		2: []byte("updated page"),
	})
	f, err := openReplicaFile(context.Background(), client, "replica.db", testLogger(), DefaultCacheSize, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })

	permanent := fmt.Errorf("permanent backend failure")
	var opens atomic.Int32
	client.OpenLTXFileFunc = func(context.Context, int, ltx.TXID, ltx.TXID, int64, int64) (io.ReadCloser, error) {
		opens.Add(1)
		return nil, permanent
	}

	buf := make([]byte, len("updated page"))
	_, err = f.ReadAt(buf, int64(f.pageSize))
	require.Error(t, err)
	require.ErrorContains(t, err, permanent.Error())
	require.Equal(t, 1, int(opens.Load()))
	require.False(t, isBusySystemError(err))
}

func TestReplicaFileCloseCancelsPageFetch(t *testing.T) {
	client := newReplicaClientStubFromPages(t, map[uint32][]byte{
		1: sqliteHeaderPage(),
		2: []byte("updated page"),
	})
	f, err := openReplicaFile(context.Background(), client, "replica.db", testLogger(), DefaultCacheSize, 0)
	require.NoError(t, err)

	entered := make(chan struct{})
	var opens atomic.Int32
	client.OpenLTXFileFunc = func(ctx context.Context, level int, minTXID, maxTXID ltx.TXID, offset, size int64) (io.ReadCloser, error) {
		opens.Add(1)
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}

	errCh := make(chan error, 1)
	go func() {
		buf := make([]byte, len("updated page"))
		_, readErr := f.ReadAt(buf, int64(f.pageSize))
		errCh <- readErr
	}()

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for page fetch to start")
	}

	require.NoError(t, f.Close())

	select {
	case readErr := <-errCh:
		require.Error(t, readErr)
		require.True(t, errors.Is(readErr, context.Canceled), "want context.Canceled, got %v", readErr)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for canceled ReadAt")
	}
	require.Equal(t, 1, int(opens.Load()), "canceled fetch must not retry")
	require.Equal(t, 0, client.activeRequests())
}

func TestReplicaFileDoesNotCacheOldPageAfterPublish(t *testing.T) {
	client := newReplicaClientStubFromPages(t, map[uint32][]byte{
		1: sqliteHeaderPage(),
		2: []byte("page two v1"),
	})
	f, err := openReplicaFile(context.Background(), client, "replica.db", testLogger(), DefaultCacheSize, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })

	entered := make(chan struct{})
	release := make(chan struct{})
	client.OpenLTXFileFunc = func(ctx context.Context, level int, minTXID, maxTXID ltx.TXID, offset, size int64) (io.ReadCloser, error) {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return client.openLTXFileData(level, minTXID, maxTXID, offset, size)
	}

	errCh := make(chan error, 1)
	go func() {
		buf := make([]byte, len("page two v1"))
		n, readErr := f.ReadAt(buf, int64(f.pageSize))
		if readErr == nil && string(buf[:n]) == "page two v1" {
			errCh <- fmt.Errorf("stale page bytes returned successfully")
			return
		}
		errCh <- readErr
	}()

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for old page fetch")
	}

	data, info := encodeTestLTX(t, 2, map[uint32][]byte{
		2: []byte("page two v2"),
	})
	client.addLTX(info, data)
	require.NoError(t, f.pollReplicaClient(context.Background()))
	require.Equal(t, ltx.TXID(2), f.pos.TXID)
	require.False(t, f.cache.Contains(2))

	close(release)

	select {
	case readErr := <-errCh:
		require.Error(t, readErr)
		require.True(t, isBusySystemError(readErr), "want temporary/BUSY after generation change, got %T %v", readErr, readErr)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for stale ReadAt to finish")
	}

	require.False(t, f.cache.Contains(2), "stale page must not be cached")

	buf := make([]byte, len("page two v2"))
	_, err = f.ReadAt(buf, int64(f.pageSize))
	require.NoError(t, err)
	require.Equal(t, "page two v2", string(buf))
}

// isBusySystemError detects ncrucesvfs.SystemError(..., sqlite3.BUSY). The
// concrete type is unexported, so match by package-qualified type name.
func isBusySystemError(err error) bool {
	for e := err; e != nil; e = errors.Unwrap(e) {
		if fmt.Sprintf("%T", e) == "vfs.sysError" {
			return true
		}
	}
	// Embedded cause unwrap may skip the sysError value when only the cause is exposed.
	return fmt.Sprintf("%T", err) == "vfs.sysError"
}

func openTestReplicaFile(t *testing.T) *replicaFile {
	t.Helper()
	client := newReplicaClientStubFromPages(t, map[uint32][]byte{
		1: sqliteHeaderPage(),
		2: []byte("updated page"),
	})
	f, err := openReplicaFile(context.Background(), client, "replica.db", testLogger(), DefaultCacheSize, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func newReplicaClientStubFromPages(t *testing.T, pages map[uint32][]byte) *replicaClientStub {
	t.Helper()
	data, info := encodeTestLTX(t, 1, pages)
	client := newReplicaClientStub()
	client.addFile(info, data)
	return client
}

func encodeTestLTX(t *testing.T, txid ltx.TXID, pages map[uint32][]byte) ([]byte, *ltx.FileInfo) {
	return encodeTestLTXRange(t, 0, txid, txid, maxPageNumber(pages), pages)
}

func encodeTestLTXRange(t *testing.T, level int, minTXID, maxTXID ltx.TXID, commit uint32, pages map[uint32][]byte) ([]byte, *ltx.FileInfo) {
	return encodeTestLTXWithPageSize(t, testPageSize, level, minTXID, maxTXID, commit, pages)
}

func encodeTestLTXWithPageSize(t *testing.T, pageSize uint32, level int, minTXID, maxTXID ltx.TXID, commit uint32, pages map[uint32][]byte) ([]byte, *ltx.FileInfo) {
	t.Helper()
	require.NotEmpty(t, pages)

	pgnos := make([]uint32, 0, len(pages))
	for pgno := range pages {
		pgnos = append(pgnos, pgno)
	}
	// Encode pages in ascending order.
	for i := 0; i < len(pgnos); i++ {
		for j := i + 1; j < len(pgnos); j++ {
			if pgnos[j] < pgnos[i] {
				pgnos[i], pgnos[j] = pgnos[j], pgnos[i]
			}
		}
	}

	var buf bytes.Buffer
	enc, err := ltx.NewEncoder(&buf)
	require.NoError(t, err)
	hdr := ltx.Header{
		Version:   ltx.Version,
		PageSize:  pageSize,
		Commit:    commit,
		MinTXID:   minTXID,
		MaxTXID:   maxTXID,
		Timestamp: time.Now().UnixMilli(),
		Flags:     ltx.HeaderFlagNoChecksum,
	}
	require.NoError(t, enc.EncodeHeader(hdr))
	for _, pgno := range pgnos {
		page := make([]byte, pageSize)
		copy(page, pages[pgno])
		require.NoError(t, enc.EncodePage(ltx.PageHeader{Pgno: pgno}, page))
	}
	require.NoError(t, enc.Close())

	info := &ltx.FileInfo{
		Level:     level,
		MinTXID:   minTXID,
		MaxTXID:   maxTXID,
		Size:      int64(buf.Len()),
		CreatedAt: time.Now().UTC(),
	}
	return buf.Bytes(), info
}

func maxPageNumber(pages map[uint32][]byte) uint32 {
	var max uint32
	for pgno := range pages {
		if pgno > max {
			max = pgno
		}
	}
	return max
}

func sqliteHeaderPage() []byte {
	return sqliteHeaderPageForSize(testPageSize)
}

func sqliteHeaderPageForSize(pageSize uint32) []byte {
	page := make([]byte, pageSize)
	copy(page, "SQLite format 3\x00")
	// Page size big-endian at offset 16.
	page[16], page[17] = byte(pageSize>>8), byte(pageSize)
	// Force journal mode bytes; VFS remasks these on read of page 1.
	page[18], page[19] = 0x01, 0x01
	// File change counter etc. left zero.
	// Database size in pages (uint32 BE at offset 28).
	page[28], page[29], page[30], page[31] = 0, 0, 0, 2
	return page
}
