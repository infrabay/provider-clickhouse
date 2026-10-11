// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package ownership

import (
	"context"
	"strings"
	"testing"
	"time"

	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/crossplane/crossplane-runtime/v2/pkg/meta"
	"github.com/crossplane/crossplane-runtime/v2/pkg/resource"

	"github.com/infrabay/provider-clickhouse/apis/v1alpha1"
	ch "github.com/infrabay/provider-clickhouse/internal/clients/clickhouse"
	"github.com/infrabay/provider-clickhouse/internal/reason"
)

const (
	uid      = "mr-uid"
	observed = "11111111-1111-1111-1111-111111111111"
	other    = "22222222-2222-2222-2222-222222222222"
)

var (
	t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	t1 = t0.Add(time.Minute)
)

type mrOpt func(*v1alpha1.Database)

func withAnn(k, v string) mrOpt {
	return func(d *v1alpha1.Database) { meta.AddAnnotations(d, map[string]string{k: v}) }
}

func validClaim(ownedID string) mrOpt {
	return func(d *v1alpha1.Database) {
		meta.AddAnnotations(d, map[string]string{v1alpha1.AnnotationOwnerUID: uid})
		if ownedID != "" {
			meta.AddAnnotations(d, map[string]string{v1alpha1.AnnotationOwnedID: ownedID})
		}
	}
}

func copiedClaim(ownedID string) mrOpt {
	return func(d *v1alpha1.Database) {
		meta.AddAnnotations(d, map[string]string{v1alpha1.AnnotationOwnerUID: "other-uid", v1alpha1.AnnotationOwnedID: ownedID})
	}
}

func deleting() mrOpt {
	return func(d *v1alpha1.Database) {
		now := metav1.NewTime(t1)
		d.SetDeletionTimestamp(&now)
	}
}

func policies(p ...xpv2.ManagementAction) mrOpt {
	return func(d *v1alpha1.Database) { d.SetManagementPolicies(p) }
}

func created(pending, succeeded, failed time.Time) mrOpt {
	return func(d *v1alpha1.Database) {
		if !pending.IsZero() {
			meta.SetExternalCreatePending(d, pending)
		}
		if !succeeded.IsZero() {
			meta.SetExternalCreateSucceeded(d, succeeded)
		}
		if !failed.IsZero() {
			meta.SetExternalCreateFailed(d, failed)
		}
	}
}

func newMR(opts ...mrOpt) *v1alpha1.Database {
	d := &v1alpha1.Database{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "db", UID: uid}}
	for _, o := range opts {
		o(d)
	}
	return d
}

