package litestreamvfs

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"

	"github.com/superfly/ltx"
)

// replicaClientStub is an in-memory ReplicaClient for tests.
type replicaClientStub struct {
	mu    sync.Mutex
	files []*ltx.FileInfo
	data  map[string][]byte
}

func newReplicaClientStub() *replicaClientStub {
	return &replicaClientStub{data: make(map[string][]byte)}
}

func (c *replicaClientStub) Type() string { return "stub" }

func (c *replicaClientStub) Init(context.Context) error { return nil }

func (c *replicaClientStub) SetLogger(*slog.Logger) {}

func (c *replicaClientStub) LTXFiles(ctx context.Context, level int, seek ltx.TXID, useMetadata bool) (ltx.FileIterator, error) {
	_ = ctx
	_ = useMetadata
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
	_ = ctx
	c.mu.Lock()
	defer c.mu.Unlock()
	data, ok := c.data[c.makeKey(level, minTXID, maxTXID)]
	if !ok {
		return nil, fmt.Errorf("ltx file not found")
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
	c.mu.Lock()
	defer c.mu.Unlock()
	c.files = append(c.files, info)
	c.data[c.makeKey(info.Level, info.MinTXID, info.MaxTXID)] = data
}
