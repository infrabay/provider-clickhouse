// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package clickhouse

import (
	"context"
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
	QueryJWTProbe         = "EXPLAIN AST CREATE USER capability_probe IDENTIFIED WITH jwt"
	QueryUserDirectories  = "SELECT name, type, precedence FROM system.user_directories ORDER BY precedence"
	QueryClusterHosts     = "SELECT count() FROM system.clusters WHERE cluster = {cluster:String}"
	QuerySystemColumns    = "SELECT name FROM system.columns WHERE database = 'system' AND table = {table:String}"
	QueryPrivilegeExists  = "SELECT count() FROM system.privileges WHERE privilege = {privilege:String}"
	privilegeDBCommentMod = "ALTER MODIFY DATABASE COMMENT"
)

// Writable user directory types.
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
	Name       string
	Type       string
	Precedence uint64
}

// AccessStorage returns the type of the first writable user directory, where
// new access entities are stored: local_directory, replicated or memory.
func (c *Capabilities) AccessStorage(ctx context.Context, db DB) (string, error) {
	return probe(ctx, c, "storage", func(ctx context.Context) (string, error) {
		var dirs []UserDirectory
		err := db.Query(ctx, QueryUserDirectories, nil, func(r Row) error {
			var d UserDirectory
			if err := r.Scan(&d.Name, &d.Type, &d.Precedence); err != nil {
				return err
			}
			dirs = append(dirs, d)
			return nil
		})
		if err != nil {
			return "", WithDefaultReason(err)
		}
		return FirstWritableDirectory(dirs)
	})
}

// FirstWritableDirectory returns the type of the first writable directory.
func FirstWritableDirectory(dirs []UserDirectory) (string, error) {
	for _, d := range dirs {
		if slices.Contains([]string{StorageLocalDirectory, StorageReplicated, StorageMemory}, d.Type) {
			return d.Type, nil
		}
	}
	return "", reason.Errorf(reason.InvalidConfig, "the server has no writable user directory")
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

// SystemColumns returns the column names of a system table.
func (c *Capabilities) SystemColumns(ctx context.Context, db DB, table string) ([]string, error) {
	return probe(ctx, c, "columns/"+table, func(ctx context.Context) ([]string, error) {
		var cols []string
		err := db.Query(ctx, QuerySystemColumns, Params{"table": table}, func(r Row) error {
			var n string
			if err := r.Scan(&n); err != nil {
				return err
			}
			cols = append(cols, n)
			return nil
		})
		return cols, WithDefaultReason(err)
	})
}

// HasSystemColumn reports whether a system table has a column, e.g.
// system.grants.is_wildcard or system.users.valid_until.
func (c *Capabilities) HasSystemColumn(ctx context.Context, db DB, table, column string) (bool, error) {
	cols, err := c.SystemColumns(ctx, db, table)
	if err != nil {
		return false, err
	}
	return slices.Contains(cols, column), nil
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
