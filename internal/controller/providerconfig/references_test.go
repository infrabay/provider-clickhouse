// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package providerconfig

import (
	"context"
	"errors"
	"strings"
	"testing"

	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/crossplane/crossplane-runtime/v2/pkg/meta"
	"github.com/crossplane/crossplane-runtime/v2/pkg/resource"

	"github.com/infrabay/provider-clickhouse/apis/v1alpha1"
	"github.com/infrabay/provider-clickhouse/internal/reason"
)

func grantRefs(mg resource.Managed) []Reference {
	g := mg.(*v1alpha1.Grant)
	p := g.Spec.ForProvider
	return []Reference{
		{Field: "databaseRef", Ref: p.DatabaseRef, Selector: p.DatabaseSelector, New: func() resource.ModernManaged { return &v1alpha1.Database{} }},
		{Field: "userRef", Ref: p.UserRef, Selector: p.UserSelector, New: func() resource.ModernManaged { return &v1alpha1.User{} }},
		{Field: "roleRef", Ref: p.RoleRef, Selector: p.RoleSelector, New: func() resource.ModernManaged { return &v1alpha1.Role{} }},
	}
}

func pcRef(kind, name string) *xpv2.ProviderConfigReference {
	return &xpv2.ProviderConfigReference{Kind: kind, Name: name}
}

func testGrant(mutate func(*v1alpha1.GrantParameters)) *v1alpha1.Grant {
	g := &v1alpha1.Grant{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "g", UID: "grant-uid"},
		Spec: v1alpha1.GrantSpec{
			ManagedResourceSpec: xpv2.ManagedResourceSpec{ProviderConfigReference: pcRef("ClusterProviderConfig", "shared")},
			ForProvider:         v1alpha1.GrantParameters{Privileges: []string{"SELECT"}, Database: new("team-a_db")},
		},
	}
	mutate(&g.Spec.ForProvider)
	return g
}

func testUser(ns, name, extName string, pc *xpv2.ProviderConfigReference) *v1alpha1.User {
	u := &v1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       v1alpha1.UserSpec{ManagedResourceSpec: xpv2.ManagedResourceSpec{ProviderConfigReference: pc}},
	}
	meta.SetExternalName(u, extName)
	return u
}

func TestCheckReferenceNamespaces(t *testing.T) {
	cases := map[string]struct {
		mutate  func(*v1alpha1.GrantParameters)
		wantErr string
	}{
		"NoReferences":   {mutate: func(p *v1alpha1.GrantParameters) { p.User = new("team-a_api") }},
		"SameNamespace":  {mutate: func(p *v1alpha1.GrantParameters) { p.UserRef = &xpv2.NamespacedReference{Name: "api"} }},
		"EmptyNamespace": {mutate: func(p *v1alpha1.GrantParameters) { p.RoleRef = &xpv2.NamespacedReference{Name: "r", Namespace: ""} }},
		"CrossNamespaceRef": {
			mutate: func(p *v1alpha1.GrantParameters) {
				p.UserRef = &xpv2.NamespacedReference{Name: "api", Namespace: "team-b"}
			},
			wantErr: "userRef: cross-namespace references",
		},
		"CrossNamespaceSelector": {
			mutate: func(p *v1alpha1.GrantParameters) {
				p.DatabaseSelector = &xpv2.NamespacedSelector{MatchLabels: map[string]string{"a": "b"}, Namespace: "team-b"}
			},
			wantErr: "databaseSelector: cross-namespace selectors",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := CheckReferenceNamespaces(grantRefs(testGrant(tc.mutate)))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if !reason.Is(err, reason.InvalidSpec) || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want InvalidSpec %q", err, tc.wantErr)
			}
		})
	}
}

func TestReferenceResolver(t *testing.T) {
	ctx := context.Background()
	shared := pcRef("ClusterProviderConfig", "shared")

	t.Run("ResolvesInNamespace", func(t *testing.T) {
		g := testGrant(func(p *v1alpha1.GrantParameters) { p.UserRef = &xpv2.NamespacedReference{Name: "api"} })
		// The resolver persists the result with a server-side apply patch,
		// which needs the kind.
		g.SetGroupVersionKind(v1alpha1.GrantGroupVersionKind)
		kube := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(g, testUser("team-a", "api", "team-a_api", shared)).Build()
		if err := NewReferenceResolver(kube, grantRefs).ResolveReferences(ctx, g); err != nil {
			t.Fatal(err)
		}
		if g.Spec.ForProvider.User == nil || *g.Spec.ForProvider.User != "team-a_api" {
			t.Errorf("user = %v, want the referenced MR's external name", g.Spec.ForProvider.User)
		}
	})

	// A cross-namespace reference is rejected before the resolver reads
	// anything with the provider's cluster-wide RBAC.
	t.Run("RejectsBeforeReading", func(t *testing.T) {
		errRead := errors.New("the resolver must not read")
		kube := fake.NewClientBuilder().WithScheme(scheme(t)).WithInterceptorFuncs(interceptor.Funcs{
			Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				return errRead
			},
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error { return errRead },
		}).Build()
		for _, g := range []*v1alpha1.Grant{
			testGrant(func(p *v1alpha1.GrantParameters) {
				p.UserRef = &xpv2.NamespacedReference{Name: "api", Namespace: "team-b"}
			}),
			testGrant(func(p *v1alpha1.GrantParameters) {
				p.RoleSelector = &xpv2.NamespacedSelector{MatchLabels: map[string]string{"a": "b"}, Namespace: "team-b"}
			}),
		} {
			err := NewReferenceResolver(kube, grantRefs).ResolveReferences(ctx, g)
			if !reason.Is(err, reason.InvalidSpec) || errors.Is(err, errRead) {
				t.Errorf("ResolveReferences() = %v, want InvalidSpec without a read", err)
			}
		}
	})
}

func TestCheckReferencedConfigs(t *testing.T) {
	ctx := context.Background()
	shared := pcRef("ClusterProviderConfig", "shared")
	kube := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(
		testUser("team-a", "same", "team-a_same", shared),
		testUser("team-a", "other", "team-a_other", pcRef("ClusterProviderConfig", "elsewhere")),
		testUser("team-a", "namespaced", "team-a_ns", pcRef("ProviderConfig", "shared")),
		testUser("team-b", "foreign", "team-b_x", shared),
	).Build()
	ref := func(name string) func(*v1alpha1.GrantParameters) {
		return func(p *v1alpha1.GrantParameters) { p.UserRef = &xpv2.NamespacedReference{Name: name} }
	}
	cases := map[string]struct {
		mutate func(*v1alpha1.GrantParameters)
		want   reason.Reason
	}{
		"SameConfig":     {mutate: ref("same")},
		"OtherConfig":    {mutate: ref("other"), want: reason.InvalidSpec},
		"OtherKind":      {mutate: ref("namespaced"), want: reason.InvalidSpec},
		"MissingIsPlain": {mutate: ref("gone")},
		// Only the MR's own namespace is read.
		"OtherNamespaceIsNotRead": {mutate: ref("foreign")},
		"PlainName":               {mutate: func(p *v1alpha1.GrantParameters) { p.User = new("anything") }},
		"CrossNamespace": {
			mutate: func(p *v1alpha1.GrantParameters) {
				p.UserRef = &xpv2.NamespacedReference{Name: "foreign", Namespace: "team-b"}
			},
			want: reason.InvalidSpec,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			g := testGrant(tc.mutate)
			wantReason(t, name, CheckReferencedConfigs(ctx, kube, g, grantRefs(g)), tc.want)
		})
	}
}
