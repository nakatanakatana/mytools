package otel

import (
	"database/sql"

	"github.com/XSAM/otelsql"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

// OpenSQLite opens a modernc.org/sqlite database wrapped with otelsql.
// It records db.system.name=sqlite and placeholder SQL without raw bind parameter values.
func OpenSQLite(dsn string) (*sql.DB, error) {
	return otelsql.Open(
		"sqlite",
		dsn,
		otelsql.WithAttributes(semconv.DBSystemNameSQLite),
		otelsql.WithSpanOptions(otelsql.SpanOptions{
			DisableErrSkip:       true,
			OmitConnResetSession: true,
		}),
	)
}
