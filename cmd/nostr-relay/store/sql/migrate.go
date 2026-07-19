package schema

import (
	"context"
	"fmt"
	"strings"

	"github.com/sqldef/sqldef/v3/database"
	"github.com/sqldef/sqldef/v3/database/sqlite3"
	"github.com/sqldef/sqldef/v3/parser"
	sqldefschema "github.com/sqldef/sqldef/v3/schema"
)

func Migrate(ctx context.Context, databasePath string, dryRun bool) error {
	db, err := sqlite3.NewDatabase(database.Config{DbName: databasePath})
	if err != nil {
		return fmt.Errorf("open sqldef database: %w", err)
	}
	defer func() { _ = db.Close() }()

	currentDDLs, err := db.ExportDDLs()
	if err != nil {
		return fmt.Errorf("export current relay schema: %w", err)
	}

	sqlParser := database.NewParser(parser.ParserModeSQLite3)
	ddls, err := sqldefschema.GenerateIdempotentDDLs(
		sqldefschema.GeneratorModeSQLite3,
		sqlParser,
		Schema,
		currentDDLs,
		database.GeneratorConfig{},
		"",
	)
	if err != nil {
		return fmt.Errorf("generate relay schema DDL: %w", err)
	}
	if len(ddls) == 0 {
		return nil
	}
	if dryRun {
		return fmt.Errorf("database schema is out of sync:\n%s", strings.Join(ddls, ";\n"))
	}
	if err := database.RunDDLs(db, ddls, "", "", database.StdoutLogger{}); err != nil {
		return fmt.Errorf("apply relay schema DDL: %w", err)
	}
	return nil
}
