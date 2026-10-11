// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package database

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
	"github.com/infrabay/provider-clickhouse/internal/controller/providerconfig"
	"github.com/infrabay/provider-clickhouse/internal/reason"
)

const (
	mrUID   = "mr-uid"
	dbUUID  = "8a1c5f4e-2b3d-4c5e-9f60-718293a4b5c6"
	otherID = "22222222-2222-2222-2222-222222222222"
	dbName  = "team-a_analytics"
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
	out := make([]string, 0, len(r.events))
	for _, e := range r.events {
		out = append(out, string(e.Reason))
	}
	return out
}

type resolverFn func(context.Context, resource.ModernManaged) (*providerconfig.Resolved, error)

func (f resolverFn) Resolve(ctx context.Context, mg resource.ModernManaged) (*providerconfig.Resolved, error) {
	return f(ctx, mg)
}

type tracker struct {
	calls int
	err   error
}

func (t *tracker) Track(context.Context, resource.ModernManaged) error {
	t.calls++
	return t.err
}

type cacheFn func(context.Context, ch.Key, ch.Config) (ch.Client, error)

func (f cacheFn) Acquire(ctx context.Context, k ch.Key, c ch.Config) (ch.Client, error) {
	return f(ctx, k, c)
}

// --- fixtures ---

type dbOpt func(*v1alpha1.Database)

func newDB(opts ...dbOpt) *v1alpha1.Database {
	d := &v1alpha1.Database{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "analytics", UID: mrUID},
		Spec: v1alpha1.DatabaseSpec{
			ManagedResourceSpec: xpv2.ManagedResourceSpec{
				ProviderConfigReference: &xpv2.ProviderConfigReference{Kind: "ClusterProviderConfig", Name: "shared"},
			},
			ForProvider: v1alpha1.DatabaseParameters{Name: new(dbName)},
		},
	}
	for _, o := range opts {
		o(d)
	}
	return d
}

func owned(id string) dbOpt {
	return func(d *v1alpha1.Database) {
		meta.AddAnnotations(d, map[string]string{v1alpha1.AnnotationOwnerUID: mrUID, v1alpha1.AnnotationOwnedID: id})
	}
}

func annotated(k, v string) dbOpt {
	return func(d *v1alpha1.Database) { meta.AddAnnotations(d, map[string]string{k: v}) }
}

func atomic(comment string) dbOpt {
	return func(d *v1alpha1.Database) {
		d.Spec.ForProvider.Engine = &v1alpha1.DatabaseEngine{Name: v1alpha1.DatabaseEngineAtomic}
		d.Spec.ForProvider.Comment = new(comment)
	}
}

func beingDeleted() dbOpt {
	return func(d *v1alpha1.Database) {
		now := metav1.NewTime(time.Now())
		d.SetDeletionTimestamp(&now)
		d.SetFinalizers([]string{"finalizer.managedresource.crossplane.io"})
	}
}

func managementPolicies(p ...xpv2.ManagementAction) dbOpt {
	return func(d *v1alpha1.Database) { d.SetManagementPolicies(p) }
}

func sharedPolicy(t *testing.T, allowAdoption bool) v1alpha1.ClusterPolicy {
	t.Helper()
	return v1alpha1.ClusterPolicy{
		AllowedNamespaces:    []string{"team-.*"},
		DatabaseNamePatterns: []string{"{namespace}_[a-z0-9_]+"},
		AllowAdoption:        allowAdoption,
	}
}

func resolved(t *testing.T, cluster string, allowAdoption bool) *providerconfig.Resolved {
	t.Helper()
	return resolvedWith(t, cluster, sharedPolicy(t, allowAdoption))
}

func resolvedWith(t *testing.T, cluster string, cp v1alpha1.ClusterPolicy) *providerconfig.Resolved {
	t.Helper()
	p, err := providerconfig.CompilePolicy(cp)
	if err != nil {
		t.Fatal(err)
	}
	return &providerconfig.Resolved{
		Kind: "ClusterProviderConfig", Name: "shared",
		CacheKey:        ch.Key{Identity: "ClusterProviderConfig//shared", Version: "v"},
		Config:          ch.Config{Host: "h", Port: 9000, Protocol: ch.ProtocolNative, Username: "u", Password: "p"},
		Policy:          p,
		AdoptionAllowed: p.AdoptionAllowed(),
		Cluster:         cluster,
	}
}

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

// scripted returns a DB where the database exists with the given details.
func scripted(id, engine, engineFull, comment string) *fake.DB {
	return fake.NewDB().
		On(sqlx.ExistsDatabase(dbName), fake.Row(uint8(1))).
		On(sqlx.QueryDatabase, fake.Row(id, engine, engineFull, comment)).
		On(ch.QueryPrivilegeExists, fake.Row(uint64(1)))
}

// hostIDs scripts the per-host UUIDs that clusterAllReplicas returns.
func hostIDs(rows ...[2]string) fake.Result {
	out := make([][]any, 0, len(rows))
	for _, r := range rows {
		out = append(out, []any{r[0], r[1]})
	}
	return fake.Rows(out...)
}

const cluster = "my-cluster"

func readyStatus(d *v1alpha1.Database) string {
	return string(d.GetCondition(xpv2.TypeReady).Status)
}

