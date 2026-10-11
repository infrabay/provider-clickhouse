// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package sqlx

import "testing"

// roleTestNames are role and grantee names that must survive quoting:
// backtick, backslash, quote, NUL, TAB, newline, '@', '.', '*', Unicode, a
// keyword, the two characters \t and an attempt to end the identifier.
var roleTestNames = map[string]string{
	"Plain":         "team-a_reader",
	"Backtick":      "a`b",
	"Backslash":     `a\b`,
	"Quote":         "it's",
	"NUL":           "a\x00b",
	"TAB":           "a\tb",
	"Newline":       "a\nb",
	"EmailLike":     "svc@my-project.iam.example.com",
	"Star":          "team_*",
	"Unicode":       "ü-rôle",
	"Keyword":       "SELECT",
	"EscapedTabRaw": `a\tb`,
	"Injection":     "r` TO `x`; DROP ROLE `y",
}

// roleTestQuoted are the expected QuoteIdent results of roleTestNames.
var roleTestQuoted = map[string]string{
	"Plain":         "`team-a_reader`",
	"Backtick":      "`a\\`b`",
	"Backslash":     "`a\\\\b`",
	"Quote":         "`it's`",
	"NUL":           "`a\x00b`",
	"TAB":           "`a\tb`",
	"Newline":       "`a\nb`",
	"EmailLike":     "`svc@my-project.iam.example.com`",
	"Star":          "`team_*`",
	"Unicode":       "`ü-rôle`",
	"Keyword":       "`SELECT`",
	"EscapedTabRaw": "`a\\\\tb`",
	"Injection":     "`r\\` TO \\`x\\`; DROP ROLE \\`y`",
}

func TestRoleStmts(t *testing.T) {
	for name, n := range roleTestNames {
		t.Run(name, func(t *testing.T) {
			q := roleTestQuoted[name]
			if got, want := CreateRoleStmt(n), "CREATE ROLE "+q; got != want {
				t.Errorf("CreateRoleStmt:\ngot:  %s\nwant: %s", got, want)
			}
			if got, want := DropRoleStmt(n), "DROP ROLE IF EXISTS "+q; got != want {
				t.Errorf("DropRoleStmt:\ngot:  %s\nwant: %s", got, want)
			}
		})
	}
}
