// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package sqlx

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

func TestNewGrantTarget(t *testing.T) {
	cases := map[string]struct {
		db      string
		table   *string
		columns []string
		level   TargetLevel
		on      string
		str     string
		err     bool
	}{
		"Global":        {db: "*", level: TargetGlobal, on: "*.*", str: "*.*"},
		"GlobalStar":    {db: "*", table: ptr("*"), level: TargetGlobal, on: "*.*", str: "*.*"},
		"Database":      {db: "app", level: TargetDatabase, on: "`app`.*", str: "app.*"},
		"DatabaseStar":  {db: "app", table: ptr("*"), level: TargetDatabase, on: "`app`.*", str: "app.*"},
		"Table":         {db: "app", table: ptr("events"), level: TargetTable, on: "`app`.`events`", str: "app.events"},
		"Columns":       {db: "app", table: ptr("t"), columns: []string{"b", "a", "b"}, level: TargetColumn, on: "`app`.`t`", str: "app.t(a, b)"},
		"LiteralStar":   {db: "odd*name", table: ptr("t*"), level: TargetTable, on: "`odd*name`.`t*`", str: "odd*name.t*"},
		"Adversarial":   {db: "a`b\\c", table: ptr("t'1"), level: TargetTable, on: "`a\\`b\\\\c`.`t'1`", str: "a`b\\c.t'1"},
		"NoDatabase":    {db: "", err: true},
		"TableOnGlobal": {db: "*", table: ptr("t"), err: true},
		"ColumnsNoTbl":  {db: "app", columns: []string{"a"}, err: true},
		"EmptyColumn":   {db: "app", table: ptr("t"), columns: []string{""}, err: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := NewGrantTarget(tc.db, tc.table, tc.columns)
			if tc.err {
				if err == nil {
					t.Fatalf("NewGrantTarget() = %+v, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Level() != tc.level || got.On() != tc.on || got.String() != tc.str {
				t.Errorf("level %s, on %s, string %s", got.Level(), got.On(), got.String())
			}
		})
	}
}

func TestGrantQueries(t *testing.T) {
	cases := map[string]struct{ got, want string }{
		"User253": {QueryGrants(false, GrantColumns{}),
			"SELECT toString(access_type), ifNull(database, ''), ifNull(table, ''), ifNull(column, ''), toUInt8(is_partial_revoke), toUInt8(grant_option) FROM system.grants WHERE user_name = {grantee:String}"},
		"Role258": {QueryGrants(true, GrantColumns{AccessObject: true}),
			"SELECT toString(access_type), ifNull(database, ''), ifNull(table, ''), ifNull(column, ''), toUInt8(is_partial_revoke), toUInt8(grant_option), ifNull(access_object, '') FROM system.grants WHERE role_name = {grantee:String}"},
		"Role268": {QueryGrants(true, GrantColumns{AccessObject: true, IsWildcard: true}),
			"SELECT toString(access_type), ifNull(database, ''), ifNull(table, ''), ifNull(column, ''), toUInt8(is_partial_revoke), toUInt8(grant_option), ifNull(access_object, ''), toUInt8(is_wildcard) FROM system.grants WHERE role_name = {grantee:String}"},
		"ShowGrants": {ShowGrants("a`b"), "SHOW GRANTS FOR `a\\`b`"},
	}
	for name, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s:\ngot:  %s\nwant: %s", name, tc.got, tc.want)
		}
	}
}

// testRow is a row of scanned values, like the driver's.
type testRow []any

func (r testRow) Scan(dest ...any) error {
	if len(dest) != len(r) {
		return fmt.Errorf("%d columns, %d destinations", len(r), len(dest))
	}
	for i, d := range dest {
		dv := reflect.ValueOf(d).Elem()
		v := reflect.ValueOf(r[i])
		if !v.Type().AssignableTo(dv.Type()) {
			return fmt.Errorf("cannot scan %T into %T", r[i], d)
		}
		dv.Set(v)
	}
	return nil
}

func TestScanGrantRow(t *testing.T) {
	got, err := ScanGrantRow(testRow{"SELECT", "app", "t", "", uint8(1), uint8(0)}, GrantColumns{})
	if err != nil || got != (GrantRow{AccessType: "SELECT", Database: "app", Table: "t", PartialRevoke: true}) {
		t.Errorf("25.3 row: %+v, %v", got, err)
	}
	got, err = ScanGrantRow(testRow{"READ", "", "", "", uint8(0), uint8(1), "S3", uint8(1)}, GrantColumns{AccessObject: true, IsWildcard: true})
	if err != nil || got != (GrantRow{AccessType: "READ", GrantOption: true, AccessObject: "S3", Wildcard: true}) {
		t.Errorf("26.8 row: %+v, %v", got, err)
	}
	if _, err := ScanGrantRow(testRow{"SELECT"}, GrantColumns{}); err == nil {
		t.Error("a short row must fail")
	}
}

