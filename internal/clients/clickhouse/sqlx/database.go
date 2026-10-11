// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

// Package sqlx builds the SQL statements of every kind. DDL is built as text
// from validated input with the quoting helpers; reads use server-side
// parameters. Builders are pure functions so that they can be tested with
// golden strings.
package sqlx

import (
	"errors"
	"fmt"
	"strings"

	ch "github.com/infrabay/provider-clickhouse/internal/clients/clickhouse"
)

// Database queries with server-side parameters.
const (
	// QueryDatabase reads a database's details. system.databases is filtered
	// by privileges: zero rows after EXISTS DATABASE said 1 means "filtered",
	// never "not found".
	QueryDatabase = "SELECT toString(uuid), engine, engine_full, comment FROM system.databases WHERE name = {name:String}"

	// QueryDatabaseUUID re-reads a database's UUID immediately before a
	// write. Zero rows are checked with EXISTS DATABASE.
	QueryDatabaseUUID = "SELECT toString(uuid) FROM system.databases WHERE name = {name:String}"

	// QueryDatabaseTableCount counts the tables of a database on the
	// connected server. system.tables is filtered by privileges, so it is
	// only trusted after CHECK GRANT SHOW TABLES returned 1.
	QueryDatabaseTableCount = "SELECT count() FROM system.tables WHERE database = {name:String}"
)

// ExistsDatabase returns EXISTS DATABASE `name`. It fails with 497 instead
// of returning 0 when the identity has no privilege on the database.
func ExistsDatabase(name string) string {
	return "EXISTS DATABASE " + ch.QuoteIdent(name)
}

// CheckGrantShowTables returns CHECK GRANT SHOW TABLES ON `name`.*.
func CheckGrantShowTables(name string) string {
	return "CHECK GRANT SHOW TABLES ON " + ch.QuoteIdent(name) + ".*"
}

// ClusterDatabaseIDs reads the host name and UUID of a database on every
// replica of a cluster. A host without the database (or that hides it from
// the identity) returns no row; an unreachable host fails the query. The
// view() form needs only the REMOTE privilege (READ ON REMOTE); reading
// clusterAllReplicas(c, system.databases) directly also needs SHOW COLUMNS
// ON system.databases [verified on 26.8].
func ClusterDatabaseIDs(cluster string) string {
	return "SELECT h, u FROM clusterAllReplicas(" + ch.QuoteString(cluster) +
		", view(SELECT hostName() AS h, toString(uuid) AS u FROM system.databases WHERE name = {name:String}))"
}

// Engine is a database engine with structured arguments.
type Engine struct {
	Name          string
	ZooKeeperPath string
	ShardName     string
	ReplicaName   string
}

// Database engines.
const (
	EngineAtomic     = "Atomic"
	EngineReplicated = "Replicated"
)

// CreateDatabase are the inputs to CreateDatabaseStmt.
type CreateDatabase struct {
	Name    string
	UUID    string
	Cluster string
	Engine  *Engine
	Comment *string
}

func onCluster(cluster string) string {
	if cluster == "" {
		return ""
	}
	return " ON CLUSTER " + ch.QuoteIdent(cluster)
}

// EngineClause renders "Atomic" or "Replicated('<path>', '<shard>', '<replica>')".
func EngineClause(e Engine) (string, error) {
	switch e.Name {
	case EngineAtomic:
		return EngineAtomic, nil
	case EngineReplicated:
		return fmt.Sprintf("%s(%s, %s, %s)", EngineReplicated,
			ch.QuoteString(e.ZooKeeperPath), ch.QuoteString(e.ShardName), ch.QuoteString(e.ReplicaName)), nil
	}
	return "", fmt.Errorf("unsupported database engine %q", e.Name)
}

// CreateDatabaseStmt returns
//
//	CREATE DATABASE `name` UUID '<uuid>' [ON CLUSTER `c`] [ENGINE = ...] [COMMENT '...']
//
// The clause order parses on every supported server version. The UUID is
// always sent: ownership is then known before the statement runs, and every
// ON CLUSTER replica gets the same UUID.
func CreateDatabaseStmt(p CreateDatabase) (string, error) {
	if p.UUID == "" {
		return "", errors.New("a database UUID is required")
	}
	var b strings.Builder
	b.WriteString("CREATE DATABASE ")
	b.WriteString(ch.QuoteIdent(p.Name))
	b.WriteString(" UUID ")
	b.WriteString(ch.QuoteString(p.UUID))
	b.WriteString(onCluster(p.Cluster))
	if p.Engine != nil {
		e, err := EngineClause(*p.Engine)
		if err != nil {
			return "", err
		}
		b.WriteString(" ENGINE = ")
		b.WriteString(e)
	}
	if p.Comment != nil {
		b.WriteString(" COMMENT ")
		b.WriteString(ch.QuoteString(*p.Comment))
	}
	return b.String(), nil
}

