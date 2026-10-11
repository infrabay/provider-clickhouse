// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package access

import (
	"context"
	"testing"

	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	"github.com/google/go-cmp/cmp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/crossplane/crossplane-runtime/v2/pkg/meta"

	"github.com/infrabay/provider-clickhouse/apis/v1alpha1"
	ch "github.com/infrabay/provider-clickhouse/internal/clients/clickhouse"
	"github.com/infrabay/provider-clickhouse/internal/clients/clickhouse/fake"
	"github.com/infrabay/provider-clickhouse/internal/clients/clickhouse/sqlx"
	"github.com/infrabay/provider-clickhouse/internal/controller/access/accesstest"
	"github.com/infrabay/provider-clickhouse/internal/controller/providerconfig"
	"github.com/infrabay/provider-clickhouse/internal/reason"
)

const userName = "team-a_api"

type grantOpt func(*v1alpha1.Grant)

func newGrant(opts ...grantOpt) *v1alpha1.Grant {
	g := &v1alpha1.Grant{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "g", UID: "grant-uid"}}
	g.Spec.ProviderConfigReference = sharedPC
	g.Spec.ForProvider.Privileges = []string{"SELECT"}
	g.Spec.ForProvider.Database = new("team-a_db")
	for _, o := range opts {
		o(g)
	}
	return g
}

func grantDeleting() grantOpt {
	return func(g *v1alpha1.Grant) {
		now := metav1.Now()
		g.SetDeletionTimestamp(&now)
		g.SetFinalizers([]string{"finalizer.managedresource.crossplane.io"})
	}
}

func grantPolicies(p ...xpv2.ManagementAction) grantOpt {
	return func(g *v1alpha1.Grant) { g.SetManagementPolicies(p) }
}

func TestGranteeString(t *testing.T) {
	if s := (Grantee{Kind: User, Name: "a@example.com"}).String(); s != "user:a@example.com" {
		t.Errorf("user grantee = %q", s)
	}
	if s := (Grantee{Kind: Role, Name: "r"}).String(); s != "role:r" {
		t.Errorf("role grantee = %q", s)
	}
}

func TestGranteeFrom(t *testing.T) {
	if g, err := GranteeFrom(new("u"), nil); err != nil || g != (Grantee{Kind: User, Name: "u"}) {
		t.Errorf("user: %+v, %v", g, err)
	}
	if g, err := GranteeFrom(new(""), new("r")); err != nil || g != (Grantee{Kind: Role, Name: "r"}) {
		t.Errorf("role: %+v, %v", g, err)
	}
	if _, err := GranteeFrom(new("u"), new("r")); !reason.Is(err, reason.InvalidSpec) {
		t.Errorf("both: %v", err)
	}
	if _, err := GranteeFrom(nil, nil); !reason.Is(err, reason.DependencyMissing) {
		t.Errorf("unresolved: %v", err)
	}
}

