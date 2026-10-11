// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package access

import (
	"context"
	"errors"
	"testing"

	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	"github.com/google/go-cmp/cmp"

	"github.com/crossplane/crossplane-runtime/v2/pkg/reconciler/managed"
	"github.com/crossplane/crossplane-runtime/v2/pkg/resource"

	"github.com/infrabay/provider-clickhouse/apis/v1alpha1"
	ch "github.com/infrabay/provider-clickhouse/internal/clients/clickhouse"
	"github.com/infrabay/provider-clickhouse/internal/clients/clickhouse/fake"
	"github.com/infrabay/provider-clickhouse/internal/controller/access/accesstest"
	"github.com/infrabay/provider-clickhouse/internal/controller/providerconfig"
	"github.com/infrabay/provider-clickhouse/internal/reason"
)

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

func grantReferences(g *v1alpha1.Grant) []providerconfig.Reference {
	p := g.Spec.ForProvider
	return []providerconfig.Reference{
		{Field: "userRef", Ref: p.UserRef, Selector: p.UserSelector, New: func() resource.ModernManaged { return &v1alpha1.User{} }},
		{Field: "roleRef", Ref: p.RoleRef, Selector: p.RoleSelector, New: func() resource.ModernManaged { return &v1alpha1.Role{} }},
	}
}

// stubClient is the kind's external client in these tests.
type stubClient struct {
	managed.TypedExternalClient[*v1alpha1.Grant]
	env *Env
}

func TestConnector(t *testing.T) {
	errBoom := errors.New("boom")
	ok := func(t *testing.T) *providerconfig.Resolved { return clusterResolved(t, sharedPolicy()) }
	otherPC := &xpv2.ProviderConfigReference{Kind: "ClusterProviderConfig", Name: "other"}

	cases := map[string]struct {
		mr          *v1alpha1.Grant
		res         func(*testing.T) *providerconfig.Resolved
		resolveErr  error
		trackErr    error
		acquireErr  error
		wantErr     error
		wantReason  reason.Reason
		wantTracks  int
		wantAcquire int
		wantOrphan  bool
	}{
		"ResolveError": {mr: newGrant(), resolveErr: errBoom, wantErr: errBoom},
		"NamespaceDenied": {
			mr: newGrant(grantDeleting()),
			res: func(*testing.T) *providerconfig.Resolved {
				return &providerconfig.Resolved{NamespaceDenied: true, DenyReason: "flag"}
			},
			wantOrphan: true,
		},
		"TrackError":   {mr: newGrant(), res: ok, trackErr: errBoom, wantErr: errBoom, wantTracks: 1},
		"AcquireError": {mr: newGrant(), res: ok, acquireErr: errBoom, wantErr: errBoom, wantTracks: 1, wantAcquire: 1},
		"Success":      {mr: newGrant(), res: ok, wantTracks: 1, wantAcquire: 1},
		"SameConfigReference": {
			mr:  newGrant(func(g *v1alpha1.Grant) { g.Spec.ForProvider.RoleRef = &xpv2.NamespacedReference{Name: "reader"} }),
			res: ok, wantTracks: 1, wantAcquire: 1,
		},
		"OtherConfigReference": {
			mr:  newGrant(func(g *v1alpha1.Grant) { g.Spec.ForProvider.UserRef = &xpv2.NamespacedReference{Name: "elsewhere"} }),
			res: ok, wantReason: reason.InvalidSpec, wantTracks: 1,
		},
		"CrossNamespaceReference": {
			mr: newGrant(func(g *v1alpha1.Grant) {
				g.Spec.ForProvider.RoleRef = &xpv2.NamespacedReference{Name: "reader", Namespace: "team-b"}
			}),
			res: ok, wantReason: reason.InvalidSpec, wantTracks: 1,
		},
		// References are not resolved while deleting; deletion is never
		// blocked by them.
		"ReferencesNotCheckedWhileDeleting": {
			mr: newGrant(grantDeleting(), func(g *v1alpha1.Grant) {
				g.Spec.ForProvider.UserRef = &xpv2.NamespacedReference{Name: "elsewhere"}
			}),
			res: ok, wantTracks: 1, wantAcquire: 1,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			tr := &tracker{err: tc.trackErr}
			acquired := 0
			kube := kubeWith(t,
				accesstest.OwnedRole(ns, "reader", roleName, roleID, sharedPC),
				accesstest.OwnedUser(ns, "elsewhere", userName, userID, otherPC))
			var built *Env
			c := &Connector[*v1alpha1.Grant]{
				Kind:      "Grant",
				Kube:      kube,
				APIReader: kube,
				Usage:     tr,
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
				References: grantReferences,
				NewClient: func(env *Env) managed.TypedExternalClient[*v1alpha1.Grant] {
					built = env
					return &stubClient{env: env}
				},
			}
			ext, err := c.Connect(context.Background(), tc.mr)
			switch {
			case tc.wantErr != nil:
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Connect() error = %v, want %v", err, tc.wantErr)
				}
			case tc.wantReason != "":
				if !reason.Is(err, tc.wantReason) {
					t.Fatalf("Connect() error = %v, want %s", err, tc.wantReason)
				}
			case err != nil:
				t.Fatalf("Connect() error = %v", err)
			}
			if tr.calls != tc.wantTracks || acquired != tc.wantAcquire {
				t.Errorf("tracks %d (want %d), acquires %d (want %d)", tr.calls, tc.wantTracks, acquired, tc.wantAcquire)
			}
			if _, isOrphan := ext.(*Orphan[*v1alpha1.Grant]); isOrphan != tc.wantOrphan {
				t.Errorf("orphan client = %t, want %t", isOrphan, tc.wantOrphan)
			}
			if tc.wantAcquire == 1 && tc.wantErr == nil {
				if built == nil || built.Client == nil || built.Resolved == nil || built.Kube == nil || built.Reader == nil || built.APIReader == nil || built.Recorder == nil {
					t.Errorf("the external client must get a complete Env: %+v", built)
				}
			}
		})
	}
}

