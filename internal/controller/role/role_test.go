// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package role

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

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
	"github.com/infrabay/provider-clickhouse/internal/controller/ownership"
	"github.com/infrabay/provider-clickhouse/internal/controller/providerconfig"
	"github.com/infrabay/provider-clickhouse/internal/reason"
)

const (
	ns       = "team-a"
	mrUID    = "mr-uid"
	roleName = "team-a_reader"
	roleID   = "11111111-1111-1111-1111-111111111111"
	otherID  = "22222222-2222-2222-2222-222222222222"
	userID   = "33333333-3333-3333-3333-333333333333"

	createStmt = "CREATE ROLE `team-a_reader`"
	dropStmt   = "DROP ROLE IF EXISTS `team-a_reader`"
)

var sharedPC = &xpv2.ProviderConfigReference{Kind: "ClusterProviderConfig", Name: "shared"}

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

// sharedPolicy is the shared ClusterProviderConfig of design 3.3.
func sharedPolicy() v1alpha1.ClusterPolicy {
	return v1alpha1.ClusterPolicy{
		AllowedNamespaces: []string{"team-.*"},
		UserNamePatterns:  []string{"{namespace}_[a-z0-9_]+"},
		RoleNamePatterns:  []string{"{namespace}_[a-z0-9_]+"},
		GrantableRoles:    []string{"{namespace}_[a-z0-9_]+"},
	}
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

// namespacedResolved is a namespaced ProviderConfig: no policy, adoption
// always allowed.
func namespacedResolved() *providerconfig.Resolved {
	return &providerconfig.Resolved{
		Kind: "ProviderConfig", Name: "own",
		CacheKey:        ch.Key{Identity: "ProviderConfig/team-a/own", Version: "v"},
		Endpoint:        providerconfig.Endpoint{Host: "clickhouse.team-a.svc", Port: 9440, Protocol: v1alpha1.ProtocolNative, TLS: true},
		AdoptionAllowed: true,
	}
}

type testExternal struct {
	*External
	db   *fake.DB
	cl   *fake.Client
	rec  *recorder
	kube client.Client
}

// newExternal returns a Role external client on a fake 25.3.3 server (the
// oldest supported version) with db as its system tables.
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

type roleOpt func(*v1alpha1.Role)

func newRole(opts ...roleOpt) *v1alpha1.Role {
	r := &v1alpha1.Role{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "reader", UID: mrUID}}
	r.Spec.ProviderConfigReference = sharedPC
	r.Spec.ForProvider.Name = new(roleName)
	for _, o := range opts {
		o(r)
	}
	return r
}

func named(n string) roleOpt {
	return func(r *v1alpha1.Role) { r.Spec.ForProvider.Name = new(n) }
}

// claimed gives the MR a valid claim, as the initializer writes it, and
// owned-id if id is not empty, as Create or an adoption writes it.
func claimed(id string) roleOpt {
	return func(r *v1alpha1.Role) {
		meta.AddAnnotations(r, map[string]string{v1alpha1.AnnotationOwnerUID: mrUID})
		if id != "" {
			meta.AddAnnotations(r, map[string]string{v1alpha1.AnnotationOwnedID: id})
		}
	}
}

func annotated(k, v string) roleOpt {
	return func(r *v1alpha1.Role) { meta.AddAnnotations(r, map[string]string{k: v}) }
}

func pending() roleOpt {
	return func(r *v1alpha1.Role) { meta.SetExternalCreatePending(r, time.Now()) }
}

func succeeded() roleOpt {
	return func(r *v1alpha1.Role) { meta.SetExternalCreateSucceeded(r, time.Now()) }
}

func deleting() roleOpt {
	return func(r *v1alpha1.Role) {
		now := metav1.Now()
		r.SetDeletionTimestamp(&now)
		r.SetFinalizers([]string{"finalizer.managedresource.crossplane.io"})
	}
}

func policies(p ...xpv2.ManagementAction) roleOpt {
	return func(r *v1alpha1.Role) { r.SetManagementPolicies(p) }
}

func onNamespacedConfig() roleOpt {
	return func(r *v1alpha1.Role) {
		r.Spec.ProviderConfigReference = &xpv2.ProviderConfigReference{Kind: "ProviderConfig", Name: "own"}
	}
}

func readyStatus(r *v1alpha1.Role) string { return string(r.GetCondition(xpv2.TypeReady).Status) }

func local(db *fake.DB) *fake.DB { return db.WithRole(roleName, roleID, ch.StorageLocalDirectory) }

// --- Observe ---