// AlterDatabaseCommentStmt returns
// ALTER DATABASE `name` [ON CLUSTER `c`] MODIFY COMMENT '<comment>'.
func AlterDatabaseCommentStmt(name, cluster, comment string) string {
	return "ALTER DATABASE " + ch.QuoteIdent(name) + onCluster(cluster) + " MODIFY COMMENT " + ch.QuoteString(comment)
}

// DropDatabaseStmt returns DROP DATABASE IF EXISTS `name` [ON CLUSTER `c`].
// SYNC is not used: on Atomic it can outlast the reconcile timeout.
func DropDatabaseStmt(name, cluster string) string {
	return "DROP DATABASE IF EXISTS " + ch.QuoteIdent(name) + onCluster(cluster)
}

// maxFileNameBytes is the file name limit of common file systems.
const maxFileNameBytes = 255

// tmpSuffix is appended to the escaped name while the server writes metadata.
const tmpSuffix = ".sql.tmp"

// EscapedFileName escapes a name like the server does for file names: bytes
// other than ASCII letters, digits and '_' become %XX.
func EscapedFileName(name string) string {
	var b strings.Builder
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

// CheckDatabaseNameLength reports an error if the escaped name plus
// ".sql.tmp" does not fit in 255 bytes. The server would fail with 458
// "File name too long".
func CheckDatabaseNameLength(name string) error {
	n := len(EscapedFileName(name)) + len(tmpSuffix)
	if n > maxFileNameBytes {
		return fmt.Errorf("database name is too long: its escaped file name needs %d bytes, the limit is %d", n, maxFileNameBytes)
	}
	return nil
}

// ParseEngineArgs parses the string literal arguments of an engine_full
// value such as "Replicated('/clickhouse/databases/{uuid}', '{shard}',
// '{replica}')". It returns the engine name and its literal arguments. The
// server keeps macros unexpanded in engine_full, so arguments are compared
// verbatim. Anything after the closing parenthesis (e.g. SETTINGS) is
// ignored.
func ParseEngineArgs(engineFull string) (string, []string, error) {
	s := strings.TrimSpace(engineFull)
	open := strings.IndexByte(s, '(')
	if open < 0 {
		return s, nil, nil
	}
	name := strings.TrimSpace(s[:open])
	var args []string
	i := open + 1
	for {
		for i < len(s) && (s[i] == ' ' || s[i] == ',') {
			i++
		}
		if i >= len(s) {
			return "", nil, fmt.Errorf("unterminated engine arguments in %q", engineFull)
		}
		if s[i] == ')' {
			return name, args, nil
		}
		if s[i] != '\'' {
			return "", nil, fmt.Errorf("unexpected engine argument in %q", engineFull)
		}
		lit, n, err := readLiteral(s[i:])
		if err != nil {
			return "", nil, fmt.Errorf("%w in %q", err, engineFull)
		}
		args = append(args, lit)
		i += n
	}
}

// readLiteral reads a single-quoted ClickHouse string literal at the start of
// s and returns its value and length.
func readLiteral(s string) (string, int, error) {
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		c := s[i]
		switch c {
		case '\'':
			return b.String(), i + 1, nil
		case '\\':
			i++
			if i >= len(s) {
				return "", 0, errors.New("unterminated escape")
			}
			switch s[i] {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case '0':
				b.WriteByte(0)
			case 'b':
				b.WriteByte('\b')
			case 'f':
				b.WriteByte('\f')
			case 'x':
				if i+2 >= len(s) {
					return "", 0, errors.New("short \\x escape")
				}
				var v byte
				if _, err := fmt.Sscanf(s[i+1:i+3], "%02x", &v); err != nil {
					return "", 0, errors.New("invalid \\x escape")
				}
				b.WriteByte(v)
				i += 2
			default:
				b.WriteByte(s[i])
			}
		default:
			b.WriteByte(c)
		}
	}
	return "", 0, errors.New("unterminated string literal")
}
