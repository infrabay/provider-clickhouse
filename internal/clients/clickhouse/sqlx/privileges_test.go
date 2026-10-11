// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package sqlx

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	ch "github.com/infrabay/provider-clickhouse/internal/clients/clickhouse"
)

// The server versions of the CI matrix, as version() reports them; their
// catalogues are recorded in testdata/privileges.
const (
	v253  = "25.3.3.42"
	v258  = "25.8.33.6"
	v268  = "26.8.23.6"
	va263 = "26.3.13.20001.altinityantalya"
	va266 = "26.6.4.20001.altinityantalya"
)

var versions = []string{v253, v258, v268, va263, va266}

func readCatalogue(t *testing.T, version string) []ch.Privilege {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", "privileges", version+".tsv"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	privs, err := ReadPrivilegesTSV(f)
	if err != nil {
		t.Fatal(err)
	}
	return privs
}

func catalogue(t *testing.T, version string) *Catalogue {
	t.Helper()
	c, err := NewCatalogue(readCatalogue(t, version))
	if err != nil {
		t.Fatalf("%s: %v", version, err)
	}
	return c
}

// rowsFlags expands access types of rows at one level, as Analyze does.
func rowsFlags(c *Catalogue, level TargetLevel, accessTypes ...string) PrivilegeSet {
	s := PrivilegeSet{}
	for _, a := range accessTypes {
		for _, l := range c.RowFlags(a, level) {
			s[l] = struct{}{}
		}
	}
	return s
}

func TestRecordedCatalogues(t *testing.T) {
	// Design section 7: the row counts of system.privileges.
	want := map[string]int{v253: 206, v258: 211, v268: 248, va263: 233, va266: 246}
	for _, v := range versions {
		if got := catalogue(t, v).Len(); got != want[v] {
			t.Errorf("%s: %d privileges, want %d", v, got, want[v])
		}
	}
}

func TestPrivilegesTSVRoundTrip(t *testing.T) {
	in := []ch.Privilege{
		{Name: "SELECT", Aliases: []string{}, Level: "COLUMN", Parent: "ALL"},
		{Name: "SHOW USERS", Aliases: []string{"SHOW CREATE USER"}, Level: "GLOBAL", Parent: "SHOW ACCESS"},
		{Name: "ALL", Aliases: []string{"ALL PRIVILEGES"}},
	}
	var b bytes.Buffer
	if err := WritePrivilegesTSV(&b, "a\nb", in); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(b.String(), "# a\n# b\nSELECT\t[]\tCOLUMN\tALL\n") {
		t.Errorf("format:\n%s", b.String())
	}
	out, err := ReadPrivilegesTSV(&b)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(in, out); diff != "" {
		t.Errorf("round trip -in +out:\n%s", diff)
	}
	if err := WritePrivilegesTSV(&b, "", []ch.Privilege{{Name: "A\tB"}}); err == nil {
		t.Error("a tab in a name must be refused")
	}
	for _, bad := range []string{"X\t[]\tGLOBAL\n", "X\tnot-json\tGLOBAL\tALL\n"} {
		if _, err := ReadPrivilegesTSV(strings.NewReader(bad)); err == nil {
			t.Errorf("ReadPrivilegesTSV(%q) = nil error", bad)
		}
	}
}

func TestNewCatalogueErrors(t *testing.T) {
	cases := map[string][]ch.Privilege{
		"Empty":         nil,
		"Duplicate":     {{Name: "A"}, {Name: "A"}},
		"NoName":        {{Name: ""}},
		"UnknownParent": {{Name: "A", Parent: "B"}},
		"Cycle":         {{Name: "A", Parent: "B"}, {Name: "B", Parent: "A"}},
	}
	for name, rows := range cases {
		if _, err := NewCatalogue(rows); err == nil {
			t.Errorf("%s: NewCatalogue() = nil error", name)
		}
	}
}

