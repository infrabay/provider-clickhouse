// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

//go:build envtest

package envtest

import (
	"context"
	"testing"

	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/crossplane/crossplane-runtime/v2/pkg/meta"
	"github.com/crossplane/crossplane-runtime/v2/pkg/resource"

	"github.com/infrabay/provider-clickhouse/apis/v1alpha1"
	"github.com/infrabay/provider-clickhouse/internal/controller/grant"
	"github.com/infrabay/provider-clickhouse/internal/controller/providerconfig"
	"github.com/infrabay/provider-clickhouse/internal/controller/rolegrant"
)

// TestSelectorResolution resolves the selectors of a Grant and a RoleGrant
// with the resolver their controllers use (providerconfig.NewReferenceResolver
// around the generated one) against the real API server and CRDs.
// crossplane-runtime writes the reference that a selector resolves to with
// the MR's own namespace and persists it with a server-side apply patch; the
// CRDs must accept it, and the next reconcile must resolve it again.
func TestSelectorResolution(t *testing.T) {
	ctx := context.Background()
	ensureNamespace(t, "sel-a")
	pc := &xpv2.ProviderConfigReference{Kind: v1alpha1.ClusterProviderConfigKind, Name: "shared"}
	om := func(name string, labels map[string]string) metav1.ObjectMeta {
		return metav1.ObjectMeta{Namespace: "sel-a", Name: name, Labels: labels}
	}

	db := &v1alpha1.Database{ObjectMeta: om("shop", map[string]string{"sel": "shop"})}
	db.Spec.ProviderConfigReference = pc
	db.Spec.ForProvider.Name = new("sel-a_shop")
	role := &v1alpha1.Role{ObjectMeta: om("reader", map[string]string{"sel": "reader"})}
	role.Spec.ProviderConfigReference = pc
	role.Spec.ForProvider.Name = new("sel-a_reader")
	user := &v1alpha1.User{ObjectMeta: om("api", map[string]string{"sel": "api"})}
	user.Spec.ProviderConfigReference = pc
	user.Spec.ForProvider.Name = new("sel-a_api")
	user.Spec.ForProvider.Authentication.JWT = &v1alpha1.JWTAuthentication{Processor: "kubernetes"}
	for _, mg := range []resource.Managed{db, role, user} {
		// The resolvers read the external name, which the initializer sets.
		meta.SetExternalName(mg, *nameOf(mg))
		if err := kube.Create(ctx, mg); err != nil {
			t.Fatal(err)
		}
	}

	g := &v1alpha1.Grant{ObjectMeta: om("reader-shop", nil)}
	g.Spec.ProviderConfigReference = pc
	g.Spec.ForProvider = v1alpha1.GrantParameters{
		Privileges:       []string{"SELECT"},
		DatabaseSelector: &xpv2.NamespacedSelector{MatchLabels: map[string]string{"sel": "shop"}},
		RoleSelector:     &xpv2.NamespacedSelector{MatchLabels: map[string]string{"sel": "reader"}},
	}
	rg := &v1alpha1.RoleGrant{ObjectMeta: om("api-reader", nil)}
	rg.Spec.ProviderConfigReference = pc
	rg.Spec.ForProvider = v1alpha1.RoleGrantParameters{
		MemberOfSelector: &xpv2.NamespacedSelector{MatchLabels: map[string]string{"sel": "reader"}},
		UserSelector:     &xpv2.NamespacedSelector{MatchLabels: map[string]string{"sel": "api"}},
	}
	for _, mg := range []resource.Managed{g, rg} {
		if err := kube.Create(ctx, mg); err != nil {
			t.Fatal(err)
		}
	}

	resolveTwice := func(mg resource.Managed, refs providerconfig.ReferencesFunc) {
		t.Helper()
		r := providerconfig.NewReferenceResolver(kube, refs)
		for i := range 2 {
			if err := kube.Get(ctx, client.ObjectKeyFromObject(mg), mg); err != nil {
				t.Fatal(err)
			}
			// The resolver persists with a server-side apply patch, which
			// needs the kind.
			mg.GetObjectKind().SetGroupVersionKind(gvkOf(t, mg))
			if err := r.ResolveReferences(ctx, mg); err != nil {
				t.Fatalf("reconcile %d of %s: %v", i+1, mg.GetName(), err)
			}
		}
		if err := kube.Get(ctx, client.ObjectKeyFromObject(mg), mg); err != nil {
			t.Fatal(err)
		}
	}

	resolveTwice(g, func(mg resource.Managed) []providerconfig.Reference { return grant.References(mg.(*v1alpha1.Grant)) })
	p := g.Spec.ForProvider
	if p.Database == nil || *p.Database != "sel-a_shop" || p.Role == nil || *p.Role != "sel-a_reader" {
		t.Errorf("Grant resolved database %v, role %v", p.Database, p.Role)
	}
	wantRef(t, "databaseRef", p.DatabaseRef, "shop")
	wantRef(t, "roleRef", p.RoleRef, "reader")
	if err := providerconfig.CheckReferencedConfigs(ctx, kube, g, grant.References(g)); err != nil {
		t.Errorf("Connect's reference check: %v", err)
	}

	resolveTwice(rg, func(mg resource.Managed) []providerconfig.Reference {
		return rolegrant.References(mg.(*v1alpha1.RoleGrant))
	})
	q := rg.Spec.ForProvider
	if q.MemberOf == nil || *q.MemberOf != "sel-a_reader" || q.User == nil || *q.User != "sel-a_api" {
		t.Errorf("RoleGrant resolved memberOf %v, user %v", q.MemberOf, q.User)
	}
	wantRef(t, "memberOfRef", q.MemberOfRef, "reader")
	wantRef(t, "userRef", q.UserRef, "api")
	if err := providerconfig.CheckReferencedConfigs(ctx, kube, rg, rolegrant.References(rg)); err != nil {
		t.Errorf("Connect's reference check: %v", err)
	}
}

func wantRef(t *testing.T, field string, ref *xpv2.NamespacedReference, name string) {
	t.Helper()
	if ref == nil || ref.Name != name || (ref.Namespace != "" && ref.Namespace != "sel-a") {
		t.Errorf("%s = %+v, want %q in the MR's own namespace", field, ref, name)
	}
}

func nameOf(mg resource.Managed) *string {
	switch o := mg.(type) {
	case *v1alpha1.Database:
		return o.Spec.ForProvider.Name
	case *v1alpha1.Role:
		return o.Spec.ForProvider.Name
	case *v1alpha1.User:
		return o.Spec.ForProvider.Name
	}
	return nil
}

func gvkOf(t *testing.T, mg resource.Managed) schema.GroupVersionKind {
	t.Helper()
	gvks, _, err := kube.Scheme().ObjectKinds(mg)
	if err != nil || len(gvks) == 0 {
		t.Fatalf("no kind for %T: %v", mg, err)
	}
	return gvks[0]
}