func TestDecide(t *testing.T) {
	var zero time.Time
	cases := map[string]struct {
		mr          *v1alpha1.Database
		in          Input
		decision    Decision
		exists      bool
		reason      reason.Reason // "" means no error
		message     string        // a substring of the error, if set
		ready       bool
		setOwnedID  string
		removeAdopt bool
		event       string
	}{
		"Row1ReadOnly": {
			mr: newMR(), in: Input{Storage: ch.StorageUsersXML},
			decision: ReadOnly, exists: true, reason: reason.ReadOnlyStorage,
		},
		"Row1ReadOnlyDeleting": {
			mr: newMR(deleting()), in: Input{Storage: ch.StorageUsersXML},
			decision: ReadOnly, exists: false,
		},
		"Row1ReadOnlyObserveOnly": {
			mr: newMR(policies(xpv2.ManagementActionObserve)), in: Input{Storage: ch.StorageUsersXML},
			decision: ReadOnly, exists: true, ready: true,
		},
		"Row2Owned": {
			mr: newMR(validClaim(observed)), decision: Owned, exists: true, ready: true,
		},
		"Row2OwnedLeftoverAdopt": {
			mr: newMR(validClaim(observed), withAnn(v1alpha1.AnnotationAdopt, observed)), decision: Owned, exists: true, ready: true, removeAdopt: true,
		},
		"Row2OwnedDeleting": {
			mr: newMR(validClaim(observed), deleting()), decision: Owned, exists: true, ready: true,
		},
		"Row3Adopted": {
			mr: newMR(validClaim(other), withAnn(v1alpha1.AnnotationAdopt, observed)), in: Input{AdoptionAllowed: true},
			decision: Adopted, exists: true, ready: true, setOwnedID: observed, removeAdopt: true, event: "Adopted",
		},
		"Row4AdoptWrongID": {
			mr: newMR(validClaim(other), withAnn(v1alpha1.AnnotationAdopt, other)), in: Input{AdoptionAllowed: true},
			decision: AdoptionRejected, exists: true, reason: reason.AdoptionRejected,
		},
		"Row4AdoptTrueRejected": {
			mr: newMR(validClaim(""), withAnn(v1alpha1.AnnotationAdopt, "true")), in: Input{AdoptionAllowed: true},
			decision: AdoptionRejected, exists: true, reason: reason.AdoptionRejected,
		},
		"Row4AdoptNotAllowed": {
			mr:       newMR(validClaim(other), withAnn(v1alpha1.AnnotationAdopt, observed)),
			decision: AdoptionRejected, exists: true, reason: reason.AdoptionRejected,
		},
		"Row4Deleting": {
			mr: newMR(validClaim(other), withAnn(v1alpha1.AnnotationAdopt, other), deleting()), in: Input{AdoptionAllowed: true},
			decision: AdoptionRejected, exists: false,
		},
		"Row5RecoveredPending": {
			mr: newMR(validClaim(""), created(t1, zero, zero)), in: Input{Recoverable: true, CreateEvidence: Evidence{Recorded: true}},
			decision: Recovered, exists: true, ready: true, setOwnedID: observed, event: "CreateRecovered",
		},
		"Row5RecoveredUnknownOutcome": {
			mr: newMR(validClaim(""), created(t0, zero, t1), withAnn(v1alpha1.AnnotationCreateOutcome, v1alpha1.CreateOutcomeUnknown)), in: Input{Recoverable: true, CreateEvidence: Evidence{Recorded: true}},
			decision: Recovered, exists: true, ready: true, setOwnedID: observed, event: "CreateRecovered",
		},
		"Row5RecoveredReadBackID": {
			mr: newMR(validClaim(""), created(t1, zero, zero)), in: Input{Recoverable: true, CreateEvidence: Evidence{Recorded: true, ID: observed}},
			decision: Recovered, exists: true, ready: true, setOwnedID: observed, event: "CreateRecovered",
		},
		// The CREATE succeeded with another UUID; the entity was replaced
		// since. Only the read-back UUID can be recovered.
		"Row5ReadBackIDMismatchIsNotRecovered": {
			mr: newMR(validClaim(""), created(t1, zero, zero)), in: Input{Recoverable: true, CreateEvidence: Evidence{Recorded: true, ID: other}},
			decision: NotOwned, exists: true, reason: reason.NotOwned,
		},
		// A tenant points a User at an existing user and forges
		// external-create-pending. The provider never sent a CREATE for it.
		"Row5ForgedPendingIsNotRecovered": {
			mr: newMR(validClaim(""), created(t1, zero, zero)), in: Input{Recoverable: true},
			decision: NotOwned, exists: true, reason: reason.NotOwned, message: "no record of sending it",
		},
		"Row5ForgedUnknownOutcomeIsNotRecovered": {
			mr: newMR(validClaim(""), created(t0, zero, t1), withAnn(v1alpha1.AnnotationCreateOutcome, v1alpha1.CreateOutcomeUnknown)), in: Input{Recoverable: true},
			decision: NotOwned, exists: true, reason: reason.NotOwned, message: "no record of sending it",
		},
		"Row5ForgedPendingWhileDeleting": {
			mr: newMR(validClaim(""), created(t1, zero, zero), deleting()), in: Input{Recoverable: true},
			decision: NotOwned, exists: false,
		},
		"Row5RejectedOutcomeIsNotRecovered": {
			mr: newMR(validClaim(""), created(t0, zero, t1), withAnn(v1alpha1.AnnotationCreateOutcome, v1alpha1.CreateOutcomeRejected)), in: Input{Recoverable: true, CreateEvidence: Evidence{Recorded: true}},
			decision: NotOwned, exists: true, reason: reason.NotOwned,
		},
		"Row5NotForDatabase": {
			mr: newMR(validClaim(""), created(t1, zero, zero)), in: Input{CreateEvidence: Evidence{Recorded: true}},
			decision: NotOwned, exists: true, reason: reason.NotOwned,
		},
		"Row5CopiedClaimNotRecovered": {
			mr: newMR(copiedClaim(""), created(t1, zero, zero)), in: Input{Recoverable: true, CreateEvidence: Evidence{Recorded: true}},
			decision: NotOwned, exists: true, reason: reason.NotOwned,
		},
		"Row5ObserveOnlyNeverClaims": {
			mr: newMR(policies(xpv2.ManagementActionObserve), validClaim(""), created(t1, zero, zero)), in: Input{Recoverable: true, CreateEvidence: Evidence{Recorded: true}},
			decision: Recovered, exists: true, ready: true,
		},
		"Row6Replaced": {
			mr:       newMR(validClaim(other), created(t0, t1, zero)),
			decision: Replaced, exists: true, reason: reason.Replaced,
		},
		"Row6ReplacedTieCountsAsNewest": {
			mr:       newMR(validClaim(other), created(t1, t1, zero)),
			decision: Replaced, exists: true, reason: reason.Replaced,
		},
		"Row6Deleting": {
			mr:       newMR(validClaim(other), created(t0, t1, zero), deleting()),
			decision: Replaced, exists: false,
		},
		"Row7FailedCreateIsNotReplaced": {
			mr:       newMR(validClaim(other), created(t0, zero, t1)),
			decision: NotOwned, exists: true, reason: reason.NotOwned,
		},
		"Row7NameCollision": {
			mr: newMR(validClaim(other)), decision: NotOwned, exists: true, reason: reason.NotOwned,
		},
		"Row7CopiedManifest": {
			mr: newMR(copiedClaim(observed), created(t0, t1, zero)), decision: NotOwned, exists: true, reason: reason.NotOwned,
		},
		"Row7Deleting": {
			mr: newMR(validClaim(other), deleting()), decision: NotOwned, exists: false,
		},
		"ObserveOnlyNotOwned": {
			mr: newMR(policies(xpv2.ManagementActionObserve)), decision: NotOwned, exists: true, ready: true,
		},
		"ImportNotOwned": {
			mr:       newMR(policies(xpv2.ManagementActionObserve, xpv2.ManagementActionLateInitialize), withAnn(v1alpha1.AnnotationAdopt, other)),
			decision: AdoptionRejected, exists: true, ready: true,
		},
		// Without write policies, nothing but the MR's own object is reported
		// while deleting, so Delete is never reached for it even if
		// crossplane-runtime ignored the management policies.
		"ObserveOnlyNotOwnedDeleting": {
			mr: newMR(policies(xpv2.ManagementActionObserve), deleting()), decision: NotOwned, exists: false,
		},
		"ObserveOnlyCopiedClaimDeleting": {
			mr: newMR(policies(xpv2.ManagementActionObserve), copiedClaim(observed), deleting()), decision: NotOwned, exists: false,
		},
		"ObserveOnlyAdoptDeleting": {
			mr: newMR(policies(xpv2.ManagementActionObserve), withAnn(v1alpha1.AnnotationAdopt, observed), deleting()), in: Input{AdoptionAllowed: true},
			decision: Adopted, exists: false,
		},
		"ObserveOnlyOwnedDeleting": {
			mr: newMR(policies(xpv2.ManagementActionObserve), validClaim(observed), deleting()), decision: Owned, exists: true, ready: true,
		},
		"ObserveOnlyAdoptWritesNothing": {
			mr: newMR(policies(xpv2.ManagementActionObserve), withAnn(v1alpha1.AnnotationAdopt, observed)), in: Input{AdoptionAllowed: true},
			decision: Adopted, exists: true, ready: true,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			in := tc.in
			in.Kind, in.Name, in.ObservedID = "Database", "db", observed
			r := Decide(tc.mr, in)
			if r.Decision != tc.decision {
				t.Errorf("Decision = %s, want %s", r.Decision, tc.decision)
			}
			if r.Exists != tc.exists {
				t.Errorf("Exists = %t, want %t", r.Exists, tc.exists)
			}
			if tc.reason == "" && r.Err != nil {
				t.Errorf("Err = %v, want nil", r.Err)
			}
			if tc.reason != "" && !reason.Is(r.Err, tc.reason) {
				t.Errorf("Err = %v, want %s", r.Err, tc.reason)
			}
			if tc.message != "" && (r.Err == nil || !strings.Contains(r.Err.Error(), tc.message)) {
				t.Errorf("Err = %v, want it to contain %q", r.Err, tc.message)
			}
			if r.Ready() != tc.ready {
				t.Errorf("Ready() = %t, want %t", r.Ready(), tc.ready)
			}
			if got := r.Set[v1alpha1.AnnotationOwnedID]; got != tc.setOwnedID {
				t.Errorf("Set owned-id = %q, want %q", got, tc.setOwnedID)
			}
			if tc.setOwnedID != "" && r.Set[v1alpha1.AnnotationOwnerUID] != uid {
				t.Errorf("Set owner-uid = %q", r.Set[v1alpha1.AnnotationOwnerUID])
			}
			removesAdopt := false
			for _, k := range r.Remove {
				if k == v1alpha1.AnnotationAdopt {
					removesAdopt = true
				}
			}
			if removesAdopt != tc.removeAdopt {
				t.Errorf("removes adopt = %t, want %t", removesAdopt, tc.removeAdopt)
			}
			if r.Event != tc.event {
				t.Errorf("Event = %q, want %q", r.Event, tc.event)
			}
			if r.ObserveOnly && len(r.Set) > 0 {
				t.Error("observe-only must never write a claim")
			}
		})
	}
}

