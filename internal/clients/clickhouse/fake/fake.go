// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

// Package fake provides fakes of the ClickHouse client interfaces for unit
// tests.
package fake

import (
	"context"
	"fmt"
	"reflect"
	"sync"

	ch "github.com/infrabay/provider-clickhouse/internal/clients/clickhouse"
)

// A Result is the scripted result of a query.
type Result struct {
	Rows [][]any
	Err  error
}

// Rows returns a result with the given rows.
func Rows(rows ...[]any) Result { return Result{Rows: rows} }

// Row returns a result with one row.
func Row(vals ...any) Result { return Result{Rows: [][]any{vals}} }

// Error returns a failing result.
func Error(err error) Result { return Result{Err: err} }

// Exception returns a ClickHouse server error.
func Exception(code int32, msg string) error { return &ch.ServerError{Code: code, Message: msg} }

// DB is a scripted ch.DB. Queries are matched by their exact text.
type DB struct {
	mu sync.Mutex

	// Queries maps a query to its result.
	Queries map[string]func(ch.Params) Result
	// ExecFn handles statements. nil accepts every statement.
	ExecFn func(stmt string) error

	// Execs records every statement passed to Exec.
	Execs []string
	// Seen records every query passed to Query.
	Seen []string
	// Params records the parameters of every query.
	Params []ch.Params
}

// NewDB returns an empty DB.
func NewDB() *DB { return &DB{Queries: map[string]func(ch.Params) Result{}} }

// On scripts a query with a fixed result.
func (d *DB) On(query string, r Result) *DB {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.Queries[query] = func(ch.Params) Result { return r }
	return d
}

// OnFunc scripts a query with a function of its parameters.
func (d *DB) OnFunc(query string, fn func(ch.Params) Result) *DB {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.Queries[query] = fn
	return d
}

// Exec implements ch.DB.
func (d *DB) Exec(_ context.Context, stmt string) error {
	d.mu.Lock()
	d.Execs = append(d.Execs, stmt)
	fn := d.ExecFn
	d.mu.Unlock()
	if fn == nil {
		return nil
	}
	return fn(stmt)
}

// Query implements ch.DB.
func (d *DB) Query(_ context.Context, query string, params ch.Params, fn func(ch.Row) error) error {
	d.mu.Lock()
	d.Seen = append(d.Seen, query)
	d.Params = append(d.Params, params)
	h, ok := d.Queries[query]
	d.mu.Unlock()
	if !ok {
		return fmt.Errorf("fake: unexpected query %q", query)
	}
	r := h(params)
	if r.Err != nil {
		return r.Err
	}
	for _, vals := range r.Rows {
		if err := fn(row(vals)); err != nil {
			return err
		}
	}
	return nil
}

type row []any

// Scan assigns the row's values to dest, which must be pointers to values of
// exactly the same types.
func (r row) Scan(dest ...any) error {
	if len(dest) != len(r) {
		return fmt.Errorf("fake: scan of %d columns into %d destinations", len(r), len(dest))
	}
	for i, d := range dest {
		dv := reflect.ValueOf(d)
		if dv.Kind() != reflect.Pointer || dv.IsNil() {
			return fmt.Errorf("fake: destination %d is not a non-nil pointer", i)
		}
		v := reflect.ValueOf(r[i])
		if !v.Type().AssignableTo(dv.Elem().Type()) {
			return fmt.Errorf("fake: cannot scan %T into %T", r[i], d)
		}
		dv.Elem().Set(v)
	}
	return nil
}

// Client is a fake ch.Client over a DB.
type Client struct {
	*DB
	ServerInfo ch.ServerInfo
	Caps       *ch.Capabilities
	Servers    []string

	mu     sync.Mutex
	Closed int
}

// NewClient returns a client over db.
func NewClient(db *DB) *Client {
	return &Client{DB: db, ServerInfo: ch.ServerInfo{Version: "26.8.23.6", CurrentUser: "provider", ServerUUID: "s1"}, Caps: ch.NewCapabilities()}
}

// Info implements ch.Client.
func (c *Client) Info() ch.ServerInfo { return c.ServerInfo }

// Capabilities implements ch.Client.
func (c *Client) Capabilities() *ch.Capabilities { return c.Caps }

// ProbeServer implements ch.Client.
func (c *Client) ProbeServer(context.Context) ([]string, error) { return c.Servers, nil }

// Close implements ch.Client.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Closed++
	return nil
}

// Interface checks.
var (
	_ ch.DB     = &DB{}
	_ ch.Client = &Client{}
)
