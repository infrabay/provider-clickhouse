// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

// Package rolegrant contains the RoleGrant controller (design 3.8, 4.3, 5.7).
//
// A RoleGrant owns one membership: the role memberOf granted to one grantee,
// a user or a role, with or without the admin option. It has no UUID of its
// own, so every write is bound to its grantee (D-30): the provider grants to
// and revokes from a grantee only when a User or Role MR in the same
// namespace, using the same ProviderConfig, owns it, and it re-checks that
// binding, the grantee's UUID and the topology immediately before every
// GRANT and REVOKE. The grantee and memberOf must be allowed by the
// ClusterProviderConfig policy (also while deleting, D-25); memberOf needs no
// owner and may be a role from users.xml/users.d that policy.grantableRoles
// lists. Deleting a RoleGrant revokes exactly its membership and nothing
// else; the grantee's other memberships, its privileges and its default
// roles are never changed (use User.defaultRoles for those).
package rolegrant

import (
	"context"
	"errors"
	"fmt"

	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/crossplane/crossplane-runtime/v2/pkg/controller"
	"github.com/crossplane/crossplane-runtime/v2/pkg/event"
	"github.com/crossplane/crossplane-runtime/v2/pkg/meta"
	"github.com/crossplane/crossplane-runtime/v2/pkg/reconciler/managed"
	"github.com/crossplane/crossplane-runtime/v2/pkg/resource"

	"github.com/infrabay/provider-clickhouse/apis/v1alpha1"
	ch "github.com/infrabay/provider-clickhouse/internal/clients/clickhouse"
	"github.com/infrabay/provider-clickhouse/internal/clients/clickhouse/sqlx"
	"github.com/infrabay/provider-clickhouse/internal/controller/access"
	"github.com/infrabay/provider-clickhouse/internal/controller/ownership"
	"github.com/infrabay/provider-clickhouse/internal/controller/providerconfig"
	"github.com/infrabay/provider-clickhouse/internal/reason"
)

const kind = "RoleGrant"

// SetupGated registers the RoleGrant controller with safe-start: it is set up
// once the RoleGrant CRD is installed (and activated).
func SetupGated(mgr ctrl.Manager, o controller.Options, d access.Dependencies) error {
	access.SetupGated(o, v1alpha1.RoleGrantGroupVersionKind, func() error { return Setup(mgr, o, d) })
	return nil
}

// Setup adds the RoleGrant controller. A RoleGrant has no external name and
// no initializer. References are resolved by the generated resolver, wrapped
// so that a cross-namespace reference is rejected before anything is read;
// the connector checks that referenced MRs use the same ProviderConfig.
func Setup(mgr ctrl.Manager, o controller.Options, d access.Dependencies) error {
	return access.Setup(mgr, o, access.Controller[*v1alpha1.RoleGrant]{
		GroupKind: v1alpha1.RoleGrantGroupKind,
		GVK:       v1alpha1.RoleGrantGroupVersionKind,
		Object:    &v1alpha1.RoleGrant{},
		List:      &v1alpha1.RoleGrantList{},
		NewConnector: func(r event.Recorder) managed.TypedExternalConnector[*v1alpha1.RoleGrant] {
			return access.NewConnector(mgr, d, kind, r, NewExternal, References)
		},
		References: providerconfig.NewReferenceResolver(mgr.GetClient(), ReferencesOf),
	})
}

// References returns the references of a RoleGrant: memberOfRef and
// memberOfSelector (a Role), userRef and userSelector (a User), roleRef and
// roleSelector (a Role).
func References(cr *v1alpha1.RoleGrant) []providerconfig.Reference {
	p := cr.Spec.ForProvider
	return []providerconfig.Reference{
		{Field: "memberOfRef", Ref: p.MemberOfRef, Selector: p.MemberOfSelector, New: func() resource.ModernManaged { return &v1alpha1.Role{} }},
		{Field: "userRef", Ref: p.UserRef, Selector: p.UserSelector, New: func() resource.ModernManaged { return &v1alpha1.User{} }},
		{Field: "roleRef", Ref: p.RoleRef, Selector: p.RoleSelector, New: func() resource.ModernManaged { return &v1alpha1.Role{} }},
	}
}

