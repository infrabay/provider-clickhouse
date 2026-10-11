// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package rolegrant

import (
	"context"
	"sync"
	"testing"

	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/crossplane/crossplane-runtime/v2/pkg/event"
	"github.com/crossplane/crossplane-runtime/v2/pkg/meta"
	"github.com/crossplane/crossplane-runtime/v2/pkg/reconciler/managed"
	"github.com/crossplane/crossplane-runtime/v2/pkg/resource"

	"github.com/infrabay/provider-clickhouse/apis/v1alpha1"
	ch "github.com/infrabay/provider-clickhouse/internal/clients/clickhouse"
	"github.com/infrabay/provider-clickhouse/internal/clients/clickhouse/fake"
	"github.com/infrabay/provider-clickhouse/internal/clients/clickhouse/sqlx"
	"github.com/infrabay/provider-clickhouse/internal/controller/access"
	"github.com/infrabay/provider-clickhouse/internal/controller/access/accesstest"
	"github.com/infrabay/provider-clickhouse/internal/controller/ownership"
	"github.com/infrabay/provider-clickhouse/internal/controller/providerconfig"
	"github.com/infrabay/provider-clickhouse/internal/reason"
)

const (
	ns          = "team-a"
	memberOf    = "team-a_reader"
	userName    = "team-a_api"
	granteeRole = "team-a_writer"
	provider    = "provider" // fake.NewClient's currentUser()

	memberID  = "11111111-1111-1111-1111-111111111111"
	userID    = "33333333-3333-3333-3333-333333333333"
	granteeID = "44444444-4444-4444-4444-444444444444"
	otherID   = "22222222-2222-2222-2222-222222222222"

	grantStmt       = "GRANT `team-a_reader` TO `team-a_api`"
	grantAdminStmt  = "GRANT `team-a_reader` TO `team-a_api` WITH ADMIN OPTION"
	revokeAdminStmt = "REVOKE ADMIN OPTION FOR `team-a_reader` FROM `team-a_api`"
	revokeStmt      = "REVOKE `team-a_reader` FROM `team-a_api`"
)

var (
	sharedPC = &xpv2.ProviderConfigReference{Kind: "ClusterProviderConfig", Name: "shared"}
	ownPC    = &xpv2.ProviderConfigReference{Kind: "ProviderConfig", Name: "own"}
)

// --- test doubles ---

type recorder struct {
	mu     sync.Mutex
	events []event.Event
}

func (r *recorder) Event(_ runtime.Object, e event.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *recorder) WithAnnotations(...string) event.Recorder { return r }

func (r *recorder) reasons() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []string{}
	for _, e := range r.events {
		out = append(out, string(e.Reason))
	}
	return out
}

type resolverFn func(context.Context, resource.ModernManaged) (*providerconfig.Resolved, error)

func (f resolverFn) Resolve(ctx context.Context, mg resource.ModernManaged) (*providerconfig.Resolved, error) {
	return f(ctx, mg)
}

type nopTracker struct{}

func (nopTracker) Track(context.Context, resource.ModernManaged) error { return nil }

type cacheFn func(context.Context, ch.Key, ch.Config) (ch.Client, error)

func (f cacheFn) Acquire(ctx context.Context, k ch.Key, c ch.Config) (ch.Client, error) {
	return f(ctx, k, c)
}

// --- fixtures ---

func kubeWith(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.SchemeBuilder.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return clientfake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
}

// sharedPolicy is the shared ClusterProviderConfig of design 3.3, which
// grants roles of the namespace only.
func sharedPolicy() v1alpha1.ClusterPolicy {
	return v1alpha1.ClusterPolicy{
		AllowedNamespaces: []string{"team-.*"},
		UserNamePatterns:  []string{"{namespace}_[a-z0-9_]+"},
		RoleNamePatterns:  []string{"{namespace}_[a-z0-9_]+"},
		GrantableRoles:    []string{"{namespace}_[a-z0-9_]+", "analytics_reader"},
	}
}

func withAdminOption() v1alpha1.ClusterPolicy {
	p := sharedPolicy()
	p.AllowAdminOption = true
	return p
}

func clusterResolved(t *testing.T, cp v1alpha1.ClusterPolicy) *providerconfig.Resolved {
	t.Helper()
	p, err := providerconfig.CompilePolicy(cp)
	if err != nil {
		t.Fatal(err)
	}
	return &providerconfig.Resolved{
		Kind: "ClusterProviderConfig", Name: "shared",
		CacheKey:        ch.Key{Identity: "ClusterProviderConfig//shared", Version: "v"},
		Endpoint:        providerconfig.Endpoint{Host: "clickhouse.example.com", Port: 9440, Protocol: v1alpha1.ProtocolNative, TLS: true},
		Policy:          p,
		AdoptionAllowed: p.AdoptionAllowed(),
	}
}

func namespacedResolved() *providerconfig.Resolved {
	return &providerconfig.Resolved{
		Kind: "ProviderConfig", Name: "own",
		CacheKey:        ch.Key{Identity: "ProviderConfig/team-a/own", Version: "v"},
		Endpoint:        providerconfig.Endpoint{Host: "clickhouse.team-a.svc", Port: 9440, Protocol: v1alpha1.ProtocolNative, TLS: true},
		AdoptionAllowed: true,
	}
}

// The owners of the grantees: MRs of the namespace with a valid claim on the
// grantee's UUID, on the RoleGrant's ProviderConfig.
func apiOwner() client.Object {
	return accesstest.OwnedUser(ns, "api", userName, userID, sharedPC)
}

func writerOwner() client.Object {
	return accesstest.OwnedRole(ns, "writer", granteeRole, granteeID, sharedPC)
}

type testExternal struct {
	*External
	db   *fake.DB
	cl   *fake.Client
	rec  *recorder
	kube client.Client
}

func newExternal(t *testing.T, db *fake.DB, res *providerconfig.Resolved, objs ...client.Object) *testExternal {
	t.Helper()
	return newExternalFor(t, db, fake.Upstream253, res, objs...)
}

