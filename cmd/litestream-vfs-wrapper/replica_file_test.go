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
	f, err := openReplicaFile(context.Background(), client, "replica.db", testLogger(), DefaultCacheSize)
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

func openTestReplicaFile(t *testing.T) *replicaFile {
	t.Helper()
	client := newReplicaClientStubFromPages(t, map[uint32][]byte{
		1: sqliteHeaderPage(),
		2: []byte("updated page"),
	})
	f, err := openReplicaFile(context.Background(), client, "replica.db", testLogger(), DefaultCacheSize)
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
	t.Helper()
	require.NotEmpty(t, pages)

	pgnos := make([]uint32, 0, len(pages))
	var maxPg uint32
	for pgno := range pages {
		pgnos = append(pgnos, pgno)
		if pgno > maxPg {
			maxPg = pgno
		}
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
		PageSize:  testPageSize,
		Commit:    maxPg,
		MinTXID:   txid,
		MaxTXID:   txid,
		Timestamp: time.Now().UnixMilli(),
		Flags:     ltx.HeaderFlagNoChecksum,
	}
	require.NoError(t, enc.EncodeHeader(hdr))
	for _, pgno := range pgnos {
		page := make([]byte, testPageSize)
		copy(page, pages[pgno])
		require.NoError(t, enc.EncodePage(ltx.PageHeader{Pgno: pgno}, page))
	}
	require.NoError(t, enc.Close())

	info := &ltx.FileInfo{
		Level:     0,
		MinTXID:   txid,
		MaxTXID:   txid,
		Size:      int64(buf.Len()),
		CreatedAt: time.Now().UTC(),
	}
	return buf.Bytes(), info
}

func sqliteHeaderPage() []byte {
	page := make([]byte, testPageSize)
	copy(page, "SQLite format 3\x00")
	// Page size big-endian at offset 16.
	page[16], page[17] = 0x10, 0x00 // 4096
	// Force journal mode bytes; VFS remasks these on read of page 1.
	page[18], page[19] = 0x01, 0x01
	// File change counter etc. left zero.
	// Database size in pages (uint32 BE at offset 28).
	page[28], page[29], page[30], page[31] = 0, 0, 0, 2
	return page
}
