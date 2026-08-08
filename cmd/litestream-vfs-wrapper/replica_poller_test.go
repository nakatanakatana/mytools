package litestreamvfs

import (
	"context"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

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

func TestReplicaPollCompaction(t *testing.T) {
	client := newReplicaClientStubFromPages(t, map[uint32][]byte{
		1: sqliteHeaderPage(),
		2: []byte("page two v1"),
	})
	f, err := openReplicaFile(context.Background(), client, "replica.db", testLogger(), DefaultCacheSize, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })
	require.Equal(t, ltx.TXID(0), f.pollMaxTXID1)

	l1Data, l1Info := encodeTestLTXRange(t, 1, 1, 2, 2, map[uint32][]byte{
		1: sqliteHeaderPage(),
		2: []byte("page two v2"),
	})
	client.replaceLTXFiles(map[*ltx.FileInfo][]byte{l1Info: l1Data})

	require.NoError(t, f.pollReplicaClient(context.Background()))
	require.Equal(t, ltx.TXID(2), f.pos.TXID)
	require.Equal(t, ltx.TXID(2), f.pollPos.TXID)
	require.Equal(t, ltx.TXID(2), f.pollMaxTXID1)
	require.Equal(t, uint32(2), f.commit)
	require.True(t, f.index[1].Level == 1 && f.index[1].MaxTXID == 2)
	require.True(t, f.index[2].Level == 1 && f.index[2].MaxTXID == 2)

	buf := make([]byte, len("page two v2"))
	_, err = f.ReadAt(buf, int64(f.pageSize))
	require.NoError(t, err)
	require.Equal(t, "page two v2", string(buf))

	hdr := make([]byte, 16)
	_, err = f.ReadAt(hdr, 0)
	require.NoError(t, err)
	require.Equal(t, "SQLite format 3\x00", string(hdr))
}

func TestReplicaPollL0Gap(t *testing.T) {
	client := newReplicaClientStubFromPages(t, map[uint32][]byte{
		1: sqliteHeaderPage(),
		2: []byte("page two v1"),
	})
	f, err := openReplicaFile(context.Background(), client, "replica.db", testLogger(), DefaultCacheSize, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })

	l1Data, l1Info := encodeTestLTXRange(t, 1, 1, 2, 2, map[uint32][]byte{
		1: sqliteHeaderPage(),
		2: []byte("page two v2"),
	})
	client.addLTX(l1Info, l1Data)
	l0Data, l0Info := encodeTestLTX(t, 3, map[uint32][]byte{
		2: []byte("page two v3"),
	})
	client.addLTX(l0Info, l0Data)

	require.NoError(t, f.pollReplicaClient(context.Background()))
	require.Equal(t, ltx.TXID(2), f.pos.TXID)
	require.Equal(t, ltx.TXID(2), f.pollPos.TXID)
	require.Equal(t, ltx.TXID(2), f.pollMaxTXID1)
	require.Equal(t, 1, client.listCount(0, 2))
	require.Equal(t, 0, client.listCount(0, 3))
	require.Equal(t, 1, client.listCount(1, 1))

	buf := make([]byte, len("page two v2"))
	_, err = f.ReadAt(buf, int64(f.pageSize))
	require.NoError(t, err)
	require.Equal(t, "page two v2", string(buf))

	require.NoError(t, f.pollReplicaClient(context.Background()))
	require.Equal(t, ltx.TXID(3), f.pos.TXID)
	require.Equal(t, ltx.TXID(3), f.pollPos.TXID)
	require.Equal(t, 1, client.listCount(0, 2))
	require.Equal(t, 1, client.listCount(0, 3))
	require.Equal(t, 1, client.listCount(1, 1))

	buf = make([]byte, len("page two v3"))
	_, err = f.ReadAt(buf, int64(f.pageSize))
	require.NoError(t, err)
	require.Equal(t, "page two v3", string(buf))
}

