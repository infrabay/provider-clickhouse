// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package user

import (
	"context"
	"fmt"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/crossplane/crossplane-runtime/v2/pkg/reconciler/managed"

	"github.com/infrabay/provider-clickhouse/apis/v1alpha1"
	"github.com/infrabay/provider-clickhouse/internal/clients/clickhouse/sqlx"
	"github.com/infrabay/provider-clickhouse/internal/reason"
)

// The connection Secret of a password user is the record of the password
// that was last applied: the server never shows the hash, and status can be
// lost. It is read with the uncached API reader (right after Create
// published it the reconciler requeues at once, and a stale cache would
// report it missing and trigger a needless new password), and it is trusted
// only when its controller reference is the User (design 5.4, 9.5).

// connectionSecret reads the connection Secret of cr. It returns nil when
// cr has no writeConnectionSecretToRef or the Secret does not exist. A
// Secret that exists but is not controlled by cr is SecretConflict: it is
// never the record of a password, and no password is applied for it.
func (e *External) connectionSecret(ctx context.Context, cr *v1alpha1.User) (*corev1.Secret, error) {
	ref := cr.GetWriteConnectionSecretToReference()
	if ref == nil {
		return nil, nil
	}
	if e.APIReader == nil {
		return nil, reason.Errorf(reason.Internal, "no uncached API reader to read the connection Secret")
	}
	key := types.NamespacedName{Namespace: cr.GetNamespace(), Name: ref.Name}
	s := &corev1.Secret{}
	err := e.APIReader.Get(ctx, key, s)
	if kerrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("cannot get connection Secret %s: %w", key, err)
	}
	if c := metav1.GetControllerOf(s); c == nil || c.UID != cr.GetUID() {
		return nil, reason.Errorf(reason.SecretConflict,
			"connection Secret %s exists and is not controlled by this User; it is never treated as the record of a password: delete it, or set another writeConnectionSecretToRef", key)
	}
	return s, nil
}

// referencedPassword reads the password a passwordSecretRef selects, with
// the uncached API reader. A missing Secret or key is DependencyMissing; an
// empty value is InvalidSpec, because a user with an empty password lets
// anyone log in.
func (e *External) referencedPassword(ctx context.Context, cr *v1alpha1.User) (string, error) {
	ref := cr.Spec.ForProvider.Authentication.Password.PasswordSecretRef
	if e.APIReader == nil {
		return "", reason.Errorf(reason.Internal, "no uncached API reader to read the password Secret")
	}
	key := types.NamespacedName{Namespace: cr.GetNamespace(), Name: ref.Name}
	s := &corev1.Secret{}
	if err := e.APIReader.Get(ctx, key, s); err != nil {
		if kerrors.IsNotFound(err) {
			return "", reason.Errorf(reason.DependencyMissing, "password Secret %s does not exist", key)
		}
		return "", fmt.Errorf("cannot get password Secret %s: %w", key, err)
	}
	v, ok := s.Data[ref.Key]
	if !ok {
		return "", reason.Errorf(reason.DependencyMissing, "password Secret %s has no key %q", key, ref.Key)
	}
	if len(v) == 0 {
		return "", reason.Errorf(reason.InvalidSpec, "key %q of password Secret %s is empty", ref.Key, key)
	}
	return string(v), nil
}

// A passwordChange says why the password of a password user is applied.
type passwordChange int

const (
	// passwordKept: the recorded password stays. It is applied again only
	// to repair the authentication method or an expiry.
	passwordKept passwordChange = iota
	// passwordRotated: the passwordSecretRef value or generate.rotationToken
	// changed (Normal event PasswordRotated).
	passwordRotated
	// passwordRegenerated: generate, and the connection Secret or its
	// password key is missing; a new password is generated as recovery
	// (Warning event PasswordRegenerated).
	passwordRegenerated
	// passwordUnrecorded: passwordSecretRef, and the connection Secret or its
	// password key is missing; the desired password is applied and
	// published again.
	passwordUnrecorded
)

// passwordState is the password aspect of a password user, from its spec
// and its connection Secret.
type passwordState struct {
	// secret is the data of the connection Secret, nil when it does not
	// exist.
	secret map[string][]byte
	// recorded and recordedToken are the password and rotation token the
	// connection Secret records, nil when absent.
	recorded      *string
	recordedToken *string
	// next is the password to apply with the IDENTIFIED clause: the
	// passwordSecretRef value, or for generate the recorded password; nil
	// means a new random password of length characters.
	next   *string
	length int
	// token is generate.rotationToken; "" when unset.
	token  string
	change passwordChange
}

// recordOf reads the password and rotation token that the connection
// Secret records. An empty value counts as absent.
func recordOf(s *corev1.Secret) (data map[string][]byte, password, token *string) {
	if s == nil {
		return nil, nil, nil
	}
	if v := s.Data[v1alpha1.ConnectionKeyPassword]; len(v) > 0 {
		password = new(string(v))
	}
	if v := s.Data[v1alpha1.ConnectionKeyRotationToken]; len(v) > 0 {
		token = new(string(v))
	}
	return s.Data, password, token
}

