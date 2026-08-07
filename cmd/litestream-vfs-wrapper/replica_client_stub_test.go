package litestreamvfs

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/superfly/ltx"
)

func TestReplicaClientStubMutableFiles(t *testing.T) {
	client := newReplicaClientStub()
	level1 := &ltx.FileInfo{Level: 1, MinTXID: 1, MaxTXID: 2}
	level0TX3 := &ltx.FileInfo{Level: 0, MinTXID: 3, MaxTXID: 3}
	level0TX2 := &ltx.FileInfo{Level: 0, MinTXID: 2, MaxTXID: 2}

	client.addLTX(level0TX3, []byte("tx3"))
	client.addLTX(level1, []byte("level1"))
	client.addLTX(level0TX2, []byte("tx2"))

	itr, err := client.LTXFiles(context.Background(), 0, 2, false)
	require.NoError(t, err)
	infos, err := ltx.SliceFileIterator(itr)
	require.NoError(t, err)
	require.Equal(t, []*ltx.FileInfo{level0TX2, level0TX3}, infos)
	require.Equal(t, 1, client.listCount(0, 2))
	require.Equal(t, 0, client.listCount(1, 2))

	client.removeLTX(0, 2, 2)
	_, err = client.OpenLTXFile(context.Background(), 0, 2, 2, 0, 0)
	require.ErrorIs(t, err, os.ErrNotExist)

	replacement := &ltx.FileInfo{Level: 0, MinTXID: 4, MaxTXID: 4}
	client.replaceLTXFiles(map[*ltx.FileInfo][]byte{replacement: []byte("tx4")})
	_, err = client.OpenLTXFile(context.Background(), 0, 3, 3, 0, 0)
	require.ErrorIs(t, err, os.ErrNotExist)
	rc, err := client.OpenLTXFile(context.Background(), 0, 4, 4, 0, 0)
	require.NoError(t, err)
	data, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close())
	require.Equal(t, []byte("tx4"), data)
}

func TestReplicaClientStubOneShotFailures(t *testing.T) {
	client := newReplicaClientStub()
	info := &ltx.FileInfo{Level: 0, MinTXID: 1, MaxTXID: 1}
	client.addLTX(info, []byte("data"))

	listErr := fmt.Errorf("list failed")
	client.LTXFilesFunc = func(context.Context, int, ltx.TXID, bool) (ltx.FileIterator, error) {
		return nil, listErr
	}
	_, err := client.LTXFiles(context.Background(), 0, 1, false)
	require.ErrorIs(t, err, listErr)
	itr, err := client.LTXFiles(context.Background(), 0, 1, false)
	require.NoError(t, err)
	infos, err := ltx.SliceFileIterator(itr)
	require.NoError(t, err)
	require.Equal(t, []*ltx.FileInfo{info}, infos)
	require.Equal(t, 2, client.listCount(0, 1))

	openErr := fmt.Errorf("open failed")
	client.OpenLTXFileFunc = func(context.Context, int, ltx.TXID, ltx.TXID, int64, int64) (io.ReadCloser, error) {
		return nil, openErr
	}
	_, err = client.OpenLTXFile(context.Background(), 0, 1, 1, 0, 0)
	require.ErrorIs(t, err, openErr)
	rc, err := client.OpenLTXFile(context.Background(), 0, 1, 1, 0, 0)
	require.NoError(t, err)
	require.NoError(t, rc.Close())
}

// replicaClientStub is an in-memory ReplicaClient for tests.
type replicaClientStub struct {
	mu              sync.Mutex
	files           []*ltx.FileInfo
	data            map[string][]byte
	listCounts      map[replicaListCall]int
	requests        int
	active          int
	blockLTXFiles   chan struct{}
	LTXFilesFunc    func(context.Context, int, ltx.TXID, bool) (ltx.FileIterator, error)
	OpenLTXFileFunc func(context.Context, int, ltx.TXID, ltx.TXID, int64, int64) (io.ReadCloser, error)
}

type replicaListCall struct {
	level int
	seek  ltx.TXID
}

func newReplicaClientStub() *replicaClientStub {
	return &replicaClientStub{
		data:       make(map[string][]byte),
		listCounts: make(map[replicaListCall]int),
	}
}

func (c *replicaClientStub) Type() string { return "stub" }