func TestObserve(t *testing.T) {
	cases := map[string]struct {
		mr         *v1alpha1.Role
		db         *fake.DB
		res        *providerconfig.Resolved
		want       managed.ExternalObservation
		wantStatus v1alpha1.RoleObservation
		reason     reason.Reason
		anyErr     bool
		ready      string
		events     []string
		noQuery    bool
		check      func(t *testing.T, mr *v1alpha1.Role, kube client.Client)
	}{
		"PolicyViolation": {
			mr: newRole(named("team-b_reader"), claimed(roleID)), db: fake.NewDB(),
			reason: reason.PolicyViolation, ready: "False", noQuery: true,
		},
		// A forged claim on a name outside the policy never leads to SQL,
		// and deletion is not blocked.
		"PolicyViolationWhileDeleting": {
			mr: newRole(named("team-b_reader"), claimed(roleID), deleting()), db: fake.NewDB(),
			events: []string{"OrphanedByPolicy"}, noQuery: true,
		},
		"ExternalNameConflict": {
			mr: newRole(annotated(meta.AnnotationKeyExternalName, "team-a_other")), db: fake.NewDB(),
			reason: reason.ExternalNameConflict, ready: "False", noQuery: true,
		},
		"ExternalNameConflictWhileDeleting": {
			mr: newRole(annotated(meta.AnnotationKeyExternalName, "team-a_other"), deleting()), db: fake.NewDB(),
			events: []string{"Orphaned"}, noQuery: true,
		},
		// The annotation alone is policy-checked too: it is the effective
		// name when forProvider.name is not set.
		"ExternalNameOutsidePolicy": {
			mr: newRole(named(""), annotated(meta.AnnotationKeyExternalName, "team-b_reader"), claimed(roleID)), db: fake.NewDB(),
			reason: reason.PolicyViolation, noQuery: true,
		},
		"NotExists": {
			mr: newRole(claimed("")), db: fake.NewDB().WithEntities(),
		},
		// The collision is reported before Create could run.
		"NotExistsButUserHasTheName": {
			mr: newRole(claimed("")), db: fake.NewDB().WithUser(roleName, userID, ch.StorageLocalDirectory),
			reason: reason.NameCollision, ready: "False",
		},
		"ReadDenied": {
			mr: newRole(claimed(roleID)), db: fake.NewDB().On(sqlx.QueryRoleEntity, fake.Error(fake.Exception(497, "necessary to have the grant SHOW ROLES"))),
			reason: reason.PermissionDenied,
		},
		"TransportErrorIsNotNotFound": {
			mr: newRole(claimed(roleID)), db: fake.NewDB().On(sqlx.QueryRoleEntity, fake.Error(&ch.TransportError{Category: "dial failed"})),
			anyErr: true,
		},
		"Owned": {
			mr: newRole(claimed(roleID)), db: local(fake.NewDB()),
			want:       managed.ExternalObservation{ResourceExists: true, ResourceUpToDate: true},
			wantStatus: v1alpha1.RoleObservation{ID: roleID, Storage: ch.StorageLocalDirectory},
			ready:      "True",
		},
		"OwnedReplicatedStorage": {
			mr: newRole(claimed(roleID)), db: fake.NewDB().WithRole(roleName, roleID, ch.StorageReplicated),
			want:       managed.ExternalObservation{ResourceExists: true, ResourceUpToDate: true},
			wantStatus: v1alpha1.RoleObservation{ID: roleID, Storage: ch.StorageReplicated},
			ready:      "True",
		},
		// A user created with the role's name after the role would capture
		// every grant meant for the role.
		"OwnedButUserHasTheName": {
			mr: newRole(claimed(roleID)), db: local(fake.NewDB()).WithUser(roleName, userID, ch.StorageUsersXML),
			wantStatus: v1alpha1.RoleObservation{ID: roleID, Storage: ch.StorageLocalDirectory},
			reason:     reason.NameCollision, ready: "False",
		},
		"OwnedWhileDeleting": {
			mr: newRole(claimed(roleID), deleting()), db: local(fake.NewDB()),
			want:       managed.ExternalObservation{ResourceExists: true},
			wantStatus: v1alpha1.RoleObservation{ID: roleID, Storage: ch.StorageLocalDirectory},
		},
		// The collision check is skipped while deleting: it must not block
		// deleting an owned role.
		"OwnedWhileDeletingDespiteCollision": {
			mr: newRole(claimed(roleID), deleting()), db: local(fake.NewDB()).WithUser(roleName, userID, ch.StorageLocalDirectory),
			want:       managed.ExternalObservation{ResourceExists: true},
			wantStatus: v1alpha1.RoleObservation{ID: roleID, Storage: ch.StorageLocalDirectory},
		},
		"NotOwned": {
			mr: newRole(claimed("")), db: local(fake.NewDB()),
			wantStatus: v1alpha1.RoleObservation{ID: roleID, Storage: ch.StorageLocalDirectory},
			reason:     reason.NotOwned, ready: "False",
			check: func(t *testing.T, mr *v1alpha1.Role, _ client.Client) {
				t.Helper()
				if !strings.Contains(mr.GetCondition(xpv2.TypeReady).Message, roleID) {
					t.Errorf("Ready message must name the id to adopt: %q", mr.GetCondition(xpv2.TypeReady).Message)
				}
			},
		},
		// A manifest copied from another MR carries its claim: owner-uid is
		// not this MR's UID.
		"CopiedClaim": {
			mr: newRole(annotated(v1alpha1.AnnotationOwnerUID, "someone-else"), annotated(v1alpha1.AnnotationOwnedID, roleID)), db: local(fake.NewDB()),
			reason: reason.NotOwned, ready: "False",
		},
		"NotOwnedWhileDeleting": {
			mr: newRole(claimed(otherID), deleting()), db: local(fake.NewDB()),
			wantStatus: v1alpha1.RoleObservation{ID: roleID, Storage: ch.StorageLocalDirectory},
			events:     []string{"Orphaned"},
		},
		"Replaced": {
			mr: newRole(claimed(otherID), succeeded()), db: local(fake.NewDB()),
			reason: reason.Replaced, ready: "False",
		},
		"ReplacedWhileDeleting": {
			mr: newRole(claimed(otherID), succeeded(), deleting()), db: local(fake.NewDB()),
			wantStatus: v1alpha1.RoleObservation{ID: roleID, Storage: ch.StorageLocalDirectory},
			events:     []string{"Orphaned"},
		},
		"ReadOnly": {
			mr: newRole(claimed(roleID)), db: fake.NewDB().WithRole(roleName, roleID, ch.StorageUsersXML),
			wantStatus: v1alpha1.RoleObservation{ID: roleID, Storage: ch.StorageUsersXML},
			reason:     reason.ReadOnlyStorage, ready: "False",
		},
		"ReadOnlyWhileDeleting": {
			mr: newRole(claimed(roleID), deleting()), db: fake.NewDB().WithRole(roleName, roleID, ch.StorageUsersXML),
			wantStatus: v1alpha1.RoleObservation{ID: roleID, Storage: ch.StorageUsersXML},
			events:     []string{"Orphaned"},
		},
		// Observe-only MRs may report a users.d role.
		"ReadOnlyObserveOnly": {
			mr: newRole(policies(xpv2.ManagementActionObserve)), db: fake.NewDB().WithRole(roleName, roleID, ch.StorageUsersXML),
			want:       managed.ExternalObservation{ResourceExists: true, ResourceUpToDate: true},
			wantStatus: v1alpha1.RoleObservation{ID: roleID, Storage: ch.StorageUsersXML},
			ready:      "True",
		},
		"ObserveOnlyNeverClaims": {
			mr: newRole(policies(xpv2.ManagementActionObserve, xpv2.ManagementActionLateInitialize), annotated(v1alpha1.AnnotationAdopt, roleID)),
			db: local(fake.NewDB()),
			res: clusterResolved(t, func() v1alpha1.ClusterPolicy {
				p := sharedPolicy()
				p.AllowAdoption = true
				return p
			}()),
			want:       managed.ExternalObservation{ResourceExists: true, ResourceUpToDate: true},
			wantStatus: v1alpha1.RoleObservation{ID: roleID, Storage: ch.StorageLocalDirectory},
			ready:      "True",
			check: func(t *testing.T, mr *v1alpha1.Role, _ client.Client) {
				t.Helper()
				if c := ownership.ClaimOf(mr); c.OwnerUID != "" || c.OwnedID != "" {
					t.Errorf("an observe-only MR must not claim: %+v", c)
				}
			},
		},
		// The name policy applies to observe-only MRs too: otherwise they
		// could read any role's metadata through status.
		"ObserveOnlyOutsidePolicy": {
			mr: newRole(named("team-b_reader"), policies(xpv2.ManagementActionObserve)), db: fake.NewDB(),
			reason: reason.PolicyViolation, noQuery: true,
		},
		"ObserveOnlyWhileDeleting": {
			mr: newRole(policies(xpv2.ManagementActionObserve), deleting()), db: local(fake.NewDB()),
			events: []string{"Orphaned"}, noQuery: true,
		},
		// D-26: even an owned role is never dropped without Delete in the
		// management policies, whatever crossplane-runtime does.
		"NoDeletePolicyWhileDeleting": {
			mr: newRole(claimed(roleID), deleting(), policies(xpv2.ManagementActionObserve, xpv2.ManagementActionCreate, xpv2.ManagementActionUpdate)),
			db: local(fake.NewDB()), events: []string{"Orphaned"}, noQuery: true,
		},
		"AdoptionRejectedByPolicy": {
			mr: newRole(claimed(""), annotated(v1alpha1.AnnotationAdopt, roleID)), db: local(fake.NewDB()),
			reason: reason.AdoptionRejected, ready: "False",
		},
		"AdoptWrongUUID": {
			mr: newRole(claimed(""), annotated(v1alpha1.AnnotationAdopt, otherID), onNamespacedConfig()), db: local(fake.NewDB()),
			res:    namespacedResolved(),
			reason: reason.AdoptionRejected, ready: "False",
		},
		"AdoptTrueIsRejected": {
			mr: newRole(claimed(""), annotated(v1alpha1.AnnotationAdopt, "true"), onNamespacedConfig()), db: local(fake.NewDB()),
			res:    namespacedResolved(),
			reason: reason.AdoptionRejected,
		},
		"Adopted": {
			mr: newRole(claimed(""), annotated(v1alpha1.AnnotationAdopt, roleID), onNamespacedConfig()), db: local(fake.NewDB()),
			res:        namespacedResolved(),
			want:       managed.ExternalObservation{ResourceExists: true, ResourceUpToDate: true},
			wantStatus: v1alpha1.RoleObservation{ID: roleID, Storage: ch.StorageLocalDirectory},
			ready:      "True", events: []string{"Adopted"},
			check: func(t *testing.T, mr *v1alpha1.Role, kube client.Client) {
				t.Helper()
				got := &v1alpha1.Role{}
				if err := kube.Get(context.Background(), client.ObjectKeyFromObject(mr), got); err != nil {
					t.Fatal(err)
				}
				if c := ownership.ClaimOf(got); c.OwnedID != roleID || c.OwnerUID != mrUID || c.Adopt != "" {
					t.Errorf("claim not persisted: %+v", c)
				}
			},
		},
		// A tenant points a Role at an existing role and forges
		// external-create-pending: this provider process never sent that
		// CREATE, so the role is not claimed.
		"ForgedPendingIsNotRecovered": {
			mr: newRole(claimed(""), pending()), db: local(fake.NewDB()),
			reason: reason.NotOwned, ready: "False",
		},
		"ForgedUnknownOutcomeIsNotRecovered": {
			mr: newRole(claimed(""), pending(), annotated(v1alpha1.AnnotationCreateOutcome, v1alpha1.CreateOutcomeUnknown), func(r *v1alpha1.Role) {
				meta.SetExternalCreateFailed(r, time.Now().Add(time.Minute))
			}),
			db:     local(fake.NewDB()),
			reason: reason.NotOwned, ready: "False",
		},
		"NamespacedConfigHasNoNamePolicy": {
			mr: newRole(named("anything"), claimed(roleID), onNamespacedConfig()), db: fake.NewDB().WithRole("anything", roleID, ch.StorageLocalDirectory),
			res:        namespacedResolved(),
			want:       managed.ExternalObservation{ResourceExists: true, ResourceUpToDate: true},
			wantStatus: v1alpha1.RoleObservation{ID: roleID, Storage: ch.StorageLocalDirectory},
			ready:      "True",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e := newExternal(t, tc.db, tc.res, tc.mr.DeepCopy())
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
			if diff := cmp.Diff(tc.wantStatus, tc.mr.Status.AtProvider); diff != "" && (tc.wantStatus != v1alpha1.RoleObservation{} || tc.reason == "") {
				t.Errorf("atProvider -want +got:\n%s", diff)
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
			if tc.check != nil {
				tc.check(t, tc.mr, e.kube)
			}
		})
	}
}

