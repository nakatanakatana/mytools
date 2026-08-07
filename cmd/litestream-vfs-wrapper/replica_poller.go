package litestreamvfs

import (
	"context"
	"time"
)

// monitorReplicaClient owns the cancellable polling lifecycle. A later polling
// implementation can replace pollReplicaClient without changing file ownership.
func (f *replicaFile) monitorReplicaClient(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = f.pollReplicaClient(ctx)
		}
	}
}

// pollReplicaClient establishes the cancellable replica-access boundary used by
// the file lifecycle. Atomic collection and publication are added separately.
func (f *replicaFile) pollReplicaClient(ctx context.Context) error {
	itr, err := f.client.LTXFiles(ctx, 0, 1, false)
	if err != nil {
		return err
	}
	return itr.Close()
}
