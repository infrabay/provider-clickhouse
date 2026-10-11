// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package ownership

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/crossplane/crossplane-runtime/v2/pkg/meta"
	"github.com/crossplane/crossplane-runtime/v2/pkg/reconciler/managed"
	"github.com/crossplane/crossplane-runtime/v2/pkg/resource"

	"github.com/infrabay/provider-clickhouse/apis/v1alpha1"
)

// NameFunc returns spec.forProvider.name of a managed resource.
type NameFunc func(mg resource.Managed) *string

// An Initializer replaces crossplane-runtime's NameAsExternalName for
// Database, User and Role. It:
//   - sets the external-name annotation to the effective name if it is empty,
//     and fails with ExternalNameConflict if forProvider.name and the
//     annotation differ;
//   - when the management policies include Create and the MR holds no valid
//     claim, writes owner-uid = metadata.uid. For a Database it also writes a
//     fresh random owned-id, which CREATE DATABASE then uses as the UUID;
//     for a User or Role it removes owned-id (Create sets it). Create history
//     annotations of a previous owner are removed with an invalid claim, so a
//     copied or restored manifest is reported as NotOwned, not Replaced.
//
// It does nothing while the MR is being deleted, so a conflict can never
// block deletion. Changes are persisted with a client Update.
type Initializer struct {
	kube       client.Client
	name       NameFunc
	preassign  bool
	newOwnedID func() string
}

// An InitializerOption configures an Initializer.
type InitializerOption func(*Initializer)

// WithPreassignedID makes the initializer write a random owned-id before
// Create (Database).
func WithPreassignedID() InitializerOption {
	return func(i *Initializer) { i.preassign = true }
}

// WithIDGenerator sets the owned-id generator. Used by tests.
func WithIDGenerator(fn func() string) InitializerOption {
	return func(i *Initializer) { i.newOwnedID = fn }
}

// NewInitializer returns an Initializer.
func NewInitializer(kube client.Client, name NameFunc, opts ...InitializerOption) *Initializer {
	i := &Initializer{kube: kube, name: name, newOwnedID: func() string { return uuid.NewString() }}
	for _, o := range opts {
		o(i)
	}
	return i
}

// Initialize implements managed.Initializer.
func (i *Initializer) Initialize(ctx context.Context, mg resource.Managed) error {
	if meta.WasDeleted(mg) {
		return nil
	}
	if _, err := EffectiveName(mg, i.name(mg)); err != nil {
		return err
	}
	if !Prepare(mg, i.name(mg), i.preassign, i.newOwnedID) {
		return nil
	}
	if err := i.kube.Update(ctx, mg); err != nil {
		return fmt.Errorf("cannot update managed resource: %w", err)
	}
	return nil
}

// Prepare applies the initializer's changes to mg in memory and reports
// whether anything changed. It returns false (and changes nothing) when the
// effective name cannot be determined; Observe reports that conflict.
func Prepare(mg resource.Managed, forProviderName *string, preassign bool, newID func() string) bool {
	name, err := EffectiveName(mg, forProviderName)
	if err != nil {
		return false
	}
	changed := false
	if meta.GetExternalName(mg) == "" {
		meta.SetExternalName(mg, name)
		changed = true
	}
	if PoliciesOf(mg).Create() && !ClaimValid(mg) {
		set := map[string]string{v1alpha1.AnnotationOwnerUID: string(mg.GetUID())}
		remove := []string{
			v1alpha1.AnnotationCreateOutcome,
			meta.AnnotationKeyExternalCreatePending,
			meta.AnnotationKeyExternalCreateSucceeded,
			meta.AnnotationKeyExternalCreateFailed,
		}
		if preassign {
			set[v1alpha1.AnnotationOwnedID] = newID()
		} else {
			remove = append(remove, v1alpha1.AnnotationOwnedID)
		}
		Apply(mg, set, remove)
		changed = true
	} else if preassign && PoliciesOf(mg).Create() && ClaimOf(mg).OwnedID == "" {
		// A valid claim without an owned-id (removed by hand): choose a new
		// one so that Create has a UUID to use.
		meta.AddAnnotations(mg, map[string]string{v1alpha1.AnnotationOwnedID: newID()})
		changed = true
	}
	return changed
}

// Interface check.
var _ managed.Initializer = &Initializer{}
