// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package clickhouse

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"

	ch "github.com/ClickHouse/clickhouse-go/v2"

	"github.com/infrabay/provider-clickhouse/internal/clients/clickhouse/auth"
	"github.com/infrabay/provider-clickhouse/internal/reason"
)

// ClickHouse error codes the provider branches on.
const (
	CodeBadArguments              int32 = 36
	CodeTableAlreadyExists        int32 = 57
	CodeSyntaxError               int32 = 62
	CodeUnknownDatabase           int32 = 81
	CodeDatabaseAlreadyExists     int32 = 82
	CodeTimeoutExceeded           int32 = 159
	CodeThereIsNoProfile          int32 = 180
	CodeUnknownUser               int32 = 192
	CodeBadQueryParameter         int32 = 457
	CodeCannotUnlink              int32 = 458
	CodeAccessEntityAlreadyExists int32 = 493
	CodeAccessStorageReadonly     int32 = 495
	CodeAccessDenied              int32 = 497
	CodeInvalidGrant              int32 = 509
	CodeUnknownRole               int32 = 511
	CodeSetNonGrantedRole         int32 = 512
	CodeAuthenticationFailed      int32 = 516
)

var codeNames = map[int32]string{
	CodeBadArguments:              "BAD_ARGUMENTS",
	CodeTableAlreadyExists:        "TABLE_ALREADY_EXISTS",
	CodeSyntaxError:               "SYNTAX_ERROR",
	CodeUnknownDatabase:           "UNKNOWN_DATABASE",
	CodeDatabaseAlreadyExists:     "DATABASE_ALREADY_EXISTS",
	CodeTimeoutExceeded:           "TIMEOUT_EXCEEDED",
	CodeThereIsNoProfile:          "THERE_IS_NO_PROFILE",
	CodeUnknownUser:               "UNKNOWN_USER",
	CodeBadQueryParameter:         "BAD_QUERY_PARAMETER",
	CodeCannotUnlink:              "CANNOT_UNLINK",
	CodeAccessEntityAlreadyExists: "ACCESS_ENTITY_ALREADY_EXISTS",
	CodeAccessStorageReadonly:     "ACCESS_STORAGE_READONLY",
	CodeAccessDenied:              "ACCESS_DENIED",
	CodeInvalidGrant:              "INVALID_GRANT",
	CodeUnknownRole:               "UNKNOWN_ROLE",
	CodeSetNonGrantedRole:         "SET_NON_GRANTED_ROLE",
	CodeAuthenticationFailed:      "AUTHENTICATION_FAILED",
}

const (
	maxServerMessage    = 1024
	maxTransportMessage = 256
)

// A ServerError is a ClickHouse exception. Only ClickHouse's own error text
// reaches a managed resource; it is redacted for statements that carry
// secrets.
type ServerError struct {
	Code    int32
	Name    string
	Message string
}

func (e *ServerError) Error() string {
	if e.Name != "" {
		return fmt.Sprintf("code %d (%s): %s", e.Code, e.Name, e.Message)
	}
	return fmt.Sprintf("code %d: %s", e.Code, e.Message)
}

// A TransportError is any error that is not a ClickHouse exception: dial,
// TLS, protocol or token fetch failures. Its message is a short category plus
// at most 256 characters of the cause, with any HTTP response body removed, so
// that a tenant-chosen endpoint cannot reflect arbitrary content into
// conditions and events. The full cause is kept for errors.Is and debug logs.
type TransportError struct {
	Category string
	Message  string
	cause    error
}

func (e *TransportError) Error() string {
	if e.Message == "" {
		return e.Category
	}
	return e.Category + ": " + e.Message
}

// Unwrap returns the full cause.
func (e *TransportError) Unwrap() error { return e.cause }

// Code returns the ClickHouse error code of err, if err is a ClickHouse
// exception.
func Code(err error) (int32, bool) {
	var se *ServerError
	if errors.As(err, &se) {
		return se.Code, true
	}
	var ex *ch.Exception
	if errors.As(err, &ex) {
		return ex.Code, true
	}
	return 0, false
}

// IsCode reports whether err is a ClickHouse exception with one of the codes.
func IsCode(err error, codes ...int32) bool {
	c, ok := Code(err)
	if !ok {
		return false
	}
	for _, want := range codes {
		if c == want {
			return true
		}
	}
	return false
}

// IsAuthenticationFailed reports whether err is error 516.
func IsAuthenticationFailed(err error) bool { return IsCode(err, CodeAuthenticationFailed) }

