// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package sqlx

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// The lines are SHOW GRANTS output read through the driver on 25.3.3.42 and
// 26.8.23.6 (identical on both unless noted).
func TestWildcardObjects(t *testing.T) {
	cases := map[string]struct {
		lines []string
		want  []string
	}{
		"DatabaseAndTableWildcards": {
			lines: []string{
				"GRANT INSERT ON app.ev* TO gw",
				"GRANT SELECT ON team_*.* TO gw",
				"REVOKE SELECT ON team_b*.* FROM gw",
				"REVOKE SELECT ON team_c.* FROM gw",
			},
			want: []string{"app.ev*", "team_*.*", "team_b*.*"},
		},
		"QuotedPrefix": {
			lines: []string{"GRANT SELECT ON `team-a`*.* TO gw", "GRANT SELECT ON `odd*name`.* TO gw"},
			want:  []string{"`team-a`*.*"},
		},
		// GRANT SELECT ON team_*.ev* and ON `odd`*.`x` are stored and shown
		// as a table prefix in a literal database.
		"PrefixPrintedOnTable": {
			lines: []string{"GRANT SELECT ON team_.ev* TO w1", "GRANT SELECT ON odd.x* TO w4", "GRANT SELECT ON `we*rd`.`t*` TO w4"},
			want:  []string{"team_.ev*", "odd.x*"},
		},
		"LiteralStars": {
			lines: []string{"GRANT SELECT ON `we*rd`.`t` TO `a'b\\`c`", "GRANT SELECT ON *.* TO w5", "GRANT SELECT ON app.* TO x"},
		},
		"Columns": {
			lines: []string{"GRANT SELECT(a, b), INSERT(a) ON app.t TO gc", "REVOKE SELECT(a) ON app.t FROM gc", "GRANT SELECT(`on`, `x*`) ON app.t TO `x*`"},
		},
		"GrantOption": {
			lines: []string{"GRANT SELECT ON app.* TO go WITH GRANT OPTION", "REVOKE GRANT OPTION FOR SELECT ON app.t FROM go", "GRANT SELECT ON app_*.* TO w7 WITH GRANT OPTION"},
			want:  []string{"app_*.*"},
		},
		"RoleGrants": {
			lines: []string{"GRANT rr1 TO ra1 WITH ADMIN OPTION", "GRANT `r*` TO `u*`"},
		},
		// User and role DDL: ON * is every name, ON svc_* a prefix. Neither
		// has a database part, and their rows are of the unsupported level
		// USER_NAME.
		"AccessManagement": {
			lines: []string{
				"GRANT ROLE ADMIN, SHOW ACCESS ON *.* TO gx",
				"GRANT CREATE USER, ALTER USER, DROP USER, CREATE ROLE, ALTER ROLE, DROP ROLE ON * TO gx",
				"GRANT CREATE USER ON svc_* TO gg",
			},
		},
		// Objects are read as the run of tokens without spaces, so names
		// that are keywords do not end them early.
		"KeywordNames": {
			lines: []string{"GRANT SELECT ON TO*.* TO r", "GRANT SELECT ON ON.* TO r", "REVOKE SELECT ON FROM.ev* FROM r"},
			want:  []string{"TO*.*", "FROM.ev*"},
		},
		"SeveralObjects": {
			lines: []string{"GRANT SELECT ON a.*, INSERT ON b*.* TO x"},
			want:  []string{"b*.*"},
		},
		"OnAtTheEnd": {
			lines: []string{"GRANT SELECT ON"},
		},
		"Sources": {
			lines: []string{"GRANT SELECT, URL, S3 ON *.* TO gg", "GRANT S3 ON *.* TO gg", "GRANT USAGE ON *.* TO c1"},
		},
		"RevokeWildcardUnderGlobal": {
			lines: []string{"GRANT SELECT ON *.* TO w5", "REVOKE SELECT ON app*.* FROM w5"},
			want:  []string{"app*.*"},
		},
		"Duplicate": {
			lines: []string{"GRANT SELECT ON team_*.* TO a", "GRANT INSERT ON team_*.* TO a WITH GRANT OPTION"},
			want:  []string{"team_*.*"},
		},
		"Empty": {},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := WildcardObjects(tc.lines)
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tc.want, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("-want +got:\n%s", diff)
			}
		})
	}
}
