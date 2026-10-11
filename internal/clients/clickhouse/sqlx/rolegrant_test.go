// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package sqlx

import (
	"strings"
	"testing"
)

func TestRoleGrantStmts(t *testing.T) {
	cases := map[string]struct {
		got, want string
	}{
		"Grant": {
			got:  GrantRoleStmt("team-a_reader", "team-a_api", false),
			want: "GRANT `team-a_reader` TO `team-a_api`",
		},
		"GrantWithAdminOption": {
			got:  GrantRoleStmt("team-a_reader", "team-a_api", true),
			want: "GRANT `team-a_reader` TO `team-a_api` WITH ADMIN OPTION",
		},
		"RevokeAdminOption": {
			got:  RevokeAdminOptionStmt("team-a_reader", "team-a_api"),
			want: "REVOKE ADMIN OPTION FOR `team-a_reader` FROM `team-a_api`",
		},
		"Revoke": {
			got:  RevokeRoleStmt("team-a_reader", "team-a_api"),
			want: "REVOKE `team-a_reader` FROM `team-a_api`",
		},
		// A role named like a privilege is still granted as a role.
		"KeywordRole": {
			got:  GrantRoleStmt("SELECT", "svc@my-project.iam.example.com", false),
			want: "GRANT `SELECT` TO `svc@my-project.iam.example.com`",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("\ngot:  %s\nwant: %s", tc.got, tc.want)
			}
		})
	}
}

// Every name is quoted, so no name can end the identifier and inject a
// clause: the statement keeps exactly its keywords.
func TestRoleGrantStmtsAdversarial(t *testing.T) {
	for mName, m := range roleTestNames {
		for gName, g := range roleTestNames {
			qm, qg := roleTestQuoted[mName], roleTestQuoted[gName]
			if got, want := GrantRoleStmt(m, g, true), "GRANT "+qm+" TO "+qg+" WITH ADMIN OPTION"; got != want {
				t.Errorf("GrantRoleStmt(%q, %q):\ngot:  %s\nwant: %s", m, g, got, want)
			}
			if got, want := RevokeAdminOptionStmt(m, g), "REVOKE ADMIN OPTION FOR "+qm+" FROM "+qg; got != want {
				t.Errorf("RevokeAdminOptionStmt(%q, %q):\ngot:  %s\nwant: %s", m, g, got, want)
			}
			if got, want := RevokeRoleStmt(m, g), "REVOKE "+qm+" FROM "+qg; got != want {
				t.Errorf("RevokeRoleStmt(%q, %q):\ngot:  %s\nwant: %s", m, g, got, want)
			}
		}
	}
}

func TestQueryRoleGrant(t *testing.T) {
	if q := QueryRoleGrant(false); q != QueryUserRoleGrant || !strings.Contains(q, "user_name = {grantee:String}") {
		t.Errorf("user grantee: %s", q)
	}
	if q := QueryRoleGrant(true); q != QueryRoleRoleGrant || !strings.Contains(q, "role_name = {grantee:String}") {
		t.Errorf("role grantee: %s", q)
	}
	for _, q := range []string{QueryUserRoleGrant, QueryRoleRoleGrant} {
		// Names travel as server-side parameters, never in the query text.
		if !strings.Contains(q, "granted_role_name = {memberOf:String}") || strings.Contains(q, "'") || strings.Contains(q, "`") {
			t.Errorf("query must use parameters only: %s", q)
		}
	}
}