// --- Create ---

// createRole makes db behave like a server that runs CREATE ROLE and DROP
// ROLE statements for name, assigning id on create.
func createRole(db *fake.DB, name, id string) func(string) error {
	return func(stmt string) error {
		switch stmt {
		case sqlx.CreateRoleStmt(name):
			db.WithRole(name, id, ch.StorageLocalDirectory)
		case sqlx.DropRoleStmt(name):
			db.RemoveRole(name)
		}
		return nil
	}
}

func TestCreate(t *testing.T) {
	cases := map[string]struct {
		mr          *v1alpha1.Role
		res         *providerconfig.Resolved
		setup       func(db *fake.DB, cl *fake.Client)
		wantStmts   []string
		reason      reason.Reason
		anyErr      bool
		wantOutcome string
		wantOwnedID string
	}{
		"Success": {
			mr:          newRole(claimed("")),
			setup:       func(db *fake.DB, _ *fake.Client) { db.WithEntities(); db.ExecFn = createRole(db, roleName, roleID) },
			wantStmts:   []string{createStmt},
			wantOwnedID: roleID,
		},
		// Dropped out of band and created again: the new UUID replaces the
		// recorded one.
		"RecreateAfterDrop": {
			mr:          newRole(claimed(otherID), succeeded()),
			setup:       func(db *fake.DB, _ *fake.Client) { db.WithEntities(); db.ExecFn = createRole(db, roleName, roleID) },
			wantStmts:   []string{createStmt},
			wantOwnedID: roleID,
		},
		"AdversarialName": {
			mr:  newRole(named("r`; DROP ROLE x\\ü@*.y"), claimed(""), onNamespacedConfig()),
			res: namespacedResolved(),
			setup: func(db *fake.DB, _ *fake.Client) {
				db.WithEntities()
				db.ExecFn = createRole(db, "r`; DROP ROLE x\\ü@*.y", roleID)
			},
			wantStmts:   []string{"CREATE ROLE `r\\`; DROP ROLE x\\\\ü@*.y`"},
			wantOwnedID: roleID,
		},
		"MissingClaim": {
			mr: newRole(), setup: func(db *fake.DB, _ *fake.Client) { db.WithEntities() },
			reason: reason.Internal,
		},
		// A claim from before the MR was switched to observe-only.
		"ObserveOnly": {
			mr: newRole(claimed(""), policies(xpv2.ManagementActionObserve)), setup: func(db *fake.DB, _ *fake.Client) { db.WithEntities() },
			reason: reason.Internal,
		},
		"CopiedClaim": {
			mr:     newRole(annotated(v1alpha1.AnnotationOwnerUID, "someone-else")),
			setup:  func(db *fake.DB, _ *fake.Client) { db.WithEntities() },
			reason: reason.Internal,
		},
		"PolicyViolation": {
			mr: newRole(named("team-b_reader"), claimed("")), setup: func(db *fake.DB, _ *fake.Client) { db.WithEntities() },
			reason: reason.PolicyViolation,
		},
		"ExternalNameConflict": {
			mr:     newRole(claimed(""), annotated(meta.AnnotationKeyExternalName, "team-a_other")),
			reason: reason.ExternalNameConflict,
		},
		"NameCollision": {
			mr:     newRole(claimed("")),
			setup:  func(db *fake.DB, _ *fake.Client) { db.WithUser(roleName, userID, ch.StorageLocalDirectory) },
			reason: reason.NameCollision,
		},
		"MultipleServersWithLocalDirectory": {
			mr:     newRole(claimed("")),
			setup:  func(db *fake.DB, cl *fake.Client) { db.WithEntities(); cl.Servers = []string{"s1", "s2"} },
			reason: reason.AccessStorageNotReplicated,
		},
		"AlreadyExists": {
			mr: newRole(claimed("")),
			setup: func(db *fake.DB, _ *fake.Client) {
				db.WithEntities().OnExec(createStmt, fake.Exception(493, "role `team-a_reader`: cannot insert because role `team-a_reader` already exists"))
			},
			wantStmts: []string{createStmt}, reason: reason.AlreadyExists, wantOutcome: v1alpha1.CreateOutcomeRejected,
		},
		// The name of a users.d role.
		"ReadOnlyStorage": {
			mr: newRole(claimed("")),
			setup: func(db *fake.DB, _ *fake.Client) {
				db.WithEntities().OnExec(createStmt, fake.Exception(495, "Cannot insert role `team-a_reader` in users_xml because this storage is readonly"))
			},
			wantStmts: []string{createStmt}, reason: reason.ReadOnlyStorage, wantOutcome: v1alpha1.CreateOutcomeRejected,
		},
		// Outside the identity's server-side prefix grant.
		"PermissionDenied": {
			mr: newRole(claimed("")),
			setup: func(db *fake.DB, _ *fake.Client) {
				db.WithEntities().OnExec(createStmt, fake.Exception(497, "necessary to have the grant CREATE ROLE ON team-a_reader"))
			},
			wantStmts: []string{createStmt}, reason: reason.PermissionDenied, wantOutcome: v1alpha1.CreateOutcomeRejected,
		},
		"ResponseLost": {
			mr: newRole(claimed("")),
			setup: func(db *fake.DB, _ *fake.Client) {
				db.WithEntities().OnExec(createStmt, &ch.TransportError{Category: "connection closed"})
			},
			wantStmts: []string{createStmt}, anyErr: true, wantOutcome: v1alpha1.CreateOutcomeUnknown,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			db := fake.NewDB()
			e := newExternal(t, db, tc.res)
			if tc.setup != nil {
				tc.setup(db, e.cl)
			}
			_, err := e.Create(context.Background(), tc.mr)
			switch {
			case tc.anyErr:
				if err == nil {
					t.Fatal("want an error")
				}
			case tc.reason != "":
				if !reason.Is(err, tc.reason) {
					t.Fatalf("Create() error = %v, want %s", err, tc.reason)
				}
			case err != nil:
				t.Fatalf("Create() error = %v", err)
			}
			if diff := cmp.Diff(tc.wantStmts, db.Execs); diff != "" {
				t.Errorf("statements -want +got:\n%s", diff)
			}
			c := ownership.ClaimOf(tc.mr)
			if c.CreateOutcome != tc.wantOutcome {
				t.Errorf("create-outcome = %q, want %q", c.CreateOutcome, tc.wantOutcome)
			}
			if tc.wantOwnedID != "" && c.OwnedID != tc.wantOwnedID {
				t.Errorf("owned-id = %q, want %q", c.OwnedID, tc.wantOwnedID)
			}
		})
	}
}