func newExternalFor(t *testing.T, db *fake.DB, p fake.Profile, res *providerconfig.Resolved, objs ...client.Object) *testExternal {
	t.Helper()
	if res == nil {
		res = clusterResolved(t, sharedPolicy())
	}
	cl := fake.NewClientFor(db, p)
	kube := kubeWith(t, objs...)
	rec := &recorder{}
	env := &access.Env{Client: cl, Resolved: res, Kube: kube, Reader: kube, Recorder: rec, Journal: ownership.NewJournal()}
	ext, ok := NewExternal(env).(*External)
	if !ok {
		t.Fatal("NewExternal must return *External")
	}
	return &testExternal{External: ext, db: db, cl: cl, rec: rec, kube: kube}
}

type rgOpt func(*v1alpha1.RoleGrant)

// newRoleGrant returns a RoleGrant of team-a_reader to the user team-a_api.
func newRoleGrant(opts ...rgOpt) *v1alpha1.RoleGrant {
	g := &v1alpha1.RoleGrant{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "api-reader", UID: "rg-uid"}}
	g.Spec.ProviderConfigReference = sharedPC
	g.Spec.ForProvider.MemberOf = new(memberOf)
	g.Spec.ForProvider.User = new(userName)
	g.Spec.ForProvider.AdminOption = new(false)
	for _, o := range opts {
		o(g)
	}
	return g
}

func toRole(name string) rgOpt {
	return func(g *v1alpha1.RoleGrant) { g.Spec.ForProvider.User, g.Spec.ForProvider.Role = nil, new(name) }
}

func toUser(name string) rgOpt {
	return func(g *v1alpha1.RoleGrant) { g.Spec.ForProvider.User, g.Spec.ForProvider.Role = new(name), nil }
}

func of(role string) rgOpt {
	return func(g *v1alpha1.RoleGrant) { g.Spec.ForProvider.MemberOf = new(role) }
}

func adminOption(v bool) rgOpt {
	return func(g *v1alpha1.RoleGrant) { g.Spec.ForProvider.AdminOption = new(v) }
}

func deleting() rgOpt {
	return func(g *v1alpha1.RoleGrant) {
		now := metav1.Now()
		g.SetDeletionTimestamp(&now)
		g.SetFinalizers([]string{"finalizer.managedresource.crossplane.io"})
	}
}

func policies(p ...xpv2.ManagementAction) rgOpt {
	return func(g *v1alpha1.RoleGrant) { g.SetManagementPolicies(p) }
}

var observeOnly = policies(xpv2.ManagementActionObserve)

func readyStatus(g *v1alpha1.RoleGrant) string { return string(g.GetCondition(xpv2.TypeReady).Status) }

// server returns a DB with the user team-a_api, the role team-a_writer (a
// grantee) and the role team-a_reader (memberOf), all SQL entities, and no
// memberships.
func server() *fake.DB {
	return fake.NewDB().
		WithUser(userName, userID, ch.StorageLocalDirectory).
		WithRole(granteeRole, granteeID, ch.StorageLocalDirectory).
		WithRole(memberOf, memberID, ch.StorageLocalDirectory).
		On(sqlx.QueryUserRoleGrant, fake.Rows()).
		On(sqlx.QueryRoleRoleGrant, fake.Rows())
}

// member scripts the user's (or the role's) membership in team-a_reader.
func member(db *fake.DB, roleGrantee bool, admin, isDefault uint8) *fake.DB {
	return db.On(sqlx.QueryRoleGrant(roleGrantee), fake.Row(admin, isDefault))
}

// --- Observe ---

