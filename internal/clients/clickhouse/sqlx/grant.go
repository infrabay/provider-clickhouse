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
	// hidden is set when the server may leave grant option information about
	// the point out of system.grants and SHOW GRANTS (see goMayBeHidden): a
	// grant option granted or partially revoked there, or between the point
	// and the deepest object above it that the rows show, would be missing
	// from the rows.
	hidden bool
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
	// cat expands a revoked privilege to the flags the REVOKE removes.
	cat *Catalogue
}

type expandedRow struct {
	scope
	accessType     string
	flags          PrivilegeSet
	revoke, option bool
}

// Analyze computes the analysis of the target t of a grantee with the rows
// of system.grants, for the desired flags (Catalogue.Expand). rows are all
// the grantee's rows, including those Grant ignores: they decide where the
// server shows grant options (goMayBeHidden). Without
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
	a := &Analysis{target: t, desired: desired, cat: c}
	privileges := PrivilegeSet{}
	for _, p := range t.points() {
		st := pointState{column: p.column,
			effective: PrivilegeSet{}, effectiveGO: PrivilegeSet{}, exact: PrivilegeSet{}, exactGO: PrivilegeSet{}, above: PrivilegeSet{}, aboveGO: PrivilegeSet{},
			hidden: goMayBeHidden(c, rows, p)}
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

// goMayBeHidden reports whether the server may leave grant option
// information about the point p out of system.grants and SHOW GRANTS.
//
// The server keeps a grantee's privileges in two radix trees of object
// names, one for the privileges and one for their grant options, and lists
// both together by walking the first. A node of the grant option tree that
// has no counterpart in the first tree, because the privileges there equal
// those inherited from above (for example SELECT ON app.events WITH GRANT
// OPTION under SELECT ON app.*), is listed only if no sibling in the first
// tree begins with the same character. So with a row on app.events_raw or
// app.evil, the grant option on app.events is listed nowhere, although it is
// in effect; a partial revoke of a grant option is hidden the same way. This
// is a server bug, seen on every supported version, in
// AccessRights::Node::getElementsRec.
//
// Every row the server lists comes from a node it visited, so any row at or
// below an object proves that the object's grant option information is
// listed. For each object on the path from the database down to the point,
// the check is therefore: if the rows show nothing at or below it, its grant
// option information is missing when another name at its level that the
// rows show begins with the same character. This over-approximates the
// server, which splits names at their common prefixes, and it treats every
// row of a parameterized level without an access_object (on 25.3, a grant
// on a user name prefix, which the server keeps among the database names) as
// a name that may begin with any character.
func goMayBeHidden(c *Catalogue, rows []GrantRow, p scope) bool {
	if p.level == TargetGlobal {
		return false
	}
	at := func(match func(GrantRow) bool) bool {
		for _, r := range rows {
			if match(r) {
				return true
			}
		}
		return false
	}
	sibling := func(name string, names func(GrantRow) string) bool {
		if name == "" {
			return true
		}
		for _, r := range rows {
			if n := names(r); n != "" && n != name && n[0] == name[0] {
				return true
			}
		}
		return false
	}
	// A wildcard row's object is its prefix, not the object of that name:
	// it does not count as a row at the database (or table) it names.
	dbWildcard := func(r GrantRow) bool { return r.Wildcard && r.Table == "" }
	tableWildcard := func(r GrantRow) bool { return r.Wildcard && r.Column == "" }
	// The database, among the names at the database level: databases,
	// wildcard prefixes and the parameters of parameterized grants.
	if !at(func(r GrantRow) bool { return r.Database == p.db && !dbWildcard(r) }) {
		unknown := at(func(r GrantRow) bool {
			return r.Database == "" && r.AccessObject == "" && c.Parameterized(r.AccessType)
		})
		return unknown ||
			sibling(p.db, func(r GrantRow) string { return r.Database }) ||
			sibling(p.db, func(r GrantRow) string { return r.AccessObject })
	}
	if p.level == TargetDatabase {
		return false
	}
	if !at(func(r GrantRow) bool { return r.Database == p.db && r.Table == p.table && !tableWildcard(r) }) {
		return sibling(p.table, func(r GrantRow) string {
			if r.Database != p.db {
				return ""
			}
			return r.Table
		})
	}
	if p.level == TargetTable {
		return false
	}
	if !at(func(r GrantRow) bool { return r.Database == p.db && r.Table == p.table && r.Column == p.column }) {
		return sibling(p.column, func(r GrantRow) string {
			if r.Database != p.db || r.Table != p.table {
				return ""
			}
			return r.Column
		})
	}
	return false
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

// Hidden reports whether the server may leave grant option information
// about some point of the target out of system.grants (goMayBeHidden).
func (a *Analysis) Hidden() bool {
	for _, st := range a.points {
		if st.hidden {
			return true
		}
	}
	return false
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
// flags. The order is REVOKE, REVOKE GRANT OPTION FOR, GRANT: a REVOKE also
// removes the flags of the privilege's children, so the GRANT, which repeats
// every desired flag a REVOKE removes, comes last.
type Plan struct {
	// Grant are the flags granted on the whole target, WITH GRANT OPTION
	// when WithGrantOption is set.
	Grant           []string
	WithGrantOption bool
	// Revoke are the flags revoked; RevokeGrantOption the flags whose
	// grant option is revoked.
	Revoke            []Revocation
	RevokeGrantOption []Revocation
	// Record is the Grant's record of the grant option it gave at the
	// target once every statement ran (see Analysis.Apply). Revoke leaves it
	// empty: a deleted Grant gave nothing.
	Record PrivilegeSet

	// granted are the flags that only the GRANT puts into Record; revoked
	// and revokedGO are the flags of the record before the plan that the
	// REVOKE and the REVOKE GRANT OPTION FOR take out of it (RecordAfter).
	granted, revoked, revokedGO PrivilegeSet
}

// A StepKind is the kind of a statement of a Plan.
type StepKind int

// Statement kinds, in the order in which a plan runs them.
const (
	// StepRevoke is REVOKE <flags> ON <target> FROM <grantee>.
	StepRevoke StepKind = iota
	// StepRevokeGrantOption is REVOKE GRANT OPTION FOR <flags> ON <target>
	// FROM <grantee>.
	StepRevokeGrantOption
	// StepGrant is GRANT <flags> ON <target> TO <grantee>
	// [WITH GRANT OPTION].
	StepGrant
)

// A Step is one statement of a Plan.
type Step struct {
	Kind StepKind
	SQL  string
}

// Empty reports whether the plan has no statements.
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
	if !slices.Contains(r[l], column) {
		r[l] = append(r[l], column)
	}
}

// has reports whether l is revoked from column (or from the whole target).
func (r revocations) has(l, column string) bool {
	cols, ok := r[l]
	return ok && (column == "" || cols == nil || slices.Contains(cols, column))
}

// drop removes the revocation of l from column (from the whole target when
// column is "").
func (r revocations) drop(l, column string) {
	cols, ok := r[l]
	if !ok {
		return
	}
	if column == "" || cols == nil {
		delete(r, l)
		return
	}
	cols = slices.DeleteFunc(slices.Clone(cols), func(c string) bool { return c == column })
	if len(cols) == 0 {
		delete(r, l)
		return
	}
	r[l] = cols
}

func (r revocations) sorted() []Revocation {
	out := make([]Revocation, 0, len(r))
	for l, cols := range r {
		out = append(out, Revocation{Privilege: l, Columns: cols})
	}
	slices.SortFunc(out, func(a, b Revocation) int { return strings.Compare(a.Privilege, b.Privilege) })
	return out
}

// removes returns the flags that revoking the privileges r removes at the
// point of the target with the given column ("" for every point): each
// privilege's own flag and those of its children (REVOKE CREATE ARBITRARY
// TEMPORARY TABLE also removes CREATE TEMPORARY TABLE).
func (a *Analysis) removes(r revocations, column string) PrivilegeSet {
	out := PrivilegeSet{}
	for l := range r {
		if column != "" && !r.has(l, column) {
			continue
		}
		out[l] = struct{}{}
		for _, f := range a.cat.RowFlags(l, a.target.Level()) {
			out[f] = struct{}{}
		}
	}
	return out
}

// dedupe drops from revokeGO what a REVOKE in revoke removes at the same
// point anyway: a REVOKE removes a flag together with its grant option.
func (a *Analysis) dedupe(revoke, revokeGO revocations) {
	for _, st := range a.points {
		for l := range a.removes(revoke, st.column) {
			revokeGO.drop(l, st.column)
		}
	}
}

// canRevoke reports whether REVOKE l at the point creates no partial revoke:
// l is granted at the point and no broader row grants it (the caller
// checks), and no broader row grants one of the flags of its children
// either, unless the plan grants that flag again (regrant), which removes
// the partial revoke the REVOKE made.
func (a *Analysis) canRevoke(st pointState, l string, regrant func(string) bool) bool {
	for _, f := range a.cat.RowFlags(l, a.target.Level()) {
		if f != l && st.above.Has(f) && !regrant(f) {
			return false
		}
	}
	return true
}

// canRevokeGO reports whether REVOKE GRANT OPTION FOR l at the point creates
// no partial revoke of a grant option: no broader row grants l with grant
// option, nor one of the flags of its children, unless the plan grants that
// flag again WITH GRANT OPTION (regrant).
func (a *Analysis) canRevokeGO(st pointState, l string, regrant func(string) bool) bool {
	if st.aboveGO.Has(l) {
		return false
	}
	for _, f := range a.cat.RowFlags(l, a.target.Level()) {
		if f != l && st.aboveGO.Has(f) && !regrant(f) {
			return false
		}
	}
	return true
}

// Apply returns the plan of Create and Update (design 5.6). A Grant owns its
// exact target for its grantee:
//
//  1. REVOKE the other flags granted at exactly the target that no broader
//     row grants;
//  2. REVOKE GRANT OPTION FOR the other flags that a broader row grants
//     without grant option while the row at the target has it, which
//     removes the narrow row cleanly;
//  3. without withGrantOption, REVOKE GRANT OPTION FOR the desired flags
//     that hold grant option at the target, unless a broader row holds it;
//  4. REVOKE GRANT OPTION FOR the flags of record whose grant option is no
//     longer wanted, where the server may not show it (see below);
//  5. GRANT the desired flags that are not effective, those a REVOKE above
//     removed (a privilege's children lose their flags with it), and with
//     withGrantOption those without an effective grant option, WITH GRANT
//     OPTION.
//
// The provider never creates a partial revoke: it revokes only what a row at
// exactly the target grants and no broader row covers, so a flag shadowed by
// a broader grant is left alone.
//
// record is the Grant's record of the flags it granted WITH GRANT OPTION at
// the target and has not revoked since. The server leaves some grant options
// out of system.grants (goMayBeHidden); where it may, a flag of record counts
// as holding grant option, so that withGrantOption converges instead of
// granting again on every reconcile, and a flag of record whose grant option
// is no longer wanted has it revoked although no row shows it (step 4, which
// creates no partial revoke where no broader row with grant option is
// shown). Plan.Record is the record once the statements ran (see
// Analysis.record); the Grant saves it with them, or, when the plan has no
// statement, on its own. The Grant is up to date exactly when the plan is
// empty.
func (a *Analysis) Apply(withGrantOption bool, record PrivilegeSet) Plan {
	p := Plan{WithGrantOption: withGrantOption}
	wantGO := func(l string) bool { return withGrantOption && a.desired.Has(l) }
	revoke, revokeGO := revocations{}, revocations{}
	for _, st := range a.points {
		for _, l := range st.exact.Sorted() {
			switch {
			case a.desired.Has(l):
				if !withGrantOption && st.exactGO.Has(l) && a.canRevokeGO(st, l, wantGO) {
					revokeGO.add(l, st.column)
				}
			case !st.above.Has(l):
				if a.canRevoke(st, l, a.desired.Has) {
					revoke.add(l, st.column)
				}
			case st.exactGO.Has(l) && a.canRevokeGO(st, l, wantGO):
				revokeGO.add(l, st.column)
			}
		}
		for _, l := range record.Sorted() {
			if !wantGO(l) && st.hidden && st.effective.Has(l) && st.above.Has(l) &&
				!revoke.has(l, st.column) && !revokeGO.has(l, st.column) && a.canRevokeGO(st, l, wantGO) {
				revokeGO.add(l, st.column)
			}
		}
	}
	a.dedupe(revoke, revokeGO)
	removed, removedGO := a.removes(revoke, ""), a.removes(revokeGO, "")
	for _, l := range a.desired.Sorted() {
		missing := !a.all(l, func(st pointState) PrivilegeSet { return st.effective })
		missingGO := false
		if withGrantOption {
			for _, st := range a.points {
				// Where the server may hide the grant option, a recorded flag
				// counts as holding it.
				recorded := st.hidden && record.Has(l)
				if !st.effectiveGO.Has(l) && !recorded {
					missingGO = true
				}
			}
		}
		if missing || missingGO || removed.Has(l) || (withGrantOption && removedGO.Has(l)) {
			p.Grant = append(p.Grant, l)
		}
	}
	p.Revoke, p.RevokeGrantOption = revoke.sorted(), revokeGO.sorted()
	if len(p.Grant) == 0 {
		p.WithGrantOption = false
	}
	a.record(&p, withGrantOption, record, removed, removedGO)
	return p
}

// record sets the Grant's record of the grant option that the plan p
// leaves at the target (Plan.Record), and what a statement that the server
// refuses changes in it (Plan.RecordAfter). old is the record before the
// plan; removed and removedGO are the flags and grant options that the
// plan's REVOKE and REVOKE GRANT OPTION FOR remove.
//
// With withGrantOption a desired flag is recorded when the plan grants it
// WITH GRANT OPTION, when a row shows its grant option at the target, or
// when it was recorded and the server may hide its grant option at the
// target. A flag of record is forgotten where a broader row shows its grant
// option on the whole target: the grant option at the target adds nothing
// there, and whatever revokes the broader one revokes it too (a REVOKE
// removes its flags from every object below its own), so once the broader
// grant option is gone, the Grant grants its own again instead of trusting
// a stale record. A flag that the plan's REVOKE removes is recorded only if
// the GRANT gives it again. Without withGrantOption nothing is recorded:
// the plan revokes every grant option of record that it can revoke without
// cutting a partial revoke into a broader one, and the others are covered
// by a broader grant option.
func (a *Analysis) record(p *Plan, withGrantOption bool, old, removed, removedGO PrivilegeSet) {
	p.Record, p.granted = PrivilegeSet{}, PrivilegeSet{}
	if withGrantOption {
		for l := range a.desired {
			granted := p.WithGrantOption && slices.Contains(p.Grant, l)
			shown := false
			for _, st := range a.points {
				shown = shown || st.exactGO.Has(l)
			}
			covered := a.all(l, func(st pointState) PrivilegeSet { return st.aboveGO })
			kept := old.Has(l) && a.Hidden() && !covered
			switch {
			case (shown || kept) && !removed.Has(l) && !removedGO.Has(l):
				p.Record[l] = struct{}{}
			case granted:
				p.Record[l] = struct{}{}
				p.granted[l] = struct{}{}
			}
		}
	}
	p.revoked, p.revokedGO = PrivilegeSet{}, PrivilegeSet{}
	for l := range old {
		if removed.Has(l) {
			p.revoked[l] = struct{}{}
		}
		if removedGO.Has(l) {
			p.revokedGO[l] = struct{}{}
		}
	}
}

// RecordAfter returns the record once the plan's statements ran, where
// refused reports the kinds of statement that the server refused for a
// missing privilege (497). The server checks every privilege of a GRANT or
// REVOKE before it changes any [verified on every supported version], so a
// refused statement changed nothing: what only the GRANT would have recorded
// stays out of the record, and the flags of record that a REVOKE would have
// taken out stay in it. With nothing refused it is Record.
func (p Plan) RecordAfter(refused func(StepKind) bool) PrivilegeSet {
	out := PrivilegeSet{}
	for l := range p.Record {
		if !refused(StepGrant) || !p.granted.Has(l) {
			out[l] = struct{}{}
		}
	}
	if refused(StepRevoke) {
		for l := range p.revoked {
			out[l] = struct{}{}
		}
	}
	if refused(StepRevokeGrantOption) {
		for l := range p.revokedGO {
			out[l] = struct{}{}
		}
	}
	return out
}

// Revoke returns the plan of Delete: for every desired flag granted at
// exactly the target, REVOKE it if no broader row grants it, or REVOKE GRANT
// OPTION FOR it if a broader row grants it without grant option while the
// row at the target has it (which removes the narrow row). Where the server
// may not show the grant option (goMayBeHidden), REVOKE GRANT OPTION FOR
// every flag of record, the Grant's record of the grant option it gave at
// the target (see Apply), unless a broader row with grant option is shown.
// Privileges that someone else granted at the target, and desired flags that
// are only covered by a broader grant, are left alone. The Grant exists
// while it is being deleted exactly when this plan is not empty.
func (a *Analysis) Revoke(record PrivilegeSet) Plan {
	never := func(string) bool { return false }
	revoke, revokeGO := revocations{}, revocations{}
	for _, st := range a.points {
		for _, l := range st.exact.Sorted() {
			switch {
			case !a.desired.Has(l):
			case !st.above.Has(l):
				if a.canRevoke(st, l, never) {
					revoke.add(l, st.column)
				}
			case st.exactGO.Has(l) && a.canRevokeGO(st, l, never):
				revokeGO.add(l, st.column)
			}
		}
		for _, l := range record.Sorted() {
			if st.hidden && st.effective.Has(l) && st.above.Has(l) &&
				!revoke.has(l, st.column) && !revokeGO.has(l, st.column) && a.canRevokeGO(st, l, never) {
				revokeGO.add(l, st.column)
			}
		}
	}
	a.dedupe(revoke, revokeGO)
	return Plan{Revoke: revoke.sorted(), RevokeGrantOption: revokeGO.sorted()}
}

// Steps renders the plan for the target and grantee, in the order REVOKE,
// REVOKE GRANT OPTION FOR, GRANT.
func (p Plan) Steps(t GrantTarget, grantee string) ([]Step, error) {
	var out []Step
	for _, r := range []struct {
		kind StepKind
		revs []Revocation
	}{{StepRevoke, p.Revoke}, {StepRevokeGrantOption, p.RevokeGrantOption}} {
		if len(r.revs) == 0 {
			continue
		}
		s, err := RevokeStmt(r.revs, t, grantee, r.kind == StepRevokeGrantOption)
		if err != nil {
			return nil, err
		}
		out = append(out, Step{Kind: r.kind, SQL: s})
	}
	if len(p.Grant) > 0 {
		s, err := GrantStmt(p.Grant, t, grantee, p.WithGrantOption)
		if err != nil {
			return nil, err
		}
		out = append(out, Step{Kind: StepGrant, SQL: s})
	}
	return out, nil
}

// Statements returns the statements of Steps.
func (p Plan) Statements(t GrantTarget, grantee string) ([]string, error) {
	steps, err := p.Steps(t, grantee)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, s := range steps {
		out = append(out, s.SQL)
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