func (c *replicaClientStub) Init(context.Context) error { return nil }

func (c *replicaClientStub) SetLogger(*slog.Logger) {}

func (c *replicaClientStub) LTXFiles(ctx context.Context, level int, seek ltx.TXID, useMetadata bool) (ltx.FileIterator, error) {
	_ = useMetadata
	c.mu.Lock()
	c.requests++
	c.active++
	c.listCounts[replicaListCall{level: level, seek: seek}]++
	block := c.blockLTXFiles
	fn := c.LTXFilesFunc
	c.LTXFilesFunc = nil
	c.mu.Unlock()
	defer func() { c.mu.Lock(); c.active--; c.mu.Unlock() }()
	if fn != nil {
		return fn(ctx, level, seek, useMetadata)
	}
	if block != nil && seek > 0 {
		select {
		case <-block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []*ltx.FileInfo
	for _, info := range c.files {
		if info.Level == level && info.MinTXID >= seek {
			out = append(out, info)
		}
	}
	return ltx.NewFileInfoSliceIterator(out), nil
}

func (c *replicaClientStub) OpenLTXFile(ctx context.Context, level int, minTXID, maxTXID ltx.TXID, offset, size int64) (io.ReadCloser, error) {
	c.mu.Lock()
	c.requests++
	c.active++
	fn := c.OpenLTXFileFunc
	c.OpenLTXFileFunc = nil
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.active--
		c.mu.Unlock()
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if fn != nil {
		return fn(ctx, level, minTXID, maxTXID, offset, size)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	data, ok := c.data[c.makeKey(level, minTXID, maxTXID)]
	if !ok {
		return nil, os.ErrNotExist
	}
	if offset > int64(len(data)) {
		return nil, fmt.Errorf("offset beyond data")
	}
	slice := data[offset:]
	if size > 0 && size < int64(len(slice)) {
		slice = slice[:size]
	}
	return io.NopCloser(bytes.NewReader(slice)), nil
}

func (c *replicaClientStub) requestCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.requests
}

func (c *replicaClientStub) activeRequests() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.active
}

func (c *replicaClientStub) listCount(level int, seek ltx.TXID) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.listCounts[replicaListCall{level: level, seek: seek}]
}

func (c *replicaClientStub) WriteLTXFile(context.Context, int, ltx.TXID, ltx.TXID, io.Reader) (*ltx.FileInfo, error) {
	return nil, fmt.Errorf("not implemented")
}

func (c *replicaClientStub) DeleteLTXFiles(context.Context, []*ltx.FileInfo) error {
	return fmt.Errorf("not implemented")
}

func (c *replicaClientStub) DeleteAll(context.Context) error {
	return fmt.Errorf("not implemented")
}

func (c *replicaClientStub) makeKey(level int, minTXID, maxTXID ltx.TXID) string {
	return fmt.Sprintf("%d:%s:%s", level, minTXID.String(), maxTXID.String())
}

func (c *replicaClientStub) addFile(info *ltx.FileInfo, data []byte) {
	c.addLTX(info, data)
}

func (c *replicaClientStub) addLTX(info *ltx.FileInfo, data []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := c.makeKey(info.Level, info.MinTXID, info.MaxTXID)
	for i, existing := range c.files {
		if c.makeKey(existing.Level, existing.MinTXID, existing.MaxTXID) == key {
			c.files[i] = info
			c.data[key] = data
			return
		}
	}
	c.files = append(c.files, info)
	c.data[key] = data
}

func (c *replicaClientStub) removeLTX(level int, minTXID, maxTXID ltx.TXID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := c.makeKey(level, minTXID, maxTXID)
	files := c.files[:0]
	for _, info := range c.files {
		if c.makeKey(info.Level, info.MinTXID, info.MaxTXID) != key {
			files = append(files, info)
		}
	}
	c.files = files
	delete(c.data, key)
}

func (c *replicaClientStub) replaceLTXFiles(files map[*ltx.FileInfo][]byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.files = make([]*ltx.FileInfo, 0, len(files))
	c.data = make(map[string][]byte, len(files))
	for info, data := range files {
		c.files = append(c.files, info)
		c.data[c.makeKey(info.Level, info.MinTXID, info.MaxTXID)] = data
	}
}