// --- Connect ---

func TestConnect(t *testing.T) {
	errBoom := errors.New("boom")
	ok := func(t *testing.T) *providerconfig.Resolved { return resolved(t, "", false) }

	cases := map[string]struct {
		res         func(*testing.T) *providerconfig.Resolved
		resolveErr  error
		trackErr    error
		acquireErr  error
		wantErr     error
		wantTracks  int
		wantAcquire int
		wantOrphan  bool
	}{
		"ResolveError":    {resolveErr: errBoom, wantErr: errBoom},
		"NamespaceDenied": {res: func(*testing.T) *providerconfig.Resolved { return &providerconfig.Resolved{NamespaceDenied: true} }, wantOrphan: true},
		"TrackError":      {res: ok, trackErr: errBoom, wantErr: errBoom, wantTracks: 1},
		"AcquireError":    {res: ok, acquireErr: errBoom, wantErr: errBoom, wantTracks: 1, wantAcquire: 1},
		"Success":         {res: ok, wantTracks: 1, wantAcquire: 1},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			tr := &tracker{err: tc.trackErr}
			acquired := 0
			c := &Connector{
				Kube:  kubeWith(t),
				Usage: tr,
				Resolver: resolverFn(func(context.Context, resource.ModernManaged) (*providerconfig.Resolved, error) {
					if tc.resolveErr != nil {
						return nil, tc.resolveErr
					}
					return tc.res(t), nil
				}),
				Cache: cacheFn(func(context.Context, ch.Key, ch.Config) (ch.Client, error) {
					acquired++
					if tc.acquireErr != nil {
						return nil, tc.acquireErr
					}
					return fake.NewClient(fake.NewDB()), nil
				}),
			}
			ext, err := c.Connect(context.Background(), newDB())
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Connect() error = %v, want %v", err, tc.wantErr)
			}
			if tr.calls != tc.wantTracks || acquired != tc.wantAcquire {
				t.Errorf("tracks %d (want %d), acquires %d (want %d)", tr.calls, tc.wantTracks, acquired, tc.wantAcquire)
			}
			if _, isOrphan := ext.(*orphan); isOrphan != tc.wantOrphan {
				t.Errorf("orphan client = %t, want %t", isOrphan, tc.wantOrphan)
			}
		})
	}
}

func TestOrphanObserve(t *testing.T) {
	rec := &recorder{}
	o := &orphan{recorder: rec}
	obs, err := o.Observe(context.Background(), newDB(beingDeleted()))
	if err != nil || obs.ResourceExists {
		t.Fatalf("Observe() = %+v, %v", obs, err)
	}
	if diff := cmp.Diff([]string{string(ReasonOrphanedByPolicy)}, rec.reasons()); diff != "" {
		t.Errorf("events: %s", diff)
	}
	if _, err := o.Delete(context.Background(), newDB()); err != nil {
		t.Errorf("Delete() = %v", err)
	}
}

// --- Observe ---

