// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

// Package ownership decides whether the ClickHouse object behind a managed
// resource's name is the object that resource created (or adopted). The
// provider identity can alter or drop objects it did not create, so every
// write is gated by this decision.
//
// The claim lives in annotations bound to the MR's UID: owner-uid must equal
// metadata.uid, and owned-id is the UUID of the ClickHouse object. Copied or
// restored manifests carry an invalid claim, which is ignored.
package ownership

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/crossplane/crossplane-runtime/v2/pkg/meta"
	"github.com/crossplane/crossplane-runtime/v2/pkg/resource"

	"github.com/infrabay/provider-clickhouse/apis/v1alpha1"
	"github.com/infrabay/provider-clickhouse/internal/reason"
)

// ZeroUUID is the UUID of objects without one (e.g. Memory databases).
const ZeroUUID = "00000000-0000-0000-0000-000000000000"

// EffectiveName returns the ClickHouse name of a Database, User or Role:
// spec.forProvider.name if set, otherwise the crossplane.io/external-name
// annotation, otherwise metadata.name. If forProvider.name and the annotation
// are both set and differ, it returns ExternalNameConflict. All policy checks
// and all SQL use this name.
func EffectiveName(mg resource.Managed, forProviderName *string) (string, error) {
	ext := meta.GetExternalName(mg)
	if forProviderName != nil && *forProviderName != "" {
		if ext != "" && ext != *forProviderName {
			return "", reason.Errorf(reason.ExternalNameConflict,
				"spec.forProvider.name %q and annotation %s %q differ; remove the annotation or make them equal",
				*forProviderName, meta.AnnotationKeyExternalName, ext)
		}
		return *forProviderName, nil
	}
	if ext != "" {
		return ext, nil
	}
	return mg.GetName(), nil
}

// Policies classifies management policies.
type Policies struct{ p xpv2.ManagementPolicies }

// PoliciesOf returns the management policies of mg. Empty policies mean "*".
func PoliciesOf(mg resource.Managed) Policies { return Policies{p: mg.GetManagementPolicies()} }

func (p Policies) has(a xpv2.ManagementAction) bool {
	return len(p.p) == 0 || slices.Contains(p.p, xpv2.ManagementActionAll) || slices.Contains(p.p, a)
}

// Create reports whether Create is allowed.
func (p Policies) Create() bool { return p.has(xpv2.ManagementActionCreate) }

// LateInitialize reports whether LateInitialize is allowed.
func (p Policies) LateInitialize() bool { return p.has(xpv2.ManagementActionLateInitialize) }

// Update reports whether Update is allowed.
func (p Policies) Update() bool { return p.has(xpv2.ManagementActionUpdate) }

// Delete reports whether Delete is allowed.
func (p Policies) Delete() bool { return p.has(xpv2.ManagementActionDelete) }

// Writes reports whether any write (Create, Update or Delete) is allowed.
// Without writes, the MR is in observe-only or import mode.
func (p Policies) Writes() bool {
	return p.has(xpv2.ManagementActionCreate) || p.has(xpv2.ManagementActionUpdate) || p.has(xpv2.ManagementActionDelete)
}

// Claim is the ownership claim of an MR, read from its annotations.
type Claim struct {
	OwnerUID      string
	OwnedID       string
	CreateOutcome string
	Adopt         string
}

// ClaimOf reads the claim of mg.
func ClaimOf(mg resource.Managed) Claim {
	a := mg.GetAnnotations()
	return Claim{
		OwnerUID:      a[v1alpha1.AnnotationOwnerUID],
		OwnedID:       a[v1alpha1.AnnotationOwnedID],
		CreateOutcome: a[v1alpha1.AnnotationCreateOutcome],
		Adopt:         a[v1alpha1.AnnotationAdopt],
	}
}

