package litestreamvfs

import (
	"context"
	"fmt"
	"testing"

	ncrucesvfs "github.com/ncruces/go-sqlite3/vfs"
	"github.com/stretchr/testify/require"
	"github.com/superfly/ltx"
)

func TestReplicaPollAppliesL0Immediately(t *testing.T) {
	client := newReplicaClientStubFromPages(t, map[uint32][]byte{
		1: sqliteHeaderPage(),
		2: []byte("page two v1"),
	})
	f, err := openReplicaFile(context.Background(), client, "replica.db", testLogger(), DefaultCacheSize, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })

	data, info := encodeTestLTX(t, 2, map[uint32][]byte{
		2: []byte("page two v2"),
	})
	client.addLTX(info, data)

	require.NoError(t, f.pollReplicaClient(context.Background()))

	buf := make([]byte, len("page two v2"))
	_, err = f.ReadAt(buf, int64(f.pageSize))
	require.NoError(t, err)
	require.Equal(t, "page two v2", string(buf))
	require.Equal(t, ltx.TXID(2), f.pos.TXID)
	require.Equal(t, ltx.TXID(2), f.pollPos.TXID)
}

func TestReplicaPollAccumulatesPendingUnderSharedLock(t *testing.T) {
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

	require.Equal(t, 1, client.listCount(0, 2))
	require.Equal(t, 1, client.listCount(0, 3))
	require.Equal(t, 0, client.listCount(0, 1))
	require.NotNil(t, f.pending)
	require.Equal(t, ltx.TXID(3), f.pending.pos.TXID)
	require.Equal(t, ltx.TXID(3), f.pollPos.TXID)
	require.Equal(t, ltx.TXID(1), f.pos.TXID)
	require.Equal(t, uint32(2), f.commit)

	buf := make([]byte, len("page two v1"))
	_, err = f.ReadAt(buf, int64(f.pageSize))
	require.NoError(t, err)
	require.Equal(t, "page two v1", string(buf))
}

func TestReplicaPollL1FailureIsAtomic(t *testing.T) {
	client := newReplicaClientStubFromPages(t, map[uint32][]byte{
		1: sqliteHeaderPage(),
		2: []byte("page two v1"),
	})
	f, err := openReplicaFile(context.Background(), client, "replica.db", testLogger(), DefaultCacheSize, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })

	data, info := encodeTestLTX(t, 2, map[uint32][]byte{
		2: []byte("page two v2"),
	})
	client.addLTX(info, data)

	beforeIndex := clonePageIndex(f.index)
	beforeCommit := f.commit
	beforePos := f.pos
	beforePollPos := f.pollPos
	beforePollMaxTXID1 := f.pollMaxTXID1
	beforePollCommit := f.pollCommit

	l1Err := fmt.Errorf("l1 list failed")
	client.LTXFilesFunc = func(ctx context.Context, level int, seek ltx.TXID, useMetadata bool) (ltx.FileIterator, error) {
		_ = useMetadata
		require.Equal(t, 0, level)
		client.mu.Lock()
		client.LTXFilesFunc = func(context.Context, int, ltx.TXID, bool) (ltx.FileIterator, error) {
			return nil, l1Err
		}
		var out []*ltx.FileInfo
		for _, existing := range client.files {
			if existing.Level == level && existing.MinTXID >= seek {
				out = append(out, existing)
			}
		}
		client.mu.Unlock()
		return ltx.NewFileInfoSliceIterator(out), nil
	}

	err = f.pollReplicaClient(context.Background())
	require.ErrorIs(t, err, l1Err)
	require.Equal(t, beforeIndex, f.index)
	require.Equal(t, beforeCommit, f.commit)
	require.Equal(t, beforePos, f.pos)
	require.Equal(t, beforePollPos, f.pollPos)
	require.Equal(t, beforePollMaxTXID1, f.pollMaxTXID1)
	require.Equal(t, beforePollCommit, f.pollCommit)
	require.Nil(t, f.pending)
	require.ErrorIs(t, f.lastPollErr, l1Err)

	require.NoError(t, f.pollReplicaClient(context.Background()))
	require.Equal(t, ltx.TXID(2), f.pos.TXID)
	require.Equal(t, ltx.TXID(2), f.pollPos.TXID)
	require.Nil(t, f.lastPollErr)

	buf := make([]byte, len("page two v2"))
	_, err = f.ReadAt(buf, int64(f.pageSize))
	require.NoError(t, err)
	require.Equal(t, "page two v2", string(buf))
}

