// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package access

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/crossplane/crossplane-runtime/v2/pkg/event"
	"github.com/crossplane/crossplane-runtime/v2/pkg/meta"
	"github.com/crossplane/crossplane-runtime/v2/pkg/resource"

	"github.com/infrabay/provider-clickhouse/apis/v1alpha1"
	"github.com/infrabay/provider-clickhouse/internal/controller/ownership"
	"github.com/infrabay/provider-clickhouse/internal/controller/providerconfig"
	"github.com/infrabay/provider-clickhouse/internal/reason"
)

// A Grantee is the grantee of a Grant or RoleGrant: a user or a role.
type Grantee struct {
	Kind EntityKind
	Name string
}

// String renders the grantee as status.atProvider.grantee does:
// "user:<name>" or "role:<name>".
func (g Grantee) String() string { return g.Kind.lower() + ":" + g.Name }

// GranteeFrom returns the grantee of a Grant or RoleGrant from its resolved
// spec.forProvider.user and role fields. CEL requires exactly one grantee
// (by name, reference or selector); an unresolved reference leaves both
// fields empty, which is DependencyMissing.
func GranteeFrom(user, role *string) (Grantee, error) {
	u, r := user != nil && *user != "", role != nil && *role != ""
	switch {
	case u && r:
		return Grantee{}, reason.Errorf(reason.InvalidSpec, "set exactly one grantee: user or role")
	case u:
		return Grantee{Kind: User, Name: *user}, nil
	case r:
		return Grantee{Kind: Role, Name: *role}, nil
	}
	return Grantee{}, reason.Errorf(reason.DependencyMissing, "the grantee is not resolved yet (userRef, userSelector, roleRef or roleSelector)")
}

// A Binding is the outcome of BindGrantee: the grantee a Grant or RoleGrant
// is bound to.
type Binding struct {
	Grantee Grantee
	// Entity is the grantee's row, when Exists.
	Entity Entity
	// Exists is set when the grantee exists on the server. Grants and role
	// grants of a grantee that does not exist do not exist either.
	Exists bool
	// Owner names the MR that owns the grantee, e.g. "User team-a/api". It
	// is set when the management policies allow writes.
	Owner string
	// Orphaned is set while the MR is being deleted when the grantee's
	// grants must be left in place: a Warning event was emitted. Report
	// ResourceExists=false and send no SQL.
	Orphaned bool
}

type orphanCause struct {
	reason event.Reason
	err    error
}

// BindGrantee runs the grantee checks of a Grant or RoleGrant in Observe
// (design 4.3, 5.6, 5.7, 5.8). Grants and role grants have no UUID of their
// own; they are bound to their grantee, which must be:
//
//   - allowed by the ClusterProviderConfig policy (userNamePatterns or
//     roleNamePatterns), for every management policy and also while deleting
//     (D-25);
//   - for a role, not shadowed by a user with the same name: the server
//     resolves GRANT and REVOKE grantees user-first (AmbiguousGrantee);
//
// and, when the management policies allow writes:
//
//   - not in a read-only user directory such as users.xml/users.d
//     (ReadOnlyStorage);
//   - not the provider's own identity (SelfModification);
//   - owned: a User or Role MR in the MR's namespace that uses the same
//     ProviderConfig holds a valid claim on the grantee's current UUID
//     (NotOwned otherwise). The provider never grants to or revokes from an
//     access entity it did not create or adopt by UUID (D-30).
//
// A grantee that does not exist gives DependencyMissing when writes are
// allowed, and Exists=false otherwise. While the MR is being deleted every
// failed check orphans the MR (Warning event, Orphaned, no SQL), and so does
// a missing Delete management policy (D-26); a missing grantee means there
// is nothing left to revoke.
//
// The checks of the other names (a Grant's target database, a RoleGrant's
// memberOf) and the kind's write checks are the caller's; see
// providerconfig.Policy.CheckGrantTarget and CheckRoleGrantNames. On a
// failed check BindGrantee sets Ready=False.
func (e *Env) BindGrantee(ctx context.Context, mg resource.ModernManaged, g Grantee) (Binding, error) {
	b, orphan, checkErr, err := e.bind(ctx, mg, g)
	switch {
	case err != nil:
		return b, err
	case checkErr != nil:
		return b, Unavailable(mg, checkErr)
	case orphan != nil:
		e.LeaveInPlace(mg, orphan.reason, orphan.err)
		b.Orphaned, b.Exists = true, false
	}
	return b, nil
}

