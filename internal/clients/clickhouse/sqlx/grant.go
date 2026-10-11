// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package sqlx

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	ch "github.com/infrabay/provider-clickhouse/internal/clients/clickhouse"
)

// GlobalDatabase is the database of a global grant target (ON *.*), and
// AllTables the table of a database-wide one (ON db.*).
const (
	GlobalDatabase = "*"
	AllTables      = "*"
)

// A GrantTarget is the exact target of a Grant: *.*, db.*, db.t or
// db.t(columns). Names are always literal: a '*' inside a name is quoted and
// therefore not a wildcard.
type GrantTarget struct {
	// Database is the database, or GlobalDatabase.
	Database string
	// Table is the table, or "" for every table of the database.
	Table string
	// Columns are the columns of a column target, sorted and unique.
	Columns []string
}

// NewGrantTarget returns the target of a Grant from its spec: database "*"
// is ON *.*; a nil, empty or "*" table means every table.
func NewGrantTarget(database string, table *string, columns []string) (GrantTarget, error) {
	if database == "" {
		return GrantTarget{}, errors.New("database is empty")
	}
	t := GrantTarget{Database: database}
	if table != nil && *table != "" && *table != AllTables {
		if database == GlobalDatabase {
			return GrantTarget{}, errors.New("a specific table requires a specific database")
		}
		t.Table = *table
	}
	if len(columns) > 0 {
		if t.Table == "" {
			return GrantTarget{}, errors.New("columns require a specific table")
		}
		cols := slices.Clone(columns)
		slices.Sort(cols)
		cols = slices.Compact(cols)
		if cols[0] == "" {
			return GrantTarget{}, errors.New("a column name is empty")
		}
		t.Columns = cols
	}
	return t, nil
}

// Level returns the level of the target.
func (t GrantTarget) Level() TargetLevel {
	switch {
	case t.Database == GlobalDatabase:
		return TargetGlobal
	case t.Table == "":
		return TargetDatabase
	case len(t.Columns) == 0:
		return TargetTable
	}
	return TargetColumn
}

// On renders the target's object in a statement: *.*, `db`.* or `db`.`t`.
// The columns of a column target follow each privilege instead.
func (t GrantTarget) On() string {
	switch t.Level() {
	case TargetGlobal:
		return "*.*"
	case TargetDatabase:
		return ch.QuoteIdent(t.Database) + ".*"
	}
	return ch.QuoteIdent(t.Database) + "." + ch.QuoteIdent(t.Table)
}

// String renders the target for status.atProvider.target, e.g. "*.*",
// "app.*", "app.events(a, b)".
func (t GrantTarget) String() string {
	switch t.Level() {
	case TargetGlobal:
		return "*.*"
	case TargetDatabase:
		return t.Database + ".*"
	case TargetTable:
		return t.Database + "." + t.Table
	}
	return t.Database + "." + t.Table + "(" + strings.Join(t.Columns, ", ") + ")"
}

// GrantColumns are the optional columns of system.grants (design section 7).
type GrantColumns struct {
	// AccessObject is system.grants.access_object (25.8 and later).
	AccessObject bool
	// IsWildcard is system.grants.is_wildcard (26.8 and later).
	IsWildcard bool
}

// QueryGrants returns the read of a grantee's rows of system.grants, with the
// grantee's name in the parameter {grantee:String}: the rows of a role with
// role, of a user otherwise. system.grants is not filtered by grantee: with
// SELECT on it every row is visible, and without it the query fails with
// 497. NULL names (the broader levels) are read as "": the server rejects an
// empty name in GRANT (62), so "" never is a real name.
func QueryGrants(role bool, cols GrantColumns) string {
	var b strings.Builder
	b.WriteString("SELECT toString(access_type), ifNull(database, ''), ifNull(table, ''), ifNull(column, ''), toUInt8(is_partial_revoke), toUInt8(grant_option)")
	if cols.AccessObject {
		b.WriteString(", ifNull(access_object, '')")
	}
	if cols.IsWildcard {
		b.WriteString(", toUInt8(is_wildcard)")
	}
	b.WriteString(" FROM system.grants WHERE ")
	if role {
		b.WriteString("role_name")
	} else {
		b.WriteString("user_name")
	}
	b.WriteString(" = {grantee:String}")
	return b.String()
}

