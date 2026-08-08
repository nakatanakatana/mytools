# litestream-vfs-wrapper

This package provides a read-only SQLite VFS for `ncruces/go-sqlite3` that reads Litestream replicas and follows newly published LTX files without reopening the database.

## Requirements

- It must build and pass tests with `CGO_ENABLED=0`.
- It must not use Litestream's `vfs` build tag.
- It must not import `github.com/mattn/go-sqlite3` or `github.com/psanford/sqlite3vfs`.

## Usage

```go
client := file.NewReplicaClient(replicaPath)
v := litestreamvfs.New(client, slog.Default())
v.PollInterval = 250 * time.Millisecond // optional; default is DefaultPollInterval (one second)
vfs.Register("litestream", v)
db, err := sqlite3.OpenContext(ctx, "file:app.db?vfs=litestream&mode=ro")
```

`New` sets `PollInterval` to `DefaultPollInterval` (`time.Second`). Set `PollInterval <= 0` to disable live-follow polling after the initial open.

## Live-follow behavior

- Each open main-database file starts **one poller per open connection**. Pollers list L0/L1 LTX files on the configured interval and advance only after a successful atomic poll.
- A successful poll becomes visible immediately when there is no active reader. Otherwise the update is held until `Unlock(LOCK_NONE)` and appears on the **next read transaction**.
- From `LOCK_SHARED` through `LOCK_NONE`, a connection sees one immutable snapshot: page index, commit size, visible TXID metadata, and cache view stay pinned for that read transaction.
- Failed polls (listing, header validation, gaps that cannot be applied safely, timeouts, missing files) leave visible state and poll cursors unchanged and are retried on a later interval.
- Page fetches use the file's cancellation context, bounded transient retries, and a temporary SQLite `BUSY` error after exhaustion. Cache inserts are rejected if the visible snapshot changed during the fetch.
- Closing a file cancels in-flight replica requests and stops its poller. Repeated `Close()` calls are harmless.

Because each `database/sql` connection can open its own VFS file, object-storage LIST traffic grows with open connections. Prefer `db.SetMaxOpenConns(...)` (and a matching idle limit when appropriate) so poll request volume stays proportional to the intended concurrency. A shared VFS-level poller is out of scope.

## Limitations

- The main database is read-only. Writes, truncation, and locks at or above `RESERVED` are rejected.
- Temporary files and journals requested by SQLite are created locally and must be writable.
- Litestream SQL functions such as `litestream_set_time`, `litestream_txid`, and `litestream_lag` are not provided.
- Write support, local hydration, time travel, compaction execution, and a shared VFS-wide poller are not provided.

## Verification

```bash
CGO_ENABLED=0 go test ./cmd/litestream-vfs-wrapper/...
CGO_ENABLED=0 go build ./cmd/litestream-vfs-wrapper/...
go test -race ./cmd/litestream-vfs-wrapper/...
```

The consumer verification example is available as a standalone module under `example/consumer`.
It is not bundled into application binaries that import only `litestreamvfs`.

```bash
cd cmd/litestream-vfs-wrapper/example/consumer
CGO_ENABLED=0 go build -o consumer .
./consumer -replica /path/to/replica -database app.db -query 'SELECT value FROM records'
```