// ReferencesOf returns References of mg if it is a RoleGrant, and nil
// otherwise.
func ReferencesOf(mg resource.Managed) []providerconfig.Reference {
	if cr, ok := mg.(*v1alpha1.RoleGrant); ok {
		return References(cr)
	}
	return nil
}

// NewExternal returns the external client of a RoleGrant around env.
func NewExternal(env *access.Env) managed.TypedExternalClient[*v1alpha1.RoleGrant] {
	return &External{Env: env}
}

// External manages one RoleGrant through a ClickHouse client. The embedded
// Env provides Disconnect. An External lives for one reconcile: Observe
// records the grantee binding that Create, Update and Delete re-check before
// they write.
type External struct {
	*access.Env

	observed *observation
}

// desired is the membership a RoleGrant asks for.
type desired struct {
	memberOf    string
	grantee     access.Grantee
	adminOption bool
}

// observation is what Observe found for a bound grantee.
type observation struct {
	desired desired
	binding access.Binding
}

// desiredOf returns the membership of cr from its resolved spec. An
// unresolved memberOf or grantee is DependencyMissing. A role granted to
// itself is InvalidSpec: 26.8 refuses it, older servers store a useless
// self-membership.
func desiredOf(cr *v1alpha1.RoleGrant) (desired, error) {
	p := cr.Spec.ForProvider
	if p.MemberOf == nil || *p.MemberOf == "" {
		return desired{}, reason.Errorf(reason.DependencyMissing, "memberOf is not resolved yet (memberOf, memberOfRef or memberOfSelector)")
	}
	g, err := access.GranteeFrom(p.User, p.Role)
	if err != nil {
		return desired{}, err
	}
	if g.Kind == access.Role && g.Name == *p.MemberOf {
		return desired{}, reason.Errorf(reason.InvalidSpec, "role %q cannot be granted to itself", g.Name)
	}
	return desired{memberOf: *p.MemberOf, grantee: g, adminOption: p.AdminOption != nil && *p.AdminOption}, nil
}

