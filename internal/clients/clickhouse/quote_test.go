// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package clickhouse

import "testing"

func TestQuoteIdent(t *testing.T) {
	cases := map[string]struct {
		in, want string
	}{
		"Plain":             {"app", "`app`"},
		"Email":             {"svc@my-project.iam.gserviceaccount.com", "`svc@my-project.iam.gserviceaccount.com`"},
		"Backtick":          {"a`b", "`a\\`b`"},
		"Backslash":         {`a\b`, "`a\\\\b`"},
		"Quote":             {"a'b", "`a'b`"},
		"Star":              {"team_*", "`team_*`"},
		"Unicode":           {"дані", "`дані`"},
		"BackslashTLiteral": {`a\t`, "`a\\\\t`"},
		"InjectionAttempt":  {"x` ; DROP DATABASE y; --", "`x\\` ; DROP DATABASE y; --`"},
		"Placeholders":      {"?@x$1", "`?@x$1`"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := QuoteIdent(tc.in); got != tc.want {
				t.Errorf("QuoteIdent(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestQuoteString(t *testing.T) {
	cases := map[string]struct {
		in, want string
	}{
		"Plain":     {"abc", "'abc'"},
		"Quote":     {"it's", `'it\'s'`},
		"Backslash": {`a\b`, `'a\\b'`},
		"NUL":       {"a\x00b", `'a\0b'`},
		"Backtick":  {"a`b", "'a`b'"},
		"Breakout":  {`x\' OR 1`, `'x\\\' OR 1'`},
		"TABkept":   {"a\tb", "'a\tb'"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := QuoteString(tc.in); got != tc.want {
				t.Errorf("QuoteString(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestParamValue(t *testing.T) {
	cases := map[string]struct {
		in, want string
	}{
		"Plain":             {"app@example.com", "app@example.com"},
		"TAB":               {"a\tb", `a\tb`},
		"Newline":           {"a\nb", `a\nb`},
		"CR":                {"a\rb", `a\rb`},
		"NUL":               {"a\x00b", `a\0b`},
		"Backslash":         {`a\b`, `a\\b`},
		"BackslashTLiteral": {`a\t`, `a\\t`},
		"QuoteUntouched":    {"a'b`c", "a'b`c"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := ParamValue(tc.in); got != tc.want {
				t.Errorf("ParamValue(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