// A create whose response was lost is recovered by the next Observe through
// the journal of this process, and only by the MR that sent it.
func TestCreateResponseLostThenRecovered(t *testing.T) {
	ctx := context.Background()
	db := fake.NewDB().WithEntities()
	db.ExecFn = func(stmt string) error {
		if stmt == createStmt {
			db.WithRole(roleName, roleID, ch.StorageLocalDirectory)
			return &ch.TransportError{Category: "connection closed"}
		}
		return nil
	}
	mine := newRole(claimed(""), pending())
	copied := newRole(pending(), func(r *v1alpha1.Role) {
		r.SetName("copy")
		r.SetUID("copy-uid")
		meta.AddAnnotations(r, map[string]string{v1alpha1.AnnotationOwnerUID: "copy-uid", v1alpha1.AnnotationCreateOutcome: v1alpha1.CreateOutcomeUnknown})
	})
	e := newExternal(t, db, nil, mine.DeepCopy(), copied.DeepCopy())
	if _, err := e.Create(ctx, mine); err == nil {
		t.Fatal("the lost response must be reported")
	}
	if ownership.ClaimOf(mine).CreateOutcome != v1alpha1.CreateOutcomeUnknown {
		t.Fatalf("create-outcome = %q", ownership.ClaimOf(mine).CreateOutcome)
	}
	meta.SetExternalCreateFailed(mine, time.Now())
	obs, err := e.Observe(ctx, mine)
	if err != nil || !obs.ResourceExists {
		t.Fatalf("recovery: %+v, %v", obs, err)
	}
	if diff := cmp.Diff([]string{"CreateRecovered"}, e.rec.reasons()); diff != "" {
		t.Errorf("events: %s", diff)
	}
	got := &v1alpha1.Role{}
	if err := e.kube.Get(ctx, client.ObjectKeyFromObject(mine), got); err != nil {
		t.Fatal(err)
	}
	if ownership.ClaimOf(got).OwnedID != roleID {
		t.Errorf("owned-id not persisted: %v", got.GetAnnotations())
	}
	// Another MR on the same name with forged create annotations.
	if _, err := e.Observe(ctx, copied); !reason.Is(err, reason.NotOwned) {
		t.Errorf("forged create annotations: %v, want NotOwned", err)
	}
}