func TestObserve(t *testing.T) {
	boomTransport := &ch.TransportError{Category: "dial failed", Message: "connection refused"}
	cases := map[string]struct {
		mr          *v1alpha1.Database
		db          *fake.DB
		cluster     string
		adoption    bool
		want        managed.ExternalObservation
		wantReason  reason.Reason
		wantAnyErr  bool
		wantReady   string
		wantNoQuery bool
		wantEvents  []string
		check       func(t *testing.T, mr *v1alpha1.Database, kube client.Client, rec *recorder)
	}{
		"PolicyViolation": {
			mr:          newDB(owned(dbUUID), func(d *v1alpha1.Database) { d.Spec.ForProvider.Name = new("team-b_analytics") }),
			db:          fake.NewDB(),
			wantReason:  reason.PolicyViolation,
			wantReady:   "False",
			wantNoQuery: true,
		},
		"ExternalNameConflict": {
			mr:         newDB(annotated(meta.AnnotationKeyExternalName, "other")),
			db:         fake.NewDB(),
			wantReason: reason.ExternalNameConflict,
			wantReady:  "False",
		},
		"NotExists": {
			mr: newDB(owned(dbUUID)),
			db: fake.NewDB().On(sqlx.ExistsDatabase(dbName), fake.Row(uint8(0))),
		},
		"ExistsPermissionDenied": {
			mr:         newDB(owned(dbUUID)),
			db:         fake.NewDB().On(sqlx.ExistsDatabase(dbName), fake.Error(fake.Exception(497, "Not enough privileges"))),
			wantReason: reason.PermissionDenied,
		},
		"TransportErrorIsNotNotFound": {
			mr:         newDB(owned(dbUUID)),
			db:         fake.NewDB().On(sqlx.ExistsDatabase(dbName), fake.Error(boomTransport)),
			wantAnyErr: true,
		},
		"FilteredIsNotNotFound": {
			mr: newDB(owned(dbUUID)),
			db: fake.NewDB().
				On(sqlx.ExistsDatabase(dbName), fake.Row(uint8(1))).
				On(sqlx.QueryDatabase, fake.Rows()),
			wantReason: reason.PermissionDenied,
		},
		"OwnedUpToDate": {
			mr:        newDB(owned(dbUUID), atomic("hello")),
			db:        scripted(dbUUID, "Atomic", "Atomic", "hello"),
			want:      managed.ExternalObservation{ResourceExists: true, ResourceUpToDate: true},
			wantReady: "True",
			check: func(t *testing.T, mr *v1alpha1.Database, _ client.Client, _ *recorder) {
				t.Helper()
				want := v1alpha1.DatabaseObservation{ID: dbUUID, Engine: "Atomic", EngineFull: "Atomic", Comment: "hello"}
				if diff := cmp.Diff(want, mr.Status.AtProvider); diff != "" {
					t.Errorf("atProvider: %s", diff)
				}
			},
		},
		"OwnedLateInitialized": {
			mr:        newDB(owned(dbUUID)),
			db:        scripted(dbUUID, "Atomic", "Atomic", ""),
			want:      managed.ExternalObservation{ResourceExists: true, ResourceUpToDate: true, ResourceLateInitialized: true},
			wantReady: "True",
			check: func(t *testing.T, mr *v1alpha1.Database, _ client.Client, _ *recorder) {
				t.Helper()
				p := mr.Spec.ForProvider
				if p.Engine == nil || p.Engine.Name != "Atomic" || p.Comment == nil || *p.Comment != "" {
					t.Errorf("late-initialized spec: %+v", p)
				}
			},
		},
		"LateInitReplicated": {
			mr:   newDB(owned(dbUUID), func(d *v1alpha1.Database) { d.Spec.ForProvider.Comment = new("") }),
			db:   scripted(dbUUID, "Replicated", "Replicated('/clickhouse/databases/{uuid}', '{shard}', '{replica}')", ""),
			want: managed.ExternalObservation{ResourceExists: true, ResourceUpToDate: true, ResourceLateInitialized: true},
			check: func(t *testing.T, mr *v1alpha1.Database, _ client.Client, _ *recorder) {
				t.Helper()
				e := mr.Spec.ForProvider.Engine
				if e == nil || e.Replicated == nil || e.Replicated.ZooKeeperPath != "/clickhouse/databases/{uuid}" {
					t.Errorf("late-initialized engine: %+v", e)
				}
			},
			wantReady: "True",
		},
		"NoLateInitWithoutPolicy": {
			mr:        newDB(owned(dbUUID), managementPolicies(xpv2.ManagementActionObserve, xpv2.ManagementActionCreate, xpv2.ManagementActionUpdate)),
			db:        scripted(dbUUID, "Atomic", "Atomic", ""),
			want:      managed.ExternalObservation{ResourceExists: true, ResourceUpToDate: true},
			wantReady: "True",
		},
		"CommentDrift": {
			mr:        newDB(owned(dbUUID), atomic("desired")),
			db:        scripted(dbUUID, "Atomic", "Atomic", "changed out of band"),
			want:      managed.ExternalObservation{ResourceExists: true, ResourceUpToDate: false},
			wantReady: "True",
		},
		"CommentChangeUnsupported": {
			mr: newDB(owned(dbUUID), atomic("desired")),
			db: scripted(dbUUID, "Atomic", "Atomic", "old").
				On(ch.QueryPrivilegeExists, fake.Row(uint64(0))),
			wantReason: reason.Unsupported,
			wantReady:  "False",
		},
		"EngineMismatch": {
			mr:         newDB(owned(dbUUID), atomic("")),
			db:         scripted(dbUUID, "Replicated", "Replicated('/p', 's', 'r')", ""),
			wantReason: reason.InvalidSpec,
			wantReady:  "True",
		},
		"ReplicatedArgsMatchWithDefaults": {
			mr: newDB(owned(dbUUID), func(d *v1alpha1.Database) {
				d.Spec.ForProvider.Engine = &v1alpha1.DatabaseEngine{Name: "Replicated"}
				d.Spec.ForProvider.Comment = new("")
			}),
			db:        scripted(dbUUID, "Replicated", "Replicated('/clickhouse/databases/{uuid}', '{shard}', '{replica}')", ""),
			want:      managed.ExternalObservation{ResourceExists: true, ResourceUpToDate: true},
			wantReady: "True",
		},
		"ReplicatedArgsMismatch": {
			mr: newDB(owned(dbUUID), func(d *v1alpha1.Database) {
				d.Spec.ForProvider.Engine = &v1alpha1.DatabaseEngine{Name: "Replicated", Replicated: &v1alpha1.ReplicatedEngine{ZooKeeperPath: "/other", ShardName: "{shard}", ReplicaName: "{replica}"}}
			}),
			db:         scripted(dbUUID, "Replicated", "Replicated('/clickhouse/databases/{uuid}', '{shard}', '{replica}')", ""),
			wantReason: reason.InvalidSpec,
			wantReady:  "True",
		},
		"NotOwned": {
			mr:         newDB(owned(otherID)),
			db:         scripted(dbUUID, "Atomic", "Atomic", ""),
			wantReason: reason.NotOwned,
			wantReady:  "False",
			check: func(t *testing.T, mr *v1alpha1.Database, _ client.Client, _ *recorder) {
				t.Helper()
				if mr.Status.AtProvider.ID != dbUUID {
					t.Errorf("the observed id must be reported: %q", mr.Status.AtProvider.ID)
				}
				if !strings.Contains(mr.GetCondition(xpv2.TypeReady).Message, dbUUID) {
					t.Errorf("Ready message must name the id to adopt: %q", mr.GetCondition(xpv2.TypeReady).Message)
				}
			},
		},
		"NotOwnedWhileDeletingIsOrphaned": {
			mr:         newDB(owned(otherID), beingDeleted()),
			db:         scripted(dbUUID, "Atomic", "Atomic", ""),
			wantEvents: []string{string(ReasonOrphaned)},
		},
		"InPolicyWhileDeleting": {
			mr:   newDB(owned(dbUUID), beingDeleted(), func(d *v1alpha1.Database) { d.Spec.ForProvider.Name = nil; meta.SetExternalName(d, dbName) }),
			db:   scripted(dbUUID, "Atomic", "Atomic", ""),
			want: managed.ExternalObservation{ResourceExists: true},
		},
		// A tenant points its MR at another tenant's database with a forged
		// claim and forceDrop, then deletes it. The name policy still applies
		// while deleting: nothing is read or dropped, and the MR goes away.
		"ForgedClaimOutsidePolicyWhileDeletingIsOrphaned": {
			mr: newDB(owned(dbUUID), beingDeleted(), func(d *v1alpha1.Database) {
				d.Spec.ForProvider.Name = nil
				d.Spec.ForProvider.ForceDrop = new(true)
				meta.SetExternalName(d, "team-b_prod")
			}),
			db:          fake.NewDB(),
			wantNoQuery: true,
			wantEvents:  []string{string(ReasonOrphanedByPolicy)},
		},
		"ExternalNameConflictWhileDeletingIsOrphaned": {
			mr:          newDB(owned(dbUUID), beingDeleted(), annotated(meta.AnnotationKeyExternalName, "other")),
			db:          fake.NewDB(),
			wantNoQuery: true,
			wantEvents:  []string{string(ReasonOrphaned)},
		},
		// Even if crossplane-runtime did not honour the management policies,
		// an observe-only MR never reports a database it does not own while
		// being deleted, so Delete is never called for it.
		"ObserveOnlyNotOwnedWhileDeleting": {
			mr:         newDB(managementPolicies(xpv2.ManagementActionObserve), beingDeleted()),
			db:         scripted(dbUUID, "Atomic", "Atomic", ""),
			wantEvents: []string{string(ReasonOrphaned)},
		},
		"ObserveOnlyCopiedClaimWhileDeleting": {
			mr: newDB(managementPolicies(xpv2.ManagementActionObserve), beingDeleted(),
				annotated(v1alpha1.AnnotationOwnerUID, "someone-else"), annotated(v1alpha1.AnnotationOwnedID, dbUUID)),
			db:         scripted(dbUUID, "Atomic", "Atomic", ""),
			wantEvents: []string{string(ReasonOrphaned)},
		},
		"ClusterOwnedEverywhereWhileDeleting": {
			mr: newDB(owned(dbUUID), beingDeleted()),
			db: scripted(dbUUID, "Atomic", "Atomic", "").
				On(sqlx.ClusterDatabaseIDs(cluster), hostIDs([2]string{"h1", dbUUID}, [2]string{"h2", dbUUID})),
			cluster: cluster,
			want:    managed.ExternalObservation{ResourceExists: true},
		},
		// CREATE ... ON CLUSTER failed on h2 (82) because h2 already had a
		// database of that name; DROP ... ON CLUSTER would drop it there.
		"ClusterForeignHostWhileDeletingIsOrphaned": {
			mr: newDB(owned(dbUUID), beingDeleted()),
			db: scripted(dbUUID, "Atomic", "Atomic", "").
				On(sqlx.ClusterDatabaseIDs(cluster), hostIDs([2]string{"h1", dbUUID}, [2]string{"h2", otherID})),
			cluster:    cluster,
			wantEvents: []string{string(ReasonOrphaned)},
		},
		"ClusterWithoutRemoteWhileDeleting": {
			mr: newDB(owned(dbUUID), beingDeleted()),
			db: scripted(dbUUID, "Atomic", "Atomic", "").
				On(sqlx.ClusterDatabaseIDs(cluster), fake.Error(fake.Exception(497, "necessary to have the grant REMOTE ON *.*"))),
			cluster:    cluster,
			wantReason: reason.PermissionDenied,
		},
		"AdoptionRejectedByPolicy": {
			mr:         newDB(owned(otherID), annotated(v1alpha1.AnnotationAdopt, dbUUID)),
			db:         scripted(dbUUID, "Atomic", "Atomic", ""),
			wantReason: reason.AdoptionRejected,
			wantReady:  "False",
		},
		"Adopted": {
			mr:        newDB(owned(otherID), annotated(v1alpha1.AnnotationAdopt, dbUUID), atomic("")),
			db:        scripted(dbUUID, "Atomic", "Atomic", ""),
			adoption:  true,
			want:      managed.ExternalObservation{ResourceExists: true, ResourceUpToDate: true},
			wantReady: "True",
			check: func(t *testing.T, mr *v1alpha1.Database, kube client.Client, rec *recorder) {
				t.Helper()
				got := &v1alpha1.Database{}
				_ = kube.Get(context.Background(), client.ObjectKeyFromObject(mr), got)
				a := got.GetAnnotations()
				if a[v1alpha1.AnnotationOwnedID] != dbUUID || a[v1alpha1.AnnotationOwnerUID] != mrUID {
					t.Errorf("claim not persisted: %v", a)
				}
				if _, ok := a[v1alpha1.AnnotationAdopt]; ok {
					t.Error("adopt annotation not removed")
				}
				if diff := cmp.Diff([]string{"Adopted"}, rec.reasons()); diff != "" {
					t.Errorf("events: %s", diff)
				}
			},
		},
		"ObserveOnlyNotOwned": {
			mr:        newDB(managementPolicies(xpv2.ManagementActionObserve), atomic("x")),
			db:        scripted(dbUUID, "Atomic", "Atomic", "y"),
			want:      managed.ExternalObservation{ResourceExists: true, ResourceUpToDate: false},
			wantReady: "True",
			check: func(t *testing.T, mr *v1alpha1.Database, _ client.Client, _ *recorder) {
				t.Helper()
				if mr.GetAnnotations()[v1alpha1.AnnotationOwnerUID] != "" {
					t.Error("observe-only must not claim")
				}
			},
		},
		"UnknownCluster": {
			mr:         newDB(owned(dbUUID)),
			db:         fake.NewDB().On(ch.QueryClusterHosts, fake.Row(uint64(0))),
			cluster:    "nope",
			wantReason: reason.InvalidConfig,
			wantReady:  "False",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			kube := kubeWith(t, tc.mr.DeepCopy())
			rec := &recorder{}
			e := &External{Client: fake.NewClient(tc.db), Resolved: resolved(t, tc.cluster, tc.adoption), Kube: kube, Recorder: rec}
			got, err := e.Observe(context.Background(), tc.mr)

			switch {
			case tc.wantAnyErr:
				if err == nil || got.ResourceExists {
					t.Fatalf("a transport error must be returned, never 'not found': %+v, %v", got, err)
				}
			case tc.wantReason != "":
				if !reason.Is(err, tc.wantReason) {
					t.Fatalf("Observe() error = %v, want %s", err, tc.wantReason)
				}
			case err != nil:
				t.Fatalf("Observe() error = %v", err)
			default:
				if diff := cmp.Diff(tc.want, got); diff != "" {
					t.Errorf("Observe() -want +got:\n%s", diff)
				}
			}
			if tc.wantReady != "" && readyStatus(tc.mr) != tc.wantReady {
				t.Errorf("Ready = %q, want %q", readyStatus(tc.mr), tc.wantReady)
			}
			if tc.wantNoQuery && len(tc.db.Seen) > 0 {
				t.Errorf("no query may run before the policy check, ran %v", tc.db.Seen)
			}
			if len(tc.db.Execs) > 0 {
				t.Errorf("Observe must never write, ran %v", tc.db.Execs)
			}
			if tc.wantEvents != nil {
				if diff := cmp.Diff(tc.wantEvents, rec.reasons()); diff != "" {
					t.Errorf("events -want +got:\n%s", diff)
				}
			}
			if tc.check != nil {
				tc.check(t, tc.mr, kube, rec)
			}
		})
	}
}