func TestGrantStatements(t *testing.T) {
	db := GrantTarget{Database: "app"}
	tbl := GrantTarget{Database: "a`b\\c", Table: "t'1"}
	cols := GrantTarget{Database: "app", Table: "t", Columns: []string{"a", "b`c"}}
	global := GrantTarget{Database: "*"}
	ok := func(s string, err error) string {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	cases := map[string]struct{ got, want string }{
		"GrantDatabase": {ok(GrantStmt([]string{"INSERT", "SELECT"}, db, "r", false)), "GRANT INSERT, SELECT ON `app`.* TO `r`"},
		"GrantOption":   {ok(GrantStmt([]string{"SELECT"}, global, "a@example.com", true)), "GRANT SELECT ON *.* TO `a@example.com` WITH GRANT OPTION"},
		"GrantTable":    {ok(GrantStmt([]string{"dictGet", "ALTER UPDATE"}, tbl, "u`x", false)), "GRANT dictGet, ALTER UPDATE ON `a\\`b\\\\c`.`t'1` TO `u\\`x`"},
		"GrantColumns":  {ok(GrantStmt([]string{"INSERT", "SELECT"}, cols, "r", false)), "GRANT INSERT(`a`, `b\\`c`), SELECT(`a`, `b\\`c`) ON `app`.`t` TO `r`"},
		"Revoke":        {ok(RevokeStmt([]Revocation{{Privilege: "INSERT"}}, db, "r", false)), "REVOKE INSERT ON `app`.* FROM `r`"},
		"RevokeOption":  {ok(RevokeStmt([]Revocation{{Privilege: "SELECT"}}, tbl, "r", true)), "REVOKE GRANT OPTION FOR SELECT ON `a\\`b\\\\c`.`t'1` FROM `r`"},
		"RevokeColumns": {ok(RevokeStmt([]Revocation{{Privilege: "INSERT", Columns: []string{"b`c"}}, {Privilege: "SELECT", Columns: []string{"a"}}}, cols, "r", false)),
			"REVOKE INSERT(`b\\`c`), SELECT(`a`) ON `app`.`t` FROM `r`"},
	}
	for name, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s:\ngot:  %s\nwant: %s", name, tc.got, tc.want)
		}
	}
	grantErr := func(privs []string, t GrantTarget) error { _, err := GrantStmt(privs, t, "r", false); return err }
	revokeErr := func(revs []Revocation, t GrantTarget) error { _, err := RevokeStmt(revs, t, "r", false); return err }
	for name, err := range map[string]error{
		"NoPrivileges":   grantErr(nil, db),
		"NoRevocations":  revokeErr(nil, db),
		"BadKeyword":     grantErr([]string{"SELECT; DROP"}, db),
		"TrailingSpace":  grantErr([]string{"SELECT "}, db),
		"MissingColumns": revokeErr([]Revocation{{Privilege: "SELECT"}}, cols),
		"ExtraColumns":   revokeErr([]Revocation{{Privilege: "SELECT", Columns: []string{"a"}}}, db),
	} {
		if err == nil {
			t.Errorf("%s: nil error", name)
		}
	}
	if _, err := (Plan{Grant: []string{"BAD!"}}).Statements(db, "r"); err == nil {
		t.Error("Plan.Statements must refuse a bad keyword")
	}
	if _, err := (Plan{Revoke: []Revocation{{Privilege: "BAD!"}}}).Statements(db, "r"); err == nil {
		t.Error("Plan.Statements must refuse a bad keyword in a revocation")
	}
}

// gr builds a row of system.grants. object is "*.*", "db.*", "db.t" or
// "db.t(c)"; flags may contain p (partial revoke), g (grant option) and w
// (wildcard).
func gr(access, object, flags string) GrantRow {
	r := GrantRow{AccessType: access, PartialRevoke: strings.Contains(flags, "p"), GrantOption: strings.Contains(flags, "g"), Wildcard: strings.Contains(flags, "w")}
	if object == "*.*" {
		return r
	}
	db, rest, _ := strings.Cut(object, ".")
	r.Database = db
	if rest == "*" {
		return r
	}
	if i := strings.IndexByte(rest, '('); i >= 0 {
		r.Table, r.Column = rest[:i], strings.TrimSuffix(rest[i+1:], ")")
		return r
	}
	r.Table = rest
	return r
}

// withObject sets the access_object of a row (25.8 and later).
func withObject(r GrantRow, object string) GrantRow {
	r.AccessObject = object
	return r
}

func dbRows(object string, accessTypes ...string) []GrantRow {
	out := make([]GrantRow, 0, len(accessTypes))
	for _, a := range accessTypes {
		out = append(out, gr(a, object, ""))
	}
	return out
}

