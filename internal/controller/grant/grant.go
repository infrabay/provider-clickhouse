// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

// Package grant contains the Grant controller (design 5.6). A Grant owns one
// exact target (*.*, db.*, db.t or db.t(columns)) for one grantee: it grants
// the privileges that are missing there, revokes the privileges that someone
// else added there, and on deletion revokes what it granted. Privileges are
// compared as access flags of the server's own privilege catalogue, so the
// version-dependent form in which the server stores them never matters.
//
// The provider never grants to or revokes from a grantee it did not create
// or adopt by UUID (D-30), never creates a partial revoke, and never touches
// source privileges, privileges of unsupported levels or wildcard grants.
package grant

import (
	"context"
	"fmt"
	"strings"

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

const kind = "Grant"

// maxListed bounds the number of privileges named in a condition message.
const maxListed = 8

// SetupGated registers the Grant controller with safe-start: it is set up
// once the Grant CRD is installed (and activated).
func SetupGated(mgr ctrl.Manager, o controller.Options, d access.Dependencies) error {
	access.SetupGated(o, v1alpha1.GrantGroupVersionKind, func() error { return Setup(mgr, o, d) })
	return nil
}

// Setup adds the Grant controller. A Grant has no external name and no
// initializer: its identity is the immutable spec tuple of grantee and
// target (design 4.1).
func Setup(mgr ctrl.Manager, o controller.Options, d access.Dependencies) error {
	return access.Setup(mgr, o, access.Controller[*v1alpha1.Grant]{
		GroupKind: v1alpha1.GrantGroupKind,
		GVK:       v1alpha1.GrantGroupVersionKind,
		Object:    &v1alpha1.Grant{},
		List:      &v1alpha1.GrantList{},
		NewConnector: func(r event.Recorder) managed.TypedExternalConnector[*v1alpha1.Grant] {
			return access.NewConnector(mgr, d, kind, r, NewExternal, References)
		},
		References: providerconfig.NewReferenceResolver(mgr.GetClient(), referencesOf),
	})
}

// References returns the references of a Grant: its database, user and role
// references and selectors. They never leave the Grant's namespace, and the
// referenced MRs must use the Grant's ProviderConfig (design 3.1).
func References(g *v1alpha1.Grant) []providerconfig.Reference {
	p := g.Spec.ForProvider
	return []providerconfig.Reference{
		{Field: "databaseRef", Ref: p.DatabaseRef, Selector: p.DatabaseSelector, New: func() resource.ModernManaged { return &v1alpha1.Database{} }},
		{Field: "userRef", Ref: p.UserRef, Selector: p.UserSelector, New: func() resource.ModernManaged { return &v1alpha1.User{} }},
		{Field: "roleRef", Ref: p.RoleRef, Selector: p.RoleSelector, New: func() resource.ModernManaged { return &v1alpha1.Role{} }},
	}
}

func referencesOf(mg resource.Managed) []providerconfig.Reference {
	g, ok := mg.(*v1alpha1.Grant)
	if !ok {
		return nil
	}
	return References(g)
}

// NewExternal returns the external client of a Grant for one reconcile.
func NewExternal(env *access.Env) managed.TypedExternalClient[*v1alpha1.Grant] {
	return &External{Env: env}
}

// External manages one Grant. It embeds the reconcile's access.Env, which
// also provides Disconnect.
type External struct {
	*access.Env

	// binding is the grantee bound in Observe. Every GRANT and REVOKE
	// re-checks it, including the grantee's UUID.
	binding access.Binding
}

// spec is the resolved spec of a Grant.
type spec struct {
	database        string
	target          sqlx.GrantTarget
	grantee         access.Grantee
	withGrantOption bool
}

// specOf resolves the spec. An unresolved database or grantee reference is
// DependencyMissing.
func specOf(cr *v1alpha1.Grant) (spec, error) {
	p := cr.Spec.ForProvider
	if p.Database == nil || *p.Database == "" {
		return spec{}, reason.Errorf(reason.DependencyMissing, "the database is not resolved yet (databaseRef or databaseSelector)")
	}
	g, err := access.GranteeFrom(p.User, p.Role)
	if err != nil {
		return spec{}, err
	}
	t, err := sqlx.NewGrantTarget(*p.Database, p.Table, p.Columns)
	if err != nil {
		return spec{}, reason.Wrap(reason.InvalidSpec, err)
	}
	return spec{database: *p.Database, target: t, grantee: g, withGrantOption: p.WithGrantOption != nil && *p.WithGrantOption}, nil
}

// Observe reads the grantee's rows of system.grants and compares them with
// the spec (design 4.3, 5.6, 5.8, 5.10):
//
//  1. the target database must match the policy's databaseNamePatterns
//     ("*" is limited by the write check allowGlobalGrants instead);
//  2. the grantee is bound (access.Env.BindGrantee): name policy, no user
//     shadowing a role grantee, and, when the management policies allow
//     writes, neither users.xml nor the provider's own identity, and owned
//     by a User or Role MR of the namespace that uses the same
//     ProviderConfig;
//  3. with Create or Update in the management policies, the global target
//     needs allowGlobalGrants and withGrantOption needs allowGrantOption;
//  4. the privileges are expanded with the server's catalogue, and the rows
//     are read. Without system.grants.is_wildcard a grantee with wildcard
//     grants is Unsupported (D-10).
//
// Where the server may not list the grant option (sqlx.Analyze), the
// Grant's record of the grant option it gave (record.go) stands in for the
// rows. The Grant is up to date when the plan of Update has no statement.
//
// While the MR is being deleted, every failed check orphans it: a Warning
// event is emitted, the grant is reported as absent and nothing is revoked.
// The grant then exists only while Delete has something to revoke, so a
// Grant covered by a broader grant, or one whose target holds only someone
// else's privileges, is released at once.
func (e *External) Observe(ctx context.Context, cr *v1alpha1.Grant) (managed.ExternalObservation, error) {
	deleting := meta.WasDeleted(cr)
	ctx = access.SQLContext(ctx, kind, cr)
	s, err := specOf(cr)
	if err != nil {
		if deleting {
			return e.LeaveInPlace(cr, access.ReasonOrphaned, fmt.Errorf("%w; nothing is revoked", err)), nil
		}
		return managed.ExternalObservation{}, access.Unavailable(cr, err)
	}
	cr.Status.AtProvider = v1alpha1.GrantObservation{Grantee: s.grantee.String(), Target: s.target.String()}

	// The name policy applies to every management policy, including
	// observe-only, and also while deleting.
	if err := e.Policy().CheckGrantTarget(cr.GetNamespace(), s.database); err != nil {
		if deleting {
			return e.LeaveInPlace(cr, access.ReasonOrphanedByPolicy, fmt.Errorf("%w; the grants of %s on %s are left in place", err, s.grantee, s.target)), nil
		}
		return managed.ExternalObservation{}, access.Unavailable(cr, err)
	}
	b, err := e.BindGrantee(ctx, cr, s.grantee)
	if err != nil {
		return managed.ExternalObservation{}, err
	}
	if b.Orphaned || !b.Exists {
		return managed.ExternalObservation{ResourceExists: false}, nil
	}
	e.binding = b
	if pol := ownership.PoliciesOf(cr); !deleting && (pol.Create() || pol.Update()) {
		if err := e.Policy().CheckGrantWrite(s.database, s.withGrantOption); err != nil {
			return managed.ExternalObservation{}, access.Unavailable(cr, err)
		}
	}

	a, err := e.analyze(ctx, cr, s)
	switch {
	case err == nil:
	case deleting && (reason.Is(err, reason.InvalidSpec) || reason.Is(err, reason.Unsupported)):
		return e.LeaveInPlace(cr, access.ReasonOrphaned, fmt.Errorf("%w; the grants of %s on %s are left in place", err, s.grantee, s.target)), nil
	case reason.Is(err, reason.InvalidSpec) || reason.Is(err, reason.Unsupported):
		return managed.ExternalObservation{}, access.Unavailable(cr, err)
	default:
		return managed.ExternalObservation{}, err
	}
	cr.Status.AtProvider.Privileges = a.Privileges()
	record := recordOf(cr, b.Entity.ID)
	if deleting {
		return managed.ExternalObservation{ResourceExists: !a.Revoke(record).Empty()}, nil
	}
	if !a.Exists() {
		return managed.ExternalObservation{ResourceExists: false}, nil
	}
	if missing := a.Missing(); len(missing) > 0 {
		cr.SetConditions(xpv2.Unavailable().WithMessage(fmt.Sprintf("%s does not hold %s on %s", s.grantee, list(missing), s.target)))
	} else {
		cr.SetConditions(xpv2.Available())
	}
	return managed.ExternalObservation{ResourceExists: true, ResourceUpToDate: a.Apply(s.withGrantOption, record).Empty()}, nil
}

// list names privileges in a message, at most maxListed of them.
func list(privs []string) string {
	if len(privs) <= maxListed {
		return strings.Join(privs, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(privs[:maxListed], ", "), len(privs)-maxListed)
}

// analyze expands the desired privileges with the server's catalogue and
// reads the grantee's rows. An invalid privilege list is InvalidSpec, a
// grantee with wildcard grants on a server without is_wildcard Unsupported.
func (e *External) analyze(ctx context.Context, cr *v1alpha1.Grant, s spec) (*sqlx.Analysis, error) {
	privs, err := e.Client.Capabilities().Privileges(ctx, e.Client)
	if err != nil {
		return nil, err
	}
	cat, err := sqlx.NewCatalogue(privs)
	if err != nil {
		return nil, reason.Wrap(reason.Internal, err)
	}
	desired, err := cat.Expand(cr.Spec.ForProvider.Privileges, s.target.Level())
	if err != nil {
		return nil, reason.Wrap(reason.InvalidSpec, err)
	}
	rows, err := e.readGrants(ctx, s.grantee)
	if err != nil {
		return nil, err
	}
	return sqlx.Analyze(cat, s.target, desired, rows), nil
}

// readGrants reads the grantee's rows of system.grants, with access_object
// and is_wildcard where the server has them. system.grants fails with 497
// without SELECT on it; it is never silently filtered.
func (e *External) readGrants(ctx context.Context, g access.Grantee) ([]sqlx.GrantRow, error) {
	caps := e.Client.Capabilities()
	ao, err := caps.GrantsAccessObject(ctx, e.Client)
	if err != nil {
		return nil, err
	}
	wildcard, err := caps.GrantsIsWildcard(ctx, e.Client)
	if err != nil {
		return nil, err
	}
	cols := sqlx.GrantColumns{AccessObject: ao, IsWildcard: wildcard}
	var rows []sqlx.GrantRow
	err = e.Client.Query(ctx, sqlx.QueryGrants(g.Kind == access.Role, cols), ch.Params{"grantee": g.Name}, func(r ch.Row) error {
		row, err := sqlx.ScanGrantRow(r, cols)
		if err != nil {
			return err
		}
		rows = append(rows, row)
		return nil
	})
	if err != nil {
		return nil, ch.WithDefaultReason(err)
	}
	if !wildcard {
		if err := e.checkWildcards(ctx, g); err != nil {
			return nil, err
		}
	}
	return rows, nil
}

// checkWildcards refuses a grantee with wildcard grants on a server without
// system.grants.is_wildcard: there a wildcard grant ON team_*.* is stored
// like a grant on a database named "team_", so coverage could not be
// computed, and a REVOKE could cut a partial revoke into it (design 5.6,
// D-10).
func (e *External) checkWildcards(ctx context.Context, g access.Grantee) error {
	var lines []string
	err := e.Client.Query(ctx, sqlx.ShowGrants(g.Name), nil, func(r ch.Row) error {
		var s string
		if err := r.Scan(&s); err != nil {
			return err
		}
		lines = append(lines, s)
		return nil
	})
	if err != nil {
		return ch.WithDefaultReason(err)
	}
	objs, err := sqlx.WildcardObjects(lines)
	if err != nil {
		return reason.Wrap(reason.Internal, err)
	}
	if len(objs) > 0 {
		return reason.Errorf(reason.Unsupported,
			"grantee %s has wildcard grants (ON %s); ClickHouse %s stores them like grants on literal names, so managing its grants needs system.grants.is_wildcard (ClickHouse 26.8 or later)",
			g, strings.Join(objs, ", "), e.Client.Info().Version)
	}
	return nil
}

// bound returns an error unless Observe bound the grantee g in this
// reconcile. Every write re-checks that binding.
func (e *External) bound(g access.Grantee) error {
	if !e.binding.Exists || e.binding.Grantee != g {
		return reason.Errorf(reason.Internal, "grantee %s was not bound in this reconcile; nothing was granted or revoked", g)
	}
	return nil
}

// Create grants the missing privileges.
func (e *External) Create(ctx context.Context, cr *v1alpha1.Grant) (managed.ExternalCreation, error) {
	if !ownership.PoliciesOf(cr).Create() {
		return managed.ExternalCreation{}, notAllowed("Create")
	}
	return managed.ExternalCreation{}, e.apply(ctx, cr)
}

// Update revokes the foreign privileges at the target and grants the
// missing ones, and keeps the record of the grant option.
func (e *External) Update(ctx context.Context, cr *v1alpha1.Grant) (managed.ExternalUpdate, error) {
	if !ownership.PoliciesOf(cr).Update() {
		return managed.ExternalUpdate{}, notAllowed("Update")
	}
	return managed.ExternalUpdate{}, e.apply(ctx, cr)
}

// notAllowed is the error of a write that the management policies do not
// allow. crossplane-runtime never calls it then (management policies are
// always honoured, D-26); this second guard matters because the grantee
// binding checks ownership only when the policies allow writes.
func notAllowed(op string) error {
	return reason.Errorf(reason.Internal, "the management policies do not include %s; nothing was granted or revoked", op)
}

// apply runs the plan of Create and Update (sqlx.Analysis.Apply): REVOKE,
// REVOKE GRANT OPTION FOR, then GRANT. It repeats the policy checks,
// re-reads the rows (a third party could have changed them since Observe)
// and, immediately before the statements, re-checks the grantee binding:
// the grantee must still exist, be owned and unambiguous, have the UUID
// bound in Observe, and the access storage must be writable from this
// endpoint. Once the statements ran it saves the record of the grant option
// the plan leaves.
func (e *External) apply(ctx context.Context, cr *v1alpha1.Grant) error {
	ctx = access.SQLContext(ctx, kind, cr)
	s, err := specOf(cr)
	if err != nil {
		return err
	}
	if err := e.Policy().CheckGrantTarget(cr.GetNamespace(), s.database); err != nil {
		return err
	}
	if err := e.Policy().CheckGrantWrite(s.database, s.withGrantOption); err != nil {
		return err
	}
	if err := e.bound(s.grantee); err != nil {
		return err
	}
	a, err := e.analyze(ctx, cr, s)
	if err != nil {
		return err
	}
	granteeID := e.binding.Entity.ID
	record := recordOf(cr, granteeID)
	p := a.Apply(s.withGrantOption, record)
	stmts, err := p.Statements(s.target, s.grantee.Name)
	if err != nil {
		return reason.Wrap(reason.Internal, err)
	}
	if len(stmts) == 0 {
		// A record that changed without statements (a grant option no row
		// shows any more) is saved with the next statements.
		return nil
	}
	ok, err := e.RebindGrantee(ctx, cr, e.binding)
	if err != nil {
		return err
	}
	if !ok {
		return reason.Errorf(reason.DependencyMissing, "grantee %s no longer exists; nothing was granted or revoked", s.grantee)
	}
	for _, stmt := range stmts {
		// 497: the identity lacks the privilege WITH GRANT OPTION (the
		// server-side ceiling); 509: a privilege at the wrong level; 511 and
		// 192: the grantee disappeared.
		if err := e.Client.Exec(ctx, stmt); err != nil {
			return ch.WithDefaultReason(err)
		}
	}
	return e.saveRecord(ctx, cr, granteeID, p.Record)
}

// Delete revokes the desired privileges that are granted at exactly the
// target, and the grant option of record where the server may not list it
// (sqlx.Analysis.Revoke); then it removes the record. Privileges covered by
// a broader grant and privileges that someone else granted at the target are
// left alone. It is only reached for a bound grantee: Observe orphans every
// other case. The name policy and the binding are checked again first; a
// grantee that no longer exists leaves nothing to revoke.
func (e *External) Delete(ctx context.Context, cr *v1alpha1.Grant) (managed.ExternalDelete, error) {
	cr.SetConditions(xpv2.Deleting())
	ctx = access.SQLContext(ctx, kind, cr)
	s, err := specOf(cr)
	if err != nil {
		return managed.ExternalDelete{}, err
	}
	if err := e.Policy().CheckGrantTarget(cr.GetNamespace(), s.database); err != nil {
		return managed.ExternalDelete{}, err
	}
	if err := e.bound(s.grantee); err != nil {
		return managed.ExternalDelete{}, err
	}
	a, err := e.analyze(ctx, cr, s)
	if err != nil {
		return managed.ExternalDelete{}, err
	}
	granteeID := e.binding.Entity.ID
	stmts, err := a.Revoke(recordOf(cr, granteeID)).Statements(s.target, s.grantee.Name)
	if err != nil {
		return managed.ExternalDelete{}, reason.Wrap(reason.Internal, err)
	}
	if len(stmts) > 0 {
		ok, err := e.RebindGrantee(ctx, cr, e.binding)
		if err != nil || !ok {
			return managed.ExternalDelete{}, err
		}
		for _, stmt := range stmts {
			if err := e.Client.Exec(ctx, stmt); err != nil {
				if access.IgnoreGone(err) == nil {
					// 192 or 511: the grantee is gone, and its grants with it.
					return managed.ExternalDelete{}, nil
				}
				return managed.ExternalDelete{}, ch.WithDefaultReason(err)
			}
		}
	}
	// The grant option of record is revoked: the next Observe no longer
	// counts it, and the MR is released.
	return managed.ExternalDelete{}, e.saveRecord(ctx, cr, granteeID, nil)
}

// Interface checks.
var _ managed.TypedExternalClient[*v1alpha1.Grant] = &External{}
