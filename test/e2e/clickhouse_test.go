// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	chgo "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	ch "github.com/infrabay/provider-clickhouse/internal/clients/clickhouse"
)

// server is one ClickHouse server of hack/e2e/manifests/clickhouse.yaml,
// reached through a port-forward, with an administrator connection used only
// for assertions and for out-of-band changes.
type server struct {
	// name is the Deployment and Service name: upstream or antalya.
	name string
	// host is the in-cluster DNS name the ProviderConfigs use.
	host string
	// addr is the local end of the port-forward to the native port.
	addr  string
	admin driver.Conn
}

func connectServer(t *testing.T, name string) *server {
	t.Helper()
	pod := readyPod(t, nsClickHouse, "app.kubernetes.io/instance="+name)
	s := &server{name: name, host: name + "." + nsClickHouse + ".svc.cluster.local", addr: portForward(t, nsClickHouse, pod, 9000)}
	conn, err := chgo.Open(&chgo.Options{
		Addr:        []string{s.addr},
		Auth:        chgo.Auth{Username: "admin", Password: string(secretData(t, nsClickHouse, name+"-admin", "password"))},
		DialTimeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	eventually(t, 60*time.Second, name+" accepts the administrator", func() (bool, string) {
		if err := conn.Ping(context.Background()); err != nil {
			return false, err.Error()
		}
		return true, ""
	})
	s.admin = conn
	return s
}

// exec runs admin SQL. A failure prints the statement, so statements with a
// secret go through execSecret.
func (s *server) exec(t *testing.T, stmt string) {
	t.Helper()
	if err := s.admin.Exec(context.Background(), stmt); err != nil {
		t.Fatalf("%s admin %q: %v", s.name, stmt, err)
	}
}

// execSecret runs admin SQL that contains secret (a password the test
// generated); a failure is reported with the secret replaced by [REDACTED],
// in the statement and in the error.
func (s *server) execSecret(t *testing.T, stmt, secret string) {
	t.Helper()
	if err := s.admin.Exec(context.Background(), stmt); err != nil {
		t.Fatalf("%s admin %q: %s", s.name, redact(stmt, secret), redact(err.Error(), secret))
	}
}

// redact replaces secret in s with [REDACTED].
func redact(s, secret string) string {
	if secret == "" {
		return s
	}
	return strings.ReplaceAll(s, secret, "[REDACTED]")
}

// strings runs an admin query and returns the first column of every row as
// the driver returns it.
func (s *server) strings(t *testing.T, query string, args ...any) []string {
	t.Helper()
	rows, err := s.admin.Query(context.Background(), query, args...)
	if err != nil {
		t.Fatalf("%s admin %q: %v", s.name, query, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// count runs an admin query that returns one UInt64.
func (s *server) count(t *testing.T, query string, args ...any) uint64 {
	t.Helper()
	var n uint64
	if err := s.admin.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("%s admin %q: %v", s.name, query, err)
	}
	return n
}

// Kinds of ClickHouse objects, as in SHOW CREATE <kind>.
const (
	kindUser     = "USER"
	kindRole     = "ROLE"
	kindDatabase = "DATABASE"
)

// uuid returns the UUID of a user, role or database, or "" if it does not
// exist.
func (s *server) uuid(t *testing.T, kind, name string) string {
	t.Helper()
	q := map[string]string{
		kindUser:     "SELECT toString(id) FROM system.users WHERE name = ?",
		kindRole:     "SELECT toString(id) FROM system.roles WHERE name = ?",
		kindDatabase: "SELECT toString(uuid) FROM system.databases WHERE name = ?",
	}[kind]
	ids := s.strings(t, q, name)
	switch len(ids) {
	case 0:
		return ""
	case 1:
		return ids[0]
	}
	t.Fatalf("%s: %d %ss named %q", s.name, len(ids), strings.ToLower(kind), name)
	return ""
}

// storage returns the access storage of a user or role.
func (s *server) storage(t *testing.T, kind, name string) string {
	t.Helper()
	table := "users"
	if kind == kindRole {
		table = "roles"
	}
	v := s.strings(t, "SELECT storage FROM system."+table+" WHERE name = ?", name)
	if len(v) != 1 {
		return ""
	}
	return v[0]
}

// comment returns a database's comment.
func (s *server) comment(t *testing.T, db string) string {
	t.Helper()
	v := s.strings(t, "SELECT comment FROM system.databases WHERE name = ?", db)
	if len(v) != 1 {
		t.Fatalf("%s: database %s does not exist", s.name, db)
	}
	return v[0]
}

// showCreate returns SHOW CREATE <kind> <name> exactly as the server prints
// it.
func (s *server) showCreate(t *testing.T, kind, name string) string {
	t.Helper()
	return strings.Join(s.strings(t, "SHOW CREATE "+kind+" "+ch.QuoteIdent(name)), "\n")
}

// grantsOf returns the rows of system.grants of a user or role as
// "<access type> <object>[ partial-revoke][ grant-option]", sorted.
func (s *server) grantsOf(t *testing.T, kind, name string) []string {
	t.Helper()
	col := "user_name"
	if kind == kindRole {
		col = "role_name"
	}
	rows := s.strings(t, "SELECT concat(toString(access_type), ' ', "+
		"multiIf(column IS NOT NULL, concat(database, '.', table, '(', column, ')'), table IS NOT NULL, concat(database, '.', table), database IS NOT NULL, concat(database, '.*'), '*.*'), "+
		"if(is_partial_revoke, ' partial-revoke', ''), if(grant_option, ' grant-option', '')) FROM system.grants WHERE "+col+" = ?", name)
	sort.Strings(rows)
	return rows
}

// member reports whether a user holds a role, and with the admin option.
func (s *server) member(t *testing.T, user, role string) (ok, admin bool) {
	t.Helper()
	rows := s.strings(t, "SELECT toString(with_admin_option) FROM system.role_grants WHERE user_name = ? AND granted_role_name = ?", user, role)
	if len(rows) == 0 {
		return false, false
	}
	return true, rows[0] == "1"
}

// names returns the names of the users, roles and databases that start with
// prefix, as "<kind> <name>", sorted.
func (s *server) names(t *testing.T, prefix string) []string {
	t.Helper()
	rows := s.strings(t, "SELECT concat('USER ', name) FROM system.users WHERE startsWith(name, ?) "+
		"UNION ALL SELECT concat('ROLE ', name) FROM system.roles WHERE startsWith(name, ?) "+
		"UNION ALL SELECT concat('DATABASE ', name) FROM system.databases WHERE startsWith(name, ?)", prefix, prefix, prefix)
	sort.Strings(rows)
	return rows
}

// snapshot is everything an administrator sees of a set of objects: their
// UUIDs, SHOW CREATE output and grants, and the memberships of the users
// and roles, exactly as the server prints them.
func (s *server) snapshot(t *testing.T, objs ...[2]string) []string {
	t.Helper()
	var out []string
	entities := []string{}
	for _, o := range objs {
		kind, name := o[0], o[1]
		out = append(out, kind+" "+name+" uuid="+s.uuid(t, kind, name), s.showCreate(t, kind, name))
		if kind != kindDatabase {
			out = append(out, strings.Join(s.strings(t, "SHOW GRANTS FOR "+ch.QuoteIdent(name)), "\n"))
			entities = append(entities, name)
		}
	}
	out = append(out, s.strings(t, "SELECT concat(ifNull(user_name, ''), '|', ifNull(role_name, ''), '|', granted_role_name, '|', toString(with_admin_option), '|', toString(granted_role_is_default)) "+
		"FROM system.role_grants WHERE has(?, ifNull(user_name, '')) OR has(?, ifNull(role_name, '')) OR has(?, granted_role_name) ORDER BY 1", entities, entities, entities)...)
	return out
}

// writeKinds are the query kinds of system.query_log that change something.
const writeKinds = "('Insert', 'Delete', 'Update', 'Create', 'Drop', 'Undrop', 'Rename', 'Alter', 'Grant', 'Revoke', 'Move', 'Optimize', 'System')"

// statements counts the statements the provider ran for one MR (they carry
// log_comment = "<Kind>/<namespace>/<name>", design 5.1) that changed
// something: all of them, or only those that succeeded.
func (s *server) statements(t *testing.T, logComment string, succeededOnly bool) uint64 {
	t.Helper()
	s.exec(t, "SYSTEM FLUSH LOGS")
	types := "('QueryStart')"
	if succeededOnly {
		types = "('QueryFinish')"
	}
	return s.count(t, "SELECT count() FROM system.query_log WHERE log_comment = ? AND type IN "+types+" AND query_kind IN "+writeKinds, logComment)
}

// usersOf returns the ClickHouse users that ran the statements of one MR
// (by log_comment), sorted.
func (s *server) usersOf(t *testing.T, logComment string) []string {
	t.Helper()
	s.exec(t, "SYSTEM FLUSH LOGS")
	return s.strings(t, "SELECT DISTINCT user FROM system.query_log WHERE log_comment = ? ORDER BY user", logComment)
}

// creates counts the CREATE statements for one MR that succeeded. The query
// kind does not tell them apart: ALTER USER and ALTER ROLE are logged with
// query_kind 'Create' too.
func (s *server) creates(t *testing.T, logComment string) uint64 {
	t.Helper()
	s.exec(t, "SYSTEM FLUSH LOGS")
	return s.count(t, "SELECT count() FROM system.query_log WHERE log_comment = ? AND type = 'QueryFinish' AND match(query, '^\\\\s*(?i:CREATE)\\\\s')", logComment)
}

// expectDenied checks that stmt fails with 497 ACCESS_DENIED on conn.
func expectDenied(t *testing.T, conn driver.Conn, stmt string) {
	t.Helper()
	if code := exceptionCode(conn.Exec(context.Background(), stmt)); code != 497 {
		t.Errorf("%s: code %d, want 497 (ACCESS_DENIED)", stmt, code)
	}
}

// exceptionCode returns the code of a ClickHouse exception, or 0.
func exceptionCode(err error) int32 {
	var ex *chgo.Exception
	if errors.As(err, &ex) {
		return ex.Code
	}
	return 0
}

// open connects to the server through its port-forward with the given
// options and checks the connection.
func (s *server) open(t *testing.T, opts *chgo.Options) (driver.Conn, error) {
	t.Helper()
	opts.Addr = []string{s.addr}
	opts.DialTimeout = 10 * time.Second
	conn, err := chgo.Open(opts)
	if err != nil {
		return nil, err
	}
	if err := conn.Ping(context.Background()); err != nil {
		_ = conn.Close()
		return nil, err
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn, nil
}

// whoami returns currentUser() and currentDatabase() of a connection.
func whoami(t *testing.T, conn driver.Conn) (string, string) {
	t.Helper()
	var user, db string
	if err := conn.QueryRow(context.Background(), "SELECT currentUser(), currentDatabase()").Scan(&user, &db); err != nil {
		t.Fatalf("SELECT currentUser(): %v", err)
	}
	return user, db
}

func fmtList(l []string) string { return fmt.Sprintf("%q", l) }
