// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package sqlx

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// tok is a compact token for comparisons: kind and value.
type tok struct {
	K TokenKind
	V string
}

func toks(ts []Token) []tok {
	out := make([]tok, 0, len(ts))
	for _, t := range ts {
		out = append(out, tok{t.Kind, t.Value})
	}
	return out
}

func w(v string) tok { return tok{TokenWord, v} }
func q(v string) tok { return tok{TokenQuotedIdent, v} }
func s(v string) tok { return tok{TokenString, v} }
func p(v string) tok { return tok{TokenPunct, v} }

// The inputs are server output read raw through the driver (the probes in
// stage 2 read them with FORMAT TSVRaw, which does not escape again).
func TestLexServerOutput(t *testing.T) {
	cases := map[string]struct {
		in   string
		want []tok
	}{
		"JWTUserAntalya": {
			in: `CREATE USER jwt1 IDENTIFIED WITH jwt PROCESSOR 'p\'1' CLAIMS '{"hd":"it\'s \\\\ ok"}'`,
			want: []tok{w("CREATE"), w("USER"), w("jwt1"), w("IDENTIFIED"), w("WITH"), w("jwt"),
				w("PROCESSOR"), s("p'1"), w("CLAIMS"), s(`{"hd":"it's \\ ok"}`)},
		},
		"QuotedNameAndHost": {
			in: "CREATE USER `a'b\\`c` IDENTIFIED WITH sha256_password HOST NAME 'h\\'1'",
			want: []tok{w("CREATE"), w("USER"), q("a'b`c"), w("IDENTIFIED"), w("WITH"), w("sha256_password"),
				w("HOST"), w("NAME"), s("h'1")},
		},
		// 25.3.3: VALID UNTIL is a per-method suffix followed by a comma and
		// the next method; SETTINGS PROFILE prints the profile as an
		// identifier, not as a string literal.
		"ValidUntilBetweenMethods": {
			in: "CREATE USER vu1 IDENTIFIED WITH sha256_password VALID UNTIL '2031-01-01 00:00:00', plaintext_password VALID UNTIL '2032-02-02 00:00:00' HOST IP '10.0.0.0/8' DEFAULT DATABASE db1 DEFAULT ROLE NONE SETTINGS PROFILE `default`",
			want: []tok{w("CREATE"), w("USER"), w("vu1"), w("IDENTIFIED"), w("WITH"), w("sha256_password"),
				w("VALID"), w("UNTIL"), s("2031-01-01 00:00:00"), p(","), w("plaintext_password"), w("VALID"), w("UNTIL"), s("2032-02-02 00:00:00"),
				w("HOST"), w("IP"), s("10.0.0.0/8"), w("DEFAULT"), w("DATABASE"), w("db1"), w("DEFAULT"), w("ROLE"), w("NONE"),
				w("SETTINGS"), w("PROFILE"), q("default")},
		},
		"HostRegexpEscapedBackslash": {
			in:   `HOST REGEXP '.*\\.example\\.com'`,
			want: []tok{w("HOST"), w("REGEXP"), s(`.*\.example\.com`)},
		},
		"Escapes": {
			in:   `'a\tb\nc\rd\0e\bf\fg\x41\q' "dq\"x" ` + "``",
			want: []tok{s("a\tb\nc\rd\x00e\bf\fgAq"), q(`dq"x`), q("")},
		},
		"DoubledQuotes": {
			in:   "'it''s' `a``b`",
			want: []tok{s("it's"), q("a`b")},
		},
		"UnicodeWord": {
			in:   "GRANT SELECT ON app.tëst TO ü",
			want: []tok{w("GRANT"), w("SELECT"), w("ON"), w("app"), p("."), w("tëst"), w("TO"), w("ü")},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := Lex(tc.in)
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tc.want, toks(got)); diff != "" {
				t.Errorf("-want +got:\n%s", diff)
			}
			for _, tk := range got {
				if tk.Pos < 0 || tk.End > len(tc.in) || tk.Pos >= tk.End {
					t.Errorf("bad offsets %+v", tk)
				}
			}
		})
	}
}

// wildcardAfterIdent reports whether a SHOW GRANTS line has an unquoted '*'
// directly after an identifier: the rule a scanner for wildcard grants needs
// on servers without system.grants.is_wildcard (design 5.6).
func wildcardAfterIdent(ts []Token) bool {
	for i := 1; i < len(ts); i++ {
		if ts[i].IsPunct('*') && ts[i].Adjacent(ts[i-1]) && ts[i-1].IsIdent() {
			return true
		}
	}
	return false
}

func TestLexWildcardAdjacency(t *testing.T) {
	cases := map[string]bool{
		"GRANT SELECT ON team_*.* TO gw":                true,
		"GRANT SELECT ON `team-a`*.* TO gw":             true,
		"GRANT SELECT ON app.ev* TO gw":                 true,
		"GRANT SELECT ON `odd*name`.* TO gw":            false,
		"GRANT SELECT ON `we*rd`.t TO `a'b\\`c`":        false,
		"GRANT SELECT ON *.* TO gw":                     false,
		"GRANT SELECT ON app.* TO gw":                   false,
		"GRANT CREATE USER ON svc_* TO gu":              true,
		"GRANT rr1 TO ra1 WITH ADMIN OPTION":            false,
		"GRANT SHOW USERS, SHOW ROLES ON *.* TO prov":   false,
		"GRANT SELECT ON `app`.`ev` * TO x":             false, // separated by a space
		"GRANT SELECT(a, `b*`) ON app.t TO `x*`":        false,
		"GRANT SELECT ON \"team\"*.* TO \"dq-grantee\"": true,
	}
	for in, want := range cases {
		ts, err := Lex(in)
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if got := wildcardAfterIdent(ts); got != want {
			t.Errorf("%s: wildcard = %t, want %t", in, got, want)
		}
	}
}

func TestLexErrors(t *testing.T) {
	for _, in := range []string{
		"'unterminated",
		"`unterminated",
		`"unterminated`,
		`'trailing backslash\`,
		`'\x4'`,
		`'\xZZ'`,
	} {
		if _, err := Lex(in); err == nil {
			t.Errorf("Lex(%q) = nil error", in)
		}
	}
}

func TestTokenHelpers(t *testing.T) {
	ts, err := Lex("identified WITH `x`,*")
	if err != nil {
		t.Fatal(err)
	}
	if !ts[0].IsWord("IDENTIFIED") || ts[0].IsWord("WITH") || ts[2].IsWord("x") {
		t.Error("IsWord compares bare words case-insensitively and never matches quoted identifiers")
	}
	if !ts[2].IsIdent() || !ts[3].IsPunct(',') || !ts[4].IsPunct('*') || !ts[4].Adjacent(ts[3]) || ts[1].Adjacent(ts[0]) {
		t.Errorf("helpers: %+v", ts)
	}
	for _, k := range []TokenKind{TokenWord, TokenQuotedIdent, TokenString, TokenPunct, 0} {
		if k.String() == "" || (k == 0 && !strings.HasPrefix(k.String(), "TokenKind(")) {
			t.Errorf("String() of %d = %q", int(k), k.String())
		}
	}
}
