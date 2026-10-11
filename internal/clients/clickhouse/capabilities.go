// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package clickhouse

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"time"

	"github.com/infrabay/provider-clickhouse/internal/reason"
)

// CapabilityTTL is how long a probe result is reused. Refreshing picks up
// server upgrades.
const CapabilityTTL = 10 * time.Minute

// ServerInfo is probed when a client is created. These probes need no grants,
// so a failure means the connection or authentication is broken.
type ServerInfo struct {
	Version     string
	CurrentUser string
	ServerUUID  string
}

// InfoQuery reads ServerInfo in one round trip.
const InfoQuery = "SELECT version(), currentUser(), toString(serverUUID())"

// ProbeInfo reads ServerInfo.
func ProbeInfo(ctx context.Context, db DB) (ServerInfo, error) {
	var i ServerInfo
	err := QueryRow(ctx, db, InfoQuery, nil, &i.Version, &i.CurrentUser, &i.ServerUUID)
	return i, err
}

// Probe queries. They are exported so that tests can match them.
const (
	QueryJWTProbe        = "EXPLAIN AST CREATE USER capability_probe IDENTIFIED WITH jwt"
	QueryUserDirectories = "SELECT name, type, params, precedence FROM system.user_directories ORDER BY precedence"
	QueryClusterHosts    = "SELECT count() FROM system.clusters WHERE cluster = {cluster:String}"
	QueryPrivilegeExists = "SELECT count() FROM system.privileges WHERE privilege = {privilege:String}"

	// QueryPrivileges reads the privilege catalogue. system.privileges is
	// readable without grants. NULL levels and parents are read as "".
	QueryPrivileges = "SELECT toString(privilege), aliases, ifNull(toString(level), ''), ifNull(toString(parent_group), '') FROM system.privileges"

	privilegeDBCommentMod = "ALTER MODIFY DATABASE COMMENT"
)

// System tables and columns that only some server versions have.
const (
	TableUsers  = "users"
	TableGrants = "grants"

	// ColumnValidUntil is system.users.valid_until: absent on 25.3 and 25.8,
	// Array(DateTime) on Antalya 26.3 and 26.6, Array(DateTime64(0)) on 26.8.
	ColumnValidUntil = "valid_until"
	// ColumnAccessObject is system.grants.access_object (25.8 and later).
	ColumnAccessObject = "access_object"
	// ColumnIsWildcard is system.grants.is_wildcard (26.8 and later).
	ColumnIsWildcard = "is_wildcard"
)

// QuerySystemColumnProbe returns the probe for one column of a system table:
// SELECT `column` FROM system.`table` LIMIT 0. It returns no rows and
// succeeds when the column exists; it fails with 47 UNKNOWN_IDENTIFIER (16
// NO_SUCH_COLUMN_IN_TABLE without the analyzer) when it does not.
//
// system.columns is not used: it is filtered by SHOW COLUMNS, so an identity
// that holds SHOW USERS but no SELECT ON system.users sees no columns of
// system.users there and would wrongly conclude that valid_until is missing
// [verified on 25.3.3, 26.6 and 26.8]. The probe needs exactly the grant that
// reading the table needs anyway.
func QuerySystemColumnProbe(table, column string) string {
	return "SELECT " + QuoteIdent(column) + " FROM system." + QuoteIdent(table) + " LIMIT 0"
}

// User directory types (system.user_directories.type). Entities can be
// written only in the first three; users_xml is the read-only directory of
// users.xml and users.d. An entity's storage (system.users.storage,
// system.roles.storage) is the name of its directory, which is its type
// unless the server configuration renames the directory.
const (
	StorageLocalDirectory = "local_directory"
	StorageReplicated     = "replicated"
	StorageMemory         = "memory"
	StorageUsersXML       = "users_xml"
)

type probeResult struct {
	value any
	at    time.Time
}

