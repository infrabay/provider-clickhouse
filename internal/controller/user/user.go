// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

// Package user contains the User controller (design 3.5, 5.4, 9.5).
//
// A User is a ClickHouse SQL user with a password, stored as a salted
// SHA-256 hash, or with JWT authentication pinned to a token processor
// (Altinity Antalya). The provider identity can alter any SQL user, so the
// controller writes only users it created or adopted by UUID (the shared
// ownership flow of package access), never a user from users.xml/users.d,
// never its own identity, and never a user whose name the
// ClusterProviderConfig policy does not allow, including while the MR is
// being deleted.
package user

import (
	"context"
	"errors"
	"fmt"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/crossplane/crossplane-runtime/v2/pkg/controller"
	"github.com/crossplane/crossplane-runtime/v2/pkg/event"
	"github.com/crossplane/crossplane-runtime/v2/pkg/meta"
	"github.com/crossplane/crossplane-runtime/v2/pkg/reconciler/managed"
	"github.com/crossplane/crossplane-runtime/v2/pkg/resource"

	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"

	"github.com/infrabay/provider-clickhouse/apis/v1alpha1"
	ch "github.com/infrabay/provider-clickhouse/internal/clients/clickhouse"
	"github.com/infrabay/provider-clickhouse/internal/clients/clickhouse/sqlx"
	"github.com/infrabay/provider-clickhouse/internal/controller/access"
	"github.com/infrabay/provider-clickhouse/internal/controller/ownership"
	"github.com/infrabay/provider-clickhouse/internal/reason"
)

const kind = "User"

// Event reasons of the User controller (design 5.2). The shared ones
// (Adopted, CreateRecovered, Orphaned, OrphanedByPolicy) are in package
// access.
const (
	// ReasonPasswordRotated: a new password was applied because the value of
	// the passwordSecretRef Secret or generate.rotationToken changed.
	ReasonPasswordRotated event.Reason = "PasswordRotated"
	// ReasonPasswordRegenerated: the connection Secret, the record of the
	// password that was last applied, or its password key was missing, so a
	// new password was generated as recovery. Clients holding the old
	// password can no longer log in.
	ReasonPasswordRegenerated event.Reason = "PasswordRegenerated"
	// ReasonDefaultRoleNotGranted: Listed default roles that are not granted
	// to the user are left out of its default roles until they are granted.
	ReasonDefaultRoleNotGranted event.Reason = "DefaultRoleNotGranted"
)

// SetupGated registers the User controller with safe-start: it is set up
// once the User CRD is installed (and activated).
func SetupGated(mgr ctrl.Manager, o controller.Options, d access.Dependencies) error {
	access.SetupGated(o, v1alpha1.UserGroupVersionKind, func() error { return Setup(mgr, o, d) })
	return nil
}

// Setup adds the User controller.
func Setup(mgr ctrl.Manager, o controller.Options, d access.Dependencies) error {
	return access.Setup(mgr, o, access.Controller[*v1alpha1.User]{
		GroupKind: v1alpha1.UserGroupKind,
		GVK:       v1alpha1.UserGroupVersionKind,
		Object:    &v1alpha1.User{},
		List:      &v1alpha1.UserList{},
		NewConnector: func(r event.Recorder) managed.TypedExternalConnector[*v1alpha1.User] {
			return access.NewConnector(mgr, d, kind, r, NewExternal, nil)
		},
		// The external name and the ownership claim (owner-uid; Create
		// records owned-id after reading the UUID back).
		Initializers: []managed.Initializer{ownership.NewInitializer(mgr.GetClient(), forProviderName)},
	})
}

func forProviderName(mg resource.Managed) *string {
	if u, ok := mg.(*v1alpha1.User); ok {
		return u.Spec.ForProvider.Name
	}
	return nil
}

// NewExternal returns the external client of a User for one reconcile.
func NewExternal(env *access.Env) managed.TypedExternalClient[*v1alpha1.User] {
	return &External{Env: env}
}

// External manages one User through a ClickHouse client. Disconnect, which
// releases the client, comes with the embedded Env.
type External struct {
	*access.Env

	// plan is what the last Observe decided to change.
	plan *plan
}