func TestObserve(t *testing.T) {
	cases := map[string]struct {
		mr         *v1alpha1.RoleGrant
		db         *fake.DB
		res        *providerconfig.Resolved
		objs       []client.Object
		want       managed.ExternalObservation
		wantStatus *v1alpha1.RoleGrantObservation
		reason     reason.Reason
		anyErr     bool
		ready      string
		events     []string
		noQuery    bool
	}{
		"Exists": {
			mr: newRoleGrant(), db: member(server(), false, 0, 1), objs: []client.Object{apiOwner()},
			want:       managed.ExternalObservation{ResourceExists: true, ResourceUpToDate: true},
			wantStatus: &v1alpha1.RoleGrantObservation{Grantee: "user:" + userName, IsDefault: true},
			ready:      "True",
		},
		"Absent": {
			mr: newRoleGrant(), db: server(), objs: []client.Object{apiOwner()},
			want:       managed.ExternalObservation{ResourceExists: false},
			wantStatus: &v1alpha1.RoleGrantObservation{Grantee: "user:" + userName},
		},
		// Someone added the admin option out of band.
		"AdminOptionDrift": {
			mr: newRoleGrant(), db: member(server(), false, 1, 1), objs: []client.Object{apiOwner()},
			want:       managed.ExternalObservation{ResourceExists: true, ResourceUpToDate: false},
			wantStatus: &v1alpha1.RoleGrantObservation{Grantee: "user:" + userName, AdminOption: true, IsDefault: true},
			ready:      "True",
		},
		"AdminOptionMissing": {
			mr: newRoleGrant(adminOption(true)), db: member(server(), false, 0, 0), objs: []client.Object{apiOwner()},
			res:  clusterResolved(t, withAdminOption()),
			want: managed.ExternalObservation{ResourceExists: true, ResourceUpToDate: false}, ready: "True",
		},
		"AdminOptionHeld": {
			mr: newRoleGrant(adminOption(true)), db: member(server(), false, 1, 1), objs: []client.Object{apiOwner()},
			res:  clusterResolved(t, withAdminOption()),
			want: managed.ExternalObservation{ResourceExists: true, ResourceUpToDate: true}, ready: "True",
		},
		"RoleGrantee": {
			mr: newRoleGrant(toRole(granteeRole)), db: member(server(), true, 0, 1), objs: []client.Object{writerOwner()},
			want:       managed.ExternalObservation{ResourceExists: true, ResourceUpToDate: true},
			wantStatus: &v1alpha1.RoleGrantObservation{Grantee: "role:" + granteeRole, IsDefault: true},
			ready:      "True",
		},
		// A role from users.d may be granted when grantableRoles lists it.
		"MemberOfFromUsersXML": {
			mr: newRoleGrant(of("analytics_reader")),
			db: member(server().WithRole("analytics_reader", otherID, ch.StorageUsersXML), false, 0, 1), objs: []client.Object{apiOwner()},
			want: managed.ExternalObservation{ResourceExists: true, ResourceUpToDate: true}, ready: "True",
		},
		"MemberOfUnresolved": {
			mr: newRoleGrant(func(g *v1alpha1.RoleGrant) { g.Spec.ForProvider.MemberOf = nil }), db: server(),
			reason: reason.DependencyMissing, ready: "False", noQuery: true,
		},
		"MemberOfUnresolvedWhileDeleting": {
			mr: newRoleGrant(func(g *v1alpha1.RoleGrant) { g.Spec.ForProvider.MemberOf = nil }, deleting()), db: server(),
			events: []string{"Orphaned"}, noQuery: true,
		},
		"GranteeUnresolved": {
			mr: newRoleGrant(func(g *v1alpha1.RoleGrant) { g.Spec.ForProvider.User = nil }), db: server(),
			reason: reason.DependencyMissing, noQuery: true,
		},
		"SelfGrant": {
			mr: newRoleGrant(toRole(memberOf)), db: server(),
			reason: reason.InvalidSpec, ready: "False", noQuery: true,
		},
		"GranteeOutsidePolicy": {
			mr: newRoleGrant(toUser("team-b_api")), db: server(),
			reason: reason.PolicyViolation, ready: "False", noQuery: true,
		},
		// Roles from users.d, and roles of other namespaces, may be granted
		// only if grantableRoles lists them.
		"MemberOfNotGrantable": {
			mr: newRoleGrant(of("super_admin")), db: server(),
			reason: reason.PolicyViolation, ready: "False", noQuery: true,
		},
		"MemberOfOfAnotherNamespace": {
			mr: newRoleGrant(of("team-b_reader")), db: server(),
			reason: reason.PolicyViolation, noQuery: true,
		},
		// Name checks apply to observe-only MRs too.
		"MemberOfNotGrantableObserveOnly": {
			mr: newRoleGrant(of("super_admin"), observeOnly), db: server(),
			reason: reason.PolicyViolation, noQuery: true,
		},
		"OutsidePolicyWhileDeleting": {
			mr: newRoleGrant(of("super_admin"), deleting()), db: server(), objs: []client.Object{apiOwner()},
			events: []string{"OrphanedByPolicy"}, noQuery: true,
		},
		"AdminOptionNotAllowed": {
			mr: newRoleGrant(adminOption(true)), db: server(), objs: []client.Object{apiOwner()},
			reason: reason.PolicyViolation, ready: "False", noQuery: true,
		},
		// Write checks do not apply to observe-only MRs, nor while deleting.
		"AdminOptionNotAllowedObserveOnly": {
			mr: newRoleGrant(adminOption(true), observeOnly), db: member(server(), false, 1, 1),
			want: managed.ExternalObservation{ResourceExists: true, ResourceUpToDate: true}, ready: "True",
		},
		"AdminOptionNotAllowedWhileDeleting": {
			mr: newRoleGrant(adminOption(true), deleting()), db: member(server(), false, 1, 1), objs: []client.Object{apiOwner()},
			want: managed.ExternalObservation{ResourceExists: true},
		},
		"NamespacedConfig": {
			mr:   newRoleGrant(of("anything"), toUser("anyone"), adminOption(true), func(g *v1alpha1.RoleGrant) { g.Spec.ProviderConfigReference = ownPC }),
			db:   member(fake.NewDB().WithUser("anyone", userID, ch.StorageLocalDirectory).WithRole("anything", memberID, ch.StorageLocalDirectory), false, 1, 1),
			res:  namespacedResolved(),
			objs: []client.Object{accesstest.OwnedUser(ns, "anyone", "anyone", userID, ownPC)},
			want: managed.ExternalObservation{ResourceExists: true, ResourceUpToDate: true}, ready: "True",
		},
		// D-30: the provider never grants to a user it did not create or
		// adopt.
		"GranteeNotOwned": {
			mr: newRoleGrant(), db: server(),
			reason: reason.NotOwned, ready: "False",
		},
		"GranteeOwnedThroughAnotherConfig": {
			mr: newRoleGrant(), db: server(), objs: []client.Object{accesstest.OwnedUser(ns, "api", userName, userID, ownPC)},
			reason: reason.NotOwned,
		},
		"GranteeOwnerClaimsOldUUID": {
			mr: newRoleGrant(), db: server(), objs: []client.Object{accesstest.OwnedUser(ns, "api", userName, otherID, sharedPC)},
			reason: reason.NotOwned,
		},
		// Observe-only MRs report memberships of any grantee the name policy
		// allows.
		"GranteeNotOwnedObserveOnly": {
			mr: newRoleGrant(observeOnly), db: member(server(), false, 0, 1),
			want: managed.ExternalObservation{ResourceExists: true, ResourceUpToDate: true}, ready: "True",
		},
		"GranteeNotOwnedWhileDeleting": {
			mr: newRoleGrant(deleting()), db: member(server(), false, 0, 1),
			events: []string{"Orphaned"},
		},
		"GranteeMissing": {
			mr: newRoleGrant(), db: server().RemoveUser(userName), objs: []client.Object{apiOwner()},
			reason: reason.DependencyMissing, ready: "False",
		},
		"GranteeMissingObserveOnly": {
			mr: newRoleGrant(observeOnly), db: server().RemoveUser(userName),
		},
		"GranteeMissingWhileDeleting": {
			mr: newRoleGrant(deleting()), db: server().RemoveUser(userName), objs: []client.Object{apiOwner()},
		},
		"GranteeFromUsersXML": {
			mr: newRoleGrant(), db: server().WithUser(userName, userID, ch.StorageUsersXML), objs: []client.Object{apiOwner()},
			reason: reason.ReadOnlyStorage, ready: "False",
		},
		"GranteeFromUsersXMLObserveOnly": {
			mr: newRoleGrant(observeOnly), db: member(server().WithUser(userName, userID, ch.StorageUsersXML), false, 0, 1),
			want: managed.ExternalObservation{ResourceExists: true, ResourceUpToDate: true}, ready: "True",
		},
		"GranteeFromUsersXMLWhileDeleting": {
			mr: newRoleGrant(deleting()), db: member(server().WithUser(userName, userID, ch.StorageUsersXML), false, 0, 1), objs: []client.Object{apiOwner()},
			events: []string{"Orphaned"},
		},
		"GranteeIsTheProvider": {
			mr:     newRoleGrant(toUser(provider), func(g *v1alpha1.RoleGrant) { g.Spec.ProviderConfigReference = ownPC }),
			db:     server().WithUser(provider, otherID, ch.StorageLocalDirectory),
			res:    namespacedResolved(),
			objs:   []client.Object{accesstest.OwnedUser(ns, "me", provider, otherID, ownPC)},
			reason: reason.SelfModification, ready: "False",
		},
		// GRANT and REVOKE resolve grantees user-first.
		"RoleGranteeShadowedByUser": {
			mr: newRoleGrant(toRole(granteeRole)), db: server().WithUser(granteeRole, otherID, ch.StorageLocalDirectory), objs: []client.Object{writerOwner()},
			reason: reason.AmbiguousGrantee, ready: "False",
		},
		"RoleGranteeShadowedByUserWhileDeleting": {
			mr: newRoleGrant(toRole(granteeRole), deleting()), db: server().WithUser(granteeRole, otherID, ch.StorageLocalDirectory), objs: []client.Object{writerOwner()},
			events: []string{"Orphaned"},
		},
		// A user grantee is not ambiguous: a same-named role cannot take a
		// grant meant for the user.
		"UserGranteeWithSameNamedRole": {
			mr: newRoleGrant(), db: member(server().WithRole(userName, otherID, ch.StorageLocalDirectory), false, 0, 1), objs: []client.Object{apiOwner()},
			want: managed.ExternalObservation{ResourceExists: true, ResourceUpToDate: true}, ready: "True",
		},
		"MemberOfMissing": {
			mr: newRoleGrant(), db: server().RemoveRole(memberOf), objs: []client.Object{apiOwner()},
			reason: reason.DependencyMissing, ready: "False",
		},
		"MemberOfMissingObserveOnly": {
			mr: newRoleGrant(observeOnly), db: server().RemoveRole(memberOf),
		},
		// DROP ROLE removes the role from every grantee.
		"MemberOfMissingWhileDeleting": {
			mr: newRoleGrant(deleting()), db: server().RemoveRole(memberOf), objs: []client.Object{apiOwner()},
		},
		"MembershipReadDenied": {
			mr: newRoleGrant(), db: server().On(sqlx.QueryUserRoleGrant, fake.Error(fake.Exception(497, "necessary to have the grant SELECT ON system.role_grants"))),
			objs:   []client.Object{apiOwner()},
			reason: reason.PermissionDenied,
		},
		"MembershipTransportErrorIsNotNotFound": {
			mr: newRoleGrant(), db: server().On(sqlx.QueryUserRoleGrant, fake.Error(&ch.TransportError{Category: "timeout"})),
			objs:   []client.Object{apiOwner()},
			anyErr: true,
		},
		"GranteeReadDenied": {
			mr: newRoleGrant(), db: server().On(sqlx.QueryUserEntity, fake.Error(fake.Exception(497, "necessary to have the grant SHOW USERS"))),
			objs:   []client.Object{apiOwner()},
			reason: reason.PermissionDenied,
		},
		"ExistsWhileDeleting": {
			mr: newRoleGrant(deleting()), db: member(server(), false, 0, 1), objs: []client.Object{apiOwner()},
			want: managed.ExternalObservation{ResourceExists: true},
		},
		// D-26: nothing is revoked without Delete in the management policies,
		// whatever crossplane-runtime does.
		"NoDeletePolicyWhileDeleting": {
			mr: newRoleGrant(deleting(), policies(xpv2.ManagementActionObserve, xpv2.ManagementActionCreate, xpv2.ManagementActionUpdate)),
			db: member(server(), false, 0, 1), objs: []client.Object{apiOwner()},
			events: []string{"Orphaned"}, noQuery: true,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e := newExternal(t, tc.db, tc.res, append([]client.Object{tc.mr.DeepCopy()}, tc.objs...)...)
			got, err := e.Observe(context.Background(), tc.mr)
			switch {
			case tc.anyErr:
				if err == nil || got.ResourceExists {
					t.Fatalf("an error must be returned, never 'not found': %+v, %v", got, err)
				}
			case tc.reason != "":
				if !reason.Is(err, tc.reason) {
					t.Fatalf("Observe() error = %v, want %s", err, tc.reason)
				}
			case err != nil:
				t.Fatalf("Observe() error = %v", err)
			default:
				if diff := cmp.Diff(tc.want, got); diff != "" {
					t.Errorf("Observe() -want +got:\n%s", diff)
				}
			}
			if tc.wantStatus != nil {
				if diff := cmp.Diff(*tc.wantStatus, tc.mr.Status.AtProvider); diff != "" {
					t.Errorf("atProvider -want +got:\n%s", diff)
				}
			}
			if tc.ready != "" && readyStatus(tc.mr) != tc.ready {
				t.Errorf("Ready = %q, want %q", readyStatus(tc.mr), tc.ready)
			}
			if diff := cmp.Diff(append([]string{}, tc.events...), e.rec.reasons()); diff != "" {
				t.Errorf("events -want +got:\n%s", diff)
			}
			if tc.noQuery && len(tc.db.Seen) > 0 {
				t.Errorf("no query may run, ran %v", tc.db.Seen)
			}
			if len(tc.db.Execs) > 0 {
				t.Errorf("Observe must never write, ran %v", tc.db.Execs)
			}
		})
	}
}