// ClaimValid reports whether mg holds a valid claim: owner-uid equals its
// current UID.
func ClaimValid(mg resource.Managed) bool {
	uid := string(mg.GetUID())
	return uid != "" && mg.GetAnnotations()[v1alpha1.AnnotationOwnerUID] == uid
}

// A Decision is the outcome of the ownership decision table.
type Decision int

// Decisions, in the order of the decision table.
const (
	// ReadOnly: the object is stored in users.xml/users.d. Never written.
	ReadOnly Decision = iota + 1
	// Owned: the MR holds a valid claim on exactly this object.
	Owned
	// Adopted: the adopt annotation equals the object's UUID and adoption is
	// allowed. The claim is written and the annotation removed.
	Adopted
	// AdoptionRejected: the adopt annotation is set but does not match, or
	// adoption is not allowed.
	AdoptionRejected
	// Recovered: a User or Role whose Create result was lost.
	Recovered
	// Replaced: the owned object was replaced out of band.
	Replaced
	// NotOwned: the object exists and was not created by this MR.
	NotOwned
)

func (d Decision) String() string {
	switch d {
	case ReadOnly:
		return "ReadOnly"
	case Owned:
		return "Owned"
	case Adopted:
		return "Adopted"
	case AdoptionRejected:
		return "AdoptionRejected"
	case Recovered:
		return "Recovered"
	case Replaced:
		return "Replaced"
	case NotOwned:
		return "NotOwned"
	}
	return fmt.Sprintf("Decision(%d)", int(d))
}

// Input describes an existing object with the MR's effective name.
type Input struct {
	// Kind and Name are used in messages.
	Kind string
	Name string
	// ObservedID is the object's UUID.
	ObservedID string
	// Storage is the name of the object's user directory (User and Role),
	// or "", for messages.
	Storage string
	// ReadOnly is set when that directory refuses writes: users.xml/users.d
	// (whatever the directory is called) or a read-only local directory.
	ReadOnly bool
	// Recoverable enables row 5 (User and Role).
	Recoverable bool
	// CreateEvidence is what the create journal knows about a CREATE this
	// provider process sent for the MR (User and Role). Row 5 needs it: the
	// create annotations alone can be forged.
	CreateEvidence Evidence
	// AdoptionAllowed is policy.allowAdoption of a ClusterProviderConfig, or
	// true for a namespaced ProviderConfig.
	AdoptionAllowed bool
}

// A Result is the outcome of Decide.
type Result struct {
	Decision Decision

	// Exists is what Observe reports as ResourceExists. While the MR is
	// being deleted it is true only for Owned, Adopted and Recovered, whatever
	// the management policies are: the MR then goes away and every other
	// server object is untouched.
	Exists bool

	// Err is the error Observe returns, or nil.
	Err error

	// Set and Remove are annotation changes to persist (claim writes and
	// adopt removal), or nil.
	Set    map[string]string
	Remove []string

	// Event is a Normal event to emit ("Adopted", "CreateRecovered"), or "".
	Event string

	// ObserveOnly is set when the MR has no write policies. The object is
	// reported, and nothing is claimed.
	ObserveOnly bool
}

// Ready reports whether the object is available: it exists and is owned,
// adopted or recovered, or the MR only observes it.
func (r Result) Ready() bool {
	if r.ObserveOnly {
		// Nothing is claimed in observe-only mode: the decision only says
		// what a write-enabled MR would do.
		return r.Exists
	}
	switch r.Decision {
	case Owned, Adopted, Recovered:
		return true
	}
	return false
}