func (e *External) record(cr *v1alpha1.User, ev event.Event) {
	if e.Recorder != nil {
		e.Recorder.Event(cr, ev)
	}
}

// checkWrites runs the write checks of a user (design 5.8) before anything
// is written: on every config kind the jwt processor must be a name a
// server can configure, and on a ClusterProviderConfig the method, the
// processor and the default roles must be allowed. A jwt user also needs a
// server that supports jwt users (Unsupported otherwise, and no SQL is
// sent). A failed check makes the MR unavailable.
func (e *External) checkWrites(ctx context.Context, cr *v1alpha1.User, d desired) error {
	if err := e.Policy().CheckUserWrite(cr.GetNamespace(), d.userWrite()); err != nil {
		return access.Unavailable(cr, err)
	}
	if d.jwt == nil {
		return nil
	}
	ok, err := e.Client.Capabilities().JWTUsers(ctx, e.Client)
	if err != nil {
		return err
	}
	if !ok {
		return access.Unavailable(cr, reason.Errorf(reason.Unsupported,
			"server %s does not support JWT users (requires Altinity Antalya)", e.Client.Info().Version))
	}
	return nil
}

// requireRecord refuses a password user without writeConnectionSecretToRef
// (CEL refuses it too): its connection Secret is the record of the password
// that was last applied, and without one every reconcile would apply a new
// password.
func requireRecord(cr *v1alpha1.User, d desired) error {
	if d.password != nil && cr.GetWriteConnectionSecretToReference() == nil {
		return reason.Errorf(reason.InvalidSpec, "password users require writeConnectionSecretToRef: the connection Secret records the password that was last applied")
	}
	return nil
}

// execError classifies the error of a CREATE USER or ALTER USER: 36
// BAD_ARGUMENTS is a configuration error (for example claims the server
// refuses), everything else gets the reason its code implies. Server errors
// of statements with an IDENTIFIED clause arrive with every literal
// redacted.
func execError(err error) error {
	if ch.IsCode(err, ch.CodeBadArguments) {
		return reason.Wrap(reason.InvalidSpec, err)
	}
	return ch.WithDefaultReason(err)
}

// Observe reads the user and decides ownership, drift and the connection
// details (design 5.4).
//
// While the MR is being deleted, Observe reports the user as existing only
// when it is the MR's own (owned, adopted or recovered), its name is allowed
// by the ClusterProviderConfig policy and the management policies include
// Delete. In every other case it emits a Warning event and reports it as
// absent: the MR goes away, no SQL write runs and the user is left in
// place.
func (e *External) Observe(ctx context.Context, cr *v1alpha1.User) (managed.ExternalObservation, error) {
	e.plan = nil
	pol := ownership.PoliciesOf(cr)
	if meta.WasDeleted(cr) && !pol.Delete() {
		// crossplane-runtime releases such an MR without calling Delete; this
		// second guard does not depend on it (D-26).
		return e.LeaveInPlace(cr, access.ReasonOrphaned, errors.New("the management policies do not include Delete; the user is left in place")), nil
	}
	ctx = access.SQLContext(ctx, kind, cr)
	obs, err := e.ObserveEntity(ctx, cr, access.User, cr.Spec.ForProvider.Name)
	cr.Status.AtProvider = v1alpha1.UserObservation{ID: obs.Entity.ID, Storage: obs.Entity.Storage}
	if err != nil {
		return managed.ExternalObservation{}, err
	}
	if !obs.Exists {
		return managed.ExternalObservation{ResourceExists: false}, nil
	}
	if meta.WasDeleted(cr) {
		return managed.ExternalObservation{ResourceExists: true}, nil
	}

	d, err := desiredOf(cr.Spec.ForProvider)
	if err != nil {
		return managed.ExternalObservation{}, err
	}
	mutates := pol.Create() || pol.Update()
	if mutates {
		if err := requireRecord(cr, d); err != nil {
			return managed.ExternalObservation{}, err
		}
		if err := e.checkWrites(ctx, cr, d); err != nil {
			return managed.ExternalObservation{}, err
		}
	}
	o, err := e.read(ctx, cr, obs.Name, obs.Entity.ID, d)
	if err != nil {
		return managed.ExternalObservation{}, err
	}
	cr.Status.AtProvider = o.status(obs.Entity)
	lateInit := lateInitialize(cr, o)
	if lateInit {
		if d, err = desiredOf(cr.Spec.ForProvider); err != nil {
			return managed.ExternalObservation{}, err
		}
	}
	p, err := e.planOf(ctx, cr, obs.Name, d, o, mutates)
	if err != nil {
		return managed.ExternalObservation{}, err
	}
	e.plan = p
	return managed.ExternalObservation{
		ResourceExists:          true,
		ResourceUpToDate:        p.upToDate(),
		ResourceLateInitialized: lateInit,
		ConnectionDetails:       e.details(p.conn),
	}, nil
}