func TestEffectiveName(t *testing.T) {
	name := "svc@my-project.iam.gserviceaccount.com"
	d := newMR()
	if got, _ := EffectiveName(d, nil); got != "db" {
		t.Errorf("default = %q, want metadata.name", got)
	}
	meta.SetExternalName(d, "from-annotation")
	if got, _ := EffectiveName(d, nil); got != "from-annotation" {
		t.Errorf("annotation = %q", got)
	}
	if _, err := EffectiveName(d, &name); !reason.Is(err, reason.ExternalNameConflict) {
		t.Errorf("conflict: %v", err)
	}
	d2 := newMR()
	if got, _ := EffectiveName(d2, &name); got != name {
		t.Errorf("forProvider.name = %q", got)
	}
	meta.SetExternalName(d2, name)
	if got, err := EffectiveName(d2, &name); err != nil || got != name {
		t.Errorf("equal annotation = %q, %v", got, err)
	}
}

func TestPrepare(t *testing.T) {
	gen := func() string { return observed }

	d := newMR()
	if !Prepare(d, nil, true, gen) {
		t.Fatal("a new Database must be prepared")
	}
	if meta.GetExternalName(d) != "db" || ClaimOf(d).OwnerUID != uid || ClaimOf(d).OwnedID != observed {
		t.Errorf("new Database: %v", d.GetAnnotations())
	}
	if Prepare(d, nil, true, gen) {
		t.Error("Prepare must be idempotent")
	}

	// A copied manifest: the claim is replaced and stale create history of
	// the previous owner is dropped.
	c := newMR(copiedClaim(other), created(t0, t1, zero()), withAnn(meta.AnnotationKeyExternalName, "db"))
	if !Prepare(c, nil, true, gen) {
		t.Fatal("an invalid claim must be replaced")
	}
	if ClaimOf(c).OwnerUID != uid || ClaimOf(c).OwnedID != observed || !meta.GetExternalCreateSucceeded(c).IsZero() {
		t.Errorf("copied manifest: %v", c.GetAnnotations())
	}

	// User/Role: no pre-assigned id; a stale owned-id is removed.
	u := newMR(copiedClaim(other))
	Prepare(u, nil, false, gen)
	if ClaimOf(u).OwnedID != "" || ClaimOf(u).OwnerUID != uid {
		t.Errorf("user claim: %v", u.GetAnnotations())
	}

	// Observe-only MRs never get a claim.
	o := newMR(policies(xpv2.ManagementActionObserve))
	Prepare(o, nil, true, gen)
	if ClaimOf(o).OwnerUID != "" || meta.GetExternalName(o) != "db" {
		t.Errorf("observe-only: %v", o.GetAnnotations())
	}

	// A valid claim whose owned-id was removed gets a new one (Database).
	v := newMR(validClaim(""), withAnn(meta.AnnotationKeyExternalName, "db"))
	if !Prepare(v, nil, true, gen) || ClaimOf(v).OwnedID != observed {
		t.Errorf("valid claim without owned-id: %v", v.GetAnnotations())
	}
}