// RebindGrantee repeats the grantee checks immediately before a GRANT or
// REVOKE: the statements address the grantee by name, so it could have been
// replaced, shadowed by a same-named user or released by its owner since
// Observe. It also runs CheckWritable. It refuses the write unless the
// grantee is still bound to an owner and still has the UUID observed in b.
// The grantee checks of an MR whose management policies allow no write
// stop before the owner lookup, so such an MR never passes this re-check,
// whatever crossplane-runtime calls (D-26). When the grantee no longer
// exists it returns exists=false without error while the MR is being
// deleted (there is nothing left to revoke, so Delete returns without
// error), and DependencyMissing otherwise.
func (e *Env) RebindGrantee(ctx context.Context, mg resource.ModernManaged, b Binding) (bool, error) {
	if err := e.CheckWritable(ctx); err != nil {
		return false, err
	}
	nb, orphan, checkErr, err := e.bind(ctx, mg, b.Grantee)
	switch {
	case err != nil:
		return false, err
	case checkErr != nil:
		return false, checkErr
	case orphan != nil:
		return false, reason.Wrap(reason.NotOwned, fmt.Errorf("%w; nothing was granted or revoked", orphan.err))
	case !nb.Exists:
		return false, nil
	case nb.Owner == "":
		return false, reason.Errorf(reason.NotOwned, "grantee %s is not bound to an owning User or Role (the management policies allow no write); nothing was granted or revoked", b.Grantee)
	}
	if b.Entity.ID != "" && nb.Entity.ID != b.Entity.ID {
		return false, reason.Errorf(reason.Replaced, "grantee %s was replaced since it was observed (id %s, now %s); nothing was granted or revoked",
			b.Grantee, b.Entity.ID, nb.Entity.ID)
	}
	return true, nil
}