// The membership query gets the names as server-side parameters.
func TestObserveParameters(t *testing.T) {
	db := member(server(), false, 0, 1)
	e := newExternal(t, db, nil, apiOwner())
	if _, err := e.Observe(context.Background(), newRoleGrant()); err != nil {
		t.Fatal(err)
	}
	for i, q := range db.Seen {
		if q == sqlx.QueryUserRoleGrant {
			if diff := cmp.Diff(ch.Params{"memberOf": memberOf, "grantee": userName}, db.Params[i]); diff != "" {
				t.Errorf("parameters -want +got:\n%s", diff)
			}
			return
		}
	}
	t.Fatalf("the membership was not read: %v", db.Seen)
}

// --- Create ---

func TestCreate(t *testing.T) {
	cases := map[string]struct {
		mr      *v1alpha1.RoleGrant
		db      *fake.DB
		res     *providerconfig.Resolved
		objs    []client.Object
		observe bool // run Observe first, as crossplane-runtime does
		// between runs after Observe and before Create.
		between   func(t *testing.T, e *testExternal)
		wantStmts []string
		reason    reason.Reason
	}{
		"Grant": {
			mr: newRoleGrant(), db: server(), objs: []client.Object{apiOwner()}, observe: true,
			wantStmts: []string{grantStmt},
		},
		"GrantWithAdminOption": {
			mr: newRoleGrant(adminOption(true)), db: server(), objs: []client.Object{apiOwner()}, observe: true,
			res:       clusterResolved(t, withAdminOption()),
			wantStmts: []string{grantAdminStmt},
		},
		"GrantToRole": {
			mr: newRoleGrant(toRole(granteeRole)), db: server(), objs: []client.Object{writerOwner()}, observe: true,
			wantStmts: []string{"GRANT `team-a_reader` TO `team-a_writer`"},
		},
		"AdversarialNames": {
			mr: newRoleGrant(of("r`x\\y'ü"), toUser("svc@my-project.iam.example.com"), func(g *v1alpha1.RoleGrant) { g.Spec.ProviderConfigReference = ownPC }),
			db: fake.NewDB().
				WithUser("svc@my-project.iam.example.com", userID, ch.StorageLocalDirectory).
				WithRole("r`x\\y'ü", memberID, ch.StorageLocalDirectory).
				On(sqlx.QueryUserRoleGrant, fake.Rows()),
			res:       namespacedResolved(),
			objs:      []client.Object{accesstest.OwnedUser(ns, "svc", "svc@my-project.iam.example.com", userID, ownPC)},
			observe:   true,
			wantStmts: []string{"GRANT `r\\`x\\\\y'ü` TO `svc@my-project.iam.example.com`"},
		},
		// crossplane-runtime never calls Create for an observe-only MR; if
		// it did, nothing is written (D-26).
		"ObserveOnly": {
			mr: newRoleGrant(observeOnly), db: server(), objs: []client.Object{apiOwner()}, observe: true,
			reason: reason.Internal,
		},
		// Create is never called without Observe; if it were, nothing is
		// written.
		"WithoutObserve": {
			mr: newRoleGrant(), db: server(), objs: []client.Object{apiOwner()},
			reason: reason.Internal,
		},
		"GranteeChangedSinceObserve": {
			mr: newRoleGrant(), db: server(), objs: []client.Object{apiOwner()}, observe: true,
			between: func(t *testing.T, e *testExternal) {
				t.Helper()
				e.observed.desired.grantee.Name = "team-a_other"
			},
			reason: reason.Internal,
		},
		// Dropped and re-created by someone else since Observe: the owner's
		// claim names the old UUID.
		"GranteeReplacedSinceObserve": {
			mr: newRoleGrant(), db: server(), objs: []client.Object{apiOwner()}, observe: true,
			between: func(_ *testing.T, e *testExternal) { e.db.WithUser(userName, otherID, ch.StorageLocalDirectory) },
			reason:  reason.NotOwned,
		},
		// ... and the owner adopted the new user: still not the user that
		// Observe bound.
		"GranteeReplacedAndAdoptedSinceObserve": {
			mr: newRoleGrant(), db: server(), objs: []client.Object{apiOwner()}, observe: true,
			between: func(t *testing.T, e *testExternal) {
				t.Helper()
				e.db.WithUser(userName, otherID, ch.StorageLocalDirectory)
				u := &v1alpha1.User{}
				if err := e.kube.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "api"}, u); err != nil {
					t.Fatal(err)
				}
				meta.AddAnnotations(u, map[string]string{v1alpha1.AnnotationOwnedID: otherID})
				if err := e.kube.Update(context.Background(), u); err != nil {
					t.Fatal(err)
				}
			},
			reason: reason.Replaced,
		},
		"GranteeReleasedSinceObserve": {
			mr: newRoleGrant(), db: server(), objs: []client.Object{apiOwner()}, observe: true,
			between: func(t *testing.T, e *testExternal) {
				t.Helper()
				if err := e.kube.Delete(context.Background(), apiOwner()); err != nil {
					t.Fatal(err)
				}
			},
			reason: reason.NotOwned,
		},
		"GranteeDroppedSinceObserve": {
			mr: newRoleGrant(), db: server(), objs: []client.Object{apiOwner()}, observe: true,
			between: func(_ *testing.T, e *testExternal) { e.db.RemoveUser(userName) },
			reason:  reason.DependencyMissing,
		},
		"RoleGranteeShadowedSinceObserve": {
			mr: newRoleGrant(toRole(granteeRole)), db: server(), objs: []client.Object{writerOwner()}, observe: true,
			between: func(_ *testing.T, e *testExternal) { e.db.WithUser(granteeRole, otherID, ch.StorageLocalDirectory) },
			reason:  reason.AmbiguousGrantee,
		},
		"NotWritable": {
			mr: newRoleGrant(), db: server(), objs: []client.Object{apiOwner()}, observe: true,
			between: func(_ *testing.T, e *testExternal) { e.cl.Servers = []string{"s1", "s2"} },
			reason:  reason.AccessStorageNotReplicated,
		},
		"RoleDroppedBeforeGrant": {
			mr: newRoleGrant(), db: server(), objs: []client.Object{apiOwner()}, observe: true,
			between: func(_ *testing.T, e *testExternal) {
				e.db.OnExec(grantStmt, fake.Exception(511, "There is no role `team-a_reader` in user directories"))
			},
			wantStmts: []string{grantStmt},
			reason:    reason.DependencyMissing,
		},
		"GrantDenied": {
			mr: newRoleGrant(), db: server(), objs: []client.Object{apiOwner()}, observe: true,
			between: func(_ *testing.T, e *testExternal) {
				e.db.OnExec(grantStmt, fake.Exception(497, "necessary to have the grant ROLE ADMIN ON *.*"))
			},
			wantStmts: []string{grantStmt},
			reason:    reason.PermissionDenied,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e := newExternal(t, tc.db, tc.res, append([]client.Object{tc.mr.DeepCopy()}, tc.objs...)...)
			if tc.observe {
				if obs, err := e.Observe(context.Background(), tc.mr); err != nil || obs.ResourceExists {
					t.Fatalf("Observe() = %+v, %v", obs, err)
				}
			}
			if tc.between != nil {
				tc.between(t, e)
			}
			_, err := e.Create(context.Background(), tc.mr)
			if tc.reason != "" {
				if !reason.Is(err, tc.reason) {
					t.Fatalf("Create() error = %v, want %s", err, tc.reason)
				}
			} else if err != nil {
				t.Fatalf("Create() error = %v", err)
			}
			if diff := cmp.Diff(tc.wantStmts, tc.db.Execs); diff != "" {
				t.Errorf("statements -want +got:\n%s", diff)
			}
		})
	}
}