// --- Update ---

func TestUpdateNeverWrites(t *testing.T) {
	db := local(fake.NewDB())
	e := newExternal(t, db, nil)
	if _, err := e.Update(context.Background(), newRole(claimed(roleID))); err != nil {
		t.Fatalf("Update() = %v", err)
	}
	if len(db.Execs) > 0 || len(db.Seen) > 0 {
		t.Errorf("a role has nothing to update; ran %v %v", db.Seen, db.Execs)
	}
}

// --- Delete ---

func TestDelete(t *testing.T) {
	cases := map[string]struct {
		mr        *v1alpha1.Role
		db        *fake.DB
		multi     bool
		wantStmts []string
		reason    reason.Reason
		noQuery   bool
	}{
		"Owned": {
			mr: newRole(claimed(roleID), deleting()), db: local(fake.NewDB()),
			wantStmts: []string{dropStmt},
		},
		// Dropped and re-created by someone else between Observe and Delete.
		"ReplacedSinceObserve": {
			mr: newRole(claimed(roleID), deleting()), db: fake.NewDB().WithRole(roleName, otherID, ch.StorageLocalDirectory),
			reason: reason.Replaced,
		},
		"AlreadyGone": {
			mr: newRole(claimed(roleID), deleting()), db: fake.NewDB().WithEntities(),
		},
		"GoneBetweenCheckAndDrop": {
			mr:        newRole(claimed(roleID), deleting()),
			db:        local(fake.NewDB()).OnExec(dropStmt, fake.Exception(511, "There is no role `team-a_reader` in user directories")),
			wantStmts: []string{dropStmt},
		},
		"DropDenied": {
			mr:        newRole(claimed(roleID), deleting()),
			db:        local(fake.NewDB()).OnExec(dropStmt, fake.Exception(497, "necessary to have the grant DROP ROLE ON team-a_reader")),
			wantStmts: []string{dropStmt}, reason: reason.PermissionDenied,
		},
		"NoClaim": {
			mr: newRole(deleting()), db: local(fake.NewDB()),
			reason: reason.NotOwned, noQuery: true,
		},
		"NoOwnedID": {
			mr: newRole(claimed(""), deleting()), db: local(fake.NewDB()),
			reason: reason.NotOwned, noQuery: true,
		},
		"CopiedClaim": {
			mr: newRole(deleting(), annotated(v1alpha1.AnnotationOwnerUID, "someone-else"), annotated(v1alpha1.AnnotationOwnedID, roleID)), db: local(fake.NewDB()),
			reason: reason.NotOwned, noQuery: true,
		},
		"OutsidePolicy": {
			mr: newRole(named("team-b_reader"), claimed(roleID), deleting()), db: fake.NewDB().WithRole("team-b_reader", roleID, ch.StorageLocalDirectory),
			reason: reason.PolicyViolation, noQuery: true,
		},
		"ReadOnly": {
			mr: newRole(claimed(roleID), deleting()), db: fake.NewDB().WithRole(roleName, roleID, ch.StorageUsersXML),
			reason: reason.ReadOnlyStorage,
		},
		"NotWritable": {
			mr: newRole(claimed(roleID), deleting()), db: local(fake.NewDB()), multi: true,
			reason: reason.AccessStorageNotReplicated,
		},
		"NoDeletePolicy": {
			mr: newRole(claimed(roleID), deleting(), policies(xpv2.ManagementActionObserve)), db: local(fake.NewDB()),
			noQuery: true,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e := newExternal(t, tc.db, nil)
			if tc.multi {
				e.cl.Servers = []string{"s1", "s2"}
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
			if tc.noQuery && len(tc.db.Seen) > 0 {
				t.Errorf("no query may run, ran %v", tc.db.Seen)
			}
			if got := tc.mr.GetCondition(xpv2.TypeReady).Reason; got != xpv2.ReasonDeleting {
				t.Errorf("Ready reason = %q, want %q", got, xpv2.ReasonDeleting)
			}
		})
	}
}