// TestAnalyze runs the reconcile model on rows of system.grants captured
// from the servers of the CI matrix (the comments name the version and the
// statements that produced them). Statements are rendered for a grantee
// named g.
func TestAnalyze(t *testing.T) {
	app := GrantTarget{Database: "app"}
	appT := GrantTarget{Database: "app", Table: "t"}
	global := GrantTarget{Database: "*"}
	col := func(c ...string) GrantTarget { return GrantTarget{Database: "app", Table: "t", Columns: c} }
	db := func(name string) GrantTarget { return GrantTarget{Database: name} }
	tbl := func(d, name string) GrantTarget { return GrantTarget{Database: d, Table: name} }

	type tc struct {
		version    string
		target     GrantTarget
		privileges []string
		wgo        bool
		rows       []GrantRow
		exists     bool
		effective  bool
		apply      []string
		revoke     []string
		// check replaces the comparison of apply and revoke.
		check func(t *testing.T, c *Catalogue, apply, revoke []string)
		privs []string
	}
	cases := map[string]tc{
		"Fresh": {target: app, privileges: []string{"SELECT", "INSERT"},
			apply: []string{"GRANT INSERT, SELECT ON `app`.* TO `g`"}},
		"UpToDate": {target: app, privileges: []string{"select", "insert"},
			rows:   []GrantRow{gr("SELECT", "app.*", ""), gr("INSERT", "app.*", "")},
			exists: true, effective: true,
			revoke: []string{"REVOKE INSERT, SELECT ON `app`.* FROM `g`"}, privs: []string{"INSERT", "SELECT"}},
		// The same GRANT ALL ON app.* is stored differently by every
		// version; each is up to date for ALL.
		"AllCompressed253": {version: v253, target: app, privileges: []string{"ALL"},
			rows:   dbRows("app.*", "CHECK", "SHOW", "SELECT", "INSERT", "ALTER", "CREATE DATABASE", "CREATE TABLE", "CREATE VIEW", "CREATE DICTIONARY", "DROP DATABASE", "DROP TABLE", "DROP VIEW", "DROP DICTIONARY", "UNDROP TABLE", "TRUNCATE", "OPTIMIZE", "BACKUP", "CREATE ROW POLICY", "ALTER ROW POLICY", "DROP ROW POLICY", "SHOW ROW POLICIES", "SYSTEM MERGES", "SYSTEM TTL MERGES", "SYSTEM FETCHES", "SYSTEM MOVES", "SYSTEM PULLING REPLICATION LOG", "SYSTEM CLEANUP", "SYSTEM VIEWS", "SYSTEM SENDS", "SYSTEM REPLICATION QUEUES", "SYSTEM VIRTUAL PARTS UPDATE", "SYSTEM REDUCE BLOCKING PARTS", "SYSTEM DROP REPLICA", "SYSTEM SYNC REPLICA", "SYSTEM RESTART REPLICA", "SYSTEM RESTORE REPLICA", "SYSTEM WAIT LOADING PARTS", "SYSTEM SYNC DATABASE REPLICA", "SYSTEM FLUSH DISTRIBUTED", "SYSTEM LOAD PRIMARY KEY", "SYSTEM UNLOAD PRIMARY KEY", "dictGet"),
			exists: true, effective: true,
			check: func(t *testing.T, c *Catalogue, apply, revoke []string) {
				t.Helper()
				if len(apply) != 0 || len(revoke) != 1 || !strings.Contains(revoke[0], "ALTER UPDATE") || !strings.HasSuffix(revoke[0], " ON `app`.* FROM `g`") {
					t.Errorf("apply %q, revoke %q", apply, revoke)
				}
			}},
		"AllCompressed268": {version: v268, target: app, privileges: []string{"ALL"},
			rows:   dbRows("app.*", "CHECK", "SHOW", "SELECT", "INSERT", "ALTER TABLE", "ALTER DATABASE", "ALTER VIEW", "CREATE DATABASE", "CREATE TABLE", "CREATE VIEW", "CREATE DICTIONARY", "DROP DATABASE", "DROP TABLE", "DROP VIEW", "DROP DICTIONARY", "UNDROP TABLE", "TRUNCATE", "OPTIMIZE", "BACKUP", "CREATE ROW POLICY", "ALTER ROW POLICY", "DROP ROW POLICY", "SHOW ROW POLICIES", "SYSTEM MERGES", "SYSTEM TTL MERGES", "SYSTEM FETCHES", "SYSTEM MOVES", "SYSTEM PULLING REPLICATION LOG", "SYSTEM CLEANUP", "SYSTEM BACKGROUND", "SYSTEM SENDS", "SYSTEM REPLICATION QUEUES", "SYSTEM VIRTUAL PARTS UPDATE", "SYSTEM REDUCE BLOCKING PARTS", "SYSTEM DROP REPLICA", "SYSTEM SYNC REPLICA", "SYSTEM RESTART REPLICA", "SYSTEM RESTORE REPLICA", "SYSTEM RESTORE DATABASE REPLICA", "SYSTEM WAIT LOADING PARTS", "SYSTEM WAIT QUERY RUNNER", "SYSTEM SYNC DATABASE REPLICA", "SYSTEM FLUSH DISTRIBUTED", "SYSTEM FLUSH OBJECT STORAGE QUEUE", "SYSTEM LOAD PRIMARY KEY", "SYSTEM UNLOAD PRIMARY KEY", "dictGet"),
			exists: true, effective: true,
			check: func(t *testing.T, c *Catalogue, apply, revoke []string) {
				t.Helper()
				if len(apply) != 0 || len(revoke) != 1 {
					t.Errorf("apply %q, revoke %q", apply, revoke)
				}
			}},
		// GRANT ALTER ON app.*: one ALTER row on 25.3 and 26.6; ALTER
		// TABLE, ALTER DATABASE and ALTER VIEW on 26.8. A Grant of ALTER
		// UPDATE revokes the rest, by flag.
		"AlterSubset253": {version: v253, target: app, privileges: []string{"ALTER UPDATE"},
			rows: []GrantRow{gr("ALTER", "app.*", "")}, exists: true, effective: true,
			check:  alterSubset,
			revoke: []string{"REVOKE ALTER UPDATE ON `app`.* FROM `g`"}, privs: []string{"ALTER"}},
		"AlterSubset268": {version: v268, target: app, privileges: []string{"ALTER UPDATE"},
			rows:   []GrantRow{gr("ALTER TABLE", "app.*", ""), gr("ALTER DATABASE", "app.*", ""), gr("ALTER VIEW", "app.*", "")},
			exists: true, effective: true, check: alterSubset,
			privs: []string{"ALTER DATABASE", "ALTER TABLE", "ALTER VIEW"}},
		"AlterSubset258": {version: v258, target: app, privileges: []string{"ALTER UPDATE"},
			rows: []GrantRow{gr("ALTER", "app.*", "")}, exists: true, effective: true, check: alterSubset},
		"AlterSubsetA263": {version: va263, target: app, privileges: []string{"ALTER UPDATE"},
			rows: []GrantRow{gr("ALTER", "app.*", "")}, exists: true, effective: true, check: alterSubset},
		// GRANT ALL ON app.t [N1r, Antalya 26.3].
		"AllTableA263": {version: va263, target: appT, privileges: []string{"ALL"},
			rows: dbRows("app.t", "ALTER ROW POLICY", "ALTER TABLE", "ALTER VIEW", "BACKUP", "CHECK", "CREATE DICTIONARY", "CREATE ROW POLICY",
				"CREATE TABLE", "CREATE VIEW", "DROP DICTIONARY", "DROP ROW POLICY", "DROP TABLE", "DROP VIEW", "INSERT", "OPTIMIZE", "SELECT",
				"SHOW COLUMNS", "SHOW DICTIONARIES", "SHOW ROW POLICIES", "SHOW TABLES", "SYSTEM CLEANUP", "SYSTEM DROP REPLICA", "SYSTEM FETCHES",
				"SYSTEM FLUSH DISTRIBUTED", "SYSTEM LOAD PRIMARY KEY", "SYSTEM MERGES", "SYSTEM MOVES", "SYSTEM PULLING REPLICATION LOG",
				"SYSTEM REDUCE BLOCKING PARTS", "SYSTEM REPLICATION QUEUES", "SYSTEM RESTART REPLICA", "SYSTEM RESTORE DATABASE REPLICA",
				"SYSTEM RESTORE REPLICA", "SYSTEM SENDS", "SYSTEM SYNC REPLICA", "SYSTEM TTL MERGES", "SYSTEM UNLOAD PRIMARY KEY", "SYSTEM VIEWS",
				"SYSTEM VIRTUAL PARTS UPDATE", "SYSTEM WAIT LOADING PARTS", "TRUNCATE", "UNDROP TABLE", "dictGet"),
			exists: true, effective: true,
			check: func(t *testing.T, c *Catalogue, apply, revoke []string) {
				t.Helper()
				if len(apply) != 0 || len(revoke) != 1 {
					t.Errorf("apply %q, revoke %q", apply, revoke)
				}
			}},
		"AlterUpToDate266": {version: va266, target: app, privileges: []string{"ALTER"},
			rows: []GrantRow{gr("ALTER", "app.*", "")}, exists: true, effective: true,
			revoke: nil, check: func(t *testing.T, c *Catalogue, apply, revoke []string) {
				t.Helper()
				if len(apply) != 0 || len(revoke) != 1 || !strings.Contains(revoke[0], "ALTER DATABASE SETTINGS") {
					t.Errorf("apply %q, revoke %q", apply, revoke)
				}
			}, privs: []string{"ALTER"}},
		// GRANT SELECT(a) ON app.t, then GRANT SELECT ON app.*: only the
		// database row remains [25.3.3 G1c]. The narrow Grant stays up to
		// date and is released at once on deletion; nothing is revoked.
		"MergedIntoBroader": {target: col("a"), privileges: []string{"SELECT"},
			rows: []GrantRow{gr("SELECT", "app.*", "")}, exists: true, effective: true},
		// GRANT SELECT ON app.*, REVOKE SELECT ON app.t [all versions G3c]:
		// the grantee cannot read app.t.
		"PartialRevokeAtTarget": {target: appT, privileges: []string{"SELECT"},
			rows:  []GrantRow{gr("SELECT", "app.t", "p"), gr("SELECT", "app.*", "")},
			apply: []string{"GRANT SELECT ON `app`.`t` TO `g`"}},
		// Revoke rows below the target are another target's business.
		"RevokeBelowTarget": {target: app, privileges: []string{"SELECT"},
			rows:   []GrantRow{gr("SELECT", "app.t", "p"), gr("SELECT", "app.*", "")},
			exists: true, effective: true,
			revoke: []string{"REVOKE SELECT ON `app`.* FROM `g`"}, privs: []string{"SELECT"}},
		// GRANT SELECT ON app.t, REVOKE SELECT(a) ON app.t [C5r].
		"ColumnRevokeAtTarget": {target: col("a", "b"), privileges: []string{"SELECT"},
			rows:  []GrantRow{gr("SELECT", "app.t", ""), gr("SELECT", "app.t(a)", "p")},
			apply: []string{"GRANT SELECT(`a`, `b`) ON `app`.`t` TO `g`"}},
		"ColumnRevokeBelowTarget": {target: appT, privileges: []string{"SELECT"},
			rows:   []GrantRow{gr("SELECT", "app.t", ""), gr("SELECT", "app.t(a)", "p")},
			exists: true, effective: true,
			revoke: []string{"REVOKE SELECT ON `app`.`t` FROM `g`"}, privs: []string{"SELECT"}},
		// GRANT SELECT(a, b), INSERT(a) ON app.t [C1r]: rows per column.
		"ColumnForeign": {target: col("a", "b"), privileges: []string{"SELECT"},
			rows:   []GrantRow{gr("INSERT", "app.t(a)", ""), gr("SELECT", "app.t(a)", ""), gr("SELECT", "app.t(b)", "")},
			exists: true, effective: true,
			apply:  []string{"REVOKE INSERT(`a`) ON `app`.`t` FROM `g`"},
			revoke: []string{"REVOKE SELECT(`a`, `b`) ON `app`.`t` FROM `g`"}, privs: []string{"INSERT", "SELECT"}},
		"ColumnMissing": {target: col("b"), privileges: []string{"SELECT", "INSERT"},
			rows:   []GrantRow{gr("INSERT", "app.t(a)", ""), gr("SELECT", "app.t(a)", ""), gr("SELECT", "app.t(b)", "")},
			exists: true,
			apply:  []string{"GRANT INSERT(`b`) ON `app`.`t` TO `g`"},
			revoke: []string{"REVOKE SELECT(`b`) ON `app`.`t` FROM `g`"}, privs: []string{"SELECT"}},
		// Grant option (A17). A plain GRANT keeps an existing option [J2r].
		"OptionKept": {target: app, privileges: []string{"SELECT"},
			rows: []GrantRow{gr("SELECT", "app.*", "g")}, exists: true, effective: true,
			apply:  []string{"REVOKE GRANT OPTION FOR SELECT ON `app`.* FROM `g`"},
			revoke: []string{"REVOKE SELECT ON `app`.* FROM `g`"}, privs: []string{"SELECT"}},
		"OptionWanted": {target: app, privileges: []string{"SELECT"}, wgo: true,
			rows: []GrantRow{gr("SELECT", "app.*", "g")}, exists: true, effective: true,
			revoke: []string{"REVOKE SELECT ON `app`.* FROM `g`"}, privs: []string{"SELECT"}},
		// Re-grant with option under a broader row without it: a separate
		// narrow row [H2r]; REVOKE GRANT OPTION FOR removes it cleanly [H3r].
		"NarrowOptionKept": {target: appT, privileges: []string{"SELECT"}, wgo: true,
			rows: []GrantRow{gr("SELECT", "app.*", ""), gr("SELECT", "app.t", "g")}, exists: true, effective: true,
			revoke: []string{"REVOKE GRANT OPTION FOR SELECT ON `app`.`t` FROM `g`"}, privs: []string{"SELECT"}},
		"NarrowOptionDropped": {target: appT, privileges: []string{"SELECT"},
			rows: []GrantRow{gr("SELECT", "app.*", ""), gr("SELECT", "app.t", "g")}, exists: true, effective: true,
			apply:  []string{"REVOKE GRANT OPTION FOR SELECT ON `app`.`t` FROM `g`"},
			revoke: []string{"REVOKE GRANT OPTION FOR SELECT ON `app`.`t` FROM `g`"}, privs: []string{"SELECT"}},
		"OptionUnderBroaderRow": {target: appT, privileges: []string{"SELECT"}, wgo: true,
			rows: []GrantRow{gr("SELECT", "app.*", "")}, exists: true, effective: true,
			apply: []string{"GRANT SELECT ON `app`.`t` TO `g` WITH GRANT OPTION"}},
		// No partial revoke of an option that a broader row holds.
		"BroaderOptionLeftAlone": {target: appT, privileges: []string{"SELECT"},
			rows: []GrantRow{gr("SELECT", "app.*", "g")}, exists: true, effective: true},
		// GRANT SELECT ON app.* WITH GRANT OPTION, REVOKE GRANT OPTION FOR
		// SELECT ON app.t [G2r, all versions]. A plain GRANT keeps that
		// partial revoke [G3r]; GRANT ... WITH GRANT OPTION removes it [G4r].
		"OptionPartiallyRevoked": {target: appT, privileges: []string{"SELECT"},
			rows: []GrantRow{gr("SELECT", "app.*", "g"), gr("SELECT", "app.t", "pg")}, exists: true, effective: true},
		"OptionPartiallyRevokedWanted": {target: appT, privileges: []string{"SELECT"}, wgo: true,
			rows: []GrantRow{gr("SELECT", "app.*", "g"), gr("SELECT", "app.t", "pg")}, exists: true, effective: true,
			apply: []string{"GRANT SELECT ON `app`.`t` TO `g` WITH GRANT OPTION"}},
		"OptionPartiallyRevokedWanted258": {version: v258, target: appT, privileges: []string{"SELECT"}, wgo: true,
			rows: []GrantRow{gr("SELECT", "app.*", "g"), gr("SELECT", "app.t", "pg")}, exists: true, effective: true,
			apply: []string{"GRANT SELECT ON `app`.`t` TO `g` WITH GRANT OPTION"}},
		"OptionPartiallyRevokedA263": {version: va263, target: appT, privileges: []string{"SELECT"},
			rows: []GrantRow{gr("SELECT", "app.*", "g"), gr("SELECT", "app.t", "pg")}, exists: true, effective: true},
		// GRANT SELECT ON *.* WITH GRANT OPTION, REVOKE GRANT OPTION FOR
		// SELECT ON app.*, GRANT SELECT ON app.t WITH GRANT OPTION [K3r].
		"OptionRevokedAbove": {target: appT, privileges: []string{"SELECT"},
			rows:   []GrantRow{gr("SELECT", "*.*", "g"), gr("SELECT", "app.*", "pg"), gr("SELECT", "app.t", "g")},
			exists: true, effective: true,
			apply:  []string{"REVOKE GRANT OPTION FOR SELECT ON `app`.`t` FROM `g`"},
			revoke: []string{"REVOKE GRANT OPTION FOR SELECT ON `app`.`t` FROM `g`"}, privs: []string{"SELECT"}},
		"OptionRevokedAboveWanted": {target: appT, privileges: []string{"SELECT"}, wgo: true,
			rows:   []GrantRow{gr("SELECT", "*.*", "g"), gr("SELECT", "app.*", "pg"), gr("SELECT", "app.t", "g")},
			exists: true, effective: true,
			revoke: []string{"REVOKE GRANT OPTION FOR SELECT ON `app`.`t` FROM `g`"}, privs: []string{"SELECT"}},
		// ... then REVOKE SELECT ON app.*, GRANT SELECT ON app.t [K5r]: the
		// privilege is revoked above the target, so it is revoked fully.
		"PrivilegeRevokedAbove": {target: appT, privileges: []string{"SELECT"},
			rows:   []GrantRow{gr("SELECT", "*.*", "g"), gr("SELECT", "app.*", "p"), gr("SELECT", "app.t", "")},
			exists: true, effective: true,
			revoke: []string{"REVOKE SELECT ON `app`.`t` FROM `g`"}, privs: []string{"SELECT"}},
		// A Grant owns its exact target: foreign privileges there are
		// revoked, but never on deletion.
		"ForeignAtTarget": {target: app, privileges: []string{"SELECT"},
			rows:   []GrantRow{gr("SELECT", "app.*", ""), gr("INSERT", "app.*", "")},
			exists: true, effective: true,
			apply:  []string{"REVOKE INSERT ON `app`.* FROM `g`"},
			revoke: []string{"REVOKE SELECT ON `app`.* FROM `g`"}, privs: []string{"INSERT", "SELECT"}},
		"ForeignOnlyAtTarget": {target: app, privileges: []string{"SELECT"},
			rows:   []GrantRow{gr("INSERT", "app.*", "")},
			exists: true,
			apply:  []string{"GRANT SELECT ON `app`.* TO `g`", "REVOKE INSERT ON `app`.* FROM `g`"}, privs: []string{"INSERT"}},
		// A foreign privilege covered above without option but holding it at
		// the target: only the option is revoked, which removes the row.
		"ForeignOptionCoveredAbove": {target: appT, privileges: []string{"SELECT"},
			rows:   []GrantRow{gr("INSERT", "app.*", ""), gr("INSERT", "app.t", "g")},
			exists: true,
			apply:  []string{"GRANT SELECT ON `app`.`t` TO `g`", "REVOKE GRANT OPTION FOR INSERT ON `app`.`t` FROM `g`"}, privs: []string{"INSERT"}},
		// Wildcards on 26.8 (is_wildcard) [W5]: GRANT SELECT ON team_*.*,
		// REVOKE SELECT ON team_b*.*, REVOKE SELECT ON team_c.*, GRANT
		// INSERT ON app.ev*.
		"WildcardCovers": {version: v268, target: db("team_a"), privileges: []string{"SELECT"},
			rows: wildcardRows, exists: true, effective: true},
		"WildcardCoversItsPrefix": {version: v268, target: db("team_"), privileges: []string{"SELECT"},
			rows: wildcardRows, exists: true, effective: true},
		"WildcardRevoked": {version: v268, target: db("team_bx"), privileges: []string{"SELECT"},
			rows: wildcardRows, apply: []string{"GRANT SELECT ON `team_bx`.* TO `g`"}},
		"WildcardRevokedLiteral": {version: v268, target: db("team_c"), privileges: []string{"SELECT"},
			rows: wildcardRows, apply: []string{"GRANT SELECT ON `team_c`.* TO `g`"}},
		"WildcardTable": {version: v268, target: tbl("app", "events"), privileges: []string{"INSERT"},
			rows: wildcardRows, exists: true, effective: true},
		"WildcardTableColumns": {version: v268, target: GrantTarget{Database: "app", Table: "ev", Columns: []string{"x"}}, privileges: []string{"INSERT"},
			rows: wildcardRows, exists: true, effective: true},
		"WildcardTableNoMatch": {version: v268, target: tbl("app", "e"), privileges: []string{"INSERT"},
			rows: wildcardRows, apply: []string{"GRANT INSERT ON `app`.`e` TO `g`"}},
		// GRANT INSERT ON app*.*, GRANT INSERT ON app.*, REVOKE INSERT ON
		// app.* [26.8 F8]: a literal revoke below the wildcard.
		"WildcardLiteralRevoke": {version: v268, target: app, privileges: []string{"INSERT", "SELECT"},
			rows:  []GrantRow{gr("SELECT", "app.*", "w"), gr("INSERT", "app.*", "w"), gr("INSERT", "app.*", "p")},
			apply: []string{"GRANT INSERT ON `app`.* TO `g`"}},
		// Source rows and prefix-scoped user DDL are never counted or revoked:
		// GRANT SELECT ON *.*, GRANT S3, URL ON *.*, GRANT CREATE USER ON
		// svc_* [O4] on 25.3 (stored as global rows) and on 26.8 (READ and
		// WRITE with access_object).
		"Sources253": {version: v253, target: global, privileges: []string{"SELECT"},
			rows:   []GrantRow{gr("SELECT", "*.*", ""), gr("CREATE USER", "*.*", ""), gr("URL", "*.*", ""), gr("S3", "*.*", "")},
			exists: true, effective: true,
			revoke: []string{"REVOKE SELECT ON *.* FROM `g`"}, privs: []string{"SELECT"}},
		"Sources268": {version: v268, target: global, privileges: []string{"SELECT"},
			rows: []GrantRow{gr("SELECT", "*.*", ""), withObject(gr("CREATE USER", "*.*", "w"), "svc_"),
				withObject(gr("READ", "*.*", ""), "S3"), withObject(gr("READ", "*.*", ""), "URL"),
				withObject(gr("WRITE", "*.*", ""), "S3"), withObject(gr("WRITE", "*.*", ""), "URL")},
			exists: true, effective: true,
			revoke: []string{"REVOKE SELECT ON *.* FROM `g`"}, privs: []string{"SELECT"}},
		"Sources258": {version: v258, target: global, privileges: []string{"SELECT"},
			rows: []GrantRow{gr("SELECT", "*.*", ""), withObject(gr("CREATE USER", "*.*", ""), "svc_"),
				withObject(gr("READ", "*.*", ""), "S3"), withObject(gr("READ", "*.*", ""), "URL"),
				withObject(gr("WRITE", "*.*", ""), "S3"), withObject(gr("WRITE", "*.*", ""), "URL")},
			exists: true, effective: true,
			revoke: []string{"REVOKE SELECT ON *.* FROM `g`"}, privs: []string{"SELECT"}},
		"SourcesA263": {version: va263, target: global, privileges: []string{"SELECT", "INSERT"},
			rows: []GrantRow{gr("SELECT", "*.*", ""), withObject(gr("CREATE USER", "*.*", ""), "svc_"),
				withObject(gr("READ", "*.*", ""), "S3"), withObject(gr("WRITE", "*.*", ""), "S3")},
			exists: true,
			apply:  []string{"GRANT INSERT ON *.* TO `g`"},
			revoke: []string{"REVOKE SELECT ON *.* FROM `g`"}, privs: []string{"SELECT"}},
		// Without is_wildcard (25.8 W5, the same on 25.3 and Antalya), the
		// wildcard team_* looks like a literal database team_: a Grant on
		// team_.* would see it at its target and revoke it, cutting into the
		// wildcard grant. The controller therefore refuses grantees with
		// wildcard grants on such servers before their rows are analysed
		// (WildcardObjects).
		"WithoutIsWildcardLooksLiteral258": {version: v258, target: db("team_"), privileges: []string{"INSERT"},
			rows:   []GrantRow{gr("SELECT", "team_.*", ""), gr("SELECT", "team_b.*", "p"), gr("SELECT", "team_c.*", "p"), gr("INSERT", "app.ev", "")},
			exists: true,
			apply:  []string{"GRANT INSERT ON `team_`.* TO `g`", "REVOKE SELECT ON `team_`.* FROM `g`"}, privs: []string{"SELECT"}},
		// GRANT ACCESS MANAGEMENT ON *.* [N3r 25.3]: the global and table
		// level flags are foreign at *.*; USER_NAME ones are ignored.
		"AccessManagementGlobal": {version: v253, target: global, privileges: []string{"SELECT"},
			rows: append([]GrantRow{gr("SELECT", "*.*", "")}, dbRows("*.*", "ALLOW SQL SECURITY NONE", "ALTER QUOTA", "ALTER ROLE", "ALTER ROW POLICY",
				"ALTER SETTINGS PROFILE", "ALTER USER", "CREATE QUOTA", "CREATE ROLE", "CREATE ROW POLICY", "CREATE SETTINGS PROFILE", "CREATE USER",
				"DROP QUOTA", "DROP ROLE", "DROP ROW POLICY", "DROP SETTINGS PROFILE", "DROP USER", "ROLE ADMIN", "SHOW ACCESS")...),
			exists: true, effective: true,
			apply: []string{"REVOKE ALLOW SQL SECURITY NONE, ALTER QUOTA, ALTER ROW POLICY, ALTER SETTINGS PROFILE, CREATE QUOTA, CREATE ROW POLICY, " +
				"CREATE SETTINGS PROFILE, DROP QUOTA, DROP ROW POLICY, DROP SETTINGS PROFILE, ROLE ADMIN, SHOW QUOTAS, SHOW ROLES, SHOW ROW POLICIES, " +
				"SHOW SETTINGS PROFILES, SHOW USERS ON *.* FROM `g`"},
			revoke: []string{"REVOKE SELECT ON *.* FROM `g`"},
			privs: []string{"ALLOW SQL SECURITY NONE", "ALTER QUOTA", "ALTER ROW POLICY", "ALTER SETTINGS PROFILE", "CREATE QUOTA", "CREATE ROW POLICY",
				"CREATE SETTINGS PROFILE", "DROP QUOTA", "DROP ROW POLICY", "DROP SETTINGS PROFILE", "ROLE ADMIN", "SELECT", "SHOW ACCESS"}},
		// A privilege the catalogue does not know (a newer server) is never
		// counted or revoked.
		"UnknownAccessType": {version: v253, target: app, privileges: []string{"SELECT"},
			rows: []GrantRow{gr("ALTER EXECUTE", "app.*", "")}, apply: []string{"GRANT SELECT ON `app`.* TO `g`"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			v := tc.version
			if v == "" {
				v = v253
			}
			c := catalogue(t, v)
			desired, err := c.Expand(tc.privileges, tc.target.Level())
			if err != nil {
				t.Fatal(err)
			}
			a := Analyze(c, tc.target, desired, tc.rows)
			if a.Exists() != tc.exists || a.Effective() != tc.effective {
				t.Errorf("Exists() = %t, Effective() = %t; want %t, %t (missing %v)", a.Exists(), a.Effective(), tc.exists, tc.effective, a.Missing())
			}
			apply, err := a.Apply(tc.wgo).Statements(tc.target, "g")
			if err != nil {
				t.Fatal(err)
			}
			revoke, err := a.Revoke().Statements(tc.target, "g")
			if err != nil {
				t.Fatal(err)
			}
			if a.Apply(tc.wgo).Empty() != (len(apply) == 0) || a.Revoke().Empty() != (len(revoke) == 0) {
				t.Error("Plan.Empty() disagrees with the statements")
			}
			if tc.check != nil {
				tc.check(t, c, apply, revoke)
			} else {
				if diff := cmp.Diff(tc.apply, apply, cmpopts.EquateEmpty()); diff != "" {
					t.Errorf("Apply() -want +got:\n%s", diff)
				}
				if diff := cmp.Diff(tc.revoke, revoke, cmpopts.EquateEmpty()); diff != "" {
					t.Errorf("Revoke() -want +got:\n%s", diff)
				}
			}
			if tc.privs != nil {
				if diff := cmp.Diff(tc.privs, a.Privileges()); diff != "" {
					t.Errorf("Privileges() -want +got:\n%s", diff)
				}
			}
			// The analysis never plans a partial revoke: everything it
			// revokes is granted by a row at exactly the target.
			for _, p := range []Plan{a.Apply(tc.wgo), a.Revoke()} {
				for _, r := range slices.Concat(p.Revoke, p.RevokeGrantOption) {
					if !exactlyGranted(a, r) {
						t.Errorf("%s is revoked but not granted at exactly the target", r.Privilege)
					}
				}
			}
		})
	}
}