// ShowGrants returns SHOW GRANTS FOR `grantee`. Grant scans its output for
// wildcard grants on servers without system.grants.is_wildcard (see
// WildcardObjects).
func ShowGrants(grantee string) string { return "SHOW GRANTS FOR " + ch.QuoteIdent(grantee) }

// A GrantRow is a row of system.grants.
type GrantRow struct {
	// AccessType is the privilege, in the server's group-compressed form.
	AccessType string
	// Database, Table and Column name the row's object; "" where the server
	// has NULL: a row without a database is global, one without a table is
	// database-wide, one without a column covers the whole table.
	Database, Table, Column string
	// PartialRevoke marks a revoke row: it removes the privilege (or, with
	// GrantOption, only the grant option) from a broader grant.
	PartialRevoke bool
	GrantOption   bool
	// AccessObject is the source of READ and WRITE rows, the prefix of
	// prefix-scoped access management rows, and so on (25.8 and later).
	AccessObject string
	// Wildcard marks a wildcard row (26.8 and later): the last name of the
	// object is a prefix (ON team_*.*, ON app.ev*).
	Wildcard bool
}

// ScanGrantRow scans a row of QueryGrants.
func ScanGrantRow(r ch.Row, cols GrantColumns) (GrantRow, error) {
	var g GrantRow
	var revoke, option, wildcard uint8
	dest := []any{&g.AccessType, &g.Database, &g.Table, &g.Column, &revoke, &option}
	if cols.AccessObject {
		dest = append(dest, &g.AccessObject)
	}
	if cols.IsWildcard {
		dest = append(dest, &wildcard)
	}
	if err := r.Scan(dest...); err != nil {
		return GrantRow{}, err
	}
	g.PartialRevoke, g.GrantOption, g.Wildcard = revoke != 0, option != 0, wildcard != 0
	return g, nil
}

// A scope is the object of a row or of a target point: its level, names and
// whether its last name is a prefix.
type scope struct {
	level             TargetLevel
	db, table, column string
	wildcard          bool
}

func rowScope(r GrantRow) scope {
	s := scope{db: r.Database, table: r.Table, column: r.Column, wildcard: r.Wildcard}
	switch {
	case r.Database == "":
		s = scope{level: TargetGlobal}
	case r.Table == "":
		s.level, s.table, s.column = TargetDatabase, "", ""
	case r.Column == "":
		s.level, s.column = TargetTable, ""
	default:
		s.level, s.wildcard = TargetColumn, false
	}
	return s
}

// nameCovers reports whether the name a (a prefix if aPrefix) includes the
// name b (a prefix if bPrefix). A wildcard covers every name that starts
// with its prefix, including the prefix itself: GRANT ON app*.* absorbs a
// grant ON app.* [verified on 26.8].
func nameCovers(a, b string, aPrefix, bPrefix bool) bool {
	if aPrefix {
		return strings.HasPrefix(b, a)
	}
	return !bPrefix && a == b
}

// covers reports whether everything in scope b is also in scope a.
func covers(a, b scope) bool {
	switch {
	case a.level == TargetGlobal:
		return true
	case b.level < a.level:
		return false
	}
	if !nameCovers(a.db, b.db, a.wildcard && a.level == TargetDatabase, b.wildcard && b.level == TargetDatabase) {
		return false
	}
	if a.level == TargetDatabase {
		return true
	}
	if !nameCovers(a.table, b.table, a.wildcard && a.level == TargetTable, b.wildcard && b.level == TargetTable) {
		return false
	}
	if a.level == TargetTable {
		return true
	}
	return a.column == b.column
}

// strictlyAbove reports whether scope a is broader than scope b.
func strictlyAbove(a, b scope) bool { return covers(a, b) && !covers(b, a) }

// points returns the scopes a target consists of: the target itself, or one
// per column of a column target. Privileges at a column target are decided
// per column.
func (t GrantTarget) points() []scope {
	switch t.Level() {
	case TargetGlobal:
		return []scope{{level: TargetGlobal}}
	case TargetDatabase:
		return []scope{{level: TargetDatabase, db: t.Database}}
	case TargetTable:
		return []scope{{level: TargetTable, db: t.Database, table: t.Table}}
	}
	out := make([]scope, 0, len(t.Columns))
	for _, c := range t.Columns {
		out = append(out, scope{level: TargetColumn, db: t.Database, table: t.Table, column: c})
	}
	return out
}