// --- Create ---

func TestCreate(t *testing.T) {
	cases := map[string]struct {
		mr         *v1alpha1.Database
		cluster    string
		policy     *v1alpha1.ClusterPolicy
		execErr    error
		wantStmt   string
		wantReason reason.Reason
	}{
		"AtomicWithComment": {
			mr:       newDB(owned(dbUUID), atomic("it's mine")),
			wantStmt: "CREATE DATABASE `team-a_analytics` UUID '" + dbUUID + "' ENGINE = Atomic COMMENT 'it\\'s mine'",
		},
		"DefaultEngine": {
			mr:       newDB(owned(dbUUID)),
			wantStmt: "CREATE DATABASE `team-a_analytics` UUID '" + dbUUID + "'",
		},
		"ReplicatedOnCluster": {
			mr: newDB(owned(dbUUID), func(d *v1alpha1.Database) {
				d.Spec.ForProvider.Engine = &v1alpha1.DatabaseEngine{Name: "Replicated"}
			}),
			cluster:  "my-cluster",
			wantStmt: "CREATE DATABASE `team-a_analytics` UUID '" + dbUUID + "' ON CLUSTER `my-cluster` ENGINE = Replicated('/clickhouse/databases/{uuid}', '{shard}', '{replica}')",
		},
		"ReplicatedCustomPathRefusedByPolicy": {
			mr: newDB(owned(dbUUID), func(d *v1alpha1.Database) {
				d.Spec.ForProvider.Engine = &v1alpha1.DatabaseEngine{Name: "Replicated", Replicated: &v1alpha1.ReplicatedEngine{ZooKeeperPath: "/clickhouse/databases/team-b_prod"}}
			}),
			wantReason: reason.PolicyViolation,
		},
		"ReplicatedNestedPathRefusedByPolicy": {
			mr: newDB(owned(dbUUID), func(d *v1alpha1.Database) {
				d.Spec.ForProvider.Engine = &v1alpha1.DatabaseEngine{Name: "Replicated", Replicated: &v1alpha1.ReplicatedEngine{ZooKeeperPath: "/clickhouse/databases/victim/replicas/{uuid}"}}
			}),
			wantReason: reason.PolicyViolation,
		},
		"ReplicatedNamespacedPathAllowed": {
			mr: newDB(owned(dbUUID), func(d *v1alpha1.Database) {
				d.Spec.ForProvider.Engine = &v1alpha1.DatabaseEngine{Name: "Replicated", Replicated: &v1alpha1.ReplicatedEngine{ZooKeeperPath: "/clickhouse/databases/team-a/{uuid}"}}
			}),
			policy: &v1alpha1.ClusterPolicy{
				AllowedNamespaces:     []string{"team-.*"},
				DatabaseNamePatterns:  []string{"{namespace}_[a-z0-9_]+"},
				ZooKeeperPathPatterns: []string{"/clickhouse/databases/{namespace}/\\{uuid\\}"},
			},
			wantStmt: "CREATE DATABASE `team-a_analytics` UUID '" + dbUUID + "' ENGINE = Replicated('/clickhouse/databases/team-a/{uuid}', '{shard}', '{replica}')",
		},
		"ReplicatedOtherNamespacePathRefused": {
			mr: newDB(owned(dbUUID), func(d *v1alpha1.Database) {
				d.Spec.ForProvider.Engine = &v1alpha1.DatabaseEngine{Name: "Replicated", Replicated: &v1alpha1.ReplicatedEngine{ZooKeeperPath: "/clickhouse/databases/team-b/{uuid}"}}
			}),
			policy: &v1alpha1.ClusterPolicy{
				AllowedNamespaces:     []string{"team-.*"},
				DatabaseNamePatterns:  []string{"{namespace}_[a-z0-9_]+"},
				ZooKeeperPathPatterns: []string{"/clickhouse/databases/{namespace}/\\{uuid\\}"},
			},
			wantReason: reason.PolicyViolation,
		},
		"ReplicatedCustomPathUnrestricted": {
			mr: newDB(owned(dbUUID), func(d *v1alpha1.Database) {
				d.Spec.ForProvider.Engine = &v1alpha1.DatabaseEngine{Name: "Replicated", Replicated: &v1alpha1.ReplicatedEngine{ZooKeeperPath: "/any", ShardName: "s", ReplicaName: "r"}}
			}),
			policy:   &v1alpha1.ClusterPolicy{Unrestricted: true},
			wantStmt: "CREATE DATABASE `team-a_analytics` UUID '" + dbUUID + "' ENGINE = Replicated('/any', 's', 'r')",
		},
		"NonCanonicalUUID": {
			mr:         newDB(owned(strings.ToUpper(dbUUID))),
			wantReason: reason.InvalidSpec,
		},
		"ZeroUUID": {
			mr:         newDB(owned("00000000-0000-0000-0000-000000000000")),
			wantReason: reason.InvalidSpec,
		},
		"MissingClaim": {
			mr:         newDB(),
			wantReason: reason.Internal,
		},
		"CopiedClaim": {
			mr:         newDB(annotated(v1alpha1.AnnotationOwnerUID, "someone-else"), annotated(v1alpha1.AnnotationOwnedID, dbUUID)),
			wantReason: reason.Internal,
		},
		"NameTooLong": {
			mr:         newDB(owned(dbUUID), func(d *v1alpha1.Database) { d.Spec.ForProvider.Name = new(strings.Repeat("@", 90)) }),
			wantReason: reason.InvalidSpec,
		},
		"AlreadyExists": {
			mr:         newDB(owned(dbUUID)),
			execErr:    fake.Exception(82, "Database team-a_analytics already exists"),
			wantStmt:   "CREATE DATABASE `team-a_analytics` UUID '" + dbUUID + "'",
			wantReason: reason.AlreadyExists,
		},
		"PermissionDenied": {
			mr:         newDB(owned(dbUUID)),
			execErr:    fake.Exception(497, "necessary to have the grant CREATE DATABASE"),
			wantStmt:   "CREATE DATABASE `team-a_analytics` UUID '" + dbUUID + "'",
			wantReason: reason.PermissionDenied,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			db := fake.NewDB()
			db.ExecFn = func(string) error { return tc.execErr }
			res := resolved(t, tc.cluster, false)
			if tc.policy != nil {
				res = resolvedWith(t, tc.cluster, *tc.policy)
			}
			e := &External{Client: fake.NewClient(db), Resolved: res, Kube: kubeWith(t), Recorder: &recorder{}}
			_, err := e.Create(context.Background(), tc.mr)
			if tc.wantReason != "" {
				if !reason.Is(err, tc.wantReason) {
					t.Fatalf("Create() error = %v, want %s", err, tc.wantReason)
				}
			} else if err != nil {
				t.Fatalf("Create() error = %v", err)
			}
			var want []string
			if tc.wantStmt != "" {
				want = []string{tc.wantStmt}
			}
			if diff := cmp.Diff(want, db.Execs); diff != "" {
				t.Errorf("statements -want +got:\n%s", diff)
			}
		})
	}
}