// wildcardRows are the rows of 26.8 for GRANT SELECT ON team_*.*, REVOKE
// SELECT ON team_b*.*, REVOKE SELECT ON team_c.*, GRANT INSERT ON app.ev*.
var wildcardRows = []GrantRow{gr("SELECT", "team_.*", "w"), gr("SELECT", "team_b.*", "pw"), gr("SELECT", "team_c.*", "p"), gr("INSERT", "app.ev", "w")}

func exactlyGranted(a *Analysis, r Revocation) bool {
	for _, st := range a.points {
		if st.exact.Has(r.Privilege) && (st.column == "" || slices.Contains(r.Columns, st.column)) {
			return true
		}
	}
	return false
}

// alterSubset checks that a Grant of ALTER UPDATE on app.* revokes exactly
// the other flags of ALTER at the target.
func alterSubset(t *testing.T, c *Catalogue, apply, revoke []string) {
	t.Helper()
	var others []string
	for _, l := range c.RowFlags("ALTER", TargetDatabase) {
		if l != "ALTER UPDATE" {
			others = append(others, l)
		}
	}
	want := []string{"REVOKE " + strings.Join(others, ", ") + " ON `app`.* FROM `g`"}
	if diff := cmp.Diff(want, apply); diff != "" {
		t.Errorf("Apply() -want +got:\n%s", diff)
	}
	if diff := cmp.Diff([]string{"REVOKE ALTER UPDATE ON `app`.* FROM `g`"}, revoke); diff != "" {
		t.Errorf("Revoke() -want +got:\n%s", diff)
	}
}