// pointState is what the rows say about one point of the target, in flags.
type pointState struct {
	column string
	// effective: granted by a covering grant row and not revoked on the
	// path from that row down to the point (revoke rows below the point do
	// not count). effectiveGO: the same with grant option, where revoke rows
	// of either kind count.
	effective, effectiveGO PrivilegeSet
	// exact: granted by a non-wildcard grant row at exactly the point;
	// exactGO by one with grant option.
	exact, exactGO PrivilegeSet
	// above and aboveGO are effective and effectiveGO from rows strictly
	// above the point only.
	above, aboveGO PrivilegeSet
}

// An Analysis is what system.grants says about one target of one grantee
// (design 5.6), with every row expanded to the flags it covers. Rows that
// Grant never manages are ignored: rows with an access_object (sources,
// prefix-scoped access management), and privileges that are source
// privileges, of an unsupported level, or unknown to the catalogue.
type Analysis struct {
	target  GrantTarget
	desired PrivilegeSet
	points  []pointState
	// privileges are the access types of the exact rows, for status.
	privileges []string
}

type expandedRow struct {
	scope
	accessType     string
	flags          PrivilegeSet
	revoke, option bool
}

// Analyze computes the analysis of the target t of a grantee with the rows
// of system.grants, for the desired flags (Catalogue.Expand). Without
// system.grants.is_wildcard, wildcard grants look like literal ones; Grant
// refuses such grantees before it analyses their rows (WildcardObjects).
func Analyze(c *Catalogue, t GrantTarget, desired PrivilegeSet, rows []GrantRow) *Analysis {
	var ers []expandedRow
	for _, r := range rows {
		if r.AccessObject != "" {
			continue
		}
		s := rowScope(r)
		flags := c.RowFlags(r.AccessType, s.level)
		if len(flags) == 0 {
			continue
		}
		ers = append(ers, expandedRow{scope: s, accessType: r.AccessType, flags: NewPrivilegeSet(flags...), revoke: r.PartialRevoke, option: r.GrantOption})
	}
	a := &Analysis{target: t, desired: desired}
	privileges := PrivilegeSet{}
	for _, p := range t.points() {
		st := pointState{column: p.column,
			effective: PrivilegeSet{}, effectiveGO: PrivilegeSet{}, exact: PrivilegeSet{}, exactGO: PrivilegeSet{}, above: PrivilegeSet{}, aboveGO: PrivilegeSet{}}
		var covering []expandedRow
		for _, r := range ers {
			if covers(r.scope, p) {
				covering = append(covering, r)
			}
		}
		for _, g := range covering {
			if g.revoke {
				continue
			}
			exact := !g.wildcard && g.scope == p
			if exact {
				privileges[g.accessType] = struct{}{}
			}
			// Revoke rows on the path from g down to (and including) the
			// point.
			blocked, blockedGO := PrivilegeSet{}, PrivilegeSet{}
			for _, v := range covering {
				if !v.revoke || !strictlyAbove(g.scope, v.scope) {
					continue
				}
				for l := range v.flags {
					blockedGO[l] = struct{}{}
					if !v.option {
						blocked[l] = struct{}{}
					}
				}
			}
			for l := range g.flags {
				if !blocked.Has(l) {
					st.effective[l] = struct{}{}
					if !exact {
						st.above[l] = struct{}{}
					}
				}
				if g.option && !blockedGO.Has(l) {
					st.effectiveGO[l] = struct{}{}
					if !exact {
						st.aboveGO[l] = struct{}{}
					}
				}
				if exact {
					st.exact[l] = struct{}{}
					if g.option {
						st.exactGO[l] = struct{}{}
					}
				}
			}
		}
		a.points = append(a.points, st)
	}
	a.privileges = privileges.Sorted()
	return a
}

// Privileges returns the access types of the rows at exactly the target,
// sorted, in the server's group-compressed form (status.atProvider).
func (a *Analysis) Privileges() []string { return a.privileges }

// Missing returns the desired flags that are not effective on the whole
// target, sorted.
func (a *Analysis) Missing() []string {
	var out []string
	for _, l := range a.desired.Sorted() {
		if !a.all(l, func(st pointState) PrivilegeSet { return st.effective }) {
			out = append(out, l)
		}
	}
	return out
}

