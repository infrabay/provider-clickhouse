// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package sqlx

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	ch "github.com/infrabay/provider-clickhouse/internal/clients/clickhouse"
)

// Levels of system.privileges.level. A privilege without a level (NULL) is a
// group: it carries no access flag of its own.
const (
	LevelGlobal          = "GLOBAL"
	LevelDatabase        = "DATABASE"
	LevelTable           = "TABLE"
	LevelDictionary      = "DICTIONARY"
	LevelView            = "VIEW"
	LevelColumn          = "COLUMN"
	LevelNamedCollection = "NAMED_COLLECTION"
	LevelUserName        = "USER_NAME"
	LevelTableEngine     = "TABLE_ENGINE"
	LevelDefiner         = "DEFINER"
	LevelSource          = "SOURCE"
)

// Privileges the model treats specially.
const (
	PrivilegeAll     = "ALL"
	PrivilegeNone    = "NONE"
	PrivilegeSources = "SOURCES"
)

// unsupportedLevels are the levels whose privileges Grant does not manage in
// v1alpha1. Their flags are rejected in a spec and ignored in system.grants.
// CREATE USER, ALTER USER, DROP USER and CREATE ROLE are USER_NAME-level
// privileges, so ALL on *.* is rejected too.
var unsupportedLevels = map[string]bool{
	LevelNamedCollection: true,
	LevelUserName:        true,
	LevelTableEngine:     true,
	LevelDefiner:         true,
	LevelSource:          true,
}

// parameterLevels are the levels whose privileges are granted on a parameter
// (ON name or ON prefix*: a named collection, a user or role name prefix, a
// table engine, a definer, a source) instead of on a database. The server
// keeps such a grant among the database names, named by the parameter
// (system.grants.access_object from 25.8 on).
var parameterLevels = map[string]bool{
	LevelNamedCollection: true,
	LevelUserName:        true,
	LevelTableEngine:     true,
	LevelDefiner:         true,
	LevelSource:          true,
}

// globalLeavesUnderAll are the GLOBAL leaves directly under ALL that are not
// source privileges, verified on every supported server version. From 25.8
// on, source privileges (S3, URL, REMOTE, ...) are GLOBAL leaves directly
// under ALL as well, and every release adds new ones (YTSAURUS and ARROW
// FLIGHT on Antalya 26.6 and 26.8, BIGQUERY and DISK on 26.8), so every other
// GLOBAL leaf directly under ALL is treated as a source privilege: the rule
// fails closed (D-9).
var globalLeavesUnderAll = map[string]bool{
	"KILL QUERY":                    true,
	"KILL TRANSACTION":              true,
	"MOVE PARTITION BETWEEN SHARDS": true,
	"SHOW HANDLERS":                 true,
	"displaySecretsInShowAndSelect": true,
	"CLUSTER":                       true,
}