// --- Update ---

func TestUpdate(t *testing.T) {
	cases := map[string]struct {
		mr        *v1alpha1.RoleGrant
		db        *fake.DB
		res       *providerconfig.Resolved
		objs      []client.Object
		between   func(t *testing.T, e *testExternal)
		wantStmts []string
		reason    reason.Reason
	}{
		"AddAdminOption": {
			mr: newRoleGrant(adminOption(true)), db: member(server(), false, 0, 1), objs: []client.Object{apiOwner()},
			res:       clusterResolved(t, withAdminOption()),
			wantStmts: []string{grantAdminStmt},
		},
		// A plain GRANT would keep the admin option.
		"RemoveAdminOption": {
			mr: newRoleGrant(), db: member(server(), false, 1, 1), objs: []client.Object{apiOwner()},
			wantStmts: []string{revokeAdminStmt},
		},
		"NoUpdatePolicy": {
			mr: newRoleGrant(policies(xpv2.ManagementActionObserve, xpv2.ManagementActionCreate, xpv2.ManagementActionDelete)),
			db: member(server(), false, 1, 1), objs: []client.Object{apiOwner()},
			reason: reason.Internal,
		},
		"GranteeReleasedSinceObserve": {
			mr: newRoleGrant(), db: member(server(), false, 1, 1), objs: []client.Object{apiOwner()},
			between: func(t *testing.T, e *testExternal) {
				t.Helper()
				if err := e.kube.Delete(context.Background(), apiOwner()); err != nil {
					t.Fatal(err)
				}
			},
			reason: reason.NotOwned,
		},
		"GranteeDroppedSinceObserve": {
			mr: newRoleGrant(), db: member(server(), false, 1, 1), objs: []client.Object{apiOwner()},
			between: func(_ *testing.T, e *testExternal) { e.db.RemoveUser(userName) },
			reason:  reason.DependencyMissing,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e := newExternal(t, tc.db, tc.res, append([]client.Object{tc.mr.DeepCopy()}, tc.objs...)...)
			obs, err := e.Observe(context.Background(), tc.mr)
			if err != nil || !obs.ResourceExists || obs.ResourceUpToDate {
				t.Fatalf("Observe() = %+v, %v", obs, err)
			}
			if tc.between != nil {
				tc.between(t, e)
			}
			_, err = e.Update(context.Background(), tc.mr)
			if tc.reason != "" {
				if !reason.Is(err, tc.reason) {
					t.Fatalf("Update() error = %v, want %s", err, tc.reason)
				}
			} else if err != nil {
				t.Fatalf("Update() error = %v", err)
			}
			if diff := cmp.Diff(tc.wantStmts, tc.db.Execs); diff != "" {
				t.Errorf("statements -want +got:\n%s", diff)
			}
		})
	}
	// Without Observe nothing is written.
	db := member(server(), false, 1, 1)
	e := newExternal(t, db, nil, apiOwner())
	if _, err := e.Update(context.Background(), newRoleGrant()); !reason.Is(err, reason.Internal) || len(db.Execs) > 0 {
		t.Errorf("Update() without Observe = %v, ran %v", err, db.Execs)
	}
}