func TestOrphan(t *testing.T) {
	ctx := context.Background()
	rec := &recorder{}
	o := &Orphan[*v1alpha1.Grant]{Kind: "Grant", Why: "namespaced ProviderConfigs are disabled", Recorder: rec}
	obs, err := o.Observe(ctx, newGrant(grantDeleting()))
	if err != nil || obs.ResourceExists {
		t.Fatalf("Observe() = %+v, %v", obs, err)
	}
	if diff := cmp.Diff([]string{"OrphanedByPolicy"}, rec.reasons()); diff != "" {
		t.Errorf("events: %s", diff)
	}
	if _, err := o.Create(ctx, newGrant()); !reason.Is(err, reason.PolicyViolation) {
		t.Errorf("Create() = %v", err)
	}
	if _, err := o.Update(ctx, newGrant()); !reason.Is(err, reason.PolicyViolation) {
		t.Errorf("Update() = %v", err)
	}
	if _, err := o.Delete(ctx, newGrant()); err != nil {
		t.Errorf("Delete() = %v", err)
	}
	if err := o.Disconnect(ctx); err != nil {
		t.Errorf("Disconnect() = %v", err)
	}
}

func TestEnvDisconnect(t *testing.T) {
	cl := fake.NewClient(fake.NewDB())
	if err := (&Env{Client: cl}).Disconnect(context.Background()); err != nil || cl.Closed != 1 {
		t.Errorf("Disconnect() = %v, closed %d", err, cl.Closed)
	}
}

func TestEnvWithoutConfigFailsClosed(t *testing.T) {
	env := &Env{Client: fake.NewClient(fake.NewDB())}
	if err := env.Policy().CheckRoleName("team-a", "team-a_reader"); !reason.Is(err, reason.PolicyViolation) {
		t.Errorf("an Env without a resolved config must fail closed: %v", err)
	}
}
