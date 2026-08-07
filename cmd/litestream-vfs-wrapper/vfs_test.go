package litestreamvfs

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	ncrucesvfs "github.com/ncruces/go-sqlite3/vfs"
	"github.com/stretchr/testify/require"
)

func TestNewDefaultsPollInterval(t *testing.T) {
	v := New(newReplicaClientStub(), testLogger())
	require.Equal(t, DefaultPollInterval, v.PollInterval)
}

func TestReplicaFileCloseIsIdempotentAndCancelsRequests(t *testing.T) {
	client := newReplicaClientStubFromPages(t, map[uint32][]byte{1: sqliteHeaderPage()})
	client.blockLTXFiles = make(chan struct{})
	f, err := openReplicaFile(context.Background(), client, "replica.db", testLogger(), DefaultCacheSize, time.Millisecond)
	require.NoError(t, err)
	require.Eventually(t, func() bool { return client.activeRequests() > 0 }, time.Second, time.Millisecond)
	require.NoError(t, f.Close())
	require.NoError(t, f.Close())
	require.Zero(t, client.activeRequests())
	calls := client.requestCount()
	time.Sleep(3 * time.Millisecond)
	require.Equal(t, calls, client.requestCount())
}

func TestReplicaFilePollDisabled(t *testing.T) {
	for _, interval := range []time.Duration{0, -time.Millisecond} {
		t.Run(interval.String(), func(t *testing.T) {
			client := newReplicaClientStubFromPages(t, map[uint32][]byte{1: sqliteHeaderPage()})
			f, err := openReplicaFile(context.Background(), client, "replica.db", testLogger(), DefaultCacheSize, interval)
			require.NoError(t, err)
			t.Cleanup(func() { _ = f.Close() })
			calls := client.requestCount()
			time.Sleep(3 * time.Millisecond)
			require.Equal(t, calls, client.requestCount())
		})
	}
}

func TestNewImplementsNcrucesVFS(t *testing.T) {
	var _ ncrucesvfs.VFS = New(&replicaClientStub{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}
