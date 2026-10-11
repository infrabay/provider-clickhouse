// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

// Package reason defines the fixed set of reason tokens that prefix every
// error the provider reports on a managed resource, e.g.
// "NotOwned: Database \"app\" already exists (id ...)".
package reason

import (
	"errors"
	"fmt"
)

// A Reason classifies an error. The set is fixed and part of the user-facing
// contract: alerts and runbooks may match on it.
type Reason string

// Reasons.
const (
	NotOwned                   Reason = "NotOwned"
	Replaced                   Reason = "Replaced"
	AdoptionRejected           Reason = "AdoptionRejected"
	ReadOnlyStorage            Reason = "ReadOnlyStorage"
	SelfModification           Reason = "SelfModification"
	NameCollision              Reason = "NameCollision"
	AmbiguousGrantee           Reason = "AmbiguousGrantee"
	PolicyViolation            Reason = "PolicyViolation"
	InvalidConfig              Reason = "InvalidConfig"
	Unsupported                Reason = "Unsupported"
	DependencyMissing          Reason = "DependencyMissing"
	PermissionDenied           Reason = "PermissionDenied"
	AuthenticationFailed       Reason = "AuthenticationFailed"
	AccessStorageNotReplicated Reason = "AccessStorageNotReplicated"
	ExternalNameConflict       Reason = "ExternalNameConflict"
	DatabaseNotEmpty           Reason = "DatabaseNotEmpty"
	SecretConflict             Reason = "SecretConflict"
	AlreadyExists              Reason = "AlreadyExists"
	InvalidSpec                Reason = "InvalidSpec"
	Internal                   Reason = "Internal"
)

// Error is an error with a reason. Its message is "<Reason>: <cause>".
type Error struct {
	Reason Reason
	Err    error
}

func (e *Error) Error() string {
	if e.Err == nil {
		return string(e.Reason)
	}
	return fmt.Sprintf("%s: %s", e.Reason, e.Err.Error())
}

// Unwrap returns the cause.
func (e *Error) Unwrap() error { return e.Err }

// Wrap annotates err with a reason. It returns nil if err is nil. An error
// that already carries a reason is returned unchanged, so the innermost, most
// specific reason wins.
func Wrap(r Reason, err error) error {
	if err == nil {
		return nil
	}
	if _, ok := Of(err); ok {
		return err
	}
	return &Error{Reason: r, Err: err}
}

// Errorf returns a new error with a reason.
func Errorf(r Reason, format string, args ...any) error {
	return &Error{Reason: r, Err: fmt.Errorf(format, args...)}
}

// Of returns the reason of err, if any error in its chain carries one.
func Of(err error) (Reason, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e.Reason, true
	}
	return "", false
}

// Is reports whether err carries the reason r.
func Is(err error, r Reason) bool {
	got, ok := Of(err)
	return ok && got == r
}