func TestCovers(t *testing.T) {
	s := func(r GrantRow) scope { return rowScope(r) }
	cases := []struct {
		a, b GrantRow
		want bool
	}{
		{gr("X", "*.*", ""), gr("X", "app.t(c)", ""), true},
		{gr("X", "app.*", ""), gr("X", "*.*", ""), false},
		{gr("X", "app.*", ""), gr("X", "app.t", ""), true},
		{gr("X", "app.*", ""), gr("X", "app2.t", ""), false},
		{gr("X", "app.*", "w"), gr("X", "app2.t", ""), true},
		{gr("X", "app.*", "w"), gr("X", "app.*", ""), true},
		{gr("X", "app.*", ""), gr("X", "app.*", "w"), false},
		{gr("X", "ap.*", "w"), gr("X", "app.*", "w"), true},
		{gr("X", "app.*", "w"), gr("X", "ap.*", "w"), false},
		{gr("X", "app.t", "w"), gr("X", "app.tt(c)", ""), true},
		{gr("X", "app.t", ""), gr("X", "app.t", "w"), false},
		{gr("X", "app.t", ""), gr("X", "app.*", ""), false},
		{gr("X", "app.t(c)", ""), gr("X", "app.t(d)", ""), false},
		{gr("X", "app.t(c)", ""), gr("X", "app.t(c)", ""), true},
	}
	for _, c := range cases {
		if got := covers(s(c.a), s(c.b)); got != c.want {
			t.Errorf("covers(%+v, %+v) = %t", s(c.a), s(c.b), got)
		}
	}
}

func TestWildcardObjectsErrors(t *testing.T) {
	if _, err := WildcardObjects([]string{"GRANT SELECT ON `unterminated TO r"}); err == nil || errors.Unwrap(err) == nil {
		t.Errorf("an unreadable line must fail: %v", err)
	}
}