// Effective reports whether every desired flag is effective on the whole
// target: the grantee holds what the Grant asks for (design 5.10, Ready).
func (a *Analysis) Effective() bool { return len(a.Missing()) == 0 }

// Exists reports whether the Grant exists (design 5.6): some managed
// privilege is granted at exactly the target, or every desired flag is
// effective there, for example through a broader grant.
func (a *Analysis) Exists() bool {
	for _, st := range a.points {
		if len(st.exact) > 0 {
			return true
		}
	}
	return a.Effective()
}

func (a *Analysis) all(l string, set func(pointState) PrivilegeSet) bool {
	for _, st := range a.points {
		if !set(st).Has(l) {
			return false
		}
	}
	return true
}

// A Revocation is a privilege to revoke, with the columns to revoke it from
// on a column target (nil otherwise).
type Revocation struct {
	Privilege string
	Columns   []string
}

// A Plan is the statements that bring a target to its desired state, as
// flags. The order is GRANT, REVOKE, REVOKE GRANT OPTION FOR.
type Plan struct {
	// Grant are the flags granted on the whole target, WITH GRANT OPTION
	// when WithGrantOption is set.
	Grant           []string
	WithGrantOption bool
	// Revoke are the flags revoked; RevokeGrantOption the flags whose
	// grant option is revoked.
	Revoke            []Revocation
	RevokeGrantOption []Revocation
}

// Empty reports whether the plan has nothing to do.
func (p Plan) Empty() bool {
	return len(p.Grant) == 0 && len(p.Revoke) == 0 && len(p.RevokeGrantOption) == 0
}

// revocations collects per-flag columns into sorted revocations.
type revocations map[string][]string

func (r revocations) add(l, column string) {
	if column == "" {
		r[l] = nil
		return
	}
	r[l] = append(r[l], column)
}

func (r revocations) sorted() []Revocation {
	out := make([]Revocation, 0, len(r))
	for l, cols := range r {
		out = append(out, Revocation{Privilege: l, Columns: cols})
	}
	slices.SortFunc(out, func(a, b Revocation) int { return strings.Compare(a.Privilege, b.Privilege) })
	return out
}

// Apply returns the plan of Create and Update (design 5.6). A Grant owns its
// exact target for its grantee:
//
//  1. GRANT the desired flags that are not effective, and with
//     withGrantOption those without an effective grant option, WITH GRANT
//     OPTION;
//  2. REVOKE the other flags granted at exactly the target that no broader
//     row grants;
//  3. REVOKE GRANT OPTION FOR the other flags that a broader row grants
//     without grant option while the row at the target has it, which
//     removes the narrow row cleanly;
//  4. without withGrantOption, REVOKE GRANT OPTION FOR the desired flags
//     that hold grant option at the target, unless a broader row holds it.
//
// The provider never creates a partial revoke: it revokes only what a row at
// exactly the target grants and no broader row covers, so a flag shadowed by
// a broader grant is left alone. The plan is empty exactly when the Grant is
// up to date.
func (a *Analysis) Apply(withGrantOption bool) Plan {
	p := Plan{WithGrantOption: withGrantOption}
	for _, l := range a.desired.Sorted() {
		missing := !a.all(l, func(st pointState) PrivilegeSet { return st.effective })
		missingGO := withGrantOption && !a.all(l, func(st pointState) PrivilegeSet { return st.effectiveGO })
		if missing || missingGO {
			p.Grant = append(p.Grant, l)
		}
	}
	revoke, revokeGO := revocations{}, revocations{}
	for _, st := range a.points {
		for _, l := range st.exact.Sorted() {
			switch {
			case a.desired.Has(l):
				if !withGrantOption && st.exactGO.Has(l) && !st.aboveGO.Has(l) {
					revokeGO.add(l, st.column)
				}
			case !st.above.Has(l):
				revoke.add(l, st.column)
			case st.exactGO.Has(l) && !st.aboveGO.Has(l):
				revokeGO.add(l, st.column)
			}
		}
	}
	p.Revoke, p.RevokeGrantOption = revoke.sorted(), revokeGO.sorted()
	if len(p.Grant) == 0 {
		p.WithGrantOption = false
	}
	return p
}