// Decide applies the ownership decision table to an object that exists. The
// first matching row wins:
//
//  1. a read-only user directory (users.xml/users.d, whatever it is called,
//     or a local directory configured read-only): read-only;
//  2. valid claim, owned-id equals the observed UUID: owned;
//  3. adopt equals the observed UUID and adoption is allowed: adopted;
//  4. adopt is set otherwise: adoption rejected;
//  5. (User, Role) valid claim, no owned-id, the create is incomplete or its
//     outcome unknown, and the create journal holds a record of a CREATE this
//     process sent for the MR whose outcome was not a rejection (and, if its
//     UUID was read back, the same UUID): recovered. Without the journal
//     record a forged external-create-pending or create-outcome annotation
//     could claim a foreign entity;
//  6. valid claim, owned-id differs, and the create succeeded last: replaced;
//  7. anything else: not owned.
//
// Without write policies (observe-only or import), rows 3 to 7 produce no
// error and no claim. While the MR is being deleted, every decision except
// Owned, Adopted and Recovered reports that the object does not exist, also
// without write policies. This second guard does not depend on
// crossplane-runtime honouring the management policies: Delete is only ever
// reached for the MR's own object.
func Decide(mg resource.Managed, in Input) Result {
	deleting := meta.WasDeleted(mg)
	writes := PoliciesOf(mg).Writes()
	c := ClaimOf(mg)
	valid := ClaimValid(mg)
	uid := string(mg.GetUID())

	// Row 1.
	if in.ReadOnly {
		r := Result{Decision: ReadOnly, Exists: !deleting, ObserveOnly: !writes}
		if !deleting && writes {
			r.Err = reason.Errorf(reason.ReadOnlyStorage,
				"%s %q is in the read-only user directory %q (such as users.xml/users.d) and cannot be managed", in.Kind, in.Name, in.Storage)
		}
		return r
	}

	// Row 2.
	if valid && c.OwnedID != "" && c.OwnedID == in.ObservedID {
		r := Result{Decision: Owned, Exists: true}
		if c.Adopt != "" {
			r.Remove = []string{v1alpha1.AnnotationAdopt}
		}
		return r
	}

	if !writes {
		// Observe-only or import: report the object, claim nothing. While
		// deleting, the object is not this MR's to delete.
		d, _ := decideRow3to7(mg, in, c, valid)
		return Result{Decision: d, Exists: !deleting, ObserveOnly: true}
	}

	d, unrecovered := decideRow3to7(mg, in, c, valid)
	switch d {
	case Adopted:
		return Result{
			Decision: Adopted, Exists: true, Event: "Adopted",
			Set:    map[string]string{v1alpha1.AnnotationOwnerUID: uid, v1alpha1.AnnotationOwnedID: in.ObservedID},
			Remove: []string{v1alpha1.AnnotationAdopt, v1alpha1.AnnotationCreateOutcome},
		}
	case Recovered:
		return Result{
			Decision: Recovered, Exists: true, Event: "CreateRecovered",
			Set:    map[string]string{v1alpha1.AnnotationOwnerUID: uid, v1alpha1.AnnotationOwnedID: in.ObservedID},
			Remove: []string{v1alpha1.AnnotationCreateOutcome},
		}
	case AdoptionRejected:
		if deleting {
			return Result{Decision: d}
		}
		if !in.AdoptionAllowed {
			return Result{Decision: d, Exists: true, Err: reason.Errorf(reason.AdoptionRejected,
				"%s %q (id %s) cannot be adopted: policy.allowAdoption of the ClusterProviderConfig is false", in.Kind, in.Name, in.ObservedID)}
		}
		return Result{Decision: d, Exists: true, Err: reason.Errorf(reason.AdoptionRejected,
			"annotation %s is %q but %s %q has id %s; set it to the observed id to adopt this object",
			v1alpha1.AnnotationAdopt, c.Adopt, in.Kind, in.Name, in.ObservedID)}
	case Replaced:
		if deleting {
			return Result{Decision: d}
		}
		return Result{Decision: d, Exists: true, Err: reason.Errorf(reason.Replaced,
			"%s %q was replaced (id %s, now %s); set %s=%q to take over the new object",
			in.Kind, in.Name, c.OwnedID, in.ObservedID, v1alpha1.AnnotationAdopt, in.ObservedID)}
	default:
		if deleting {
			return Result{Decision: NotOwned}
		}
		if unrecovered {
			return Result{Decision: NotOwned, Exists: true, Err: reason.Errorf(reason.NotOwned,
				"%s %q exists (id %s) and a create by this resource did not complete, but this provider process has no record of sending it "+
					"(for example after a restart), so it cannot tell it from an object created by someone else; if this resource created it, set annotation %s=%q "+
					"(requires policy.allowAdoption on a ClusterProviderConfig)",
				in.Kind, in.Name, in.ObservedID, v1alpha1.AnnotationAdopt, in.ObservedID)}
		}
		return Result{Decision: NotOwned, Exists: true, Err: reason.Errorf(reason.NotOwned,
			"%s %q already exists (id %s) and was not created by this resource; to take it over set annotation %s=%q (requires policy.allowAdoption on a ClusterProviderConfig)",
			in.Kind, in.Name, in.ObservedID, v1alpha1.AnnotationAdopt, in.ObservedID)}
	}
}

