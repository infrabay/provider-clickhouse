// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package sqlx

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// A TokenKind classifies a token of the SQL that the server prints in
// SHOW CREATE USER and SHOW GRANTS output.
type TokenKind int

// Token kinds.
const (
	// TokenWord is a bare identifier, keyword or number: a run of ASCII
	// letters, digits, '_' and non-ASCII bytes.
	TokenWord TokenKind = iota + 1
	// TokenQuotedIdent is an identifier in backticks (or double quotes).
	TokenQuotedIdent
	// TokenString is a single-quoted string literal.
	TokenString
	// TokenPunct is any other single byte, such as , . * ( ) =
	TokenPunct
)

func (k TokenKind) String() string {
	switch k {
	case TokenWord:
		return "word"
	case TokenQuotedIdent:
		return "quoted identifier"
	case TokenString:
		return "string literal"
	case TokenPunct:
		return "punctuation"
	}
	return "TokenKind(" + strconv.Itoa(int(k)) + ")"
}

// A Token is one token of the input.
type Token struct {
	Kind TokenKind
	// Value is the token's value: a word or punctuation as written, a quoted
	// identifier or string literal with its quotes removed and its escape
	// sequences decoded.
	Value string
	// Pos is the byte offset of the token's first byte in the input, and End
	// the offset just after its last byte.
	Pos, End int
}

// Adjacent reports whether t starts exactly where prev ends, with no
// whitespace in between. SHOW GRANTS prints a wildcard grant target as an
// identifier directly followed by '*' (team_*.*, `team-a`*.*, app.ev*),
// while a literal '*' in a name is always inside backticks.
func (t Token) Adjacent(prev Token) bool { return t.Pos == prev.End }

// IsWord reports whether t is the bare word w, compared case-insensitively,
// as keywords are.
func (t Token) IsWord(w string) bool { return t.Kind == TokenWord && strings.EqualFold(t.Value, w) }

// IsPunct reports whether t is the punctuation byte c.
func (t Token) IsPunct(c byte) bool {
	return t.Kind == TokenPunct && len(t.Value) == 1 && t.Value[0] == c
}

// IsIdent reports whether t names something: a bare word or a quoted
// identifier.
func (t Token) IsIdent() bool { return t.Kind == TokenWord || t.Kind == TokenQuotedIdent }

// Lex splits SQL text into tokens. Whitespace separates tokens and is
// dropped. In quoted identifiers and string literals the escape sequences
// \\ \' \` \" \0 \t \n \r \b \f and \xHH are decoded, any other escaped byte
// stands for itself, and a doubled quote stands for one quote.
//
// Lex is meant for statements the server printed (read raw through the
// driver, never through clickhouse-client, whose TSV output escapes again).
// It does not understand comments, and it fails on an unterminated quote.
func Lex(s string) ([]Token, error) {
	var out []Token
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v':
			i++
		case c == '\'' || c == '`' || c == '"':
			v, n, err := readQuoted(s[i:], c)
			if err != nil {
				return nil, fmt.Errorf("at byte %d: %w", i, err)
			}
			kind := TokenQuotedIdent
			if c == '\'' {
				kind = TokenString
			}
			out = append(out, Token{Kind: kind, Value: v, Pos: i, End: i + n})
			i += n
		case wordByte(c):
			j := i + 1
			for j < len(s) && wordByte(s[j]) {
				j++
			}
			out = append(out, Token{Kind: TokenWord, Value: s[i:j], Pos: i, End: j})
			i = j
		default:
			out = append(out, Token{Kind: TokenPunct, Value: s[i : i+1], Pos: i, End: i + 1})
			i++
		}
	}
	return out, nil
}

func wordByte(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c >= 0x80
}

// readQuoted reads a quoted token that starts at s[0] (the quote byte) and
// returns its decoded value and its length in bytes, including both quotes.
func readQuoted(s string, quote byte) (string, int, error) {
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		c := s[i]
		switch c {
		case quote:
			if i+1 < len(s) && s[i+1] == quote {
				// A doubled quote stands for one quote.
				b.WriteByte(quote)
				i++
				continue
			}
			return b.String(), i + 1, nil
		case '\\':
			i++
			if i >= len(s) {
				return "", 0, errors.New("unterminated escape")
			}
			switch s[i] {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case '0':
				b.WriteByte(0)
			case 'b':
				b.WriteByte('\b')
			case 'f':
				b.WriteByte('\f')
			case 'x':
				if i+2 >= len(s) {
					return "", 0, errors.New("short \\x escape")
				}
				v, err := strconv.ParseUint(s[i+1:i+3], 16, 8)
				if err != nil {
					return "", 0, errors.New("invalid \\x escape")
				}
				b.WriteByte(byte(v))
				i += 2
			default:
				b.WriteByte(s[i])
			}
		default:
			b.WriteByte(c)
		}
	}
	if quote == '\'' {
		return "", 0, errors.New("unterminated string literal")
	}
	return "", 0, errors.New("unterminated quoted identifier")
}
