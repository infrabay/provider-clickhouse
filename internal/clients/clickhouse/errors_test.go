// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package clickhouse

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"

	ch "github.com/ClickHouse/clickhouse-go/v2"

	"github.com/infrabay/provider-clickhouse/internal/clients/clickhouse/auth"
	"github.com/infrabay/provider-clickhouse/internal/reason"
)

func TestRedactLiterals(t *testing.T) {
	cases := map[string]struct {
		in, want string
	}{
		"None":         {"no literals here", "no literals here"},
		"One":          {"BY 'secret' SALT", "BY '***' SALT"},
		"Two":          {"'a' and 'b'", "'***' and '***'"},
		"EscapedQuote": {`BY 'se\'cret' x`, "BY '***' x"},
		"EscapedSlash": {`BY 'a\\' x 'b'`, "BY '***' x '***'"},
		"Unterminated": {"near 'abc", "near '***"},
		// The server's messages, read on every supported version: 497 for a
		// statement that carries IDENTIFIED keeps the missing grant, and a
		// syntax error keeps no literal of the statement it quotes.
		"MissingGrant": {"provider-clickhouse: Not enough privileges. To execute this query, it's necessary to have the grant CREATE USER ON zz_u",
			"provider-clickhouse: Not enough privileges. To execute this query, it's necessary to have the grant CREATE USER ON zz_u"},
		"MissingGrantHint": {"it's necessary to have the grant SELECT, ALTER UPDATE ON app.* WITH GRANT OPTION. (Missing permissions: ALTER UPDATE ON app.*). You can try to use the `GRANT CURRENT GRANTS(...)` statement",
			"it's necessary to have the grant SELECT, ALTER UPDATE ON app.* WITH GRANT OPTION. (Missing permissions: ALTER UPDATE ON app.*). You can try to use the `GRANT CURRENT GRANTS(...)` statement"},
		"SyntaxErrorAtLiteral": {"Syntax error: failed at position 48 ('6257310c'): '6257310c' SALT '00112233'. Expected one of: BY, end of query",
			"Syntax error: failed at position 48 ('***'): '***' SALT '***'. Expected one of: BY, end of query"},
		"SyntaxErrorAfter": {"Syntax error: failed at position 118 (SALTY): SALTY '00112233'. Expected one of: SALT, VALID UNTIL",
			"Syntax error: failed at position 118 (SALTY): SALTY '***'. Expected one of: SALT, VALID UNTIL"},
		// An apostrophe in a backtick-quoted name is no quote: taking it for
		// one would print the hash and the salt that follow.
		"QuoteInIdentifier": {"failed at position 13 (`o'neil`): `o'neil` IDENTIFIED WITH sha256_hash BY '6257310c' SALT '00112233'",
			"failed at position 13 (`o'neil`): `o'neil` IDENTIFIED WITH sha256_hash BY '***' SALT '***'"},
		"EscapedBacktick":    {"`a\\`'b` BY 'x'", "`a\\`'b` BY '***'"},
		"UnclosedBacktick":   {"near `abc BY 'x'", "near `abc BY '***'"},
		"ContractionAtEnd":   {"BY 'x' it's", "BY '***' it's"},
		"Contractions":       {"doesn't, you're, it'll: 'x'", "doesn't, you're, it'll: '***'"},
		"QuoteAfterLetter":   {"BY'x' y", "BY'***' y"},
		"QuoteAfterLetterS":  {"BY'sx' y", "BY'***' y"},
		"QuoteBeforeLetter":  {"BY 'x'y", "BY '***'y"},
		"LiteralWithLetters": {"BY 'abc' SALT 'def'", "BY '***' SALT '***'"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := RedactLiterals(tc.in); got != tc.want {
				t.Errorf("RedactLiterals(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSanitizeServerError(t *testing.T) {
	hash := "6257310c34f98d6e96c672db8cee44e0077f4e03eb3581a7260ccd6861848188"
	ex := &ch.Exception{Code: 62, Message: "Syntax error: failed at position 40 (BY '" + hash + "' SALT '00112233'): expected ..."}

	err := Sanitize(fmt.Errorf("wrapped: %w", ex), true)
	var se *ServerError
	if !errors.As(err, &se) {
		t.Fatalf("Sanitize() = %T, want *ServerError", err)
	}
	if se.Code != 62 || se.Name != "SYNTAX_ERROR" {
		t.Errorf("code/name = %d/%q, want 62/SYNTAX_ERROR", se.Code, se.Name)
	}
	if strings.Contains(err.Error(), hash) || strings.Contains(err.Error(), "00112233") {
		t.Errorf("Sanitize(redact) leaked a literal: %s", err)
	}
	if c, ok := Code(err); !ok || c != 62 {
		t.Errorf("Code() = %d, %t", c, ok)
	}

	plain := Sanitize(ex, false)
	if !strings.Contains(plain.Error(), hash) {
		t.Errorf("Sanitize(no redact) should keep the server message, got %s", plain)
	}
}

func TestSanitizeTransportError(t *testing.T) {
	body := strings.Repeat("x", 1000)
	err := Sanitize(fmt.Errorf("sendQuery: [HTTP 502] response body: %q", body), false)
	var te *TransportError
	if !errors.As(err, &te) {
		t.Fatalf("Sanitize() = %T, want *TransportError", err)
	}
	if strings.Contains(err.Error(), "xxxx") {
		t.Errorf("response body not removed: %s", err)
	}
	if len(te.Message) > maxTransportMessage+3 {
		t.Errorf("message too long: %d", len(te.Message))
	}

	long := Sanitize(errors.New(strings.Repeat("y", 1000)), false)
	if got := len(long.(*TransportError).Message); got > maxTransportMessage+3 {
		t.Errorf("message not truncated: %d", got)
	}

	dial := Sanitize(&net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}, false)
	if !strings.HasPrefix(dial.Error(), "dial failed") {
		t.Errorf("dial error category: %s", dial)
	}

	canceled := Sanitize(fmt.Errorf("x: %w", context.Canceled), false)
	if !errors.Is(canceled, context.Canceled) {
		t.Errorf("Sanitize() must keep the cause for errors.Is")
	}

	tok := Sanitize(fmt.Errorf("failed to get JWT: %w", &auth.Error{Source: "TokenFile", Err: reason.Errorf(reason.InvalidConfig, "token expired")}), false)
	if !strings.HasPrefix(tok.Error(), "token source failed") {
		t.Errorf("token error category: %s", tok)
	}
	if r, ok := reason.Of(tok); !ok || r != reason.InvalidConfig {
		t.Errorf("token error reason = %q, %t; want InvalidConfig", r, ok)
	}
}

func TestWithDefaultReason(t *testing.T) {
	cases := map[int32]reason.Reason{
		516: reason.AuthenticationFailed,
		497: reason.PermissionDenied,
		495: reason.ReadOnlyStorage,
		493: reason.AlreadyExists,
		82:  reason.AlreadyExists,
		57:  reason.InvalidSpec,
		509: reason.InvalidSpec,
		458: reason.InvalidSpec,
		192: reason.DependencyMissing,
		511: reason.DependencyMissing,
		512: reason.DependencyMissing,
		180: reason.DependencyMissing,
		457: reason.Internal,
		62:  reason.Internal,
	}
	for code, want := range cases {
		err := WithDefaultReason(&ServerError{Code: code, Message: "m"})
		if got, _ := reason.Of(err); got != want {
			t.Errorf("code %d: reason %q, want %q", code, got, want)
		}
		if !strings.HasPrefix(err.Error(), string(want)+": ") {
			t.Errorf("code %d: message %q does not start with the reason", code, err)
		}
	}
	if _, ok := reason.Of(WithDefaultReason(&ServerError{Code: 81})); ok {
		t.Errorf("code 81 should have no default reason")
	}
	already := reason.Errorf(reason.NotOwned, "x")
	if got := WithDefaultReason(already); got != already {
		t.Errorf("an error with a reason must be returned unchanged")
	}
	if WithDefaultReason(nil) != nil {
		t.Errorf("nil must stay nil")
	}
}

func TestIsUncertain(t *testing.T) {
	if IsUncertain(&ServerError{Code: 493}) {
		t.Error("a server rejection is certain")
	}
	if !IsUncertain(&ServerError{Code: 159}) {
		t.Error("159 TIMEOUT_EXCEEDED is uncertain")
	}
	if !IsUncertain(&TransportError{Category: "timeout"}) {
		t.Error("transport errors are uncertain")
	}
}

func TestCarriesSecret(t *testing.T) {
	if !carriesSecret("ALTER USER `a` IDENTIFIED WITH sha256_hash BY 'x'") {
		t.Error("IDENTIFIED statements carry secrets")
	}
	if carriesSecret("CREATE DATABASE `a`") {
		t.Error("CREATE DATABASE carries no secret")
	}
}