// Observe reads the membership (design 5.7):
//
//  1. resolve memberOf and the grantee from the spec;
//  2. check the grantee against userNamePatterns or roleNamePatterns and
//     memberOf against grantableRoles, for every management policy and also
//     while deleting (D-25);
//  3. when the management policies allow Create or Update, check that
//     adminOption is allowed (allowAdminOption);
//  4. bind the grantee (access.BindGrantee): not shadowed by a same-named
//     user, and when writes are allowed not from users.xml, not the
//     provider's own identity and owned by a User or Role MR of the
//     namespace (D-30);
//  5. read memberOf from system.roles; it may be a role from users.xml;
//  6. read the membership's row from system.role_grants.
//
// The RoleGrant exists when the row exists, and is up to date when its admin
// option is the desired one. While the MR is being deleted, every failed
// check, an unowned grantee and management policies without Delete orphan
// the MR: a Warning event is emitted, nothing is revoked and the MR goes
// away. A grantee or memberOf that no longer exists leaves nothing to
// revoke.
func (e *External) Observe(ctx context.Context, cr *v1alpha1.RoleGrant) (managed.ExternalObservation, error) {
	deleting := meta.WasDeleted(cr)
	pol := ownership.PoliciesOf(cr)
	e.observed = nil

	d, err := desiredOf(cr)
	if err != nil {
		if deleting {
			// Create never ran without a resolved memberOf and grantee.
			return e.LeaveInPlace(cr, access.ReasonOrphaned, fmt.Errorf("%w; nothing was revoked", err)), nil
		}
		return managed.ExternalObservation{}, access.Unavailable(cr, err)
	}
	cr.Status.AtProvider = v1alpha1.RoleGrantObservation{Grantee: d.grantee.String()}

	if err := e.Policy().CheckRoleGrantNames(cr.GetNamespace(), d.grantee.Kind.GranteeKind(), d.grantee.Name, d.memberOf); err != nil {
		if deleting {
			return e.LeaveInPlace(cr, access.ReasonOrphanedByPolicy, fmt.Errorf("%w; the membership is left in place", err)), nil
		}
		return managed.ExternalObservation{}, access.Unavailable(cr, err)
	}
	if !deleting && (pol.Create() || pol.Update()) {
		if err := e.Policy().CheckRoleGrantWrite(d.adminOption); err != nil {
			return managed.ExternalObservation{}, access.Unavailable(cr, err)
		}
	}

	ctx = access.SQLContext(ctx, kind, cr)
	b, err := e.BindGrantee(ctx, cr, d.grantee)
	if err != nil {
		return managed.ExternalObservation{}, err
	}
	if b.Orphaned || !b.Exists {
		return managed.ExternalObservation{ResourceExists: false}, nil
	}

	_, exists, err := access.ReadEntity(ctx, e.Client, access.Role, d.memberOf)
	if err != nil {
		return managed.ExternalObservation{}, err
	}
	if !exists {
		if !deleting && pol.Writes() {
			return managed.ExternalObservation{}, access.Unavailable(cr, reason.Errorf(reason.DependencyMissing,
				"role %q (memberOf) does not exist; create it first, for example with a Role", d.memberOf))
		}
		// Dropping a role removes it from every grantee: nothing to revoke.
		return managed.ExternalObservation{ResourceExists: false}, nil
	}

	var adminOption, isDefault uint8
	err = ch.QueryRow(ctx, e.Client, sqlx.QueryRoleGrant(d.grantee.Kind == access.Role),
		ch.Params{"memberOf": d.memberOf, "grantee": d.grantee.Name}, &adminOption, &isDefault)
	if err != nil && !errors.Is(err, ch.ErrNoRows) {
		return managed.ExternalObservation{}, ch.WithDefaultReason(err)
	}
	e.observed = &observation{desired: d, binding: b}
	if errors.Is(err, ch.ErrNoRows) {
		return managed.ExternalObservation{ResourceExists: false}, nil
	}
	cr.Status.AtProvider.AdminOption = adminOption == 1
	cr.Status.AtProvider.IsDefault = isDefault == 1
	if deleting {
		return managed.ExternalObservation{ResourceExists: true}, nil
	}
	cr.SetConditions(xpv2.Available())
	return managed.ExternalObservation{ResourceExists: true, ResourceUpToDate: (adminOption == 1) == d.adminOption}, nil
}

// bound returns the desired membership of cr and the grantee binding that
// Observe recorded for it in this reconcile, for a write that the management
// policies allow (allowed is false otherwise). Nothing is written without a
// binding to an owned grantee: Observe binds an owner only when the
// management policies allow writes, so this also refuses writes for an
// observe-only MR whatever crossplane-runtime does (D-26).
func (e *External) bound(cr *v1alpha1.RoleGrant, allowed bool, action string) (desired, access.Binding, error) {
	if !allowed {
		return desired{}, access.Binding{}, reason.Errorf(reason.Internal, "the management policies do not include %s; nothing was granted or revoked", action)
	}
	d, err := desiredOf(cr)
	if err != nil {
		return desired{}, access.Binding{}, err
	}
	o := e.observed
	if o == nil || o.desired.memberOf != d.memberOf || o.desired.grantee != d.grantee {
		return desired{}, access.Binding{}, reason.Errorf(reason.Internal, "the grantee of this RoleGrant was not bound by Observe; nothing was granted or revoked")
	}
	if o.binding.Owner == "" {
		return desired{}, access.Binding{}, reason.Errorf(reason.NotOwned, "grantee %s is not bound to an owning User or Role; nothing was granted or revoked", d.grantee)
	}
	return d, o.binding, nil
}

// rebind re-checks the grantee binding immediately before a GRANT or REVOKE
// in Create and Update (access.RebindGrantee): the grantee could have been
// dropped, replaced, shadowed by a same-named user or released by its owner
// since Observe.
func (e *External) rebind(ctx context.Context, cr *v1alpha1.RoleGrant, b access.Binding) error {
	ok, err := e.RebindGrantee(ctx, cr, b)
	if err != nil {
		return err
	}
	if !ok {
		return reason.Errorf(reason.DependencyMissing, "grantee %s no longer exists; nothing was granted", b.Grantee)
	}
	return nil
}