func TestReplicaPollRejectsInvalidHeader(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(t *testing.T, client *replicaClientStub)
	}{
		{
			name: "wrong page size",
			prepare: func(t *testing.T, client *replicaClientStub) {
				data, info := encodeTestLTXWithPageSize(t, 1024, 0, 2, 2, 1, map[uint32][]byte{
					1: sqliteHeaderPageForSize(1024),
				})
				client.addLTX(info, data)
			},
		},
		{
			name: "wrong txid range",
			prepare: func(t *testing.T, client *replicaClientStub) {
				data, info := encodeTestLTX(t, 2, map[uint32][]byte{
					2: []byte("page two v2"),
				})
				info.MaxTXID = 3
				client.addLTX(info, data)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newReplicaClientStubFromPages(t, map[uint32][]byte{
				1: sqliteHeaderPage(),
				2: []byte("page two v1"),
			})
			f, err := openReplicaFile(context.Background(), client, "replica.db", testLogger(), DefaultCacheSize, 0)
			require.NoError(t, err)
			t.Cleanup(func() { _ = f.Close() })

			tt.prepare(t, client)

			beforeIndex := clonePageIndex(f.index)
			beforeCommit := f.commit
			beforePos := f.pos
			beforePollPos := f.pollPos
			beforePollMaxTXID1 := f.pollMaxTXID1
			beforePollCommit := f.pollCommit

			err = f.pollReplicaClient(context.Background())
			require.Error(t, err)
			require.Equal(t, beforeIndex, f.index)
			require.Equal(t, beforeCommit, f.commit)
			require.Equal(t, beforePos, f.pos)
			require.Equal(t, beforePollPos, f.pollPos)
			require.Equal(t, beforePollMaxTXID1, f.pollMaxTXID1)
			require.Equal(t, beforePollCommit, f.pollCommit)
			require.Nil(t, f.pending)
		})
	}
}

func TestReplicaPollRejectsNonContiguousL1(t *testing.T) {
	client := newReplicaClientStubFromPages(t, map[uint32][]byte{
		1: sqliteHeaderPage(),
		2: []byte("page two v1"),
	})
	f, err := openReplicaFile(context.Background(), client, "replica.db", testLogger(), DefaultCacheSize, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })

	data, info := encodeTestLTXRange(t, 1, 2, 2, 2, map[uint32][]byte{
		2: []byte("compacted page two"),
	})
	client.addLTX(info, data)

	beforeIndex := clonePageIndex(f.index)
	beforeCommit := f.commit
	beforePos := f.pos
	beforePollPos := f.pollPos
	beforePollMaxTXID1 := f.pollMaxTXID1
	beforePollCommit := f.pollCommit

	err = f.pollReplicaClient(context.Background())
	require.Error(t, err)
	require.ErrorContains(t, err, "non-contiguous")
	require.Equal(t, beforeIndex, f.index)
	require.Equal(t, beforeCommit, f.commit)
	require.Equal(t, beforePos, f.pos)
	require.Equal(t, beforePollPos, f.pollPos)
	require.Equal(t, beforePollMaxTXID1, f.pollMaxTXID1)
	require.Equal(t, beforePollCommit, f.pollCommit)
	require.Nil(t, f.pending)
}

func clonePageIndex(src map[uint32]ltx.PageIndexElem) map[uint32]ltx.PageIndexElem {
	dst := make(map[uint32]ltx.PageIndexElem, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}