// --- Update ---

// owns scripts the pre-write ownership re-check on the connected server.
func owns(db *fake.DB, id string) *fake.DB {
	return db.On(sqlx.QueryDatabaseUUID, fake.Row(id))
}

// gone scripts a database that no longer exists on the connected server.
func gone(db *fake.DB) *fake.DB {
	return db.On(sqlx.QueryDatabaseUUID, fake.Rows()).On(sqlx.ExistsDatabase(dbName), fake.Row(uint8(0)))
}

func TestUpdate(t *testing.T) {
	alter := "ALTER DATABASE `team-a_analytics` MODIFY COMMENT 'new comment'"
	cases := map[string]struct {
		mr         *v1alpha1.Database
		db         *fake.DB
		cluster    string
		execErr    error
		wantStmts  []string
		wantReason reason.Reason
	}{
		"Owned": {
			mr:        newDB(owned(dbUUID), atomic("new comment")),
			db:        owns(fake.NewDB(), dbUUID),
			wantStmts: []string{alter},
		},
		"GoneBetweenCheckAndAlter": {
			mr:        newDB(owned(dbUUID), atomic("new comment")),
			db:        owns(fake.NewDB(), dbUUID),
			execErr:   fake.Exception(81, "Database does not exist"),
			wantStmts: []string{alter},
		},
		"GoneSinceObserve": {
			mr: newDB(owned(dbUUID), atomic("new comment")),
			db: gone(fake.NewDB()),
		},
		"ReplacedSinceObserve": {
			mr:         newDB(owned(dbUUID), atomic("new comment")),
			db:         owns(fake.NewDB(), otherID),
			wantReason: reason.Replaced,
		},
		"FilteredSinceObserve": {
			mr:         newDB(owned(dbUUID), atomic("new comment")),
			db:         fake.NewDB().On(sqlx.QueryDatabaseUUID, fake.Rows()).On(sqlx.ExistsDatabase(dbName), fake.Row(uint8(1))),
			wantReason: reason.PermissionDenied,
		},
		"NoClaim": {
			mr:         newDB(atomic("new comment")),
			db:         fake.NewDB(),
			wantReason: reason.NotOwned,
		},
		"NothingToUpdate": {
			mr: newDB(owned(dbUUID)),
			db: fake.NewDB(),
		},
		"ClusterOwnedEverywhere": {
			mr:        newDB(owned(dbUUID), atomic("")),
			db:        owns(fake.NewDB(), dbUUID).On(sqlx.ClusterDatabaseIDs(cluster), hostIDs([2]string{"h1", dbUUID}, [2]string{"h2", dbUUID})),
			cluster:   cluster,
			wantStmts: []string{"ALTER DATABASE `team-a_analytics` ON CLUSTER `my-cluster` MODIFY COMMENT ''"},
		},
		"ClusterMissingOnSomeHost": {
			mr:        newDB(owned(dbUUID), atomic("")),
			db:        owns(fake.NewDB(), dbUUID).On(sqlx.ClusterDatabaseIDs(cluster), hostIDs([2]string{"h1", dbUUID})),
			cluster:   cluster,
			wantStmts: []string{"ALTER DATABASE `team-a_analytics` ON CLUSTER `my-cluster` MODIFY COMMENT ''"},
		},
		"ClusterForeignHost": {
			mr:         newDB(owned(dbUUID), atomic("")),
			db:         owns(fake.NewDB(), dbUUID).On(sqlx.ClusterDatabaseIDs(cluster), hostIDs([2]string{"h1", dbUUID}, [2]string{"h2", otherID})),
			cluster:    cluster,
			wantReason: reason.NotOwned,
		},
		"ClusterWithoutRemote": {
			mr:         newDB(owned(dbUUID), atomic("")),
			db:         owns(fake.NewDB(), dbUUID).On(sqlx.ClusterDatabaseIDs(cluster), fake.Error(fake.Exception(497, "necessary to have the grant REMOTE ON *.*"))),
			cluster:    cluster,
			wantReason: reason.PermissionDenied,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			tc.db.ExecFn = func(string) error { return tc.execErr }
			e := &External{Client: fake.NewClient(tc.db), Resolved: resolved(t, tc.cluster, false), Kube: kubeWith(t), Recorder: &recorder{}}
			_, err := e.Update(context.Background(), tc.mr)
			if tc.wantReason != "" {
				if !reason.Is(err, tc.wantReason) {
					t.Fatalf("Update() error = %v, want %s", err, tc.wantReason)
				}
			} else if err != nil {
				t.Fatalf("Update() error = %v", err)
			}
			if diff := cmp.Diff(tc.wantStmts, tc.db.Execs); diff != "" {
				t.Errorf("statements -want +got:\n%s", diff)
			}
		})
	}
}