// Capabilities caches server capability probes. Probes run lazily, when the
// first managed resource that needs them is reconciled, and their results are
// reused for CapabilityTTL. A failing probe is reported to the caller only
// and is not cached, so it never fails other kinds.
type Capabilities struct {
	now func() time.Time
	ttl time.Duration

	mu      sync.Mutex
	results map[string]probeResult
}

// NewCapabilities returns an empty capability cache.
func NewCapabilities() *Capabilities {
	return &Capabilities{now: time.Now, ttl: CapabilityTTL, results: map[string]probeResult{}}
}

func (c *Capabilities) get(key string) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.results[key]
	if !ok || c.now().Sub(r.at) > c.ttl {
		return nil, false
	}
	return r.value, true
}

// forget drops a cached probe result, so that the next call probes again.
func (c *Capabilities) forget(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.results, key)
}

func (c *Capabilities) put(key string, v any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.results[key] = probeResult{value: v, at: c.now()}
}

func probe[T any](ctx context.Context, c *Capabilities, key string, fn func(context.Context) (T, error)) (T, error) {
	if v, ok := c.get(key); ok {
		return v.(T), nil //nolint:forcetypeassert // Keys are private and typed by their probe.
	}
	v, err := fn(ctx)
	if err != nil {
		var zero T
		return zero, err
	}
	c.put(key, v)
	return v, nil
}

// JWTUsers reports whether the server accepts IDENTIFIED WITH jwt (Altinity
// Antalya). The probe has no side effects. 36 BAD_ARGUMENTS ("CREATE USER is
// not supported for JWT") and 62 SYNTAX_ERROR mean unsupported; any other
// error is returned.
func (c *Capabilities) JWTUsers(ctx context.Context, db DB) (bool, error) {
	return probe(ctx, c, "jwt", func(ctx context.Context) (bool, error) {
		err := db.Query(ctx, QueryJWTProbe, nil, func(Row) error { return nil })
		switch {
		case err == nil:
			return true, nil
		case IsCode(err, CodeBadArguments, CodeSyntaxError):
			return false, nil
		default:
			return false, err
		}
	})
}

// UserDirectory is a row of system.user_directories.
type UserDirectory struct {
	Name string
	Type string
	// Params is the directory's configuration as JSON, for example
	// {"path":"/var/lib/clickhouse/access/","readonly":true}.
	Params     string
	Precedence uint64
}

// Writable reports whether access entities in the directory can be written:
// its type is local_directory, replicated or memory, and its configuration
// does not make it read-only (a local_directory with <readonly>true</readonly>
// shows "readonly":true in params). users_xml and every other type are
// read-only.
func (d UserDirectory) Writable() bool {
	if !slices.Contains([]string{StorageLocalDirectory, StorageReplicated, StorageMemory}, d.Type) {
		return false
	}
	var p struct {
		ReadOnly bool `json:"readonly"`
	}
	if err := json.Unmarshal([]byte(d.Params), &p); err == nil && p.ReadOnly {
		return false
	}
	return true
}

// UserDirectories returns the server's user directories in precedence order.
// The configuration of user directories is read only at server start.
func (c *Capabilities) UserDirectories(ctx context.Context, db DB) ([]UserDirectory, error) {
	return probe(ctx, c, "directories", func(ctx context.Context) ([]UserDirectory, error) {
		var dirs []UserDirectory
		err := db.Query(ctx, QueryUserDirectories, nil, func(r Row) error {
			var d UserDirectory
			if err := r.Scan(&d.Name, &d.Type, &d.Params, &d.Precedence); err != nil {
				return err
			}
			dirs = append(dirs, d)
			return nil
		})
		if err != nil {
			return nil, WithDefaultReason(err)
		}
		return dirs, nil
	})
}

// AccessStorage returns the type of the first writable user directory, where
// new access entities are stored: local_directory, replicated or memory.
func (c *Capabilities) AccessStorage(ctx context.Context, db DB) (string, error) {
	dirs, err := c.UserDirectories(ctx, db)
	if err != nil {
		return "", err
	}
	return FirstWritableDirectory(dirs)
}