// --- Delete ---

func TestDelete(t *testing.T) {
	cases := map[string]struct {
		mr        *v1alpha1.RoleGrant
		db        *fake.DB
		objs      []client.Object
		observe   bool
		between   func(t *testing.T, e *testExternal)
		wantStmts []string
		reason    reason.Reason
	}{
		"Revoke": {
			mr: newRoleGrant(deleting()), db: member(server(), false, 0, 1), objs: []client.Object{apiOwner()}, observe: true,
			wantStmts: []string{revokeStmt},
		},
		// REVOKE removes the admin option with the membership.
		"RevokeWithAdminOption": {
			mr: newRoleGrant(adminOption(true), deleting()), db: member(server(), false, 1, 1), objs: []client.Object{apiOwner()}, observe: true,
			wantStmts: []string{revokeStmt},
		},
		"RevokeFromRole": {
			mr: newRoleGrant(toRole(granteeRole), deleting()), db: member(server(), true, 0, 1), objs: []client.Object{writerOwner()}, observe: true,
			wantStmts: []string{"REVOKE `team-a_reader` FROM `team-a_writer`"},
		},
		// Nothing left to revoke.
		"GranteeDroppedSinceObserve": {
			mr: newRoleGrant(deleting()), db: member(server(), false, 0, 1), objs: []client.Object{apiOwner()}, observe: true,
			between: func(_ *testing.T, e *testExternal) { e.db.RemoveUser(userName) },
		},
		// The user behind the name is not the one Observe bound: nothing is
		// revoked from it; the next Observe orphans the MR.
		"GranteeReplacedSinceObserve": {
			mr: newRoleGrant(deleting()), db: member(server(), false, 0, 1), objs: []client.Object{apiOwner()}, observe: true,
			between: func(_ *testing.T, e *testExternal) { e.db.WithUser(userName, otherID, ch.StorageLocalDirectory) },
			reason:  reason.NotOwned,
		},
		"GranteeReleasedSinceObserve": {
			mr: newRoleGrant(deleting()), db: member(server(), false, 0, 1), objs: []client.Object{apiOwner()}, observe: true,
			between: func(t *testing.T, e *testExternal) {
				t.Helper()
				if err := e.kube.Delete(context.Background(), apiOwner()); err != nil {
					t.Fatal(err)
				}
			},
			reason: reason.NotOwned,
		},
		"AlreadyGone": {
			mr: newRoleGrant(deleting()), db: member(server(), false, 0, 1), objs: []client.Object{apiOwner()}, observe: true,
			between: func(_ *testing.T, e *testExternal) {
				e.db.OnExec(revokeStmt, fake.Exception(511, "There is no role `team-a_reader` in user directories"))
			},
			wantStmts: []string{revokeStmt},
		},
		"RevokeDenied": {
			mr: newRoleGrant(deleting()), db: member(server(), false, 0, 1), objs: []client.Object{apiOwner()}, observe: true,
			between: func(_ *testing.T, e *testExternal) {
				e.db.OnExec(revokeStmt, fake.Exception(497, "necessary to have the grant ROLE ADMIN ON *.*"))
			},
			wantStmts: []string{revokeStmt},
			reason:    reason.PermissionDenied,
		},
		"WithoutObserve": {
			mr: newRoleGrant(deleting()), db: member(server(), false, 0, 1), objs: []client.Object{apiOwner()},
			reason: reason.Internal,
		},
		"NoDeletePolicy": {
			mr: newRoleGrant(deleting(), observeOnly), db: member(server(), false, 0, 1), objs: []client.Object{apiOwner()},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e := newExternal(t, tc.db, nil, append([]client.Object{tc.mr.DeepCopy()}, tc.objs...)...)
			if tc.observe {
				if obs, err := e.Observe(context.Background(), tc.mr); err != nil || !obs.ResourceExists {
					t.Fatalf("Observe() = %+v, %v", obs, err)
				}
			}
			if tc.between != nil {
				tc.between(t, e)
			}
			_, err := e.Delete(context.Background(), tc.mr)
			if tc.reason != "" {
				if !reason.Is(err, tc.reason) {
					t.Fatalf("Delete() error = %v, want %s", err, tc.reason)
				}
			} else if err != nil {
				t.Fatalf("Delete() error = %v", err)
			}
			if diff := cmp.Diff(tc.wantStmts, tc.db.Execs); diff != "" {
				t.Errorf("statements -want +got:\n%s", diff)
			}
		})
	}
}