func TestBindGrantee(t *testing.T) {
	ownedRole := accesstest.OwnedRole(ns, "reader", roleName, roleID, sharedPC)
	ownedUser := accesstest.OwnedUser(ns, "api", userName, userID, sharedPC)
	roleGrantee := Grantee{Kind: Role, Name: roleName}
	userGrantee := Grantee{Kind: User, Name: userName}

	cases := map[string]struct {
		mr      *v1alpha1.Grant
		grantee Grantee
		db      *fake.DB
		objs    []client.Object
		res     *providerconfig.Resolved
		want    Binding
		reason  reason.Reason
		ready   string
		events  []string
		noQuery bool
	}{
		"OwnedRole": {
			mr: newGrant(), grantee: roleGrantee, objs: []client.Object{ownedRole},
			db:   fake.NewDB().WithRole(roleName, roleID, ch.StorageLocalDirectory),
			want: Binding{Grantee: roleGrantee, Entity: Entity{Kind: Role, Name: roleName, ID: roleID, Storage: ch.StorageLocalDirectory}, Exists: true, Owner: "Role team-a/reader"},
		},
		"OwnedUser": {
			mr: newGrant(), grantee: userGrantee, objs: []client.Object{ownedUser},
			db:   fake.NewDB().WithUser(userName, userID, ch.StorageReplicated),
			want: Binding{Grantee: userGrantee, Entity: Entity{Kind: User, Name: userName, ID: userID, Storage: ch.StorageReplicated}, Exists: true, Owner: "User team-a/api"},
		},
		// The provider never grants to an entity it did not create or adopt.
		"NotOwned": {
			mr: newGrant(), grantee: roleGrantee,
			db:     fake.NewDB().WithRole(roleName, roleID, ch.StorageLocalDirectory),
			reason: reason.NotOwned, ready: "False",
		},
		"NotOwnedOnNamespacedConfig": {
			mr: newGrant(func(g *v1alpha1.Grant) {
				g.Spec.ProviderConfigReference = &xpv2.ProviderConfigReference{Kind: "ProviderConfig", Name: "own"}
			}),
			grantee: roleGrantee, res: namespacedResolved(),
			db:     fake.NewDB().WithRole(roleName, roleID, ch.StorageLocalDirectory),
			reason: reason.NotOwned,
		},
		"OwnerUsesAnotherConfig": {
			mr: newGrant(), grantee: roleGrantee,
			objs:   []client.Object{accesstest.OwnedRole(ns, "reader", roleName, roleID, &xpv2.ProviderConfigReference{Kind: "ClusterProviderConfig", Name: "other"})},
			db:     fake.NewDB().WithRole(roleName, roleID, ch.StorageLocalDirectory),
			reason: reason.NotOwned,
		},
		"OwnerInAnotherNamespace": {
			mr: newGrant(), grantee: roleGrantee,
			objs:   []client.Object{accesstest.OwnedRole("team-b", "reader", roleName, roleID, sharedPC)},
			db:     fake.NewDB().WithRole(roleName, roleID, ch.StorageLocalDirectory),
			reason: reason.NotOwned,
		},
		// The role was dropped and re-created by someone else: the owner's
		// claim names the old UUID.
		"OwnerClaimsAnotherUUID": {
			mr: newGrant(), grantee: roleGrantee, objs: []client.Object{ownedRole},
			db:     fake.NewDB().WithRole(roleName, otherID, ch.StorageLocalDirectory),
			reason: reason.NotOwned,
		},
		// A copied manifest: owner-uid is not the copy's UID.
		"OwnerClaimCopied": {
			mr: newGrant(), grantee: roleGrantee,
			objs: []client.Object{func() client.Object {
				r := ownedRole.DeepCopy()
				meta.AddAnnotations(r, map[string]string{v1alpha1.AnnotationOwnerUID: "someone-else"})
				return r
			}()},
			db:     fake.NewDB().WithRole(roleName, roleID, ch.StorageLocalDirectory),
			reason: reason.NotOwned,
		},
		"Missing": {
			mr: newGrant(), grantee: roleGrantee, db: fake.NewDB().WithEntities(),
			reason: reason.DependencyMissing, ready: "False",
		},
		"MissingObserveOnly": {
			mr: newGrant(grantPolicies(xpv2.ManagementActionObserve)), grantee: roleGrantee, db: fake.NewDB().WithEntities(),
			want: Binding{Grantee: roleGrantee},
		},
		// Nothing is left to revoke; the MR goes away.
		"MissingWhileDeleting": {
			mr: newGrant(grantDeleting()), grantee: roleGrantee, db: fake.NewDB().WithEntities(),
			want: Binding{Grantee: roleGrantee},
		},
		"Ambiguous": {
			mr: newGrant(), grantee: roleGrantee, objs: []client.Object{ownedRole},
			db:     fake.NewDB().WithRole(roleName, roleID, ch.StorageLocalDirectory).WithUser(roleName, userID, ch.StorageLocalDirectory),
			reason: reason.AmbiguousGrantee, ready: "False",
		},
		"AmbiguousObserveOnly": {
			mr: newGrant(grantPolicies(xpv2.ManagementActionObserve)), grantee: roleGrantee,
			db:     fake.NewDB().WithRole(roleName, roleID, ch.StorageLocalDirectory).WithUser(roleName, userID, ch.StorageLocalDirectory),
			reason: reason.AmbiguousGrantee,
		},
		// REVOKE would hit the same-named user: nothing is revoked.
		"AmbiguousWhileDeleting": {
			mr: newGrant(grantDeleting()), grantee: roleGrantee, objs: []client.Object{ownedRole},
			db:     fake.NewDB().WithRole(roleName, roleID, ch.StorageLocalDirectory).WithUser(roleName, userID, ch.StorageLocalDirectory),
			want:   Binding{Grantee: roleGrantee, Entity: Entity{Kind: Role, Name: roleName, ID: roleID, Storage: ch.StorageLocalDirectory}, Orphaned: true},
			events: []string{"Orphaned"},
		},
		"ReadOnly": {
			mr: newGrant(), grantee: userGrantee, db: fake.NewDB().WithUser(userName, userID, ch.StorageUsersXML),
			reason: reason.ReadOnlyStorage, ready: "False",
		},
		"ReadOnlyObserveOnly": {
			mr: newGrant(grantPolicies(xpv2.ManagementActionObserve)), grantee: userGrantee, db: fake.NewDB().WithUser(userName, userID, ch.StorageUsersXML),
			want: Binding{Grantee: userGrantee, Entity: Entity{Kind: User, Name: userName, ID: userID, Storage: ch.StorageUsersXML}, Exists: true},
		},
		"Self": {
			mr: newGrant(), grantee: Grantee{Kind: User, Name: provider}, res: namespacedResolved(),
			db:     fake.NewDB().WithUser(provider, userID, ch.StorageLocalDirectory),
			reason: reason.SelfModification, ready: "False",
		},
		"PolicyViolation": {
			mr: newGrant(), grantee: Grantee{Kind: Role, Name: "team-b_reader"}, db: fake.NewDB(),
			reason: reason.PolicyViolation, ready: "False", noQuery: true,
		},
		"PolicyViolationWhileDeleting": {
			mr: newGrant(grantDeleting()), grantee: Grantee{Kind: Role, Name: "team-b_reader"}, db: fake.NewDB(),
			want: Binding{Grantee: Grantee{Kind: Role, Name: "team-b_reader"}, Orphaned: true}, events: []string{"OrphanedByPolicy"}, noQuery: true,
		},
		// D-26: without Delete in the management policies nothing is ever
		// revoked, even if crossplane-runtime called Delete.
		"NoDeletePolicyWhileDeleting": {
			mr:      newGrant(grantDeleting(), grantPolicies(xpv2.ManagementActionObserve, xpv2.ManagementActionCreate, xpv2.ManagementActionUpdate)),
			grantee: roleGrantee, objs: []client.Object{ownedRole}, db: fake.NewDB(),
			want: Binding{Grantee: roleGrantee, Orphaned: true}, events: []string{"Orphaned"}, noQuery: true,
		},
		"NotOwnedWhileDeleting": {
			mr: newGrant(grantDeleting()), grantee: roleGrantee,
			db:     fake.NewDB().WithRole(roleName, roleID, ch.StorageLocalDirectory),
			want:   Binding{Grantee: roleGrantee, Entity: Entity{Kind: Role, Name: roleName, ID: roleID, Storage: ch.StorageLocalDirectory}, Orphaned: true},
			events: []string{"Orphaned"},
		},
		"OwnedWhileDeleting": {
			mr: newGrant(grantDeleting()), grantee: roleGrantee, objs: []client.Object{ownedRole},
			db:   fake.NewDB().WithRole(roleName, roleID, ch.StorageLocalDirectory),
			want: Binding{Grantee: roleGrantee, Entity: Entity{Kind: Role, Name: roleName, ID: roleID, Storage: ch.StorageLocalDirectory}, Exists: true, Owner: "Role team-a/reader"},
		},
		"ReadErrorIsNotNotFound": {
			mr: newGrant(), grantee: roleGrantee,
			db:     fake.NewDB().On(sqlx.QueryRoleEntity, fake.Error(fake.Exception(497, "necessary to have the grant SHOW ROLES"))),
			reason: reason.PermissionDenied,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			res := tc.res
			if res == nil {
				res = clusterResolved(t, sharedPolicy())
			}
			env := newEnv(t, tc.db, res, append([]client.Object{tc.mr.DeepCopy()}, tc.objs...)...)
			got, err := env.BindGrantee(context.Background(), tc.mr, tc.grantee)
			if tc.reason != "" {
				if !reason.Is(err, tc.reason) {
					t.Fatalf("BindGrantee() error = %v, want %s", err, tc.reason)
				}
			} else {
				if err != nil {
					t.Fatalf("BindGrantee() error = %v", err)
				}
				if diff := cmp.Diff(tc.want, got); diff != "" {
					t.Errorf("-want +got:\n%s", diff)
				}
			}
			if tc.ready != "" && readyStatus(tc.mr) != tc.ready {
				t.Errorf("Ready = %q, want %q", readyStatus(tc.mr), tc.ready)
			}
			if diff := cmp.Diff(append([]string{}, tc.events...), env.rec.reasons()); diff != "" {
				t.Errorf("events -want +got:\n%s", diff)
			}
			if tc.noQuery && len(tc.db.Seen) > 0 {
				t.Errorf("no query may run, ran %v", tc.db.Seen)
			}
			if len(tc.db.Execs) > 0 {
				t.Errorf("binding must never write, ran %v", tc.db.Execs)
			}
		})
	}
}