// bind runs the checks. err is a read error, checkErr a failed check of an
// MR that is not being deleted, orphan a failed check while deleting.
func (e *Env) bind(ctx context.Context, mg resource.ModernManaged, g Grantee) (b Binding, orphan *orphanCause, checkErr, err error) {
	deleting := meta.WasDeleted(mg)
	pol := ownership.PoliciesOf(mg)
	b.Grantee = g

	// Without Delete in the management policies crossplane-runtime never
	// calls Delete; this second guard does not depend on it (D-26).
	if deleting && !pol.Delete() {
		return b, &orphanCause{ReasonOrphaned, orphanf("the management policies do not include Delete; the grants of %s are left in place", g)}, nil, nil
	}
	if err := e.Policy().CheckGrantee(mg.GetNamespace(), g.Kind.GranteeKind(), g.Name); err != nil {
		if deleting {
			return b, &orphanCause{ReasonOrphanedByPolicy, fmt.Errorf("%w; the grants of %s are left in place", err, g)}, nil, nil
		}
		return b, nil, err, nil
	}

	ent, exists, err := ReadEntity(ctx, e.Client, g.Kind, g.Name)
	if err != nil {
		return b, nil, nil, err
	}
	if !exists {
		if !deleting && pol.Writes() {
			return b, nil, reason.Errorf(reason.DependencyMissing, "grantee %s does not exist; create it first, for example with a %s", g, g.Kind), nil
		}
		return b, nil, nil, nil
	}
	b.Entity, b.Exists = ent, true

	leave := func(r event.Reason, cause error) (Binding, *orphanCause, error, error) {
		if deleting {
			return b, &orphanCause{r, fmt.Errorf("%w; the grants of %s are left in place", cause, g)}, nil, nil
		}
		return b, nil, cause, nil
	}

	if g.Kind == Role {
		user, shadowed, err := ReadEntity(ctx, e.Client, User, g.Name)
		if err != nil {
			return b, nil, nil, err
		}
		if shadowed {
			return leave(ReasonOrphaned, reason.Errorf(reason.AmbiguousGrantee,
				"a user named %q exists (id %s); ClickHouse resolves grantees user-first, so every GRANT and REVOKE for role %q would apply to that user",
				g.Name, user.ID, g.Name))
		}
	}
	if !pol.Writes() {
		return b, nil, nil, nil
	}
	readOnly, err := e.ReadOnly(ctx, ent)
	if err != nil {
		return b, nil, nil, err
	}
	if readOnly {
		return leave(ReasonOrphaned, reason.Errorf(reason.ReadOnlyStorage, "grantee %s is in the read-only user directory %q (such as users.xml/users.d) and is never written", g, ent.Storage))
	}
	if g.Kind == User && g.Name == e.CurrentUser() {
		return leave(ReasonOrphaned, reason.Errorf(reason.SelfModification, "grantee %s is the provider's own identity and is never modified", g))
	}
	owner, err := e.granteeOwner(ctx, mg, g, ent.ID)
	if err != nil {
		return b, nil, nil, err
	}
	if owner == "" {
		return leave(ReasonOrphaned, reason.Errorf(reason.NotOwned,
			"grantee %s (id %s) is not owned by a %s in namespace %q that uses the same ProviderConfig; the provider only grants to and revokes from entities that it created or adopted by UUID (create or adopt it with a %s)",
			g, ent.ID, g.Kind, mg.GetNamespace(), g.Kind))
	}
	b.Owner = owner
	return b, nil, nil, nil
}

// granteeOwner returns "<Kind> <namespace>/<name>" of the MR in mg's
// namespace that owns the grantee with UUID id, or "" if none does. An owner
// uses the same ProviderConfig as mg, its effective external name is the
// grantee's name, and it holds a valid claim whose owned-id is id.
func (e *Env) granteeOwner(ctx context.Context, mg resource.ModernManaged, g Grantee, id string) (string, error) {
	r := e.Reader
	if r == nil {
		r = e.Kube
	}
	ns := mg.GetNamespace()
	owns := func(o resource.ModernManaged, forProviderName *string) bool {
		if !providerconfig.SameConfig(o.GetProviderConfigReference(), mg.GetProviderConfigReference()) {
			return false
		}
		name, err := ownership.EffectiveName(o, forProviderName)
		if err != nil || name != g.Name {
			return false
		}
		return ownership.ClaimValid(o) && ownership.ClaimOf(o).OwnedID == id
	}
	switch g.Kind {
	case User:
		l := &v1alpha1.UserList{}
		if err := r.List(ctx, l, client.InNamespace(ns)); err != nil {
			return "", fmt.Errorf("cannot list Users in namespace %q to find the owner of grantee %s: %w", ns, g, err)
		}
		for i := range l.Items {
			if u := &l.Items[i]; owns(u, u.Spec.ForProvider.Name) {
				return "User " + ns + "/" + u.GetName(), nil
			}
		}
	case Role:
		l := &v1alpha1.RoleList{}
		if err := r.List(ctx, l, client.InNamespace(ns)); err != nil {
			return "", fmt.Errorf("cannot list Roles in namespace %q to find the owner of grantee %s: %w", ns, g, err)
		}
		for i := range l.Items {
			if o := &l.Items[i]; owns(o, o.Spec.ForProvider.Name) {
				return "Role " + ns + "/" + o.GetName(), nil
			}
		}
	}
	return "", nil
}