func TestCanonical(t *testing.T) {
	cases := map[string]struct {
		in   string
		want string
		// errs is a substring of the error, per version where the input is
		// not valid ("" means valid everywhere).
		err map[string]string
	}{
		"Upper":          {in: "select", want: "SELECT"},
		"CamelCase":      {in: "DICTGET", want: "dictGet"},
		"Alias":          {in: "update", want: "ALTER UPDATE"},
		"Spaces":         {in: "  alter   Update ", want: "ALTER UPDATE"},
		"AliasOfGroup":   {in: "all privileges", want: "ALL"},
		"Group":          {in: "Alter", want: "ALTER"},
		"AllowListed":    {in: "KILL QUERY", want: "KILL QUERY"},
		"Cluster":        {in: "cluster", want: "CLUSTER"},
		"AliasMulti":     {in: "MODIFY ORDER BY", want: "ALTER ORDER BY"},
		"SecretsDisplay": {in: "displaysecretsinshowandselect", want: "displaySecretsInShowAndSelect"},
		"UserNameLevel":  {in: "create user", want: "CREATE USER"},
		"Unknown":        {in: "FROBNICATE", err: map[string]string{v253: "unknown", v258: "unknown", v268: "unknown", va263: "unknown", va266: "unknown"}},
		"None":           {in: "NONE", err: map[string]string{v253: "grants nothing", v258: "grants nothing", v268: "grants nothing", va263: "grants nothing", va266: "grants nothing"}},
		"Usage":          {in: "usage", err: map[string]string{v253: "grants nothing", v258: "grants nothing", v268: "grants nothing", va263: "grants nothing", va266: "grants nothing"}},
		"AmbiguousAlias": {in: "ALTER TABLE MODIFY QUERY", err: map[string]string{v253: "ambiguous", v258: "ambiguous", v268: "ambiguous", va263: "ambiguous", va266: "ambiguous"}},
		"Source":         {in: "S3", err: map[string]string{v253: "not managed", v258: "not managed", v268: "not managed", va263: "not managed", va266: "not managed"}},
		"Sources":        {in: "sources", err: map[string]string{v253: "not managed", v258: "not managed", v268: "not managed", va263: "not managed", va266: "not managed"}},
		"Remote":         {in: "REMOTE", err: map[string]string{v253: "not managed", v258: "not managed", v268: "not managed", va263: "not managed", va266: "not managed"}},
		"ReadSource":     {in: "SOURCE READ", err: map[string]string{v253: "unknown", v258: "not managed", v268: "not managed", va263: "not managed", va266: "not managed"}},
		"NewSource":      {in: "YTSAURUS", err: map[string]string{v253: "unknown", v258: "unknown", v268: "not managed", va263: "unknown", va266: "not managed"}},
		"Disk":           {in: "DISK", err: map[string]string{v253: "unknown", v258: "unknown", v268: "not managed", va263: "unknown", va266: "unknown"}},
		// A group without children: it carries no flag, and granting it
		// stores nothing.
		"FilesystemCache": {in: "SHOW FILESYSTEM CACHES", err: map[string]string{v253: "grants nothing", v258: "grants nothing", v268: "grants nothing", va263: "grants nothing", va266: "grants nothing"}},
		"ParentWithFlag":  {in: "create arbitrary temporary table", want: "CREATE ARBITRARY TEMPORARY TABLE"},
	}
	for _, v := range versions {
		c := catalogue(t, v)
		for name, tc := range cases {
			t.Run(v+"/"+name, func(t *testing.T) {
				got, err := c.Canonical(tc.in)
				if want := tc.err[v]; want != "" {
					if err == nil || !strings.Contains(err.Error(), want) {
						t.Fatalf("Canonical(%q) = %q, %v; want an error with %q", tc.in, got, err, want)
					}
					return
				}
				if err != nil || got != tc.want {
					t.Fatalf("Canonical(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
				}
			})
		}
	}
}

// TestUnmanaged pins the source privilege classification (D-9) of every
// version: SOURCES, level SOURCE, the children of SOURCES (25.3) and every
// GLOBAL leaf directly under ALL that is not on the allow-list.
func TestUnmanaged(t *testing.T) {
	sources253 := []string{"AZURE", "FILE", "HDFS", "HIVE", "JDBC", "KAFKA", "MONGO", "MYSQL", "NATS", "ODBC", "POSTGRES", "RABBITMQ", "REDIS", "REMOTE", "S3", "SQLITE", "URL"}
	common := append(slices.Clone(sources253), "SOURCES")
	want := map[string][]string{
		v253:  common,
		v258:  append(slices.Clone(common), "READ", "WRITE"),
		va263: append(slices.Clone(common), "READ", "WRITE"),
		va266: append(slices.Clone(common), "ARROW FLIGHT", "READ", "WRITE", "YTSAURUS"),
		v268:  append(slices.Clone(common), "ARROW FLIGHT", "BIGQUERY", "DISK", "READ", "WRITE", "YTSAURUS"),
	}
	for _, v := range versions {
		c := catalogue(t, v)
		var got []string
		for _, p := range readCatalogue(t, v) {
			if c.Unmanaged(p.Name) {
				got = append(got, p.Name)
			}
		}
		slices.Sort(got)
		w := slices.Clone(want[v])
		slices.Sort(w)
		if diff := cmp.Diff(w, got); diff != "" {
			t.Errorf("%s: unmanaged privileges -want +got:\n%s", v, diff)
		}
		for _, allowed := range []string{"KILL QUERY", "KILL TRANSACTION", "MOVE PARTITION BETWEEN SHARDS", "displaySecretsInShowAndSelect", "CLUSTER"} {
			if c.Unmanaged(allowed) || !c.Known(allowed) {
				t.Errorf("%s: %s must be a known, managed privilege", v, allowed)
			}
		}
	}
	if c := catalogue(t, v268); c.Unmanaged("SHOW HANDLERS") || !c.Known("SHOW HANDLERS") {
		t.Error("26.8: SHOW HANDLERS is on the allow-list")
	}
}

// TestExpandMatchesServer compares the expansion model with what each server
// stored for a GRANT, captured through the driver (system.grants rows of a
// role after GRANT <privilege> ON <target>, in the server's group-compressed
// form). The stored form differs between versions; the flags must not.
func TestExpandMatchesServer(t *testing.T) {
	alterDB := map[string][]string{v253: {"ALTER"}, va266: {"ALTER"}, v268: {"ALTER TABLE", "ALTER DATABASE", "ALTER VIEW"}}
	allDB253 := []string{"CHECK", "SHOW", "SELECT", "INSERT", "ALTER", "CREATE DATABASE", "CREATE TABLE", "CREATE VIEW", "CREATE DICTIONARY",
		"DROP DATABASE", "DROP TABLE", "DROP VIEW", "DROP DICTIONARY", "UNDROP TABLE", "TRUNCATE", "OPTIMIZE", "BACKUP",
		"CREATE ROW POLICY", "ALTER ROW POLICY", "DROP ROW POLICY", "SHOW ROW POLICIES", "SYSTEM MERGES", "SYSTEM TTL MERGES",
		"SYSTEM FETCHES", "SYSTEM MOVES", "SYSTEM PULLING REPLICATION LOG", "SYSTEM CLEANUP", "SYSTEM VIEWS", "SYSTEM SENDS",
		"SYSTEM REPLICATION QUEUES", "SYSTEM VIRTUAL PARTS UPDATE", "SYSTEM REDUCE BLOCKING PARTS", "SYSTEM DROP REPLICA",
		"SYSTEM SYNC REPLICA", "SYSTEM RESTART REPLICA", "SYSTEM RESTORE REPLICA", "SYSTEM WAIT LOADING PARTS",
		"SYSTEM SYNC DATABASE REPLICA", "SYSTEM FLUSH DISTRIBUTED", "SYSTEM LOAD PRIMARY KEY", "SYSTEM UNLOAD PRIMARY KEY", "dictGet"}
	allDB := map[string][]string{
		v253:  allDB253,
		va266: append(slices.Clone(allDB253), "SYSTEM RESTORE DATABASE REPLICA", "SYSTEM FLUSH OBJECT STORAGE QUEUE"),
		v268: {"CHECK", "SHOW", "SELECT", "INSERT", "ALTER TABLE", "ALTER DATABASE", "ALTER VIEW", "CREATE DATABASE", "CREATE TABLE",
			"CREATE VIEW", "CREATE DICTIONARY", "DROP DATABASE", "DROP TABLE", "DROP VIEW", "DROP DICTIONARY", "UNDROP TABLE", "TRUNCATE",
			"OPTIMIZE", "BACKUP", "CREATE ROW POLICY", "ALTER ROW POLICY", "DROP ROW POLICY", "SHOW ROW POLICIES", "SYSTEM MERGES",
			"SYSTEM TTL MERGES", "SYSTEM FETCHES", "SYSTEM MOVES", "SYSTEM PULLING REPLICATION LOG", "SYSTEM CLEANUP", "SYSTEM BACKGROUND",
			"SYSTEM SENDS", "SYSTEM REPLICATION QUEUES", "SYSTEM VIRTUAL PARTS UPDATE", "SYSTEM REDUCE BLOCKING PARTS", "SYSTEM DROP REPLICA",
			"SYSTEM SYNC REPLICA", "SYSTEM RESTART REPLICA", "SYSTEM RESTORE REPLICA", "SYSTEM RESTORE DATABASE REPLICA",
			"SYSTEM WAIT LOADING PARTS", "SYSTEM WAIT QUERY RUNNER", "SYSTEM SYNC DATABASE REPLICA", "SYSTEM FLUSH DISTRIBUTED",
			"SYSTEM FLUSH OBJECT STORAGE QUEUE", "SYSTEM LOAD PRIMARY KEY", "SYSTEM UNLOAD PRIMARY KEY", "dictGet"},
	}
	allTable253 := []string{"ALTER ROW POLICY", "ALTER TABLE", "ALTER VIEW", "BACKUP", "CHECK", "CREATE DICTIONARY", "CREATE ROW POLICY",
		"CREATE TABLE", "CREATE VIEW", "DROP DICTIONARY", "DROP ROW POLICY", "DROP TABLE", "DROP VIEW", "INSERT", "OPTIMIZE", "SELECT",
		"SHOW COLUMNS", "SHOW DICTIONARIES", "SHOW ROW POLICIES", "SHOW TABLES", "SYSTEM CLEANUP", "SYSTEM DROP REPLICA", "SYSTEM FETCHES",
		"SYSTEM FLUSH DISTRIBUTED", "SYSTEM LOAD PRIMARY KEY", "SYSTEM MERGES", "SYSTEM MOVES", "SYSTEM PULLING REPLICATION LOG",
		"SYSTEM REDUCE BLOCKING PARTS", "SYSTEM REPLICATION QUEUES", "SYSTEM RESTART REPLICA", "SYSTEM RESTORE REPLICA", "SYSTEM SENDS",
		"SYSTEM SYNC REPLICA", "SYSTEM TTL MERGES", "SYSTEM UNLOAD PRIMARY KEY", "SYSTEM VIEWS", "SYSTEM VIRTUAL PARTS UPDATE",
		"SYSTEM WAIT LOADING PARTS", "TRUNCATE", "UNDROP TABLE", "dictGet"}
	allTable := map[string][]string{
		v253:  allTable253,
		v258:  append(slices.Clone(allTable253), "SYSTEM RESTORE DATABASE REPLICA"),
		va263: append(slices.Clone(allTable253), "SYSTEM RESTORE DATABASE REPLICA"),
		va266: append(slices.Clone(allTable253), "SYSTEM RESTORE DATABASE REPLICA", "SYSTEM FLUSH OBJECT STORAGE QUEUE"),
		v268: append(slices.DeleteFunc(slices.Clone(allTable253), func(s string) bool { return s == "SYSTEM VIEWS" }),
			"SYSTEM BACKGROUND", "SYSTEM FLUSH OBJECT STORAGE QUEUE", "SYSTEM RESTORE DATABASE REPLICA", "SYSTEM WAIT QUERY RUNNER"),
	}
	systemDB253 := []string{"SYSTEM CLEANUP", "SYSTEM DROP REPLICA", "SYSTEM FETCHES", "SYSTEM FLUSH DISTRIBUTED", "SYSTEM LOAD PRIMARY KEY",
		"SYSTEM MERGES", "SYSTEM MOVES", "SYSTEM PULLING REPLICATION LOG", "SYSTEM REDUCE BLOCKING PARTS", "SYSTEM REPLICATION QUEUES",
		"SYSTEM RESTART REPLICA", "SYSTEM RESTORE REPLICA", "SYSTEM SENDS", "SYSTEM SYNC DATABASE REPLICA", "SYSTEM SYNC REPLICA",
		"SYSTEM TTL MERGES", "SYSTEM UNLOAD PRIMARY KEY", "SYSTEM VIEWS", "SYSTEM VIRTUAL PARTS UPDATE", "SYSTEM WAIT LOADING PARTS"}
	systemDB := map[string][]string{
		v253: systemDB253,
		v268: append(slices.DeleteFunc(slices.Clone(systemDB253), func(s string) bool { return s == "SYSTEM VIEWS" }),
			"SYSTEM BACKGROUND", "SYSTEM FLUSH OBJECT STORAGE QUEUE", "SYSTEM RESTORE DATABASE REPLICA", "SYSTEM WAIT QUERY RUNNER"),
	}
	createGlobal := map[string][]string{v253: {"CREATE"}, v268: {"CREATE"}}
	createDB := map[string][]string{v253: {"CREATE DATABASE", "CREATE DICTIONARY", "CREATE TABLE", "CREATE VIEW"}, v268: {"CREATE DATABASE", "CREATE DICTIONARY", "CREATE TABLE", "CREATE VIEW"}}
	showTable := map[string][]string{v253: {"SHOW COLUMNS", "SHOW DICTIONARIES", "SHOW TABLES"}, v268: {"SHOW COLUMNS", "SHOW DICTIONARIES", "SHOW TABLES"}}

	cases := []struct {
		privilege string
		level     TargetLevel
		stored    map[string][]string
	}{
		{"ALTER", TargetDatabase, alterDB},
		{"ALL", TargetDatabase, allDB},
		{"ALL", TargetTable, allTable},
		{"SYSTEM", TargetDatabase, systemDB},
		{"CREATE", TargetGlobal, createGlobal},
		{"CREATE", TargetDatabase, createDB},
		{"SHOW", TargetTable, showTable},
	}
	for _, tc := range cases {
		for v, stored := range tc.stored {
			t.Run(tc.privilege+"/"+tc.level.String()+"/"+v, func(t *testing.T) {
				c := catalogue(t, v)
				got, err := c.Expand([]string{tc.privilege}, tc.level)
				if err != nil {
					t.Fatal(err)
				}
				want := rowsFlags(c, tc.level, stored...)
				if diff := cmp.Diff(want.Sorted(), got.Sorted()); diff != "" {
					t.Errorf("Expand(%s) on the %s level -server +model:\n%s", tc.privilege, tc.level, diff)
				}
			})
		}
	}
}

func TestExpand(t *testing.T) {
	cases := map[string]struct {
		privileges []string
		level      TargetLevel
		want       []string
		err        string
	}{
		"Flags":           {privileges: []string{"select", "INSERT"}, level: TargetDatabase, want: []string{"INSERT", "SELECT"}},
		"AliasAndName":    {privileges: []string{"UPDATE", "alter update"}, level: TargetColumn, want: []string{"ALTER UPDATE"}},
		"ShowAtColumn":    {privileges: []string{"SHOW"}, level: TargetColumn, want: []string{"SHOW COLUMNS"}},
		"GlobalLeaf":      {privileges: []string{"KILL QUERY"}, level: TargetGlobal, want: []string{"KILL QUERY"}},
		"DictGetAtTable":  {privileges: []string{"dictGet"}, level: TargetTable, want: []string{"dictGet"}},
		"WrongLevel":      {privileges: []string{"CREATE DATABASE"}, level: TargetTable, err: "cannot be granted on the table level"},
		"TableAtColumn":   {privileges: []string{"SHOW TABLES"}, level: TargetColumn, err: "cannot be granted on the column level"},
		"GlobalOnly":      {privileges: []string{"INTROSPECTION"}, level: TargetDatabase, err: "cannot be granted on the database level"},
		"AllOnGlobal":     {privileges: []string{"ALL"}, level: TargetGlobal, err: "CREATE USER (USER_NAME)"},
		"AccessMgmtGlob":  {privileges: []string{"ACCESS MANAGEMENT"}, level: TargetGlobal, err: "Grant does not support"},
		"UserNameLevel":   {privileges: []string{"CREATE USER"}, level: TargetDatabase, err: "CREATE USER (USER_NAME)"},
		"NamedCollection": {privileges: []string{"NAMED COLLECTION"}, level: TargetGlobal, err: "NAMED_COLLECTION"},
		"TableEngine":     {privileges: []string{"TABLE ENGINE"}, level: TargetGlobal, err: "TABLE_ENGINE"},
		"Source":          {privileges: []string{"SELECT", "URL"}, level: TargetGlobal, err: "not managed"},
		"Empty":           {level: TargetDatabase, err: "no privileges"},
	}
	c := catalogue(t, v253)
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := c.Expand(tc.privileges, tc.level)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("Expand() = %v, %v; want an error with %q", got, err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tc.want, got.Sorted()); diff != "" {
				t.Errorf("-want +got:\n%s", diff)
			}
		})
	}

	// CREATE on *.* grants the own flag of CREATE ARBITRARY TEMPORARY TABLE
	// besides that of its child: the server stores the expansion as one
	// CREATE row only then.
	for _, v := range versions {
		got, err := catalogue(t, v).Expand([]string{"CREATE"}, TargetGlobal)
		if err != nil {
			t.Fatal(err)
		}
		if !got.Has("CREATE ARBITRARY TEMPORARY TABLE") || !got.Has("CREATE TEMPORARY TABLE") || got.Has("CREATE") {
			t.Errorf("%s: Expand(CREATE) on *.* = %v", v, got.Sorted())
		}
	}

	// ACCESS MANAGEMENT on a database keeps the row policy privileges, as
	// the server does for GRANT ALL ON db.*.
	got, err := c.Expand([]string{"ACCESS MANAGEMENT"}, TargetDatabase)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]string{"ALTER ROW POLICY", "CREATE ROW POLICY", "DROP ROW POLICY", "SHOW ROW POLICIES"}, got.Sorted()); diff != "" {
		t.Errorf("ACCESS MANAGEMENT on a database -want +got:\n%s", diff)
	}

	// ALL on *.* is rejected on every version; source flags never appear in
	// an expansion.
	for _, v := range versions {
		c := catalogue(t, v)
		if _, err := c.Expand([]string{"ALL"}, TargetGlobal); err == nil {
			t.Errorf("%s: ALL on *.* must be rejected", v)
		}
		got, err := c.Expand([]string{"SYSTEM", "SHOW", "CREATE", "DROP", "INTROSPECTION", "dictGet", "KILL QUERY"}, TargetGlobal)
		if err != nil {
			t.Fatalf("%s: %v", v, err)
		}
		for l := range got {
			if c.Unmanaged(l) {
				t.Errorf("%s: unmanaged %s in an expansion", v, l)
			}
		}
	}
}