// FirstWritableDirectory returns the type of the first writable directory.
func FirstWritableDirectory(dirs []UserDirectory) (string, error) {
	for _, d := range dirs {
		if d.Writable() {
			return d.Type, nil
		}
	}
	return "", reason.Errorf(reason.InvalidConfig, "the server has no writable user directory")
}

// StorageReadOnly reports whether the user directory that holds an access
// entity refuses writes, given the entity's storage (system.users.storage or
// system.roles.storage). That is the name of the directory, not its type: a
// configuration may rename users_xml (<users_xml><name>...</name>), so the
// name is looked up in system.user_directories. A name that is not among
// the cached directories is looked up once more without the cache (the
// server reads its user directories only at start, so a restart may have
// changed them); a name that is still unknown counts as read-only, so the
// provider never writes to an entity whose directory it cannot tell.
func (c *Capabilities) StorageReadOnly(ctx context.Context, db DB, storage string) (bool, error) {
	for refreshed := false; ; refreshed = true {
		dirs, err := c.UserDirectories(ctx, db)
		if err != nil {
			return false, err
		}
		for _, d := range dirs {
			if d.Name == storage {
				return !d.Writable(), nil
			}
		}
		if refreshed {
			return true, nil
		}
		c.forget("directories")
	}
}

// ClusterHosts returns the number of hosts of a cluster in system.clusters.
// Zero is an InvalidConfig error ("unknown cluster").
func (c *Capabilities) ClusterHosts(ctx context.Context, db DB, cluster string) (uint64, error) {
	return probe(ctx, c, "cluster/"+cluster, func(ctx context.Context) (uint64, error) {
		var n uint64
		if err := QueryRow(ctx, db, QueryClusterHosts, Params{"cluster": cluster}, &n); err != nil {
			return 0, WithDefaultReason(err)
		}
		if n == 0 {
			return 0, reason.Errorf(reason.InvalidConfig, "unknown cluster %q (not in system.clusters)", cluster)
		}
		return n, nil
	})
}

// HasSystemColumn reports whether a system table has a column, e.g.
// system.grants.is_wildcard or system.users.valid_until (see
// QuerySystemColumnProbe). A missing column is false; any other failure,
// such as 497 without the grant that reading the table needs, is returned.
func (c *Capabilities) HasSystemColumn(ctx context.Context, db DB, table, column string) (bool, error) {
	return probe(ctx, c, "column/"+table+"."+column, func(ctx context.Context) (bool, error) {
		err := db.Query(ctx, QuerySystemColumnProbe(table, column), nil, func(Row) error { return nil })
		switch {
		case err == nil:
			return true, nil
		case IsCode(err, CodeUnknownIdentifier, CodeNoSuchColumnInTable):
			return false, nil
		default:
			return false, WithDefaultReason(err)
		}
	})
}

// UsersValidUntil reports whether system.users has valid_until. Without it
// (25.3, 25.8) VALID UNTIL is visible only in SHOW CREATE USER. Reading it
// needs SHOW USERS, which every User needs anyway.
func (c *Capabilities) UsersValidUntil(ctx context.Context, db DB) (bool, error) {
	return c.HasSystemColumn(ctx, db, TableUsers, ColumnValidUntil)
}

// GrantsAccessObject reports whether system.grants has access_object (25.8
// and later). Rows with a non-empty access_object (source privileges such as
// READ ON S3, prefix-scoped access management) are never managed by Grant.
func (c *Capabilities) GrantsAccessObject(ctx context.Context, db DB) (bool, error) {
	return c.HasSystemColumn(ctx, db, TableGrants, ColumnAccessObject)
}

// GrantsIsWildcard reports whether system.grants has is_wildcard (26.8 and
// later). Without it a wildcard grant (ON team_*.*) cannot be told apart from
// a literal grant on a database named "team_" in system.grants, and Grant
// falls back to scanning SHOW GRANTS (design 5.6, D-10).
func (c *Capabilities) GrantsIsWildcard(ctx context.Context, db DB) (bool, error) {
	return c.HasSystemColumn(ctx, db, TableGrants, ColumnIsWildcard)
}

