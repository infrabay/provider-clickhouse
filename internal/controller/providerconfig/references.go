// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package providerconfig

import (
	"context"
	"fmt"

	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/crossplane/crossplane-runtime/v2/pkg/reconciler/managed"
	"github.com/crossplane/crossplane-runtime/v2/pkg/resource"

	"github.com/infrabay/provider-clickhouse/internal/reason"
)

// A Reference is one reference of a managed resource to another: the *Ref
// and *Selector fields of one spec field (for example Grant's userRef and
// userSelector) and the kind they refer to.
type Reference struct {
	// Field names the reference in messages, e.g. "userRef".
	Field string
	// Ref and Selector are the reference and selector fields, or nil.
	Ref      *xpv2.NamespacedReference
	Selector *xpv2.NamespacedSelector
	// New returns an empty object of the referenced kind, e.g.
	// &v1alpha1.User{}.
	New func() resource.ModernManaged
}

// A ReferencesFunc returns the references of a managed resource.
type ReferencesFunc func(mg resource.Managed) []Reference

// CheckReferenceNamespaces returns InvalidSpec if any reference or selector
// of an MR in namespace names another namespace. References never leave the
// MR's namespace: the generated resolver would read the referenced MR from
// that namespace with the provider's cluster-wide RBAC, before Connect and
// before any policy check (design 3.1). The MR's own namespace is accepted:
// crossplane-runtime writes it into the reference that a selector resolves
// to. CEL rejects every non-empty selector namespace too, but cannot reject
// a reference's namespace, because it cannot see metadata.namespace.
func CheckReferenceNamespaces(refs []Reference, namespace string) error {
	for _, r := range refs {
		if r.Ref != nil && r.Ref.Namespace != "" && r.Ref.Namespace != namespace {
			return reason.Errorf(reason.InvalidSpec, "%s: cross-namespace references are not allowed (namespace %q)", r.Field, r.Ref.Namespace)
		}
		if r.Selector != nil && r.Selector.Namespace != "" && r.Selector.Namespace != namespace {
			return reason.Errorf(reason.InvalidSpec, "%s: cross-namespace selectors are not allowed (namespace %q)", selectorField(r.Field), r.Selector.Namespace)
		}
	}
	return nil
}

func selectorField(refField string) string {
	if n := len(refField); n > 3 && refField[n-3:] == "Ref" {
		return refField[:n-3] + "Selector"
	}
	return refField
}

// NewReferenceResolver returns crossplane-runtime's reference resolver, which
// calls the generated ResolveReferences method, wrapped so that a
// cross-namespace reference or selector is rejected before resolution reads
// anything. Pass it with managed.WithReferenceResolver.
func NewReferenceResolver(kube client.Client, refs ReferencesFunc) managed.ReferenceResolver {
	inner := managed.NewAPISimpleReferenceResolver(kube)
	return managed.ReferenceResolverFn(func(ctx context.Context, mg resource.Managed) error {
		if err := CheckReferenceNamespaces(refs(mg), mg.GetNamespace()); err != nil {
			return err
		}
		return inner.ResolveReferences(ctx, mg)
	})
}

// CheckReferencedConfigs returns InvalidSpec unless every MR that mg
// references (through a resolved *Ref, read from mg's namespace) uses the
// same ProviderConfig, kind and name, as mg. Plain names are not checked. A
// referenced MR that no longer exists is skipped: references are not
// resolved while an MR is being deleted, and its resolved names are then
// plain names.
func CheckReferencedConfigs(ctx context.Context, kube client.Reader, mg resource.ModernManaged, refs []Reference) error {
	if err := CheckReferenceNamespaces(refs, mg.GetNamespace()); err != nil {
		return err
	}
	for _, r := range refs {
		if r.Ref == nil || r.Ref.Name == "" || r.New == nil {
			continue
		}
		obj := r.New()
		err := kube.Get(ctx, types.NamespacedName{Namespace: mg.GetNamespace(), Name: r.Ref.Name}, obj)
		if kerrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("cannot get the MR referenced by %s: %w", r.Field, err)
		}
		if !SameConfig(obj.GetProviderConfigReference(), mg.GetProviderConfigReference()) {
			return reason.Errorf(reason.InvalidSpec, "%s: %q uses another ProviderConfig (%s); a referenced resource must use the same ProviderConfig as the resource that references it",
				r.Field, r.Ref.Name, configString(obj.GetProviderConfigReference()))
		}
	}
	return nil
}

func configString(r *xpv2.ProviderConfigReference) string {
	if r == nil {
		return "none"
	}
	return r.Kind + " " + r.Name
}
