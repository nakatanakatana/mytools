# litestream-vfs-wrapper

This package provides a read-only SQLite VFS for `ncruces/go-sqlite3` that reads Litestream replicas.

## Requirements

- It must build and pass tests with `CGO_ENABLED=0`.
- It must not use Litestream's `vfs` build tag.
- It must not import `github.com/mattn/go-sqlite3` or `github.com/psanford/sqlite3vfs`.

## Usage

```go
client := file.NewReplicaClient(replicaPath)
v := litestreamvfs.New(client, slog.Default())
vfs.Register("litestream", v)
db, err := sqlite3.OpenContext(ctx, "file:app.db?vfs=litestream&mode=ro")
```

## Limitations

- The main database is read-only. Writes, truncation, and locks at or above `RESERVED` are rejected.
- Temporary files and journals requested by SQLite are created locally and must be writable.
- Litestream SQL functions such as `litestream_set_time`, `litestream_txid`, and `litestream_lag` are not provided.
- The initial implementation reads a fixed LTX snapshot from the time the database is opened. It does not continuously poll the replica.

## Verification

```bash
CGO_ENABLED=0 go test ./cmd/litestream-vfs-wrapper/...
CGO_ENABLED=0 go build ./cmd/litestream-vfs-wrapper/...
```

The consumer verification example is available as a standalone module under `example/consumer`.
It is not bundled into application binaries that import only `litestreamvfs`.

```bash
cd cmd/litestream-vfs-wrapper/example/consumer
CGO_ENABLED=0 go build -o consumer .
./consumer -replica /path/to/replica -database app.db -query 'SELECT value FROM records'
```