func zero() time.Time { return time.Time{} }

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := v1alpha1.SchemeBuilder.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestInitializer(t *testing.T) {
	ctx := context.Background()
	name := "app_db"
	d := newMR()
	d.Spec.ForProvider.Name = &name
	kube := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(d).Build()
	i := NewInitializer(kube, func(mg resource.Managed) *string { return mg.(*v1alpha1.Database).Spec.ForProvider.Name },
		WithPreassignedID(), WithIDGenerator(func() string { return observed }))
	if err := i.Initialize(ctx, d); err != nil {
		t.Fatal(err)
	}
	got := &v1alpha1.Database{}
	_ = kube.Get(ctx, client.ObjectKeyFromObject(d), got)
	if meta.GetExternalName(got) != name || ClaimOf(got).OwnedID != observed || ClaimOf(got).OwnerUID != uid {
		t.Errorf("persisted annotations: %v", got.GetAnnotations())
	}

	// A conflict fails the reconcile, but never while deleting.
	meta.SetExternalName(got, "different")
	if err := i.Initialize(ctx, got); !reason.Is(err, reason.ExternalNameConflict) {
		t.Errorf("conflict: %v", err)
	}
	now := metav1.NewTime(t1)
	got.SetDeletionTimestamp(&now)
	if err := i.Initialize(ctx, got); err != nil {
		t.Errorf("deleting: %v", err)
	}
}