// --- lifecycle on every supported server version ---

func TestLifecycle(t *testing.T) {
	for _, p := range fake.Profiles {
		t.Run(p.Version, func(t *testing.T) {
			ctx := context.Background()
			db := fake.NewDB().WithEntities()
			db.ExecFn = createRole(db, roleName, roleID)
			mr := newRole(claimed(""))
			e := newExternalFor(t, db, p, nil, mr.DeepCopy())

			if obs, err := e.Observe(ctx, mr); err != nil || obs.ResourceExists {
				t.Fatalf("before create: %+v, %v", obs, err)
			}
			meta.SetExternalCreatePending(mr, time.Now())
			if _, err := e.Create(ctx, mr); err != nil {
				t.Fatalf("Create() = %v", err)
			}
			meta.SetExternalCreateSucceeded(mr, time.Now())
			obs, err := e.Observe(ctx, mr)
			if err != nil || !obs.ResourceExists || !obs.ResourceUpToDate || readyStatus(mr) != "True" {
				t.Fatalf("after create: %+v, %v, Ready %s", obs, err, readyStatus(mr))
			}
			if mr.Status.AtProvider.ID != roleID {
				t.Errorf("status id = %q", mr.Status.AtProvider.ID)
			}

			// Dropped out of band: re-created, and the new UUID recorded.
			db.RemoveRole(roleName)
			db.ExecFn = createRole(db, roleName, otherID)
			if obs, err := e.Observe(ctx, mr); err != nil || obs.ResourceExists {
				t.Fatalf("after out-of-band drop: %+v, %v", obs, err)
			}
			if _, err := e.Create(ctx, mr); err != nil || ownership.ClaimOf(mr).OwnedID != otherID {
				t.Fatalf("re-create: %v, owned-id %q", err, ownership.ClaimOf(mr).OwnedID)
			}

			now := metav1.Now()
			mr.SetDeletionTimestamp(&now)
			if obs, err := e.Observe(ctx, mr); err != nil || !obs.ResourceExists {
				t.Fatalf("deleting: %+v, %v", obs, err)
			}
			if _, err := e.Delete(ctx, mr); err != nil {
				t.Fatalf("Delete() = %v", err)
			}
			if obs, err := e.Observe(ctx, mr); err != nil || obs.ResourceExists {
				t.Fatalf("after delete: %+v, %v", obs, err)
			}
			want := []string{createStmt, createStmt, dropStmt}
			if diff := cmp.Diff(want, db.Execs); diff != "" {
				t.Errorf("statements -want +got:\n%s", diff)
			}
		})
	}
}