// decideRow3to7 applies rows 3 to 7. unrecovered is set when row 5 matched
// on the annotations but the create journal had no evidence for it.
func decideRow3to7(mg resource.Managed, in Input, c Claim, valid bool) (d Decision, unrecovered bool) {
	// Row 3 and 4.
	if c.Adopt != "" {
		if c.Adopt == in.ObservedID && in.AdoptionAllowed {
			return Adopted, false
		}
		return AdoptionRejected, false
	}
	pending := meta.GetExternalCreatePending(mg)
	succeeded := meta.GetExternalCreateSucceeded(mg)
	failed := meta.GetExternalCreateFailed(mg)
	// Row 5.
	if in.Recoverable && valid && c.OwnedID == "" {
		incomplete := meta.ExternalCreateIncomplete(mg)
		unknown := c.CreateOutcome == v1alpha1.CreateOutcomeUnknown && newest(failed, pending, succeeded)
		if incomplete || unknown {
			ev := in.CreateEvidence
			if ev.Recorded && (ev.ID == "" || ev.ID == in.ObservedID) {
				return Recovered, false
			}
			return NotOwned, true
		}
	}
	// Row 6.
	if valid && c.OwnedID != "" && c.OwnedID != in.ObservedID && newest(succeeded, pending, failed) {
		return Replaced, false
	}
	// Row 7.
	return NotOwned, false
}

// newest reports whether t is set and not older than any of others. Create
// annotations have second resolution; a tie counts as newest.
func newest(t time.Time, others ...time.Time) bool {
	if t.IsZero() {
		return false
	}
	for _, o := range others {
		if o.After(t) {
			return false
		}
	}
	return true
}

// Apply sets and removes annotations on mg in memory.
func Apply(mg resource.Managed, set map[string]string, remove []string) {
	if len(set) > 0 {
		meta.AddAnnotations(mg, set)
	}
	if len(remove) > 0 {
		meta.RemoveAnnotations(mg, remove...)
	}
}

// Patch persists annotation changes with a JSON merge patch on
// metadata.annotations, and applies them to mg (the patch response updates
// it). Observe uses it for adoption and recovery: annotation changes made in
// Observe are otherwise not persisted. Grant uses it for its record of the
// grant option, which Update and Delete change.
func Patch(ctx context.Context, kube client.Client, mg resource.Managed, set map[string]string, remove []string) error {
	if len(set) == 0 && len(remove) == 0 {
		return nil
	}
	ann := map[string]any{}
	for k, v := range set {
		ann[k] = v
	}
	for _, k := range remove {
		if _, ok := set[k]; !ok {
			ann[k] = nil
		}
	}
	body, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": ann}})
	if err != nil {
		return err
	}
	if err := kube.Patch(ctx, mg, client.RawPatch(types.MergePatchType, body)); err != nil {
		return fmt.Errorf("cannot patch annotations: %w", err)
	}
	Apply(mg, set, remove)
	return nil
}
