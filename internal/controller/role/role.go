// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

// Package role contains the Role controller (design 3.6, 5.5).
//
// A Role MR creates and drops one SQL role. Ownership follows design 4.2, as
// for User: the claim is bound to the MR's UID, the UUID is read back after
// CREATE ROLE, a lost response is recovered only through the create journal
// of this process (D-29), and every DROP is preceded by a re-read of the
// UUID. Roles from users.xml/users.d are never written, a role never shares
// its name with a user (the server resolves grantees user-first), and a name
// outside the ClusterProviderConfig policy is never read or dropped, also
// while the MR is being deleted (D-25).
package role

import (
	"context"
	"errors"

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
	"github.com/infrabay/provider-clickhouse/internal/reason"
)

const kind = "Role"

// SetupGated registers the Role controller with safe-start: it is set up once
// the Role CRD is installed (and activated).
func SetupGated(mgr ctrl.Manager, o controller.Options, d access.Dependencies) error {
	access.SetupGated(o, v1alpha1.RoleGroupVersionKind, func() error { return Setup(mgr, o, d) })
	return nil
}

// Setup adds the Role controller. Its initializer sets the external name and
// writes the ownership claim (owner-uid) before Create; Create records the
// role's UUID (owned-id).
func Setup(mgr ctrl.Manager, o controller.Options, d access.Dependencies) error {
	return access.Setup(mgr, o, access.Controller[*v1alpha1.Role]{
		GroupKind: v1alpha1.RoleGroupKind,
		GVK:       v1alpha1.RoleGroupVersionKind,
		Object:    &v1alpha1.Role{},
		List:      &v1alpha1.RoleList{},
		NewConnector: func(r event.Recorder) managed.TypedExternalConnector[*v1alpha1.Role] {
			return access.NewConnector(mgr, d, kind, r, NewExternal, nil)
		},
		Initializers: []managed.Initializer{ownership.NewInitializer(mgr.GetClient(), forProviderName)},
	})
}

func forProviderName(mg resource.Managed) *string {
	if r, ok := mg.(*v1alpha1.Role); ok {
		return r.Spec.ForProvider.Name
	}
	return nil
}

// NewExternal returns the external client of a Role around env.
func NewExternal(env *access.Env) managed.TypedExternalClient[*v1alpha1.Role] {
	return &External{Env: env}
}

// External manages one Role through a ClickHouse client. The embedded Env
// provides Disconnect.
type External struct {
	*access.Env
}

// Observe reads the role and decides ownership (access.ObserveEntity): the
// name policy (also while deleting), the role's row in system.roles, the
// users.xml rule, the ownership decision table with the create journal,
// adoption and recovery, and the name collision with a user. A role has
// nothing to update in v1alpha1, so an existing own role is up to date.
//
// While the MR is being deleted, Observe reports the role as existing only
// when it is the MR's own (owned, adopted or recovered) and the management
// policies include Delete. Otherwise it emits a Warning event and reports it
// as absent: the MR goes away and the role is left in place.
func (e *External) Observe(ctx context.Context, cr *v1alpha1.Role) (managed.ExternalObservation, error) {
	if meta.WasDeleted(cr) && !ownership.PoliciesOf(cr).Delete() {
		// crossplane-runtime does not call Delete without the Delete
		// management policy; this second guard does not depend on it (D-26).
		return e.LeaveInPlace(cr, access.ReasonOrphaned,
			errors.New("the management policies do not include Delete; the role is left in place")), nil
	}
	ctx = access.SQLContext(ctx, kind, cr)
	obs, err := e.ObserveEntity(ctx, cr, access.Role, cr.Spec.ForProvider.Name)
	cr.Status.AtProvider = v1alpha1.RoleObservation{ID: obs.Entity.ID, Storage: obs.Entity.Storage}
	if err != nil {
		return managed.ExternalObservation{}, err
	}
	if !obs.Exists {
		return managed.ExternalObservation{ResourceExists: false}, nil
	}
	if meta.WasDeleted(cr) {
		return managed.ExternalObservation{ResourceExists: true}, nil
	}
	return managed.ExternalObservation{ResourceExists: true, ResourceUpToDate: true}, nil
}

// Create runs CREATE ROLE (access.CreateEntity): it re-checks the claim, the
// name policy, the access storage and the name collision, journals the
// create, records create-outcome on failure and reads the new UUID back into
// owned-id.
func (e *External) Create(ctx context.Context, cr *v1alpha1.Role) (managed.ExternalCreation, error) {
	if !ownership.PoliciesOf(cr).Create() {
		// crossplane-runtime does not call Create without the Create
		// management policy; this second guard does not depend on it.
		return managed.ExternalCreation{}, reason.Errorf(reason.Internal, "the management policies do not include Create; no role was created")
	}
	name, err := ownership.EffectiveName(cr, cr.Spec.ForProvider.Name)
	if err != nil {
		return managed.ExternalCreation{}, err
	}
	ctx = access.SQLContext(ctx, kind, cr)
	return managed.ExternalCreation{}, e.CreateEntity(ctx, cr, access.Role, name, sqlx.CreateRoleStmt(name))
}

// Update does nothing: role settings are not part of v1alpha1, and Observe
// always reports an existing own role as up to date.
func (e *External) Update(context.Context, *v1alpha1.Role) (managed.ExternalUpdate, error) {
	return managed.ExternalUpdate{}, nil
}

// Delete drops the role. It is only reached for the MR's own role: Observe
// reports every other case as absent while the MR is being deleted.
// Immediately before the DROP it re-checks the claim, the name policy, the
// access storage and that the role still has the UUID in owned-id
// (access.VerifyOwned); a role replaced in the meantime is not dropped. The
// server removes a dropped role from its grantees and from every default
// role list.
func (e *External) Delete(ctx context.Context, cr *v1alpha1.Role) (managed.ExternalDelete, error) {
	cr.SetConditions(xpv2.Deleting())
	if !ownership.PoliciesOf(cr).Delete() {
		// Observe already left the role in place (D-26).
		return managed.ExternalDelete{}, nil
	}
	name, err := ownership.EffectiveName(cr, cr.Spec.ForProvider.Name)
	if err != nil {
		return managed.ExternalDelete{}, err
	}
	ctx = access.SQLContext(ctx, kind, cr)
	ok, err := e.VerifyOwned(ctx, cr, access.Role, name)
	if err != nil || !ok {
		// Not ok: the role disappeared since Observe; the next Observe
		// decides.
		return managed.ExternalDelete{}, err
	}
	return managed.ExternalDelete{}, ch.WithDefaultReason(access.IgnoreGone(e.Client.Exec(ctx, sqlx.DropRoleStmt(name))))
}

// Interface check.
var _ managed.TypedExternalClient[*v1alpha1.Role] = &External{}