// A Privilege is a row of system.privileges. Level and Parent are "" where
// the server has NULL (for example SHOW FILESYSTEM CACHES has no level, and
// ALL and NONE have no parent).
type Privilege struct {
	Name    string
	Aliases []string
	Level   string
	Parent  string
}

// Privileges returns the privilege catalogue. It is read once per client and
// cached like every probe, so a server upgrade is picked up after
// CapabilityTTL. Callers must not modify the result: it is shared.
func (c *Capabilities) Privileges(ctx context.Context, db DB) ([]Privilege, error) {
	return probe(ctx, c, "privileges", func(ctx context.Context) ([]Privilege, error) {
		var out []Privilege
		err := db.Query(ctx, QueryPrivileges, nil, func(r Row) error {
			var p Privilege
			if err := r.Scan(&p.Name, &p.Aliases, &p.Level, &p.Parent); err != nil {
				return err
			}
			out = append(out, p)
			return nil
		})
		if err != nil {
			return nil, WithDefaultReason(err)
		}
		if len(out) == 0 {
			return nil, reason.Errorf(reason.Internal, "system.privileges returned no rows")
		}
		return out, nil
	})
}

// DatabaseCommentChange reports whether the server supports
// ALTER DATABASE ... MODIFY COMMENT, by the presence of the privilege
// ALTER MODIFY DATABASE COMMENT in system.privileges (missing on 25.3).
// system.privileges is readable without grants.
func (c *Capabilities) DatabaseCommentChange(ctx context.Context, db DB) (bool, error) {
	return probe(ctx, c, "db-comment", func(ctx context.Context) (bool, error) {
		var n uint64
		if err := QueryRow(ctx, db, QueryPrivilegeExists, Params{"privilege": privilegeDBCommentMod}, &n); err != nil {
			return false, WithDefaultReason(err)
		}
		return n > 0, nil
	})
}

// ServerSet tracks the server UUIDs an endpoint reached recently. A load
// balanced Service in front of several replicas with local user directories
// is the most common topology mistake; seeing more than one server UUID within
// the window reveals it (best effort).
type ServerSet struct {
	now    func() time.Time
	window time.Duration

	mu   sync.Mutex
	seen map[string]time.Time
}

// NewServerSet returns a set that remembers servers for window.
func NewServerSet(window time.Duration) *ServerSet {
	return &ServerSet{now: time.Now, window: window, seen: map[string]time.Time{}}
}

// Add records a server UUID.
func (s *ServerSet) Add(uuid string) {
	if uuid == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen[uuid] = s.now()
}

// Servers returns the server UUIDs seen within the window, sorted.
func (s *ServerSet) Servers() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.seen))
	for u, at := range s.seen {
		if s.now().Sub(at) > s.window {
			delete(s.seen, u)
			continue
		}
		out = append(out, u)
	}
	slices.Sort(out)
	return out
}

// CheckAccessStorage decides whether access entities (users, roles, grants)
// may be written. With a cluster of more than one host, or when more than one
// server was seen behind the endpoint, the first writable user directory must
// be replicated.
func CheckAccessStorage(endpoint, storage string, clusterHosts uint64, servers []string) error {
	if storage == StorageReplicated {
		return nil
	}
	if clusterHosts > 1 {
		return reason.Errorf(reason.AccessStorageNotReplicated,
			"the cluster has %d hosts but new access entities are stored in %s; use a replicated user directory", clusterHosts, storage)
	}
	if len(servers) > 1 {
		return reason.Errorf(reason.AccessStorageNotReplicated,
			"endpoint %s reaches multiple servers (%v) using %s; use a replicated user directory or point the endpoint at one replica", endpoint, servers, storage)
	}
	return nil
}