// --- wiring ---

func TestConnect(t *testing.T) {
	cl := fake.NewClient(fake.NewDB())
	c := &access.Connector[*v1alpha1.Role]{
		Kind:  kind,
		Kube:  kubeWith(t),
		Usage: nopTracker{},
		Resolver: resolverFn(func(context.Context, resource.ModernManaged) (*providerconfig.Resolved, error) {
			return clusterResolved(t, sharedPolicy()), nil
		}),
		Cache:     cacheFn(func(context.Context, ch.Key, ch.Config) (ch.Client, error) { return cl, nil }),
		NewClient: NewExternal,
	}
	ext, err := c.Connect(context.Background(), newRole())
	if err != nil {
		t.Fatal(err)
	}
	e, ok := ext.(*External)
	if !ok || e.Client != cl {
		t.Fatalf("Connect() = %T", ext)
	}
	if err := ext.Disconnect(context.Background()); err != nil || cl.Closed != 1 {
		t.Errorf("Disconnect() = %v, closed %d", err, cl.Closed)
	}
}

func TestForProviderName(t *testing.T) {
	if n := forProviderName(newRole()); n == nil || *n != roleName {
		t.Errorf("forProviderName(Role) = %v", n)
	}
	if n := forProviderName(&v1alpha1.Database{}); n != nil {
		t.Errorf("forProviderName(Database) = %v", n)
	}
}

