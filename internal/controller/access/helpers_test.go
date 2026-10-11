// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package access

import (
	"sync"
	"testing"
	"time"

	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/crossplane/crossplane-runtime/v2/pkg/event"
	"github.com/crossplane/crossplane-runtime/v2/pkg/meta"

	"github.com/infrabay/provider-clickhouse/apis/v1alpha1"
	ch "github.com/infrabay/provider-clickhouse/internal/clients/clickhouse"
	"github.com/infrabay/provider-clickhouse/internal/clients/clickhouse/fake"
	"github.com/infrabay/provider-clickhouse/internal/controller/ownership"
	"github.com/infrabay/provider-clickhouse/internal/controller/providerconfig"
)

const (
	ns       = "team-a"
	mrUID    = "mr-uid"
	roleID   = "11111111-1111-1111-1111-111111111111"
	otherID  = "22222222-2222-2222-2222-222222222222"
	userID   = "33333333-3333-3333-3333-333333333333"
	provider = "provider" // fake.NewClient's currentUser()
)

var sharedPC = &xpv2.ProviderConfigReference{Kind: "ClusterProviderConfig", Name: "shared"}

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

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.SchemeBuilder.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func kubeWith(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return clientfake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
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

func namespacedResolved() *providerconfig.Resolved {
	return &providerconfig.Resolved{
		Kind: "ProviderConfig", Name: "own",
		CacheKey:        ch.Key{Identity: "ProviderConfig/team-a/own", Version: "v"},
		Endpoint:        providerconfig.Endpoint{Host: "clickhouse.team-a.svc", Port: 9440, Protocol: v1alpha1.ProtocolNative, TLS: true},
		AdoptionAllowed: true,
	}
}

type testEnv struct {
	*Env
	db   *fake.DB
	cl   *fake.Client
	rec  *recorder
	kube client.Client
}

func newEnv(t *testing.T, db *fake.DB, res *providerconfig.Resolved, objs ...client.Object) *testEnv {
	t.Helper()
	cl := fake.NewClientFor(db, fake.Upstream253)
	kube := kubeWith(t, objs...)
	rec := &recorder{}
	return &testEnv{
		Env: &Env{Client: cl, Resolved: res, Kube: kube, Reader: kube, Recorder: rec, Journal: ownership.NewJournal()},
		db:  db, cl: cl, rec: rec, kube: kube,
	}
}

// --- MR fixtures ---

type roleOpt func(*v1alpha1.Role)

func newRole(name string, opts ...roleOpt) *v1alpha1.Role {
	r := &v1alpha1.Role{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "reader", UID: mrUID}}
	r.Spec.ProviderConfigReference = sharedPC
	if name != "" {
		r.Spec.ForProvider.Name = &name
	}
	for _, o := range opts {
		o(r)
	}
	return r
}

func claimed(ownedID string) roleOpt {
	return func(r *v1alpha1.Role) {
		meta.AddAnnotations(r, map[string]string{v1alpha1.AnnotationOwnerUID: mrUID})
		if ownedID != "" {
			meta.AddAnnotations(r, map[string]string{v1alpha1.AnnotationOwnedID: ownedID})
		}
	}
}

func annotated(k, v string) roleOpt {
	return func(r *v1alpha1.Role) { meta.AddAnnotations(r, map[string]string{k: v}) }
}

func pending() roleOpt {
	return func(r *v1alpha1.Role) { meta.SetExternalCreatePending(r, time.Now()) }
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

func readyStatus(mg interface {
	GetCondition(xpv2.ConditionType) xpv2.Condition
}) string {
	return string(mg.GetCondition(xpv2.TypeReady).Status)
}
