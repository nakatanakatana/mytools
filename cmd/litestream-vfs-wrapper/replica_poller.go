package litestreamvfs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/benbjohnson/litestream"
	ncrucesvfs "github.com/ncruces/go-sqlite3/vfs"
	"github.com/superfly/ltx"
)

type pollLevelResult struct {
	maxTXID ltx.TXID
	index   map[uint32]ltx.PageIndexElem
	commit  uint32
	gapAt   ltx.TXID
	replace bool
}

// monitorReplicaClient owns the cancellable polling lifecycle.
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

// pollReplicaClient collects L0 and L1 updates atomically, advances fetch
// cursors only after both levels succeed, and stages the result for readers.
func (f *replicaFile) pollReplicaClient(ctx context.Context) error {
	f.pollMu.Lock()
	defer f.pollMu.Unlock()

	f.mu.Lock()
	pollPos := f.pollPos
	pollMaxTXID1 := f.pollMaxTXID1
	pollCommit := f.pollCommit
	f.mu.Unlock()

	l0, err := f.pollLevel(ctx, 0, pollPos.TXID, pollCommit)
	if err != nil {
		f.recordPollFailure(err)
		return fmt.Errorf("poll L0: %w", err)
	}

	l1, err := f.pollLevel(ctx, 1, pollMaxTXID1, l0.commit)
	if err != nil {
		f.recordPollFailure(err)
		return fmt.Errorf("poll L1: %w", err)
	}

	var update replicaUpdate
	needRebuild := l0.replace || l1.replace || (l0.gapAt != 0 && l1.maxTXID+1 < l0.gapAt)
	if needRebuild {
		update, err = f.rebuildLatest(ctx)
		if err != nil {
			f.recordPollFailure(err)
			return err
		}
	} else {
		update = mergePollLevelResults(pollPos, pollMaxTXID1, pollCommit, l0, l1)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.pollPos = update.pos
	f.pollMaxTXID1 = update.maxTXID1
	f.pollCommit = update.commit
	f.stageUpdateLocked(update)
	f.lastPollSuccess = time.Now()
	f.lastPollErr = nil
	return nil
}

func mergePollLevelResults(pollPos ltx.Pos, pollMaxTXID1 ltx.TXID, pollCommit uint32, l0, l1 pollLevelResult) replicaUpdate {
	index := make(map[uint32]ltx.PageIndexElem, len(l0.index)+len(l1.index))
	for pgno, elem := range l0.index {
		index[pgno] = elem
	}
	for pgno, elem := range l1.index {
		index[pgno] = elem
	}

	commit := pollCommit
	if l0.maxTXID > pollPos.TXID || len(l0.index) > 0 {
		commit = l0.commit
	}
	if l1.maxTXID > pollMaxTXID1 || len(l1.index) > 0 {
		commit = l1.commit
	}

	pos := pollPos
	if l0.maxTXID > pos.TXID {
		pos.TXID = l0.maxTXID
	}
	if l1.maxTXID > pos.TXID {
		pos.TXID = l1.maxTXID
	}

	return replicaUpdate{
		index:    index,
		replace:  false,
		commit:   commit,
		pos:      pos,
		maxTXID1: l1.maxTXID,
	}
}

func (f *replicaFile) pollLevel(ctx context.Context, level int, previous ltx.TXID, baseCommit uint32) (result pollLevelResult, err error) {
	pageSize := f.pageSize
	result = pollLevelResult{
		maxTXID: previous,
		index:   make(map[uint32]ltx.PageIndexElem),
		commit:  baseCommit,
	}

	itr, err := f.client.LTXFiles(ctx, level, previous+1, false)
	if err != nil {
		return pollLevelResult{}, fmt.Errorf("ltx files: %w", err)
	}
	defer func() {
		if closeErr := itr.Close(); err == nil && closeErr != nil {
			err = fmt.Errorf("close ltx iterator: %w", closeErr)
			result = pollLevelResult{}
		}
	}()

	for itr.Next() {
		info := itr.Item()
		if info.MinTXID != result.maxTXID+1 {
			if level == 0 && info.MinTXID > result.maxTXID+1 {
				result.gapAt = info.MinTXID
				break
			}
			return pollLevelResult{}, fmt.Errorf("non-contiguous ltx file: level=%d, current=%s, next=%s-%s", level, result.maxTXID, info.MinTXID, info.MaxTXID)
		}

		idx, fetchErr := litestream.FetchPageIndex(ctx, f.client, info)
		if fetchErr != nil {
			return pollLevelResult{}, fmt.Errorf("fetch page index: %w", fetchErr)
		}
		hdr, fetchErr := litestream.FetchLTXHeader(ctx, f.client, info)
		if fetchErr != nil {
			return pollLevelResult{}, fmt.Errorf("fetch header: %w", fetchErr)
		}
		if hdr.PageSize != pageSize {
			return pollLevelResult{}, fmt.Errorf("page size mismatch: want %d got %d", pageSize, hdr.PageSize)
		}
		if hdr.MinTXID != info.MinTXID || hdr.MaxTXID != info.MaxTXID {
			return pollLevelResult{}, fmt.Errorf("transaction range mismatch: file info %s-%s header %s-%s", info.MinTXID, info.MaxTXID, hdr.MinTXID, hdr.MaxTXID)
		}

		if hdr.Commit < result.commit || hdr.MinTXID == 1 {
			result.replace = true
			result.index = make(map[uint32]ltx.PageIndexElem)
		}
		result.commit = hdr.Commit
		for pgno, elem := range idx {
			result.index[pgno] = elem
		}
		result.maxTXID = info.MaxTXID
	}
	if itrErr := itr.Err(); itrErr != nil {
		return pollLevelResult{}, fmt.Errorf("iterate ltx files: %w", itrErr)
	}
	return result, nil
}

func (f *replicaFile) rebuildLatest(ctx context.Context) (replicaUpdate, error) {
	infos, err := litestream.CalcRestorePlan(ctx, f.client, 0, time.Time{}, f.logger)
	if err != nil {
		return replicaUpdate{}, fmt.Errorf("restore plan: %w", err)
	}
	if len(infos) == 0 {
		return replicaUpdate{}, fmt.Errorf("restore plan: empty")
	}
	snapshot, err := f.buildSnapshot(ctx, infos, f.pageSize)
	if err != nil {
		return replicaUpdate{}, err
	}
	return replicaUpdate{
		index:    snapshot.index,
		replace:  true,
		commit:   snapshot.commit,
		pos:      snapshot.pos,
		maxTXID1: snapshot.maxTXID1,
	}, nil
}

func (f *replicaFile) stageUpdateLocked(update replicaUpdate) {
	if !update.replace && len(update.index) == 0 {
		sameVisible := update.pos == f.pos && update.commit == f.commit && update.maxTXID1 == f.maxTXID1
		if f.pending == nil {
			if sameVisible {
				return
			}
		} else if update.pos == f.pending.pos && update.commit == f.pending.commit && update.maxTXID1 == f.pending.maxTXID1 {
			return
		}
	}

	if f.lock >= ncrucesvfs.LOCK_SHARED {
		f.mergePendingLocked(update)
		return
	}

	if update.replace {
		f.index = clonePageIndexMap(update.index)
		f.cache.Purge()
	} else {
		for pgno, elem := range update.index {
			f.index[pgno] = elem
			f.cache.Remove(pgno)
		}
	}
	f.commit = update.commit
	f.pos = update.pos
	f.maxTXID1 = update.maxTXID1
	f.visibleGeneration++
}

func (f *replicaFile) mergePendingLocked(update replicaUpdate) {
	if update.replace {
		f.pending = &replicaUpdate{
			index:    clonePageIndexMap(update.index),
			replace:  true,
			commit:   update.commit,
			pos:      update.pos,
			maxTXID1: update.maxTXID1,
		}
		return
	}

	if f.pending == nil {
		f.pending = &replicaUpdate{
			index:    clonePageIndexMap(update.index),
			replace:  false,
			commit:   update.commit,
			pos:      update.pos,
			maxTXID1: update.maxTXID1,
		}
		return
	}

	if f.pending.replace {
		for pgno, elem := range update.index {
			f.pending.index[pgno] = elem
		}
		f.pending.commit = update.commit
		f.pending.pos = update.pos
		f.pending.maxTXID1 = update.maxTXID1
		return
	}

	for pgno, elem := range update.index {
		f.pending.index[pgno] = elem
	}
	f.pending.commit = update.commit
	f.pending.pos = update.pos
	f.pending.maxTXID1 = update.maxTXID1
}

func (f *replicaFile) recordPollFailure(err error) {
	f.mu.Lock()
	f.lastPollErr = err
	f.mu.Unlock()
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	if f.logger != nil {
		f.logger.Error("cannot fetch new ltx files", "error", err)
	}
}

func clonePageIndexMap(src map[uint32]ltx.PageIndexElem) map[uint32]ltx.PageIndexElem {
	dst := make(map[uint32]ltx.PageIndexElem, len(src))
	for pgno, elem := range src {
		dst[pgno] = elem
	}
	return dst
}
