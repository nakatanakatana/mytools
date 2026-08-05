package litestreamvfs

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReadmeDocumentsConsumerCGOBuild(t *testing.T) {
	readme, err := os.ReadFile("README.md")
	require.NoError(t, err)
	require.Contains(t, string(readme), "cd cmd/litestream-vfs-wrapper/example/consumer")
	require.Contains(t, string(readme), "CGO_ENABLED=0 go build -o consumer .")
}