// Create grants memberOf to the grantee, with the admin option if desired.
// The policy write check and the grantee binding are re-checked first. 511
// (an unknown role or grantee) is DependencyMissing.
func (e *External) Create(ctx context.Context, cr *v1alpha1.RoleGrant) (managed.ExternalCreation, error) {
	d, b, err := e.bound(cr, ownership.PoliciesOf(cr).Create(), "Create")
	if err != nil {
		return managed.ExternalCreation{}, err
	}
	if err := e.Policy().CheckRoleGrantWrite(d.adminOption); err != nil {
		return managed.ExternalCreation{}, access.Unavailable(cr, err)
	}
	ctx = access.SQLContext(ctx, kind, cr)
	if err := e.rebind(ctx, cr, b); err != nil {
		return managed.ExternalCreation{}, err
	}
	return managed.ExternalCreation{}, ch.WithDefaultReason(e.Client.Exec(ctx, sqlx.GrantRoleStmt(d.memberOf, d.grantee.Name, d.adminOption)))
}

// Update changes the admin option, the only mutable aspect: GRANT ... WITH
// ADMIN OPTION adds it, REVOKE ADMIN OPTION FOR removes it and keeps the
// membership (a plain GRANT would keep an existing admin option). Both
// statements are idempotent. The policy write check and the grantee binding
// are re-checked first.
func (e *External) Update(ctx context.Context, cr *v1alpha1.RoleGrant) (managed.ExternalUpdate, error) {
	d, b, err := e.bound(cr, ownership.PoliciesOf(cr).Update(), "Update")
	if err != nil {
		return managed.ExternalUpdate{}, err
	}
	if err := e.Policy().CheckRoleGrantWrite(d.adminOption); err != nil {
		return managed.ExternalUpdate{}, access.Unavailable(cr, err)
	}
	ctx = access.SQLContext(ctx, kind, cr)
	if err := e.rebind(ctx, cr, b); err != nil {
		return managed.ExternalUpdate{}, err
	}
	stmt := sqlx.RevokeAdminOptionStmt(d.memberOf, d.grantee.Name)
	if d.adminOption {
		stmt = sqlx.GrantRoleStmt(d.memberOf, d.grantee.Name, true)
	}
	return managed.ExternalUpdate{}, ch.WithDefaultReason(e.Client.Exec(ctx, stmt))
}

// Delete revokes the membership, with its admin option, and nothing else. It
// is only reached when Observe bound an owned grantee and found the
// membership. Immediately before the REVOKE it re-checks the names against
// the policy (D-25) and the grantee binding; a grantee that is no longer
// bound is not touched, and one that no longer exists leaves nothing to
// revoke. 192 and 511 count as already gone. The server also removes the
// role from the grantee's default role list (except on 25.3, where a stale
// entry stays; User.defaultRoles converges either way).
func (e *External) Delete(ctx context.Context, cr *v1alpha1.RoleGrant) (managed.ExternalDelete, error) {
	cr.SetConditions(xpv2.Deleting())
	if !ownership.PoliciesOf(cr).Delete() {
		// Observe already left the membership in place (D-26).
		return managed.ExternalDelete{}, nil
	}
	d, b, err := e.bound(cr, true, "Delete")
	if err != nil {
		return managed.ExternalDelete{}, err
	}
	if err := e.Policy().CheckRoleGrantNames(cr.GetNamespace(), d.grantee.Kind.GranteeKind(), d.grantee.Name, d.memberOf); err != nil {
		return managed.ExternalDelete{}, err
	}
	ctx = access.SQLContext(ctx, kind, cr)
	ok, err := e.RebindGrantee(ctx, cr, b)
	if err != nil || !ok {
		return managed.ExternalDelete{}, err
	}
	return managed.ExternalDelete{}, ch.WithDefaultReason(access.IgnoreGone(e.Client.Exec(ctx, sqlx.RevokeRoleStmt(d.memberOf, d.grantee.Name))))
}

// Interface check.
var _ managed.TypedExternalClient[*v1alpha1.RoleGrant] = &External{}