// --- lifecycle on every supported server version ---

// memberships is a fake server's system.role_grants for one role: grantee
// name -> admin option.
type memberships struct {
	mu   sync.Mutex
	rows map[string]bool
}

func (m *memberships) script(db *fake.DB, role string) {
	m.rows = map[string]bool{}
	db.OnFunc(sqlx.QueryUserRoleGrant, func(p ch.Params) fake.Result {
		m.mu.Lock()
		defer m.mu.Unlock()
		admin, ok := m.rows[p["grantee"]]
		if !ok || p["memberOf"] != role {
			return fake.Rows()
		}
		a := uint8(0)
		if admin {
			a = 1
		}
		return fake.Row(a, uint8(1))
	})
	db.ExecFn = func(stmt string) error {
		m.mu.Lock()
		defer m.mu.Unlock()
		switch stmt {
		case grantStmt:
			if _, ok := m.rows[userName]; !ok {
				m.rows[userName] = false // a plain GRANT keeps an existing admin option
			}
		case grantAdminStmt:
			m.rows[userName] = true
		case revokeAdminStmt:
			if _, ok := m.rows[userName]; ok {
				m.rows[userName] = false
			}
		case revokeStmt:
			delete(m.rows, userName)
		}
		return nil
	}
}

func TestLifecycle(t *testing.T) {
	for _, p := range fake.Profiles {
		t.Run(p.Version, func(t *testing.T) {
			ctx := context.Background()
			db := fake.NewDB().
				WithUser(userName, userID, ch.StorageLocalDirectory).
				WithRole(memberOf, memberID, ch.StorageLocalDirectory)
			m := &memberships{}
			m.script(db, memberOf)
			mr := newRoleGrant(adminOption(true))
			// Each reconcile connects a new external client.
			ext := func() *testExternal {
				return newExternalFor(t, db, p, clusterResolved(t, withAdminOption()), mr.DeepCopy(), apiOwner())
			}

			e := ext()
			if obs, err := e.Observe(ctx, mr); err != nil || obs.ResourceExists {
				t.Fatalf("before create: %+v, %v", obs, err)
			}
			if _, err := e.Create(ctx, mr); err != nil {
				t.Fatalf("Create() = %v", err)
			}
			e = ext()
			if obs, err := e.Observe(ctx, mr); err != nil || !obs.ResourceExists || !obs.ResourceUpToDate || !mr.Status.AtProvider.AdminOption {
				t.Fatalf("after create: %+v, %v, %+v", obs, err, mr.Status.AtProvider)
			}

			// The admin option is no longer wanted.
			mr.Spec.ForProvider.AdminOption = new(false)
			e = ext()
			if obs, err := e.Observe(ctx, mr); err != nil || obs.ResourceUpToDate {
				t.Fatalf("admin option drift: %+v, %v", obs, err)
			}
			if _, err := e.Update(ctx, mr); err != nil {
				t.Fatalf("Update() = %v", err)
			}
			e = ext()
			if obs, err := e.Observe(ctx, mr); err != nil || !obs.ResourceUpToDate || mr.Status.AtProvider.AdminOption {
				t.Fatalf("after update: %+v, %v", obs, err)
			}

			// Revoked out of band: granted again.
			delete(m.rows, userName)
			e = ext()
			if obs, err := e.Observe(ctx, mr); err != nil || obs.ResourceExists {
				t.Fatalf("after out-of-band revoke: %+v, %v", obs, err)
			}
			if _, err := e.Create(ctx, mr); err != nil {
				t.Fatalf("Create() = %v", err)
			}

			now := metav1.Now()
			mr.SetDeletionTimestamp(&now)
			mr.SetFinalizers([]string{"finalizer.managedresource.crossplane.io"})
			e = ext()
			if obs, err := e.Observe(ctx, mr); err != nil || !obs.ResourceExists {
				t.Fatalf("deleting: %+v, %v", obs, err)
			}
			if _, err := e.Delete(ctx, mr); err != nil {
				t.Fatalf("Delete() = %v", err)
			}
			e = ext()
			if obs, err := e.Observe(ctx, mr); err != nil || obs.ResourceExists {
				t.Fatalf("after delete: %+v, %v", obs, err)
			}
			want := []string{grantAdminStmt, revokeAdminStmt, grantStmt, revokeStmt}
			if diff := cmp.Diff(want, db.Execs); diff != "" {
				t.Errorf("statements -want +got:\n%s", diff)
			}
		})
	}
}