// IsUncertain reports whether a failed statement may still have run: a
// transport error, a timeout, EOF or 159 TIMEOUT_EXCEEDED.
func IsUncertain(err error) bool {
	if err == nil {
		return false
	}
	if c, ok := Code(err); ok {
		return c == CodeTimeoutExceeded
	}
	return true
}

// WithDefaultReason annotates err with the reason that its ClickHouse error
// code implies, unless it already carries one. Codes without a default
// reason, and transport errors, are returned unchanged.
func WithDefaultReason(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := reason.Of(err); ok {
		return err
	}
	c, ok := Code(err)
	if !ok {
		return err
	}
	switch c {
	case CodeAuthenticationFailed:
		return reason.Wrap(reason.AuthenticationFailed, err)
	case CodeAccessDenied:
		return reason.Wrap(reason.PermissionDenied, err)
	case CodeAccessStorageReadonly:
		return reason.Wrap(reason.ReadOnlyStorage, err)
	case CodeAccessEntityAlreadyExists, CodeDatabaseAlreadyExists:
		return reason.Wrap(reason.AlreadyExists, err)
	case CodeTableAlreadyExists, CodeInvalidGrant, CodeCannotUnlink:
		return reason.Wrap(reason.InvalidSpec, err)
	case CodeUnknownUser, CodeUnknownRole, CodeSetNonGrantedRole, CodeThereIsNoProfile:
		return reason.Wrap(reason.DependencyMissing, err)
	case CodeBadQueryParameter, CodeSyntaxError:
		return reason.Wrap(reason.Internal, err)
	}
	return err
}

// Sanitize converts an error returned by clickhouse-go into a *ServerError or
// a *TransportError. With redact, every single-quoted literal in a server
// message is replaced with '***': ClickHouse syntax errors quote the statement
// from the failure position onward, which would expose password hashes.
func Sanitize(err error, redact bool) error {
	if err == nil {
		return nil
	}
	switch err.(type) {
	case *ServerError, *TransportError, *reason.Error:
		// Already sanitised, or the provider's own error.
		return err
	}
	var ex *ch.Exception
	if errors.As(err, &ex) {
		name := ex.CodeName
		if name == "" {
			name = codeNames[ex.Code]
		}
		msg := ex.Message
		if redact {
			msg = RedactLiterals(msg)
		}
		return &ServerError{Code: ex.Code, Name: name, Message: truncate(strings.TrimSpace(msg), maxServerMessage)}
	}
	return &TransportError{Category: category(err), Message: transportMessage(err, redact), cause: err}
}

func category(err error) string {
	var (
		ae   *auth.Error
		ne   net.Error
		oe   *net.OpError
		ce   *tls.CertificateVerificationError
		rh   tls.RecordHeaderError
		ua   x509.UnknownAuthorityError
		hn   x509.HostnameError
		cert x509.CertificateInvalidError
	)
	switch {
	case errors.As(err, &ae):
		return "token source failed"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.As(err, &ce), errors.As(err, &rh), errors.As(err, &ua), errors.As(err, &hn), errors.As(err, &cert):
		return "TLS handshake failed"
	case errors.As(err, &oe) && oe.Op == "dial":
		return "dial failed"
	case errors.As(err, &ne) && ne.Timeout():
		return "timeout"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "connection closed"
	}
	return "connection error"
}

func transportMessage(err error, redact bool) string {
	msg := err.Error()
	// clickhouse-go reports non-200 HTTP responses as `... response body: "..."`.
	for _, marker := range []string{"response body:", "body:"} {
		if i := strings.Index(strings.ToLower(msg), marker); i >= 0 {
			msg = strings.TrimSpace(msg[:i]) + " (response body removed)"
			break
		}
	}
	if redact {
		msg = RedactLiterals(msg)
	}
	return truncate(msg, maxTransportMessage)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// Cut on a rune boundary.
	for n > 0 && !isRuneStart(s[n]) {
		n--
	}
	return s[:n] + "..."
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

// RedactLiterals replaces every single-quoted literal in s with '***'.
// Backslash escapes inside literals are honoured, so an escaped quote does not
// end the literal. An unterminated literal is redacted to the end of s.
func RedactLiterals(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	in := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !in {
			b.WriteByte(c)
			if c == '\'' {
				in = true
			}
			continue
		}
		switch c {
		case '\\':
			i++ // skip the escaped byte
		case '\'':
			b.WriteString("***'")
			in = false
		}
	}
	if in {
		b.WriteString("***")
	}
	return b.String()
}

// carriesSecret reports whether a statement may carry secret literals, so
// that its errors must be redacted.
func carriesSecret(stmt string) bool {
	return strings.Contains(strings.ToUpper(stmt), "IDENTIFIED")
}