// Create runs CREATE USER (design 5.4). The write checks, the policy, the
// provider's own identity, the topology guard and the name collision check
// run first; ownership is recorded by reading the UUID back
// (access.Env.CreateEntity). It returns the connection details, including
// the password of a password user.
func (e *External) Create(ctx context.Context, cr *v1alpha1.User) (managed.ExternalCreation, error) {
	name, err := ownership.EffectiveName(cr, cr.Spec.ForProvider.Name)
	if err != nil {
		return managed.ExternalCreation{}, err
	}
	ctx = access.SQLContext(ctx, kind, cr)
	d, err := desiredOf(cr.Spec.ForProvider)
	if err != nil {
		return managed.ExternalCreation{}, err
	}
	if err := requireRecord(cr, d); err != nil {
		return managed.ExternalCreation{}, err
	}
	if err := e.checkWrites(ctx, cr, d); err != nil {
		return managed.ExternalCreation{}, err
	}
	c := sqlx.CreateUser{
		Name:            name,
		Hosts:           d.hosts,
		DefaultRole:     d.createDefaultRole(),
		DefaultDatabase: d.defaultDatabase,
		SettingsProfile: d.settingsProfile,
	}
	conn := connection{name: name}
	if d.jwt != nil {
		c.Auth.JWT = d.jwt
		conn.processor = d.jwt.Processor
		sec, err := e.connectionSecret(ctx, cr)
		if err != nil {
			return managed.ExternalCreation{}, access.Unavailable(cr, err)
		}
		if sec != nil {
			conn.secret = sec.Data
		}
	} else {
		ps, err := e.createPassword(ctx, cr)
		if err != nil {
			if reason.Is(err, reason.SecretConflict) {
				return managed.ExternalCreation{}, access.Unavailable(cr, err)
			}
			return managed.ExternalCreation{}, err
		}
		pw, h, err := ps.apply()
		if err != nil {
			return managed.ExternalCreation{}, err
		}
		c.Auth.Password = &h
		conn.password, conn.token, conn.secret = &pw, ps.tokenAfterApply(), ps.secret
	}
	stmt, err := sqlx.CreateUserStmt(c)
	if err != nil {
		return managed.ExternalCreation{}, reason.Wrap(reason.InvalidSpec, err)
	}
	if err := e.CreateEntity(ctx, cr, access.User, name, stmt); err != nil {
		return managed.ExternalCreation{}, execError(err)
	}
	return managed.ExternalCreation{ConnectionDetails: e.details(conn)}, nil
}