func TestPatch(t *testing.T) {
	ctx := context.Background()
	d := newMR(withAnn(v1alpha1.AnnotationAdopt, observed), withAnn("keep", "me"))
	kube := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(d).Build()
	if err := Patch(ctx, kube, d, map[string]string{v1alpha1.AnnotationOwnerUID: uid, v1alpha1.AnnotationOwnedID: observed}, []string{v1alpha1.AnnotationAdopt}); err != nil {
		t.Fatal(err)
	}
	got := &v1alpha1.Database{}
	_ = kube.Get(ctx, client.ObjectKeyFromObject(d), got)
	a := got.GetAnnotations()
	if a[v1alpha1.AnnotationOwnedID] != observed || a[v1alpha1.AnnotationOwnerUID] != uid || a["keep"] != "me" {
		t.Errorf("annotations after patch: %v", a)
	}
	if _, ok := a[v1alpha1.AnnotationAdopt]; ok {
		t.Error("adopt must be removed")
	}
	if d.GetAnnotations()[v1alpha1.AnnotationOwnedID] != observed {
		t.Error("the in-memory object must be updated")
	}
	if err := Patch(ctx, kube, d, nil, nil); err != nil {
		t.Errorf("empty patch: %v", err)
	}
}

func TestJournal(t *testing.T) {
	now := t0
	j := NewJournal()
	j.now = func() time.Time { return now }
	k := CreateKey{Kind: "User", UID: uid, Name: "app_u", Scope: "ClusterProviderConfig//main|server-1"}

	if ev := j.Evidence(k); ev.Recorded {
		t.Fatalf("empty journal: %+v", ev)
	}
	j.Begin(k)
	if ev := j.Evidence(k); !ev.Recorded || ev.ID != "" {
		t.Errorf("in flight: %+v", ev)
	}
	j.Unknown(k)
	if ev := j.Evidence(k); !ev.Recorded || ev.ID != "" {
		t.Errorf("unknown: %+v", ev)
	}
	j.Succeeded(k, observed)
	if ev := j.Evidence(k); !ev.Recorded || ev.ID != observed {
		t.Errorf("succeeded: %+v", ev)
	}
	// A record never carries over to another name, MR, kind or scope.
	for _, other := range []CreateKey{
		{Kind: "User", UID: uid, Name: "app_v", Scope: k.Scope},
		{Kind: "User", UID: "other-uid", Name: k.Name, Scope: k.Scope},
		{Kind: "Role", UID: uid, Name: k.Name, Scope: k.Scope},
		{Kind: "User", UID: uid, Name: k.Name, Scope: "ProviderConfig/ns/pc|server-1"},
	} {
		if j.Evidence(other).Recorded {
			t.Errorf("record leaked to %+v", other)
		}
	}
	j.Rejected(k)
	if j.Evidence(k).Recorded {
		t.Error("a rejected create must leave no evidence")
	}
	j.Unknown(k)
	now = now.Add(DefaultJournalTTL + time.Second)
	if j.Evidence(k).Recorded {
		t.Error("records expire")
	}
	j.Begin(k)
	j.Forget(k)
	if j.Evidence(k).Recorded {
		t.Error("Forget removes the record")
	}
}

func TestPolicies(t *testing.T) {
	all := PoliciesOf(newMR())
	if !all.Create() || !all.Update() || !all.Delete() || !all.LateInitialize() || !all.Writes() {
		t.Error("empty policies mean '*'")
	}
	obs := PoliciesOf(newMR(policies(xpv2.ManagementActionObserve, xpv2.ManagementActionLateInitialize)))
	if obs.Create() || obs.Update() || obs.Delete() || obs.Writes() || !obs.LateInitialize() {
		t.Error("import mode allows no writes")
	}
	upd := PoliciesOf(newMR(policies(xpv2.ManagementActionObserve, xpv2.ManagementActionUpdate)))
	if !upd.Update() || upd.Create() || upd.Delete() || !upd.Writes() {
		t.Error("Update only")
	}
}
