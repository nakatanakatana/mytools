package litestreamvfs

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/ncruces/go-sqlite3"
	ncrucesvfs "github.com/ncruces/go-sqlite3/vfs"
	"github.com/stretchr/testify/require"
	"github.com/superfly/ltx"
)

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
		_, err = f.page(pgno)
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
