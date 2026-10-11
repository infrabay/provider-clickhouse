// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package sqlx

import (
	"strings"
	"testing"
)

func ptr[T any](v T) *T { return &v }

const testUUID = "8a1c5f4e-2b3d-4c5e-9f60-718293a4b5c6"

func TestCreateDatabaseStmt(t *testing.T) {
	cases := map[string]struct {
		in   CreateDatabase
		want string
	}{
		"Minimal": {
			in:   CreateDatabase{Name: "app", UUID: testUUID},
			want: "CREATE DATABASE `app` UUID '" + testUUID + "'",
		},
		"AtomicWithComment": {
			in:   CreateDatabase{Name: "app", UUID: testUUID, Engine: &Engine{Name: EngineAtomic}, Comment: ptr("it's \\ fine")},
			want: "CREATE DATABASE `app` UUID '" + testUUID + "' ENGINE = Atomic COMMENT 'it\\'s \\\\ fine'",
		},
		"ReplicatedOnCluster": {
			in: CreateDatabase{Name: "team-a_db", UUID: testUUID, Cluster: "my-cluster", Engine: &Engine{
				Name: EngineReplicated, ZooKeeperPath: "/clickhouse/databases/{uuid}", ShardName: "{shard}", ReplicaName: "{replica}",
			}},
			want: "CREATE DATABASE `team-a_db` UUID '" + testUUID + "' ON CLUSTER `my-cluster` ENGINE = Replicated('/clickhouse/databases/{uuid}', '{shard}', '{replica}')",
		},
		"EmptyComment": {
			in:   CreateDatabase{Name: "app", UUID: testUUID, Comment: ptr("")},
			want: "CREATE DATABASE `app` UUID '" + testUUID + "' COMMENT ''",
		},
		"AdversarialName": {
			in:   CreateDatabase{Name: "a`b\\c@d.e*", UUID: testUUID},
			want: "CREATE DATABASE `a\\`b\\\\c@d.e*` UUID '" + testUUID + "'",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := CreateDatabaseStmt(tc.in)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("\ngot:  %s\nwant: %s", got, tc.want)
			}
		})
	}
	if _, err := CreateDatabaseStmt(CreateDatabase{Name: "app"}); err == nil {
		t.Error("a UUID is required")
	}
	if _, err := CreateDatabaseStmt(CreateDatabase{Name: "app", UUID: testUUID, Engine: &Engine{Name: "Memory"}}); err == nil {
		t.Error("unsupported engines must be rejected")
	}
}

func TestOtherDatabaseStatements(t *testing.T) {
	cases := map[string]struct{ got, want string }{
		"Exists":         {ExistsDatabase("a`b"), "EXISTS DATABASE `a\\`b`"},
		"CheckGrant":     {CheckGrantShowTables("app"), "CHECK GRANT SHOW TABLES ON `app`.*"},
		"AlterComment":   {AlterDatabaseCommentStmt("app", "", "x'y"), "ALTER DATABASE `app` MODIFY COMMENT 'x\\'y'"},
		"AlterOnCluster": {AlterDatabaseCommentStmt("app", "c1", ""), "ALTER DATABASE `app` ON CLUSTER `c1` MODIFY COMMENT ''"},
		"Drop":           {DropDatabaseStmt("app", ""), "DROP DATABASE IF EXISTS `app`"},
		"DropOnCluster":  {DropDatabaseStmt("app", "c1"), "DROP DATABASE IF EXISTS `app` ON CLUSTER `c1`"},
		"ClusterIDs": {ClusterDatabaseIDs("my'cluster"),
			"SELECT h, u FROM clusterAllReplicas('my\\'cluster', view(SELECT hostName() AS h, toString(uuid) AS u FROM system.databases WHERE name = {name:String}))"},
		"QueryDatabase":   {QueryDatabase, "SELECT toString(uuid), engine, engine_full, comment FROM system.databases WHERE name = {name:String}"},
		"QueryTableCount": {QueryDatabaseTableCount, "SELECT count() FROM system.tables WHERE database = {name:String}"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("\ngot:  %s\nwant: %s", tc.got, tc.want)
			}
		})
	}
}

func TestDatabaseNameLength(t *testing.T) {
	if got := EscapedFileName("a_b@c.d"); got != "a_b%40c%2Ed" {
		t.Errorf("EscapedFileName() = %q", got)
	}
	if err := CheckDatabaseNameLength(strings.Repeat("a", 247)); err != nil {
		t.Errorf("247 plain bytes + .sql.tmp fit: %v", err)
	}
	if err := CheckDatabaseNameLength(strings.Repeat("a", 248)); err == nil {
		t.Error("248 plain bytes + .sql.tmp do not fit")
	}
	// A 164-character name with '@' fails on the server (458).
	name := strings.Repeat("a", 82) + strings.Repeat("@", 82)
	if err := CheckDatabaseNameLength(name); err == nil {
		t.Error("escaped '@' takes three bytes")
	}
}

func TestParseEngineArgs(t *testing.T) {
	cases := map[string]struct {
		in      string
		name    string
		args    []string
		wantErr bool
	}{
		"Atomic":     {in: "Atomic", name: "Atomic"},
		"Replicated": {in: "Replicated('/clickhouse/databases/{uuid}', '{shard}', '{replica}')", name: "Replicated", args: []string{"/clickhouse/databases/{uuid}", "{shard}", "{replica}"}},
		"Escapes":    {in: `Replicated('/a\'b\\c', 's\x41', 'r\n')`, name: "Replicated", args: []string{`/a'b\c`, "sA", "r\n"}},
		"Settings":   {in: "Replicated('/p', 's', 'r') SETTINGS max_broken_tables_ratio = 1", name: "Replicated", args: []string{"/p", "s", "r"}},
		"Unclosed":   {in: "Replicated('/p', 's'", wantErr: true},
		"BadArg":     {in: "Replicated(42)", wantErr: true},
		"BadLiteral": {in: "Replicated('/p)", wantErr: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			n, args, err := ParseEngineArgs(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v", err)
			}
			if tc.wantErr {
				return
			}
			if n != tc.name || strings.Join(args, "|") != strings.Join(tc.args, "|") {
				t.Errorf("ParseEngineArgs() = %q %q", n, args)
			}
		})
	}
}