// passwordStateOf decides the password aspect of a password user (design
// 5.4):
//
//   - passwordSecretRef: the desired password is the Secret's value. It is
//     applied when it differs from the recorded one (rotated), or when no
//     password is recorded (unrecorded);
//   - generate: without a recorded password a new one is generated
//     (regenerated). With generate.rotationToken set and different from the
//     recorded rotation token a new one is generated too (rotated).
//     Otherwise the recorded password is kept.
//
// The Secret is read with the uncached API reader and trusted only when
// controlled by cr (SecretConflict otherwise).
func (e *External) passwordStateOf(ctx context.Context, cr *v1alpha1.User) (passwordState, error) {
	sec, err := e.connectionSecret(ctx, cr)
	if err != nil {
		return passwordState{}, err
	}
	var ps passwordState
	ps.secret, ps.recorded, ps.recordedToken = recordOf(sec)
	pw := cr.Spec.ForProvider.Authentication.Password
	if pw.PasswordSecretRef != nil {
		want, err := e.referencedPassword(ctx, cr)
		if err != nil {
			return passwordState{}, err
		}
		ps.next = &want
		switch {
		case ps.recorded == nil:
			ps.change = passwordUnrecorded
		case *ps.recorded != want:
			ps.change = passwordRotated
		}
		return ps, nil
	}
	ps.length = sqlx.DefaultPasswordLength
	if g := pw.Generate; g != nil {
		if g.Length != nil {
			ps.length = int(*g.Length)
		}
		if g.RotationToken != nil {
			ps.token = *g.RotationToken
		}
	}
	switch {
	case ps.recorded == nil:
		ps.change = passwordRegenerated
	case ps.token != "" && (ps.recordedToken == nil || *ps.recordedToken != ps.token):
		ps.change = passwordRotated
	default:
		ps.next = ps.recorded
	}
	return ps, nil
}

// createPassword returns the password state for CREATE USER. The desired
// password of a passwordSecretRef is used as is. A generated password reuses
// the recorded one when the user is re-created (dropped out of band) and no
// rotation is pending, so that clients keep working; otherwise a new one is
// generated.
func (e *External) createPassword(ctx context.Context, cr *v1alpha1.User) (passwordState, error) {
	ps, err := e.passwordStateOf(ctx, cr)
	if err != nil {
		return passwordState{}, err
	}
	ps.change = passwordKept
	return ps, nil
}

// apply returns the password to apply and its hash with a fresh salt.
func (ps passwordState) apply() (string, sqlx.PasswordHash, error) {
	pw := ""
	if ps.next != nil {
		pw = *ps.next
	} else {
		var err error
		if pw, err = sqlx.GeneratePassword(ps.length); err != nil {
			return "", sqlx.PasswordHash{}, reason.Wrap(reason.Internal, err)
		}
	}
	h, err := sqlx.NewPasswordHash(pw)
	if err != nil {
		return "", sqlx.PasswordHash{}, reason.Wrap(reason.Internal, err)
	}
	return pw, h, nil
}

// changed reports whether applying ps changes the password.
func (ps passwordState) changed() bool { return ps.change != passwordKept }

// tokenAfterApply returns the rotation token to publish together with a
// password that was just applied: generate.rotationToken, or nil when it is
// unset.
func (ps passwordState) tokenAfterApply() *string {
	if ps.token == "" {
		return nil
	}
	return new(ps.token)
}

// connection are the connection details of a user before the stale keys
// are removed.
type connection struct {
	name string
	// processor is set for a jwt user.
	processor string
	// password and token are published for a password user, when known.
	password *string
	token    *string
	// secret is the data of the current connection Secret, when it was read.
	// Keys of the other authentication method that it still holds are
	// removed.
	secret map[string][]byte
	// keepStale keeps those keys as they are instead: the switch to the
	// other method has not been applied yet, so the old credentials still
	// work. Update removes them once the ALTER USER succeeded.
	keepStale bool
}

// details returns the connection details of a user (design 9.5): username,
// endpoint, port, protocol and tls, plus jwtProcessor for a jwt user, or
// password and (when generate.rotationToken is set) rotationToken for a
// password user. Keys that the current Secret still holds but that no
// longer apply, such as the password of a user switched to jwt, are set to
// nil: crossplane-runtime publishes the details with a JSON merge patch, in
// which a null value removes the key. Keys the Secret does not hold are
// never set to nil, so that publishing the details of an unchanged user
// changes nothing.
func (e *External) details(c connection) managed.ConnectionDetails {
	cd := managed.ConnectionDetails{v1alpha1.ConnectionKeyUsername: []byte(c.name)}
	if e.Resolved != nil {
		ep := e.Resolved.Endpoint
		cd[v1alpha1.ConnectionKeyEndpoint] = []byte(ep.Host)
		cd[v1alpha1.ConnectionKeyPort] = []byte(strconv.Itoa(ep.Port))
		cd[v1alpha1.ConnectionKeyProtocol] = []byte(ep.Protocol)
		cd[v1alpha1.ConnectionKeyTLS] = []byte(strconv.FormatBool(ep.TLS))
	}
	if c.processor != "" {
		cd[v1alpha1.ConnectionKeyJWTProcessor] = []byte(c.processor)
	}
	if c.password != nil {
		cd[v1alpha1.ConnectionKeyPassword] = []byte(*c.password)
	}
	if c.token != nil {
		cd[v1alpha1.ConnectionKeyRotationToken] = []byte(*c.token)
	}
	for _, k := range []string{v1alpha1.ConnectionKeyPassword, v1alpha1.ConnectionKeyRotationToken, v1alpha1.ConnectionKeyJWTProcessor} {
		if _, set := cd[k]; set {
			continue
		}
		if v, held := c.secret[k]; held {
			if c.keepStale {
				cd[k] = v
				continue
			}
			cd[k] = nil
		}
	}
	return cd
}