func TestRowFlags(t *testing.T) {
	c253, c268 := catalogue(t, v253), catalogue(t, v268)
	cases := map[string]struct {
		c          *Catalogue
		accessType string
		level      TargetLevel
		want       []string
	}{
		"Leaf":                 {c: c253, accessType: "SELECT", level: TargetColumn, want: []string{"SELECT"}},
		"Source253":            {c: c253, accessType: "S3", level: TargetGlobal},
		"SourceRead268":        {c: c268, accessType: "READ", level: TargetGlobal},
		"UserNameLevel":        {c: c253, accessType: "CREATE USER", level: TargetGlobal},
		"Unknown":              {c: c253, accessType: "ALTER EXECUTE", level: TargetDatabase},
		"ShowFilesystemCaches": {c: c253, accessType: "SHOW FILESYSTEM CACHES", level: TargetGlobal},
		"GroupAtTable":         {c: c268, accessType: "SHOW", level: TargetTable, want: []string{"SHOW COLUMNS", "SHOW DICTIONARIES", "SHOW TABLES"}},
		// A parent with a level carries a flag of its own besides its child's.
		"ParentWithLevel":       {c: c253, accessType: "CREATE ARBITRARY TEMPORARY TABLE", level: TargetGlobal, want: []string{"CREATE ARBITRARY TEMPORARY TABLE", "CREATE TEMPORARY TABLE"}},
		"AccessMgmtKeepsGlobal": {c: c253, accessType: "SHOW ACCESS", level: TargetGlobal, want: []string{"SHOW QUOTAS", "SHOW ROLES", "SHOW ROW POLICIES", "SHOW SETTINGS PROFILES", "SHOW USERS"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if diff := cmp.Diff(tc.want, tc.c.RowFlags(tc.accessType, tc.level), cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("-want +got:\n%s", diff)
			}
		})
	}
	if got := c253.Flags("nope"); got != nil {
		t.Errorf("Flags(unknown) = %v", got)
	}
	if got := c253.Flags("ALTER TABLE"); !slices.Contains(got, "ALTER UPDATE") || slices.Contains(got, "ALTER TABLE") {
		t.Errorf("Flags(ALTER TABLE) = %v", got)
	}
}

func TestTargetLevelString(t *testing.T) {
	for l, want := range map[TargetLevel]string{TargetGlobal: "global", TargetDatabase: "database", TargetTable: "table", TargetColumn: "column", 0: "TargetLevel(0)"} {
		if got := l.String(); got != want {
			t.Errorf("%d: %q", int(l), got)
		}
	}
}
