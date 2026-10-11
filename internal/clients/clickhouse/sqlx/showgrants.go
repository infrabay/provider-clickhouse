// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package sqlx

import (
	"fmt"
	"slices"
)

// WildcardObjects returns the database and table objects of wildcard grants
// and revokes in SHOW GRANTS output, one statement per line as read raw
// through the driver, e.g. "team_*.*" from "GRANT SELECT ON team_*.* TO r".
//
// Servers without system.grants.is_wildcard (before 26.8) store a wildcard
// grant (ON team_*.*, ON app.ev*) with its prefix as a plain name (database
// "team_", table "ev"), exactly like a grant on a database or table of that
// name, so system.grants cannot tell them apart (design 5.6, D-10). SHOW
// GRANTS prints wildcards unquoted and literal names that contain '*' quoted
// (`odd*name`.*), so a '*' token directly after a name, with no space in
// between (team_*.*, `team-a`*.*, app.ev*), is a wildcard.
//
// An object is the run of tokens without spaces after a top-level ON; the
// server prints objects without spaces, so a database named like a keyword
// (ON TO*.* TO r) is still read correctly. Role grants (GRANT r TO u) have no
// ON. Objects without a database part are grants of user and role DDL, named
// collections, table engines, definers or sources (ON *, ON svc_*): they
// cannot cover a database or a table, and Grant ignores their rows, which
// are of unsupported levels. Every other wildcard is reported.
//
// A line that cannot be read is an error, never "no wildcard".
func WildcardObjects(lines []string) ([]string, error) {
	var out []string
	for _, line := range lines {
		toks, err := Lex(line)
		if err != nil {
			return nil, fmt.Errorf("cannot read SHOW GRANTS output: %w", err)
		}
		depth := 0
		for i := 0; i < len(toks); i++ {
			switch t := toks[i]; {
			case t.IsPunct('('):
				depth++
			case t.IsPunct(')'):
				depth--
			case depth == 0 && t.IsWord("ON"):
				end := objectEnd(toks, i+1)
				if obj := toks[i+1 : end]; wildcardObject(obj) {
					s := line[obj[0].Pos:obj[len(obj)-1].End]
					if !slices.Contains(out, s) {
						out = append(out, s)
					}
				}
				i = end - 1
			}
		}
	}
	return out, nil
}

// objectEnd returns the index after the run of tokens without spaces that
// starts at from.
func objectEnd(toks []Token, from int) int {
	end := from
	for end < len(toks) && (end == from || toks[end].Adjacent(toks[end-1])) {
		end++
	}
	return end
}

// wildcardObject reports whether an object has a database part and a '*'
// directly after a name.
func wildcardObject(obj []Token) bool {
	dot, wildcard := false, false
	for i, t := range obj {
		switch {
		case t.IsPunct('.'):
			dot = true
		case t.IsPunct('*') && i > 0 && obj[i-1].IsIdent():
			wildcard = true
		}
	}
	return dot && wildcard
}