// Revoke returns the plan of Delete: for every desired flag granted at
// exactly the target, REVOKE it if no broader row grants it, or REVOKE GRANT
// OPTION FOR it if a broader row grants it without grant option while the
// row at the target has it (which removes the narrow row). Privileges that
// someone else granted at the target, and desired flags that are only
// covered by a broader grant, are left alone. The Grant exists while it is
// being deleted exactly when this plan is not empty.
func (a *Analysis) Revoke() Plan {
	revoke, revokeGO := revocations{}, revocations{}
	for _, st := range a.points {
		for _, l := range st.exact.Sorted() {
			switch {
			case !a.desired.Has(l):
			case !st.above.Has(l):
				revoke.add(l, st.column)
			case st.exactGO.Has(l) && !st.aboveGO.Has(l):
				revokeGO.add(l, st.column)
			}
		}
	}
	return Plan{Revoke: revoke.sorted(), RevokeGrantOption: revokeGO.sorted()}
}

// Statements renders the plan for the target and grantee.
func (p Plan) Statements(t GrantTarget, grantee string) ([]string, error) {
	var out []string
	if len(p.Grant) > 0 {
		s, err := GrantStmt(p.Grant, t, grantee, p.WithGrantOption)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	for _, r := range []struct {
		revs     []Revocation
		goOption bool
	}{{p.Revoke, false}, {p.RevokeGrantOption, true}} {
		if len(r.revs) == 0 {
			continue
		}
		s, err := RevokeStmt(r.revs, t, grantee, r.goOption)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

func privilegeList(privs []Revocation, column bool) (string, error) {
	parts := make([]string, 0, len(privs))
	for _, p := range privs {
		if err := checkKeyword(p.Privilege); err != nil {
			return "", err
		}
		if column != (len(p.Columns) > 0) {
			return "", fmt.Errorf("privilege %s: columns are required exactly on a column target", p.Privilege)
		}
		if !column {
			parts = append(parts, p.Privilege)
			continue
		}
		cols := make([]string, 0, len(p.Columns))
		for _, c := range p.Columns {
			cols = append(cols, ch.QuoteIdent(c))
		}
		parts = append(parts, p.Privilege+"("+strings.Join(cols, ", ")+")")
	}
	return strings.Join(parts, ", "), nil
}

// GrantStmt returns
//
//	GRANT <privileges> ON <target> TO `grantee` [WITH GRANT OPTION]
//
// On a column target every privilege carries the target's columns:
// GRANT SELECT(`a`, `b`), INSERT(`a`, `b`) ON `db`.`t` TO `g`. Privileges are
// canonical names from the catalogue and are written unquoted.
func GrantStmt(privileges []string, t GrantTarget, grantee string, withGrantOption bool) (string, error) {
	if len(privileges) == 0 {
		return "", errors.New("no privileges to grant")
	}
	revs := make([]Revocation, 0, len(privileges))
	for _, p := range privileges {
		revs = append(revs, Revocation{Privilege: p, Columns: t.Columns})
	}
	list, err := privilegeList(revs, t.Level() == TargetColumn)
	if err != nil {
		return "", err
	}
	s := "GRANT " + list + " ON " + t.On() + " TO " + ch.QuoteIdent(grantee)
	if withGrantOption {
		s += " WITH GRANT OPTION"
	}
	return s, nil
}

// RevokeStmt returns
//
//	REVOKE [GRANT OPTION FOR] <privileges> ON <target> FROM `grantee`
//
// On a column target each privilege carries its own columns:
// REVOKE SELECT(`a`), INSERT(`b`) ON `db`.`t` FROM `g`. Revoking a privilege
// that is not held is a no-op.
func RevokeStmt(privileges []Revocation, t GrantTarget, grantee string, grantOptionOnly bool) (string, error) {
	if len(privileges) == 0 {
		return "", errors.New("no privileges to revoke")
	}
	list, err := privilegeList(privileges, t.Level() == TargetColumn)
	if err != nil {
		return "", err
	}
	s := "REVOKE "
	if grantOptionOnly {
		s += "GRANT OPTION FOR "
	}
	return s + list + " ON " + t.On() + " FROM " + ch.QuoteIdent(grantee), nil
}