// The initializer writes the claim that Create needs, and nothing for an
// observe-only MR.
func TestInitializer(t *testing.T) {
	ctx := context.Background()
	kube := kubeWith(t, newRole())
	mr := &v1alpha1.Role{}
	if err := kube.Get(ctx, client.ObjectKey{Namespace: ns, Name: "reader"}, mr); err != nil {
		t.Fatal(err)
	}
	if err := ownership.NewInitializer(kube, forProviderName).Initialize(ctx, mr); err != nil {
		t.Fatal(err)
	}
	if !ownership.ClaimValid(mr) || meta.GetExternalName(mr) != roleName {
		t.Errorf("annotations after initialize: %v", mr.GetAnnotations())
	}
	kube = kubeWith(t, newRole(policies(xpv2.ManagementActionObserve)))
	obs := &v1alpha1.Role{}
	if err := kube.Get(ctx, client.ObjectKey{Namespace: ns, Name: "reader"}, obs); err != nil {
		t.Fatal(err)
	}
	if err := ownership.NewInitializer(kube, forProviderName).Initialize(ctx, obs); err != nil {
		t.Fatal(err)
	}
	if ownership.ClaimValid(obs) {
		t.Error("an observe-only MR must not get a claim")
	}
}

func TestErrorsCarryNoStatement(t *testing.T) {
	db := local(fake.NewDB()).OnExec(dropStmt, errors.New("boom"))
	e := newExternal(t, db, nil)
	_, err := e.Delete(context.Background(), newRole(claimed(roleID), deleting()))
	if err == nil || strings.Contains(err.Error(), "DROP ROLE") {
		t.Errorf("errors are never wrapped with SQL text: %v", err)
	}
}