// --- Delete ---

func TestDelete(t *testing.T) {
	drop := "DROP DATABASE IF EXISTS `team-a_analytics`"
	emptyAndOwned := func() *fake.DB {
		return owns(fake.NewDB(), dbUUID).
			On(sqlx.CheckGrantShowTables(dbName), fake.Row(uint8(1))).
			On(sqlx.QueryDatabaseTableCount, fake.Row(uint64(0)))
	}
	force := func(d *v1alpha1.Database) { d.Spec.ForProvider.ForceDrop = new(true) }
	cases := map[string]struct {
		mr         *v1alpha1.Database
		db         *fake.DB
		cluster    string
		wantStmts  []string
		wantReason reason.Reason
		wantAnyErr bool
	}{
		"Empty": {
			mr:        newDB(owned(dbUUID), beingDeleted()),
			db:        emptyAndOwned(),
			wantStmts: []string{drop},
		},
		"CannotVerify": {
			mr:         newDB(owned(dbUUID), beingDeleted()),
			db:         fake.NewDB().On(sqlx.CheckGrantShowTables(dbName), fake.Row(uint8(0))),
			wantReason: reason.DatabaseNotEmpty,
		},
		"NotEmpty": {
			mr: newDB(owned(dbUUID), beingDeleted()),
			db: fake.NewDB().
				On(sqlx.CheckGrantShowTables(dbName), fake.Row(uint8(1))).
				On(sqlx.QueryDatabaseTableCount, fake.Row(uint64(3))),
			wantReason: reason.DatabaseNotEmpty,
		},
		"ForceDrop": {
			mr:        newDB(owned(dbUUID), beingDeleted(), force),
			db:        owns(fake.NewDB(), dbUUID),
			wantStmts: []string{drop},
		},
		// Dropped and re-created by someone else between Observe and Delete.
		"ReplacedSinceObserve": {
			mr:         newDB(owned(dbUUID), beingDeleted(), force),
			db:         owns(fake.NewDB(), otherID),
			wantReason: reason.Replaced,
		},
		"AlreadyGone": {
			mr: newDB(owned(dbUUID), beingDeleted(), force),
			db: gone(fake.NewDB()),
		},
		"NoClaim": {
			mr:         newDB(beingDeleted(), force),
			db:         fake.NewDB(),
			wantReason: reason.NotOwned,
		},
		"CopiedClaim": {
			mr:         newDB(beingDeleted(), force, annotated(v1alpha1.AnnotationOwnerUID, "someone-else"), annotated(v1alpha1.AnnotationOwnedID, dbUUID)),
			db:         fake.NewDB(),
			wantReason: reason.NotOwned,
		},
		"OutsidePolicy": {
			mr: newDB(owned(dbUUID), beingDeleted(), force, func(d *v1alpha1.Database) {
				d.Spec.ForProvider.Name = nil
				meta.SetExternalName(d, "team-b_prod")
			}),
			db:         fake.NewDB(),
			wantReason: reason.PolicyViolation,
		},
		"ClusterNeedsForceDrop": {
			mr:         newDB(owned(dbUUID), beingDeleted()),
			db:         fake.NewDB(),
			cluster:    cluster,
			wantReason: reason.DatabaseNotEmpty,
		},
		"ClusterForceDropOwnedEverywhere": {
			mr:        newDB(owned(dbUUID), beingDeleted(), force),
			db:        owns(fake.NewDB(), dbUUID).On(sqlx.ClusterDatabaseIDs(cluster), hostIDs([2]string{"h1", dbUUID}, [2]string{"h2", dbUUID})),
			cluster:   cluster,
			wantStmts: []string{drop + " ON CLUSTER `my-cluster`"},
		},
		"ClusterForeignHost": {
			mr:         newDB(owned(dbUUID), beingDeleted(), force),
			db:         owns(fake.NewDB(), dbUUID).On(sqlx.ClusterDatabaseIDs(cluster), hostIDs([2]string{"h1", dbUUID}, [2]string{"h2", otherID})),
			cluster:    cluster,
			wantReason: reason.NotOwned,
		},
		"ClusterWithoutRemote": {
			mr:         newDB(owned(dbUUID), beingDeleted(), force),
			db:         owns(fake.NewDB(), dbUUID).On(sqlx.ClusterDatabaseIDs(cluster), fake.Error(fake.Exception(497, "necessary to have the grant REMOTE ON *.*"))),
			cluster:    cluster,
			wantReason: reason.PermissionDenied,
		},
		"ClusterHostUnreachable": {
			mr:         newDB(owned(dbUUID), beingDeleted(), force),
			db:         owns(fake.NewDB(), dbUUID).On(sqlx.ClusterDatabaseIDs(cluster), fake.Error(fake.Exception(279, "All connection tries failed"))),
			cluster:    cluster,
			wantAnyErr: true,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e := &External{Client: fake.NewClient(tc.db), Resolved: resolved(t, tc.cluster, false), Kube: kubeWith(t), Recorder: &recorder{}}
			_, err := e.Delete(context.Background(), tc.mr)
			switch {
			case tc.wantAnyErr:
				if err == nil {
					t.Fatal("an unreachable host must fail the ownership check")
				}
			case tc.wantReason != "":
				if !reason.Is(err, tc.wantReason) {
					t.Fatalf("Delete() error = %v, want %s", err, tc.wantReason)
				}
			case err != nil:
				t.Fatalf("Delete() error = %v", err)
			}
			if diff := cmp.Diff(tc.wantStmts, tc.db.Execs); diff != "" {
				t.Errorf("statements -want +got:\n%s", diff)
			}
		})
	}
}

func TestDisconnectReleasesClient(t *testing.T) {
	c := fake.NewClient(fake.NewDB())
	e := &External{Client: c, Resolved: resolved(t, "", false)}
	if err := e.Disconnect(context.Background()); err != nil || c.Closed != 1 {
		t.Fatalf("Disconnect() = %v, closed %d", err, c.Closed)
	}
}
