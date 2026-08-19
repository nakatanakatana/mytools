package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/benbjohnson/litestream/file"
	"github.com/ncruces/go-sqlite3"
	ncrucesvfs "github.com/ncruces/go-sqlite3/vfs"

	litestreamvfs "github.com/nakatanakatana/mytools/cmd/litestream-vfs-wrapper"
)

func main() {
	if err := run(context.Background(), os.Stdout, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, stdout io.Writer, args []string) error {
	fs := flag.NewFlagSet("consumer", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	replica := fs.String("replica", "", "path to Litestream file replica directory")
	database := fs.String("database", "", "logical database name for the VFS URI")
	query := fs.String("query", "", "read-only SQL query returning a text first column")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *replica == "" || *database == "" || *query == "" {
		return fmt.Errorf("required flags: -replica, -database, -query")
	}

	client := file.NewReplicaClient(*replica)
	if err := client.Init(ctx); err != nil {
		return fmt.Errorf("init replica client: %w", err)
	}

	vfsName := fmt.Sprintf("litestream-consumer-%d", os.Getpid())
	ncrucesvfs.Register(vfsName, litestreamvfs.New(client, slog.Default()))
	defer ncrucesvfs.Unregister(vfsName)

	uri := fmt.Sprintf("file:%s?vfs=%s&mode=ro", *database, vfsName)
	db, err := sqlite3.OpenContext(ctx, uri)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()

	stmt, _, err := db.Prepare(*query)
	if err != nil {
		return fmt.Errorf("prepare: %w", err)
	}
	defer stmt.Close()

	if !stmt.Step() {
		if err := stmt.Err(); err != nil {
			return fmt.Errorf("step: %w", err)
		}
		return fmt.Errorf("query returned no rows")
	}
	if _, err := fmt.Fprintln(stdout, stmt.ColumnText(0)); err != nil {
		return err
	}
	return nil
}