func TestReplicaPollRestorePlanReplacement(t *testing.T) {
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

	require.NoError(t, f.Lock(ncrucesvfs.LOCK_SHARED))
	oldSize, err := f.Size()
	require.NoError(t, err)

	snapData, snapInfo := encodeTestLTXRange(t, 0, 1, 2, 1, map[uint32][]byte{
		1: sqliteHeaderPage(),
	})
	client.replaceLTXFiles(map[*ltx.FileInfo][]byte{snapInfo: snapData})
	require.NoError(t, f.pollReplicaClient(context.Background()))

	size, err := f.Size()
	require.NoError(t, err)
	require.Equal(t, oldSize, size)
	require.Equal(t, uint32(3), f.commit)
	require.Equal(t, ltx.TXID(1), f.pos.TXID)
	require.NotNil(t, f.pending)
	require.True(t, f.pending.replace)
	require.Equal(t, uint32(1), f.pending.commit)
	require.Equal(t, ltx.TXID(2), f.pollPos.TXID)
	require.Equal(t, uint32(1), f.pollCommit)

	buf := make([]byte, len("page three"))
	_, err = f.ReadAt(buf, int64(2*f.pageSize))
	require.NoError(t, err)
	require.Equal(t, "page three", string(buf))

	require.NoError(t, f.Unlock(ncrucesvfs.LOCK_NONE))
	require.Nil(t, f.pending)
	size, err = f.Size()
	require.NoError(t, err)
	require.Equal(t, int64(f.pageSize), size)
	require.Equal(t, uint32(1), f.commit)
	require.Equal(t, ltx.TXID(2), f.pos.TXID)
	_, ok := f.index[2]
	require.False(t, ok)
	_, ok = f.index[3]
	require.False(t, ok)
	require.Equal(t, ltx.TXID(0), f.pollMaxTXID1)
	require.Equal(t, ltx.TXID(0), f.maxTXID1)
}

func TestReplicaPollFailureRecovery(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(t *testing.T, client *replicaClientStub, f *replicaFile)
	}{
		{
			name: "l0 listing",
			prepare: func(t *testing.T, client *replicaClientStub, f *replicaFile) {
				data, info := encodeTestLTX(t, 2, map[uint32][]byte{2: []byte("page two v2")})
				client.addLTX(info, data)
				listErr := fmt.Errorf("l0 list failed")
				client.LTXFilesFunc = func(context.Context, int, ltx.TXID, bool) (ltx.FileIterator, error) {
					return nil, listErr
				}
			},
		},
		{
			name: "page index fetch",
			prepare: func(t *testing.T, client *replicaClientStub, f *replicaFile) {
				data, info := encodeTestLTX(t, 2, map[uint32][]byte{2: []byte("page two v2")})
				client.addLTX(info, data)
				indexErr := fmt.Errorf("page index failed")
				client.OpenLTXFileFunc = func(context.Context, int, ltx.TXID, ltx.TXID, int64, int64) (io.ReadCloser, error) {
					return nil, indexErr
				}
			},
		},
		{
			name: "header fetch",
			prepare: func(t *testing.T, client *replicaClientStub, f *replicaFile) {
				data, info := encodeTestLTX(t, 2, map[uint32][]byte{2: []byte("page two v2")})
				client.addLTX(info, data)
				headerErr := fmt.Errorf("header failed")
				var opens int
				client.OpenLTXFileFunc = func(ctx context.Context, level int, minTXID, maxTXID ltx.TXID, offset, size int64) (io.ReadCloser, error) {
					opens++
					if opens == 1 {
						client.mu.Lock()
						client.OpenLTXFileFunc = func(context.Context, int, ltx.TXID, ltx.TXID, int64, int64) (io.ReadCloser, error) {
							return nil, headerErr
						}
						client.mu.Unlock()
						return client.openLTXFileData(level, minTXID, maxTXID, offset, size)
					}
					return nil, headerErr
				}
			},
		},
		{
			name: "l1 listing after l0",
			prepare: func(t *testing.T, client *replicaClientStub, f *replicaFile) {
				data, info := encodeTestLTX(t, 2, map[uint32][]byte{2: []byte("page two v2")})
				client.addLTX(info, data)
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
			},
		},
		{
			name: "restore plan rebuild",
			prepare: func(t *testing.T, client *replicaClientStub, f *replicaFile) {
				data, info := encodeTestLTXRange(t, 0, 2, 2, 1, map[uint32][]byte{
					1: sqliteHeaderPage(),
				})
				client.addLTX(info, data)
				rebuildErr := fmt.Errorf("rebuild list failed")
				var lists int
				var listHook func(context.Context, int, ltx.TXID, bool) (ltx.FileIterator, error)
				listHook = func(ctx context.Context, level int, seek ltx.TXID, useMetadata bool) (ltx.FileIterator, error) {
					_ = useMetadata
					lists++
					client.mu.Lock()
					client.LTXFilesFunc = listHook
					client.mu.Unlock()
					if lists <= 2 {
						client.mu.Lock()
						var out []*ltx.FileInfo
						for _, existing := range client.files {
							if existing.Level == level && existing.MinTXID >= seek {
								out = append(out, existing)
							}
						}
						client.mu.Unlock()
						return ltx.NewFileInfoSliceIterator(out), nil
					}
					client.mu.Lock()
					client.LTXFilesFunc = nil
					client.mu.Unlock()
					return nil, rebuildErr
				}
				client.LTXFilesFunc = listHook
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

			successAt := time.Now().Add(-time.Minute).UTC()
			f.mu.Lock()
			f.lastPollSuccess = successAt
			f.mu.Unlock()

			tt.prepare(t, client, f)

			before := capturePollState(f)
			err = f.pollReplicaClient(context.Background())
			require.Error(t, err)
			after := capturePollState(f)
			require.Equal(t, before, after)
			require.NotNil(t, f.lastPollErr)
			require.Equal(t, successAt, f.lastPollSuccess)

			buf := make([]byte, len("page two v1"))
			_, err = f.ReadAt(buf, int64(f.pageSize))
			require.NoError(t, err)
			require.Equal(t, "page two v1", string(buf))

			require.NoError(t, f.pollReplicaClient(context.Background()))
			require.Nil(t, f.lastPollErr)
			require.True(t, f.lastPollSuccess.After(successAt))
			require.Equal(t, ltx.TXID(2), f.pollPos.TXID)
		})
	}
}