// privilegeKeyword is the shape of a privilege name that may be written into
// a statement unquoted. Names come from the server's catalogue; the check is
// defence in depth.
var privilegeKeyword = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*( [A-Za-z0-9_]+)*$`)

// A TargetLevel is the level of a grant target: ON *.*, ON db.*, ON db.t or
// ON db.t(columns).
type TargetLevel int

// Target levels, from the broadest.
const (
	TargetGlobal TargetLevel = iota + 1
	TargetDatabase
	TargetTable
	TargetColumn
)

func (l TargetLevel) String() string {
	switch l {
	case TargetGlobal:
		return "global"
	case TargetDatabase:
		return "database"
	case TargetTable:
		return "table"
	case TargetColumn:
		return "column"
	}
	return fmt.Sprintf("TargetLevel(%d)", int(l))
}

// applicable reports whether a flag of the given privilege level can be
// granted on a target of level t, as the server decides: a privilege of
// level DATABASE can be granted ON *.* and ON db.*, but not ON db.t (509
// INVALID_GRANT "cannot be granted on the table level").
func applicable(level string, t TargetLevel) bool {
	switch level {
	case LevelGlobal:
		return t == TargetGlobal
	case LevelDatabase:
		return t <= TargetDatabase
	case LevelTable, LevelView, LevelDictionary:
		return t <= TargetTable
	case LevelColumn:
		return true
	}
	return false
}

// A PrivilegeSet is a set of canonical privilege names.
type PrivilegeSet map[string]struct{}

// NewPrivilegeSet returns a set of the given names.
func NewPrivilegeSet(names ...string) PrivilegeSet {
	s := PrivilegeSet{}
	for _, n := range names {
		s[n] = struct{}{}
	}
	return s
}

// Has reports whether n is in s.
func (s PrivilegeSet) Has(n string) bool {
	_, ok := s[n]
	return ok
}

// Equal reports whether s and o hold the same names; nil and empty sets are
// equal.
func (s PrivilegeSet) Equal(o PrivilegeSet) bool {
	if len(s) != len(o) {
		return false
	}
	for n := range s {
		if !o.Has(n) {
			return false
		}
	}
	return true
}

// Sorted returns the names in s, sorted.
func (s PrivilegeSet) Sorted() []string {
	out := make([]string, 0, len(s))
	for n := range s {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

// catPrivilege is a privilege of the catalogue with its derived properties.
type catPrivilege struct {
	ch.Privilege
	children []string
	// flags are the privileges whose access flags this privilege grants:
	// itself if it has a level, and every descendant that has one, sorted.
	flags []string
	// unmanaged is set for source privileges (D-9).
	unmanaged bool
}

// A Catalogue is the privilege tree of one server, built from
// system.privileges (design 5.6).
//
// Privileges are compared as access flags. Every privilege with a level
// carries one flag of its own; a group (no level) carries none, and grants
// the flags of its descendants. Most privileges with a level are leaves, so
// this is mostly the model of comparing leaves, but not entirely:
//   - CREATE ARBITRARY TEMPORARY TABLE has a level and a flag of its own
//     besides that of its only child CREATE TEMPORARY TABLE: granting every
//     leaf under CREATE ON *.* stores eight rows instead of one CREATE row,
//     and after REVOKE CREATE TEMPORARY TABLE from a role that holds CREATE
//     ARBITRARY TEMPORARY TABLE, the flag that remains is shown nowhere in
//     system.grants [verified on every supported version];
//   - SHOW FILESYSTEM CACHES has neither a level nor children: it carries no
//     flag, and GRANT SHOW FILESYSTEM CACHES ON *.* stores nothing [verified
//     on 25.3.3 and 26.8].
//
// Every comparison is made on flags, because the server stores grants in a
// version-dependent group-compressed form (GRANT ALTER ON app.* is stored as
// one ALTER row on 25.3 and as ALTER TABLE, ALTER DATABASE and ALTER VIEW on
// 26.8).
type Catalogue struct {
	privs map[string]*catPrivilege
	// names maps the normalised form of every name to the privileges with
	// that name (at most one, compared case-insensitively), aliases the
	// normalised form of every alias to the privileges with that alias.
	// Aliases are not unique: on 25.3 ALTER TABLE MODIFY QUERY is an alias
	// of both ALTER VIEW MODIFY QUERY and ALTER VIEW MODIFY REFRESH.
	names   map[string][]string
	aliases map[string][]string
}

// normalise upper-cases a privilege name and collapses its spaces.
func normalise(s string) string { return strings.ToUpper(strings.Join(strings.Fields(s), " ")) }

// NewCatalogue builds the catalogue from the rows of system.privileges. It
// fails on a catalogue it cannot model: a duplicate name, a parent that does
// not exist, or a cycle.
func NewCatalogue(rows []ch.Privilege) (*Catalogue, error) {
	if len(rows) == 0 {
		return nil, errors.New("the privilege catalogue is empty")
	}
	c := &Catalogue{privs: map[string]*catPrivilege{}, names: map[string][]string{}, aliases: map[string][]string{}}
	for _, r := range rows {
		if _, dup := c.privs[r.Name]; dup || r.Name == "" {
			return nil, fmt.Errorf("the privilege catalogue lists %q twice or without a name", r.Name)
		}
		c.privs[r.Name] = &catPrivilege{Privilege: r}
		k := normalise(r.Name)
		c.names[k] = append(c.names[k], r.Name)
		for _, a := range r.Aliases {
			ka := normalise(a)
			if !slices.Contains(c.aliases[ka], r.Name) {
				c.aliases[ka] = append(c.aliases[ka], r.Name)
			}
		}
	}
	for _, p := range c.privs {
		if p.Parent == "" {
			continue
		}
		parent, ok := c.privs[p.Parent]
		if !ok {
			return nil, fmt.Errorf("privilege %q has the unknown parent %q", p.Name, p.Parent)
		}
		parent.children = append(parent.children, p.Name)
	}
	for _, p := range c.privs {
		flags, err := c.collectFlags(p.Name, map[string]bool{})
		if err != nil {
			return nil, err
		}
		slices.Sort(flags)
		p.flags = slices.Compact(flags)
	}
	for _, p := range c.privs {
		p.unmanaged = c.classifyUnmanaged(p)
	}
	return c, nil
}

func (c *Catalogue) collectFlags(name string, path map[string]bool) ([]string, error) {
	if path[name] {
		return nil, fmt.Errorf("the privilege catalogue has a cycle at %q", name)
	}
	p := c.privs[name]
	var out []string
	if p.Level != "" {
		out = append(out, name)
	}
	path[name] = true
	defer delete(path, name)
	for _, child := range p.children {
		f, err := c.collectFlags(child, path)
		if err != nil {
			return nil, err
		}
		out = append(out, f...)
	}
	return out, nil
}

// classifyUnmanaged decides whether a privilege is a source privilege (D-9):
// SOURCES itself, every privilege of level SOURCE (READ and WRITE from 25.8
// on), every descendant of SOURCES (25.3) and every GLOBAL leaf directly
// under ALL that is not on the allow-list.
func (c *Catalogue) classifyUnmanaged(p *catPrivilege) bool {
	switch {
	case p.Name == PrivilegeSources, p.Level == LevelSource:
		return true
	case len(p.children) == 0 && p.Level == LevelGlobal && p.Parent == PrivilegeAll && !globalLeavesUnderAll[p.Name]:
		return true
	}
	for a := p.Parent; a != ""; a = c.privs[a].Parent {
		if a == PrivilegeSources {
			return true
		}
	}
	return false
}

// Len returns the number of privileges in the catalogue.
func (c *Catalogue) Len() int { return len(c.privs) }

// Known reports whether name is a privilege of the catalogue (a canonical
// name, as system.grants.access_type holds).
func (c *Catalogue) Known(name string) bool {
	_, ok := c.privs[name]
	return ok
}

// Flags returns the privileges whose access flags a privilege grants: the
// privilege itself if it has a level, and every descendant that has one. It
// returns nil for an unknown name, and an empty list for a privilege that
// grants nothing (NONE, SHOW FILESYSTEM CACHES).
func (c *Catalogue) Flags(name string) []string {
	if p, ok := c.privs[name]; ok {
		return slices.Clone(p.flags)
	}
	return nil
}

// Unmanaged reports whether a privilege is a source privilege. Grant rejects
// them in a spec and never counts or revokes them.
func (c *Catalogue) Unmanaged(name string) bool {
	p, ok := c.privs[name]
	return ok && p.unmanaged
}

// lookup returns the canonical names that match a user-supplied name: the
// privilege of that name if one exists (compared case-insensitively, with
// spaces collapsed), otherwise every privilege with that alias.
func (c *Catalogue) lookup(name string) []string {
	k := normalise(name)
	if n := c.names[k]; len(n) > 0 {
		return n
	}
	return c.aliases[k]
}

// Canonical returns the canonical name of a privilege given by its name or
// one of its aliases, case-insensitively: "select" is SELECT, "dictget" is
// dictGet, "UPDATE" is ALTER UPDATE. An unknown or ambiguous name, a
// privilege that grants nothing (NONE, SHOW FILESYSTEM CACHES) and source
// privileges are errors.
func (c *Catalogue) Canonical(name string) (string, error) {
	m := c.lookup(name)
	switch {
	case len(m) == 0:
		return "", fmt.Errorf("unknown privilege %q (not in system.privileges of this server)", name)
	case len(m) > 1:
		s := slices.Clone(m)
		slices.Sort(s)
		return "", fmt.Errorf("privilege %q is ambiguous on this server (an alias of %s); use the privilege's name", name, strings.Join(s, ", "))
	}
	canonical := m[0]
	switch p := c.privs[canonical]; {
	case len(p.flags) == 0:
		return "", fmt.Errorf("privilege %q (%s) grants nothing on this server", name, canonical)
	case p.unmanaged:
		return "", fmt.Errorf("privilege %q (%s) is not managed by Grant: source privileges (S3, URL, REMOTE, READ, WRITE, ...) and global privileges directly under ALL that are not known to be something else are left alone", name, canonical)
	}
	return canonical, nil
}

// Expand returns the flags that the given privileges grant on a target of
// level t (design 5.6, normalisation):
//
//  1. each name is canonicalised (Canonical);
//  2. it is expanded to its flags (Flags), without source privileges;
//  3. on *.* a flag of an unsupported level (USER_NAME, NAMED_COLLECTION,
//     TABLE_ENGINE, DEFINER, SOURCE) is an error, so ALL on *.* is rejected.
//     On narrower targets such flags cannot be granted anyway;
//  4. only the flags that can be granted on the target are kept. A
//     privilege without any is an error: the server would reject it with
//     509 INVALID_GRANT ("CREATE DATABASE cannot be granted on the table
//     level").
func (c *Catalogue) Expand(privileges []string, t TargetLevel) (PrivilegeSet, error) {
	if len(privileges) == 0 {
		return nil, errors.New("no privileges")
	}
	out := PrivilegeSet{}
	for _, in := range privileges {
		canonical, err := c.Canonical(in)
		if err != nil {
			return nil, err
		}
		var unsupported []string
		n := 0
		for _, f := range c.privs[canonical].flags {
			flag := c.privs[f]
			if flag.unmanaged {
				continue
			}
			if unsupportedLevels[flag.Level] {
				unsupported = append(unsupported, f+" ("+flag.Level+")")
				continue
			}
			if applicable(flag.Level, t) {
				out[f] = struct{}{}
				n++
			}
		}
		switch {
		case len(unsupported) > 0 && (t == TargetGlobal || n == 0):
			return nil, fmt.Errorf("privilege %s includes privileges of levels that Grant does not support: %s", canonical, strings.Join(unsupported, ", "))
		case n == 0:
			return nil, fmt.Errorf("privilege %s cannot be granted on the %s level", canonical, t)
		}
	}
	return out, nil
}

// Parameterized reports whether a row of system.grants with the given
// access_type may be a grant on a parameter (parameterLevels): its access
// type is unknown, or one of its flags has a parameter level.
func (c *Catalogue) Parameterized(accessType string) bool {
	p, ok := c.privs[accessType]
	if !ok {
		return true
	}
	for _, f := range p.flags {
		if parameterLevels[c.privs[f].Level] {
			return true
		}
	}
	return false
}

// RowFlags returns the flags that a row of system.grants with the given
// access_type covers on a target of level t: its flags that can be granted
// on such a target, without source privileges and flags of unsupported
// levels. Grant never counts or revokes anything else. It returns nil for an
// access type the catalogue does not know.
func (c *Catalogue) RowFlags(accessType string, t TargetLevel) []string {
	p, ok := c.privs[accessType]
	if !ok {
		return nil
	}
	var out []string
	for _, f := range p.flags {
		flag := c.privs[f]
		if flag.unmanaged || unsupportedLevels[flag.Level] || !applicable(flag.Level, t) {
			continue
		}
		out = append(out, f)
	}
	return out
}

// checkKeyword returns an error unless name can be written into a statement
// unquoted.
func checkKeyword(name string) error {
	if !privilegeKeyword.MatchString(name) {
		return fmt.Errorf("privilege %q cannot be written into a statement", name)
	}
	return nil
}
