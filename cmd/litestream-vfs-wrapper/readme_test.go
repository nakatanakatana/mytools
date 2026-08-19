package litestreamvfs

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReadmeDocumentsConsumerCGOBuild(t *testing.T) {
	readme, err := os.ReadFile("README.md")
	require.NoError(t, err)
	require.Contains(t, string(readme), "cd example/litestream-vfs-consumer")
	require.Contains(t, string(readme), "CGO_ENABLED=0 go build -o consumer .")
}

func TestReadmeDocumentsLiveFollowSemantics(t *testing.T) {
	readme, err := os.ReadFile("README.md")
	require.NoError(t, err)
	text := string(readme)

	require.Contains(t, text, "DefaultPollInterval")
	require.Contains(t, text, "PollInterval <= 0")
	require.Contains(t, text, "next read transaction")
	require.Contains(t, text, "SetMaxOpenConns")
	require.Contains(t, text, "one poller per open connection")
	require.NotContains(t, text, "fixed LTX snapshot")
}
