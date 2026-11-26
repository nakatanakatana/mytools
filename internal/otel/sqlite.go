package otel

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"strings"

	"github.com/XSAM/otelsql"
	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

// SQLQuerySummary extracts the sqlc query name from a leading "-- name: <Name> :<cmd>" comment.
// It returns an empty string when the query has no sqlc name comment.
func SQLQuerySummary(query string) string {
	firstLine, _, _ := strings.Cut(strings.TrimSpace(query), "\n")
	rest, ok := strings.CutPrefix(strings.TrimSpace(firstLine), "-- name:")
	if !ok {
		return ""
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// OpenSQLite opens a modernc.org/sqlite database wrapped with otelsql.
// It records db.system.name=sqlite, placeholder SQL without raw bind parameter values,
// and db.query.summary from sqlc "-- name:" comments when present.
func OpenSQLite(dsn string) (*sql.DB, error) {
	return otelsql.Open(
		"sqlite",
		dsn,
		otelsql.WithAttributes(semconv.DBSystemNameSQLite),
		otelsql.WithAttributesGetter(func(_ context.Context, _ otelsql.Method, query string, _ []driver.NamedValue) []attribute.KeyValue {
			summary := SQLQuerySummary(query)
			if summary == "" {
				return nil
			}
			return []attribute.KeyValue{semconv.DBQuerySummary(summary)}
		}),
		otelsql.WithSpanNameFormatter(func(_ context.Context, method otelsql.Method, query string) string {
			if summary := SQLQuerySummary(query); summary != "" {
				return summary
			}
			return string(method)
		}),
		otelsql.WithSpanOptions(otelsql.SpanOptions{
			DisableErrSkip:       true,
			OmitConnResetSession: true,
		}),
	)
}