func TestReplicaPollTemporaryNotFound(t *testing.T) {
	client := newReplicaClientStubFromPages(t, map[uint32][]byte{
		1: sqliteHeaderPage(),
		2: []byte("page two v1"),
	})
	f, err := openReplicaFile(context.Background(), client, "replica.db", testLogger(), DefaultCacheSize, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })

	data, info := encodeTestLTX(t, 2, map[uint32][]byte{2: []byte("page two v2")})
	client.addLTX(info, data)
	client.OpenLTXFileFunc = func(context.Context, int, ltx.TXID, ltx.TXID, int64, int64) (io.ReadCloser, error) {
		return nil, os.ErrNotExist
	}

	before := capturePollState(f)
	err = f.pollReplicaClient(context.Background())
	require.Error(t, err)
	require.ErrorIs(t, err, os.ErrNotExist)
	require.Equal(t, before, capturePollState(f))

	buf := make([]byte, len("page two v1"))
	_, err = f.ReadAt(buf, int64(f.pageSize))
	require.NoError(t, err)
	require.Equal(t, "page two v1", string(buf))

	require.NoError(t, f.pollReplicaClient(context.Background()))
	require.Equal(t, ltx.TXID(2), f.pos.TXID)
	buf = make([]byte, len("page two v2"))
	_, err = f.ReadAt(buf, int64(f.pageSize))
	require.NoError(t, err)
	require.Equal(t, "page two v2", string(buf))
}

type pollStateSnapshot struct {
	index          map[uint32]ltx.PageIndexElem
	commit         uint32
	pos            ltx.Pos
	maxTXID1       ltx.TXID
	pollPos        ltx.Pos
	pollMaxTXID1   ltx.TXID
	pollCommit     uint32
	pendingNil     bool
	pendingReplace bool
	pendingCommit  uint32
	pendingPos     ltx.Pos
}

func capturePollState(f *replicaFile) pollStateSnapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := pollStateSnapshot{
		index:        clonePageIndex(f.index),
		commit:       f.commit,
		pos:          f.pos,
		maxTXID1:     f.maxTXID1,
		pollPos:      f.pollPos,
		pollMaxTXID1: f.pollMaxTXID1,
		pollCommit:   f.pollCommit,
		pendingNil:   f.pending == nil,
	}
	if f.pending != nil {
		s.pendingReplace = f.pending.replace
		s.pendingCommit = f.pending.commit
		s.pendingPos = f.pending.pos
	}
	return s
}

func clonePageIndex(src map[uint32]ltx.PageIndexElem) map[uint32]ltx.PageIndexElem {
	dst := make(map[uint32]ltx.PageIndexElem, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}
