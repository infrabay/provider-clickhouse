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
	"github.com/infrabay/provider-clickhouse/internal/clients/clickhouse/sqlx"
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
	// ExecFn handles statements. nil accepts every statement except those
	// scripted in ExecErrors.
	ExecFn func(stmt string) error
	// ExecErrors maps exact statements to the error Exec returns for them,
	// when ExecFn is nil.
	ExecErrors map[string]error

	// users and roles back the scripted system.users and system.roles
	// entity reads (WithUser, WithRole): name -> {id, storage}.
	users map[string][2]string
	roles map[string][2]string

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

// OnExec scripts the error Exec returns for one exact statement.
func (d *DB) OnExec(stmt string, err error) *DB {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ExecErrors == nil {
		d.ExecErrors = map[string]error{}
	}
	d.ExecErrors[stmt] = err
	return d
}

// Exec implements ch.DB.
func (d *DB) Exec(_ context.Context, stmt string) error {
	d.mu.Lock()
	d.Execs = append(d.Execs, stmt)
	fn := d.ExecFn
	err := d.ExecErrors[stmt]
	d.mu.Unlock()
	if fn == nil {
		return err
	}
	return fn(stmt)
}

// WithEntities scripts the entity reads of system.users and system.roles
// (sqlx.QueryUserEntity, sqlx.QueryRoleEntity) with no users and no roles.
// WithUser and WithRole add entities; they call it implicitly.
func (d *DB) WithEntities() *DB {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.ensureEntitiesLocked()
	return d
}

func (d *DB) ensureEntitiesLocked() {
	if d.users != nil {
		return
	}
	d.users, d.roles = map[string][2]string{}, map[string][2]string{}
	lookup := func(m map[string][2]string) func(ch.Params) Result {
		return func(p ch.Params) Result {
			d.mu.Lock()
			defer d.mu.Unlock()
			e, ok := m[p["name"]]
			if !ok {
				return Rows()
			}
			return Row(e[0], e[1])
		}
	}
	d.Queries[sqlx.QueryUserEntity] = lookup(d.users)
	d.Queries[sqlx.QueryRoleEntity] = lookup(d.roles)
}

// WithUser scripts system.users (sqlx.QueryUserEntity) to report a user
// with the given UUID and storage. Users that were not added are absent.
func (d *DB) WithUser(name, id, storage string) *DB {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.ensureEntitiesLocked()
	d.users[name] = [2]string{id, storage}
	return d
}

// WithRole scripts system.roles (sqlx.QueryRoleEntity) to report a role
// with the given UUID and storage. Roles that were not added are absent.
func (d *DB) WithRole(name, id, storage string) *DB {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.ensureEntitiesLocked()
	d.roles[name] = [2]string{id, storage}
	return d
}

// RemoveUser makes a user scripted with WithUser absent again.
func (d *DB) RemoveUser(name string) *DB {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.users, name)
	return d
}

// RemoveRole makes a role scripted with WithRole absent again.
func (d *DB) RemoveRole(name string) *DB {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.roles, name)
	return d
}

// A Profile describes the capabilities of one supported server version, as
// verified against the images of the CI matrix (design section 7).
type Profile struct {
	Version string
	// JWTUsers: the server accepts IDENTIFIED WITH jwt (Antalya).
	JWTUsers bool
	// JWTProbeCode is the error code of the jwt probe when JWTUsers is
	// false: 62 SYNTAX_ERROR on 25.3, 36 BAD_ARGUMENTS on 25.8 and 26.8.
	JWTProbeCode int32
	// ValidUntil: system.users.valid_until exists.
	ValidUntil bool
	// AccessObject and IsWildcard: system.grants has these columns.
	AccessObject bool
	IsWildcard   bool
	// DBComment: the privilege ALTER MODIFY DATABASE COMMENT exists.
	DBComment bool
	// Storage is the first writable user directory; "" means
	// local_directory.
	Storage string
}

// Profiles of the supported server versions.
var (
	Upstream253 = Profile{Version: "25.3.3.42", JWTProbeCode: ch.CodeSyntaxError}
	Upstream258 = Profile{Version: "25.8.33.6", JWTProbeCode: ch.CodeBadArguments, AccessObject: true, DBComment: true}
	Upstream268 = Profile{Version: "26.8.23.6", JWTProbeCode: ch.CodeBadArguments, ValidUntil: true, AccessObject: true, IsWildcard: true, DBComment: true}
	Antalya263  = Profile{Version: "26.3.13.20001.altinityantalya", JWTUsers: true, ValidUntil: true, AccessObject: true, DBComment: true}
	Antalya266  = Profile{Version: "26.6.4.20001.altinityantalya", JWTUsers: true, ValidUntil: true, AccessObject: true, DBComment: true}

	// Profiles lists every supported server version, oldest first.
	Profiles = []Profile{Upstream253, Upstream258, Upstream268, Antalya263, Antalya266}
)

// WithProfile scripts every capability probe for a server version: the jwt
// probe, the user directories, the system table columns and the database
// comment privilege.
func (d *DB) WithProfile(p Profile) *DB {
	jwt := Rows([]any{"CreateUserQuery (children 1)"})
	if !p.JWTUsers {
		jwt = Error(Exception(p.JWTProbeCode, "jwt users are not supported"))
	}
	column := func(present bool) Result {
		if present {
			return Rows()
		}
		return Error(Exception(ch.CodeUnknownIdentifier, "Unknown expression identifier"))
	}
	storage := p.Storage
	if storage == "" {
		storage = ch.StorageLocalDirectory
	}
	comment := uint64(0)
	if p.DBComment {
		comment = 1
	}
	return d.On(ch.QueryJWTProbe, jwt).
		On(ch.QueryUserDirectories, Rows([]any{"users_xml", ch.StorageUsersXML, uint64(0)}, []any{storage, storage, uint64(1)})).
		On(ch.QuerySystemColumnProbe(ch.TableUsers, ch.ColumnValidUntil), column(p.ValidUntil)).
		On(ch.QuerySystemColumnProbe(ch.TableGrants, ch.ColumnAccessObject), column(p.AccessObject)).
		On(ch.QuerySystemColumnProbe(ch.TableGrants, ch.ColumnIsWildcard), column(p.IsWildcard)).
		On(ch.QueryPrivilegeExists, Row(comment))
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
	// Servers is what ProbeServer reports: the server UUIDs seen behind the
	// endpoint. Empty means one server.
	Servers []string
	// ProbeErr, if set, is returned by ProbeServer.
	ProbeErr error

	mu     sync.Mutex
	Closed int
}

// NewClient returns a client over db.
func NewClient(db *DB) *Client {
	return &Client{DB: db, ServerInfo: ch.ServerInfo{Version: "26.8.23.6", CurrentUser: "provider", ServerUUID: "s1"}, Caps: ch.NewCapabilities()}
}

// NewClientFor returns a client over db that reports the version of p, with
// db scripted for p's capability probes (WithProfile).
func NewClientFor(db *DB, p Profile) *Client {
	c := NewClient(db.WithProfile(p))
	c.ServerInfo.Version = p.Version
	return c
}

// Info implements ch.Client.
func (c *Client) Info() ch.ServerInfo { return c.ServerInfo }

// Capabilities implements ch.Client.
func (c *Client) Capabilities() *ch.Capabilities { return c.Caps }

// ProbeServer implements ch.Client. It returns Servers, or ProbeErr.
func (c *Client) ProbeServer(context.Context) ([]string, error) {
	if c.ProbeErr != nil {
		return nil, c.ProbeErr
	}
	return c.Servers, nil
}

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
