// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package clickhouse

import (
	"context"
	"errors"
)

// ErrNoRows is returned by QueryRow when the query returned no row.
var ErrNoRows = errors.New("no rows in result set")

// Params are server-side query parameters, referenced in a query as
// {name:String}. Values are raw; the DB encodes them with ParamValue.
type Params map[string]string

// A Row is one row of a query result.
type Row interface {
	Scan(dest ...any) error
}

// DB is the narrow SQL interface the external clients use. It is implemented
// by *Handle (and the connections behind it) and by fakes in tests.
type DB interface {
	// Exec runs one statement as is, without client-side binding. Errors are
	// sanitised (see Sanitize); statements that carry an IDENTIFIED clause
	// have every single-quoted literal redacted from server errors.
	Exec(ctx context.Context, stmt string) error

	// Query runs a SELECT with server-side parameters and calls fn once per
	// row, until fn returns an error or the rows are exhausted.
	Query(ctx context.Context, query string, params Params, fn func(Row) error) error
}

// QueryRow runs a query and scans its first row into dest. It returns
// ErrNoRows if the query returned no rows.
func QueryRow(ctx context.Context, db DB, query string, params Params, dest ...any) error {
	found := false
	err := db.Query(ctx, query, params, func(r Row) error {
		if found {
			return nil
		}
		found = true
		return r.Scan(dest...)
	})
	if err != nil {
		return err
	}
	if !found {
		return ErrNoRows
	}
	return nil
}

type ctxKey int

const (
	ctxKeyLogComment ctxKey = iota
	ctxKeyDDLTimeout
)

// WithLogComment returns a context whose statements carry the setting
// log_comment = comment, so that every DDL in system.query_log is attributable
// to a managed resource ("<kind>/<namespace>/<name>").
func WithLogComment(ctx context.Context, comment string) context.Context {
	return context.WithValue(ctx, ctxKeyLogComment, comment)
}

// WithDistributedDDLTimeout returns a context whose statements carry the
// setting distributed_ddl_task_timeout (seconds). Used for ON CLUSTER DDL, so
// that the server gives up before the reconcile times out.
func WithDistributedDDLTimeout(ctx context.Context, seconds int) context.Context {
	return context.WithValue(ctx, ctxKeyDDLTimeout, seconds)
}

func logComment(ctx context.Context) string {
	s, _ := ctx.Value(ctxKeyLogComment).(string)
	return s
}

func ddlTimeout(ctx context.Context) int {
	n, _ := ctx.Value(ctxKeyDDLTimeout).(int)
	return n
}
