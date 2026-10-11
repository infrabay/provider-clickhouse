// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package clickhouse

import (
	"context"
	"io"
	"strings"

	ch "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// selectMaxExecutionTime bounds every SELECT (seconds).
const selectMaxExecutionTime = 30

// A Conn is a connection pool to one server.
type Conn interface {
	DB
	io.Closer
}

// An Opener opens a connection pool for a configuration. Opening must not
// dial; the first statement does.
type Opener func(cfg Config) (Conn, error)

// OpenDriver opens a clickhouse-go connection pool.
func OpenDriver(cfg Config) (Conn, error) {
	opt, err := cfg.Options()
	if err != nil {
		return nil, err
	}
	c, err := ch.Open(opt)
	if err != nil {
		return nil, Sanitize(err, false)
	}
	return &driverConn{conn: c}, nil
}

// driverConn adapts a clickhouse-go connection pool to DB.
type driverConn struct {
	conn driver.Conn
}

func settings(ctx context.Context, read bool) ch.Settings {
	s := ch.Settings{}
	if c := logComment(ctx); c != "" {
		s["log_comment"] = c
	}
	if read {
		s["max_execution_time"] = selectMaxExecutionTime
	}
	if t := ddlTimeout(ctx); t > 0 && !read {
		s["distributed_ddl_task_timeout"] = t
	}
	return s
}

// Exec runs stmt without client-side binding (no args are passed, so '?',
// '@x' and '$1' inside names are inert).
func (d *driverConn) Exec(ctx context.Context, stmt string) error {
	qctx := ch.Context(ctx, ch.WithSettings(settings(ctx, false)))
	return Sanitize(d.conn.Exec(qctx, stmt), carriesSecret(stmt))
}

// Query runs a SELECT with server-side parameters.
func (d *driverConn) Query(ctx context.Context, query string, params Params, fn func(Row) error) error {
	opts := []ch.QueryOption{ch.WithSettings(settings(ctx, isRead(query)))}
	if len(params) > 0 {
		p := make(ch.Parameters, len(params))
		for k, v := range params {
			p[k] = ParamValue(v)
		}
		opts = append(opts, ch.WithParameters(p))
	}
	rows, err := d.conn.Query(ch.Context(ctx, opts...), query)
	if err != nil {
		return Sanitize(err, carriesSecret(query))
	}
	defer rows.Close() //nolint:errcheck // Errors are reported by rows.Err.
	for rows.Next() {
		if err := fn(rows); err != nil {
			// Errors from fn (including scan errors) are the caller's.
			return err
		}
	}
	return Sanitize(rows.Err(), carriesSecret(query))
}

// Close closes the pool.
func (d *driverConn) Close() error { return d.conn.Close() }

func isRead(query string) bool {
	q := strings.ToUpper(strings.TrimSpace(query))
	for _, p := range []string{"SELECT", "WITH", "SHOW", "EXISTS", "CHECK", "EXPLAIN", "DESCRIBE"} {
		if strings.HasPrefix(q, p) {
			return true
		}
	}
	return false
}