// A RoleGrant whose grantee another tenant's (or an administrator's) user is
// never writes, whatever its management policies and its deletion state.
func TestForeignGranteeIsNeverWritten(t *testing.T) {
	for name, opts := range map[string][]rgOpt{
		"Live":          nil,
		"AdminOption":   {adminOption(true)},
		"Deleting":      {deleting()},
		"ObserveOnly":   {observeOnly},
		"ObserveDelete": {deleting(), policies(xpv2.ManagementActionObserve, xpv2.ManagementActionDelete)},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			db := member(server(), false, 1, 1)
			mr := newRoleGrant(opts...)
			// The user's MR exists, but its claim is a copy: owner-uid is not
			// its UID.
			copied := accesstest.OwnedUser(ns, "api", userName, userID, sharedPC)
			meta.AddAnnotations(copied, map[string]string{v1alpha1.AnnotationOwnerUID: "someone-else"})
			e := newExternal(t, db, clusterResolved(t, withAdminOption()), mr.DeepCopy(), copied)
			obs, _ := e.Observe(ctx, mr)
			if obs.ResourceExists && !meta.WasDeleted(mr) {
				_, _ = e.Update(ctx, mr)
			}
			if obs.ResourceExists && meta.WasDeleted(mr) {
				t.Errorf("a foreign grantee's membership must not be reported while deleting")
			}
			_, _ = e.Create(ctx, mr)
			_, _ = e.Delete(ctx, mr)
			if len(db.Execs) > 0 {
				t.Errorf("statements sent for a foreign grantee: %v", db.Execs)
			}
		})
	}
}

// --- wiring ---

func TestReferences(t *testing.T) {
	g := newRoleGrant(func(g *v1alpha1.RoleGrant) {
		g.Spec.ForProvider.MemberOfRef = &xpv2.NamespacedReference{Name: "reader"}
		g.Spec.ForProvider.UserSelector = &xpv2.NamespacedSelector{MatchLabels: map[string]string{"app": "api"}}
		g.Spec.ForProvider.RoleRef = &xpv2.NamespacedReference{Name: "writer"}
	})
	refs := References(g)
	got := map[string]string{}
	for _, r := range refs {
		var kind string
		switch r.New().(type) {
		case *v1alpha1.Role:
			kind = "Role"
		case *v1alpha1.User:
			kind = "User"
		}
		got[r.Field] = kind
	}
	if diff := cmp.Diff(map[string]string{"memberOfRef": "Role", "userRef": "User", "roleRef": "Role"}, got); diff != "" {
		t.Errorf("references -want +got:\n%s", diff)
	}
	if refs[0].Ref.Name != "reader" || refs[1].Selector == nil || refs[2].Ref.Name != "writer" {
		t.Errorf("references do not carry the spec fields: %+v", refs)
	}
	if ReferencesOf(&v1alpha1.Role{}) != nil || len(ReferencesOf(g)) != 3 {
		t.Error("ReferencesOf must only return the references of a RoleGrant")
	}
	// A cross-namespace reference is rejected before anything is read.
	g.Spec.ForProvider.MemberOfRef.Namespace = "team-b"
	if err := providerconfig.CheckReferenceNamespaces(References(g)); !reason.Is(err, reason.InvalidSpec) {
		t.Errorf("cross-namespace memberOfRef: %v", err)
	}
}

func TestConnect(t *testing.T) {
	ctx := context.Background()
	reader := accesstest.OwnedRole(ns, "reader", memberOf, memberID, sharedPC)
	elsewhere := accesstest.OwnedRole(ns, "elsewhere", "team-a_x", otherID, ownPC)
	cases := map[string]struct {
		mr      *v1alpha1.RoleGrant
		wantErr reason.Reason
	}{
		"SameConfig": {mr: newRoleGrant(func(g *v1alpha1.RoleGrant) {
			g.Spec.ForProvider.MemberOfRef = &xpv2.NamespacedReference{Name: "reader"}
		})},
		// A referenced MR must use the same ProviderConfig.
		"OtherConfig": {
			mr: newRoleGrant(func(g *v1alpha1.RoleGrant) {
				g.Spec.ForProvider.MemberOfRef = &xpv2.NamespacedReference{Name: "elsewhere"}
			}),
			wantErr: reason.InvalidSpec,
		},
		"CrossNamespace": {
			mr: newRoleGrant(func(g *v1alpha1.RoleGrant) {
				g.Spec.ForProvider.UserRef = &xpv2.NamespacedReference{Name: "api", Namespace: "team-b"}
			}),
			wantErr: reason.InvalidSpec,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cl := fake.NewClient(fake.NewDB())
			c := &access.Connector[*v1alpha1.RoleGrant]{
				Kind:  kind,
				Kube:  kubeWith(t, reader, elsewhere),
				Usage: nopTracker{},
				Resolver: resolverFn(func(context.Context, resource.ModernManaged) (*providerconfig.Resolved, error) {
					return clusterResolved(t, sharedPolicy()), nil
				}),
				Cache:      cacheFn(func(context.Context, ch.Key, ch.Config) (ch.Client, error) { return cl, nil }),
				References: References,
				NewClient:  NewExternal,
			}
			ext, err := c.Connect(ctx, tc.mr)
			if tc.wantErr != "" {
				if !reason.Is(err, tc.wantErr) {
					t.Fatalf("Connect() error = %v, want %s", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := ext.(*External); !ok {
				t.Fatalf("Connect() = %T", ext)
			}
			if err := ext.Disconnect(ctx); err != nil || cl.Closed != 1 {
				t.Errorf("Disconnect() = %v, closed %d", err, cl.Closed)
			}
		})
	}
}