// Update applies the plan of the last Observe, in at most two statements:
//
//	ALTER USER `name` [IDENTIFIED WITH ...] [HOST ...] [DEFAULT DATABASE ...] [SETTINGS ...]
//	ALTER USER `name` DEFAULT ROLE ...
//
// Ownership is re-checked immediately before each (access.Env.VerifyOwned:
// claim, name policy, own identity, users.xml, topology and UUID). DEFAULT
// ROLE has its own statement so that an error in it does not block the
// other changes. When the first statement changed the password, the second
// is left to the next reconcile: crossplane-runtime publishes the
// connection details only after Update succeeds, so the new password must
// never wait for a statement that may fail.
func (e *External) Update(ctx context.Context, cr *v1alpha1.User) (managed.ExternalUpdate, error) {
	p := e.plan
	if p == nil {
		return managed.ExternalUpdate{}, reason.Errorf(reason.Internal, "Update ran without Observe")
	}
	ctx = access.SQLContext(ctx, kind, cr)
	conn := p.conn
	changedPassword := false
	alter := sqlx.AlterUser{Name: p.name, Hosts: p.hosts, DefaultDatabase: p.defaultDatabase, SettingsProfile: p.settingsProfile}
	var applied string
	switch {
	case p.auth && p.jwt != nil:
		alter.Auth = &sqlx.UserAuth{JWT: p.jwt}
	case p.auth && p.pw != nil:
		pw, h, err := p.pw.apply()
		if err != nil {
			return managed.ExternalUpdate{}, err
		}
		alter.Auth = &sqlx.UserAuth{Password: &h}
		applied = pw
	case p.auth:
		// A password user whose connection Secret was not read: the
		// management policies allow neither Create nor Update.
		return managed.ExternalUpdate{}, reason.Errorf(reason.Internal, "the password of user %q cannot be applied without its connection Secret", p.name)
	}
	if alter.Auth != nil || alter.Hosts != nil || alter.DefaultDatabase != nil || alter.SettingsProfile != nil {
		stmt, err := sqlx.AlterUserStmt(alter)
		if err != nil {
			return managed.ExternalUpdate{}, reason.Wrap(reason.InvalidSpec, err)
		}
		ok, err := e.VerifyOwned(ctx, cr, access.User, p.name)
		if err != nil || !ok {
			// Not ok: the user disappeared since Observe; the next Observe
			// decides.
			return managed.ExternalUpdate{}, err
		}
		if err := e.Client.Exec(ctx, stmt); err != nil {
			return managed.ExternalUpdate{}, execError(err)
		}
		if alter.Auth != nil {
			// The method is applied: keys of the other method go.
			conn.keepStale = false
		}
		if alter.Auth != nil && alter.Auth.Password != nil {
			conn.password, conn.token = &applied, p.pw.tokenAfterApply()
			changedPassword = p.pw.changed()
			e.passwordEvent(cr, p)
		}
	}
	if p.defaultRole != nil && !changedPassword {
		ok, err := e.VerifyOwned(ctx, cr, access.User, p.name)
		if err != nil || !ok {
			return managed.ExternalUpdate{}, err
		}
		if err := e.Client.Exec(ctx, sqlx.AlterUserDefaultRoleStmt(p.name, *p.defaultRole)); err != nil {
			return managed.ExternalUpdate{}, execError(err)
		}
	}
	return managed.ExternalUpdate{ConnectionDetails: e.details(conn)}, nil
}

// passwordEvent records why a new password was applied.
func (e *External) passwordEvent(cr *v1alpha1.User, p *plan) {
	switch p.pw.change {
	case passwordRotated:
		e.record(cr, event.Normal(ReasonPasswordRotated, fmt.Sprintf("Applied a new password to user %q", p.name)))
	case passwordRegenerated:
		e.record(cr, event.Warning(ReasonPasswordRegenerated, fmt.Errorf(
			"the connection Secret held no password for user %q; a new password was generated and published, and the previous one no longer works", p.name)))
	case passwordKept, passwordUnrecorded:
	}
}

// Delete drops the user. It is only reached for the MR's own user: while
// the MR is being deleted Observe reports every other case as absent.
// Ownership is re-checked immediately before the DROP.
func (e *External) Delete(ctx context.Context, cr *v1alpha1.User) (managed.ExternalDelete, error) {
	cr.SetConditions(xpv2.Deleting())
	if !ownership.PoliciesOf(cr).Delete() {
		// Observe never reports the user as existing then; nothing is
		// dropped (D-26).
		return managed.ExternalDelete{}, nil
	}
	name, err := ownership.EffectiveName(cr, cr.Spec.ForProvider.Name)
	if err != nil {
		return managed.ExternalDelete{}, err
	}
	ctx = access.SQLContext(ctx, kind, cr)
	ok, err := e.VerifyOwned(ctx, cr, access.User, name)
	if err != nil || !ok {
		// Not ok: already gone.
		return managed.ExternalDelete{}, err
	}
	return managed.ExternalDelete{}, ch.WithDefaultReason(access.IgnoreGone(e.Client.Exec(ctx, sqlx.DropUserStmt(name))))
}

// Interface check.
var _ managed.TypedExternalClient[*v1alpha1.User] = &External{}