func TestRebindGrantee(t *testing.T) {
	ctx := context.Background()
	ownedRole := accesstest.OwnedRole(ns, "reader", roleName, roleID, sharedPC)
	g := Grantee{Kind: Role, Name: roleName}
	bound := Binding{Grantee: g, Entity: Entity{Kind: Role, Name: roleName, ID: roleID, Storage: ch.StorageLocalDirectory}, Exists: true}

	cases := map[string]struct {
		mr     *v1alpha1.Grant
		db     *fake.DB
		objs   []client.Object
		multi  bool
		exists bool
		reason reason.Reason
	}{
		"StillBound": {mr: newGrant(), db: fake.NewDB().WithRole(roleName, roleID, ch.StorageLocalDirectory), objs: []client.Object{ownedRole}, exists: true},
		"Gone":       {mr: newGrant(), db: fake.NewDB().WithEntities(), objs: []client.Object{ownedRole}, reason: reason.DependencyMissing},
		"GoneWhileDeleting": {
			mr: newGrant(grantDeleting()), db: fake.NewDB().WithEntities(), objs: []client.Object{ownedRole},
		},
		// Replaced, and the owner adopted the new role in the meantime: the
		// UUID still differs from what Observe bound.
		"Replaced": {
			mr: newGrant(), db: fake.NewDB().WithRole(roleName, otherID, ch.StorageLocalDirectory),
			objs:   []client.Object{accesstest.OwnedRole(ns, "reader", roleName, otherID, sharedPC)},
			reason: reason.Replaced,
		},
		"Shadowed": {
			mr: newGrant(), db: fake.NewDB().WithRole(roleName, roleID, ch.StorageLocalDirectory).WithUser(roleName, userID, ch.StorageLocalDirectory),
			objs: []client.Object{ownedRole}, reason: reason.AmbiguousGrantee,
		},
		"OwnerReleasedIt": {mr: newGrant(), db: fake.NewDB().WithRole(roleName, roleID, ch.StorageLocalDirectory), reason: reason.NotOwned},
		"OwnerReleasedItWhileDeleting": {
			mr: newGrant(grantDeleting()), db: fake.NewDB().WithRole(roleName, roleID, ch.StorageLocalDirectory), reason: reason.NotOwned,
		},
		"NotWritable": {
			mr: newGrant(), db: fake.NewDB().WithRole(roleName, roleID, ch.StorageLocalDirectory), objs: []client.Object{ownedRole},
			multi: true, reason: reason.AccessStorageNotReplicated,
		},
		// An observe-only MR binds no owner, so it never passes the re-check,
		// even for an owned grantee and even if a write were called.
		"ObserveOnly": {
			mr: newGrant(grantPolicies(xpv2.ManagementActionObserve)), db: fake.NewDB().WithRole(roleName, roleID, ch.StorageLocalDirectory),
			objs: []client.Object{ownedRole}, reason: reason.NotOwned,
		},
		"ObserveOnlyForeignGrantee": {
			mr: newGrant(grantPolicies(xpv2.ManagementActionObserve, xpv2.ManagementActionLateInitialize)),
			db: fake.NewDB().WithRole(roleName, roleID, ch.StorageLocalDirectory), reason: reason.NotOwned,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			env := newEnv(t, tc.db, clusterResolved(t, sharedPolicy()), append([]client.Object{tc.mr.DeepCopy()}, tc.objs...)...)
			if tc.multi {
				env.cl.Servers = []string{"s1", "s2"}
			}
			exists, err := env.RebindGrantee(ctx, tc.mr, bound)
			if tc.reason != "" {
				if !reason.Is(err, tc.reason) {
					t.Fatalf("RebindGrantee() = %t, %v; want %s", exists, err, tc.reason)
				}
			} else if err != nil || exists != tc.exists {
				t.Fatalf("RebindGrantee() = %t, %v; want %t", exists, err, tc.exists)
			}
			if len(env.rec.reasons()) > 0 {
				t.Errorf("a rebind emits no events: %v", env.rec.reasons())
			}
		})
	}
}
