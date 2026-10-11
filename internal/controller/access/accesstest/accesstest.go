// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

// Package accesstest builds managed resources for tests of the access entity
// controllers, for example the User or Role MR that owns the grantee of a
// Grant or RoleGrant.
package accesstest

import (
	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/crossplane/crossplane-runtime/v2/pkg/meta"

	"github.com/infrabay/provider-clickhouse/apis/v1alpha1"
)

// UID returns the UID the builders give an MR named name in namespace ns.
func UID(ns, name string) types.UID { return types.UID("uid-" + ns + "-" + name) }

func claim(o metav1.Object, name, id string) {
	o.SetUID(UID(o.GetNamespace(), o.GetName()))
	meta.AddAnnotations(o, map[string]string{
		meta.AnnotationKeyExternalName: name,
		v1alpha1.AnnotationOwnerUID:    string(o.GetUID()),
		v1alpha1.AnnotationOwnedID:     id,
	})
}

// OwnedUser returns a User MR named mrName in namespace ns whose ClickHouse
// name is name, that uses the ProviderConfig pc and holds a valid ownership
// claim on the user with UUID id, as after its Create or an adoption.
func OwnedUser(ns, mrName, name, id string, pc *xpv2.ProviderConfigReference) *v1alpha1.User {
	u := &v1alpha1.User{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: mrName}}
	u.Spec.ProviderConfigReference = pc
	u.Spec.ForProvider.Name = &name
	claim(u, name, id)
	return u
}

// OwnedRole returns a Role MR named mrName in namespace ns whose ClickHouse
// name is name, that uses the ProviderConfig pc and holds a valid ownership
// claim on the role with UUID id.
func OwnedRole(ns, mrName, name, id string, pc *xpv2.ProviderConfigReference) *v1alpha1.Role {
	r := &v1alpha1.Role{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: mrName}}
	r.Spec.ProviderConfigReference = pc
	r.Spec.ForProvider.Name = &name
	claim(r, name, id)
	return r
}
