// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package clickhouse

import "strings"

var (
	identEscaper  = strings.NewReplacer(`\`, `\\`, "`", "\\`")
	stringEscaper = strings.NewReplacer(`\`, `\\`, `'`, `\'`, "\x00", `\0`)
	paramEscaper  = strings.NewReplacer(`\`, `\\`, "\t", `\t`, "\n", `\n`, "\r", `\r`, "\x00", `\0`)
)

// QuoteIdent quotes an identifier (database, table, column, user or role
// name) in backticks. A quoted identifier is always literal: a '*' inside it
// is not a wildcard. Names are never split on '@' or '.'.
func QuoteIdent(s string) string { return "`" + identEscaper.Replace(s) + "`" }

// QuoteString quotes a string literal in single quotes. It escapes the same
// set of characters as clickhouse-go's client-side binder.
func QuoteString(s string) string { return "'" + stringEscaper.Replace(s) + "'" }

// ParamValue encodes a value for a server-side query parameter. The server
// parses parameter values in the escaped (TSV) text format, so TAB, newline
// and backslash sequences must be escaped to round-trip.
func ParamValue(s string) string { return paramEscaper.Replace(s) }
