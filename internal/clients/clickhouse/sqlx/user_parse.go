// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package sqlx

import (
	"errors"
	"fmt"
)

// A UserDefinition is the parsed output of SHOW CREATE USER: the user's name
// and its authentication methods (the IDENTIFIED clause). The other clauses
// are read structurally from system.users. The processor and claims of a jwt
// user, and on servers without system.users.valid_until (25.3, 25.8) the
// expiry of every method, are visible only here.
type UserDefinition struct {
	Name    string
	Methods []AuthMethod
}

// An AuthMethod is one method of the IDENTIFIED WITH list.
type AuthMethod struct {
	// Name is the method as printed, e.g. jwt, sha256_password or
	// no_password. A method the provider does not know is reported as is.
	Name string
	// Processor and Claims are the PROCESSOR and CLAIMS literals of a jwt
	// method, "" when absent. The server rejects an empty processor and
	// non-object claims, so "" never stands for a printed value.
	Processor string
	Claims    string
	// ValidUntil is the VALID UNTIL literal of the method, "" when it does
	// not expire.
	ValidUntil string
}

// Expires reports whether any method has VALID UNTIL. An expiry set out of
// band locks the user out once it passes, while every column of system.users
// looks unchanged.
func (s UserDefinition) Expires() bool {
	for _, m := range s.Methods {
		if m.ValidUntil != "" {
			return true
		}
	}
	return false
}

// MethodNames returns the names of the methods, in order.
func (s UserDefinition) MethodNames() []string {
	out := make([]string, 0, len(s.Methods))
	for _, m := range s.Methods {
		out = append(out, m.Name)
	}
	return out
}

// methodListEnd reports whether t ends the IDENTIFIED WITH list: the next
// clause (HOST, DEFAULT DATABASE, DEFAULT ROLE, SETTINGS, GRANTEES) or a
// storage (IN). VALID does not end it: VALID UNTIL is a suffix of each
// method.
func methodListEnd(t Token) bool {
	for _, w := range []string{"HOST", "DEFAULT", "SETTINGS", "GRANTEES", "IN"} {
		if t.IsWord(w) {
			return true
		}
	}
	return false
}

// ParseShowCreateUser parses the output of SHOW CREATE USER, read raw
// through the driver (clickhouse-client's TSV output escapes again). The
// IDENTIFIED clause follows this grammar:
//
//	identified := IDENTIFIED WITH list | NOT IDENTIFIED
//	list       := method { ',' method }
//	method     := name [PROCESSOR lit] [CLAIMS lit] [BY lit] [SALT lit] [VALID UNTIL lit]
//
// VALID UNTIL is a suffix of each method and may be followed by a comma and
// the next method, for example "IDENTIFIED WITH sha256_password VALID UNTIL
// '2031-01-01 00:00:00', plaintext_password HOST ...". The list ends at a
// top-level HOST, DEFAULT, SETTINGS, GRANTEES or IN, or at the end of the
// input. BY and SALT literals appear only when the server is configured to
// display secrets; they are skipped and never kept. Parameters of methods
// the provider does not manage (SERVER, CN, KEY, ...) are skipped too; a
// comma followed by a literal continues such a parameter, a comma followed
// by a word starts the next method.
//
// Errors never quote the input, which may hold a password hash.
func ParseShowCreateUser(stmt string) (UserDefinition, error) {
	toks, err := Lex(stmt)
	if err != nil {
		return UserDefinition{}, fmt.Errorf("cannot parse SHOW CREATE USER output: %w", err)
	}
	if len(toks) < 3 || !toks[0].IsWord("CREATE") || !toks[1].IsWord("USER") || !toks[2].IsIdent() {
		return UserDefinition{}, errors.New("cannot parse SHOW CREATE USER output: it does not start with CREATE USER <name>")
	}
	out := UserDefinition{Name: toks[2].Value}
	i := 3
	for ; i < len(toks); i++ {
		t := toks[i]
		if methodListEnd(t) {
			// No IDENTIFIED clause.
			return out, nil
		}
		if t.IsWord("NOT") && i+1 < len(toks) && toks[i+1].IsWord("IDENTIFIED") {
			out.Methods = []AuthMethod{{Name: AuthNoPassword}}
			return out, nil
		}
		if t.IsWord("IDENTIFIED") {
			break
		}
	}
	if i >= len(toks) {
		return out, nil
	}
	i++ // IDENTIFIED
	named := i < len(toks) && toks[i].IsWord("WITH")
	if named {
		i++
	}
	out.Methods = parseMethods(toks[i:], named)
	return out, nil
}

// parseMethods parses the method list of an IDENTIFIED clause, up to the
// next clause. Without WITH (IDENTIFIED BY '...', the server's default
// method) the first method has no name.
func parseMethods(toks []Token, named bool) []AuthMethod {
	var out []AuthMethod
	lit := func(i int) (string, bool) {
		if i < len(toks) && toks[i].Kind == TokenString {
			return toks[i].Value, true
		}
		return "", false
	}
	i := 0
	for i < len(toks) && !methodListEnd(toks[i]) {
		var m AuthMethod
		if named && toks[i].Kind == TokenWord {
			m.Name = toks[i].Value
			i++
		}
		named = true
	method:
		for i < len(toks) {
			t := toks[i]
			switch {
			case methodListEnd(t):
				break method
			case t.IsPunct(','):
				i++
				if _, ok := lit(i); ok {
					// A list of literals continues the parameter.
					continue
				}
				break method
			case t.IsWord("PROCESSOR"):
				if v, ok := lit(i + 1); ok {
					m.Processor = v
					i += 2
					continue
				}
				i++
			case t.IsWord("CLAIMS"):
				if v, ok := lit(i + 1); ok {
					m.Claims = v
					i += 2
					continue
				}
				i++
			case t.IsWord("VALID") && i+1 < len(toks) && toks[i+1].IsWord("UNTIL"):
				if v, ok := lit(i + 2); ok {
					m.ValidUntil = v
					i += 3
					continue
				}
				i += 2
			default:
				// BY, SALT and the parameters of other methods, with their
				// literals: skipped.
				i++
			}
		}
		out = append(out, m)
	}
	return out
}
