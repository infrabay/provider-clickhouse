// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package access

import (
	"context"
	"errors"
	"fmt"
	"strings"

	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"

	"github.com/crossplane/crossplane-runtime/v2/pkg/event"
	"github.com/crossplane/crossplane-runtime/v2/pkg/meta"
	"github.com/crossplane/crossplane-runtime/v2/pkg/resource"

	"github.com/infrabay/provider-clickhouse/apis/v1alpha1"
	ch "github.com/infrabay/provider-clickhouse/internal/clients/clickhouse"
	"github.com/infrabay/provider-clickhouse/internal/clients/clickhouse/sqlx"
	"github.com/infrabay/provider-clickhouse/internal/controller/ownership"
	"github.com/infrabay/provider-clickhouse/internal/controller/providerconfig"
	"github.com/infrabay/provider-clickhouse/internal/reason"
)

// An EntityKind is the kind of an access entity: a user or a role.
type EntityKind string

// Entity kinds. The values are the MR kinds.
const (
	User EntityKind = "User"
	Role EntityKind = "Role"
)

func (k EntityKind) query() string {
	if k == Role {
		return sqlx.QueryRoleEntity
	}
	return sqlx.QueryUserEntity
}

// other returns the kind whose names collide with k's.
func (k EntityKind) other() EntityKind {
	if k == Role {
		return User
	}
	return Role
}

// lower returns "user" or "role", as used in messages and in grantee
// strings.
func (k EntityKind) lower() string { return strings.ToLower(string(k)) }

// GranteeKind returns the policy grantee kind of k.
func (k EntityKind) GranteeKind() providerconfig.GranteeKind {
	if k == Role {
		return providerconfig.GranteeRole
	}
	return providerconfig.GranteeUser
}

// An Entity is a row of system.users or system.roles.
type Entity struct {
	Kind EntityKind
	Name string
	// ID is the UUID (system.users.id or system.roles.id), the anchor of
	// ownership.
	ID string
	// Storage is the name of the user directory that holds the entity:
	// users_xml (read-only), local_directory, replicated or memory, unless
	// the server configuration renames the directory (see Env.ReadOnly).
	Storage string
}

// ReadOnly reports whether the user directory that holds ent refuses writes:
// users.xml/users.d whatever the directory is called, or a local directory
// configured read-only (ch.Capabilities.StorageReadOnly). The server refuses
// every write to such an entity with 495, and the provider never tries.
func (e *Env) ReadOnly(ctx context.Context, ent Entity) (bool, error) {
	return e.Client.Capabilities().StorageReadOnly(ctx, e.Client, ent.Storage)
}

// ReadEntity reads the UUID and storage of the user or role named name.
// exists is false only when the query succeeded and returned no row:
// system.users and system.roles are not filtered by privileges (they fail
// with 497 instead), so that is a reliable "not found". Every error is
// returned, never turned into "not found".
func ReadEntity(ctx context.Context, db ch.DB, kind EntityKind, name string) (Entity, bool, error) {
	e := Entity{Kind: kind, Name: name}
	err := ch.QueryRow(ctx, db, kind.query(), ch.Params{"name": name}, &e.ID, &e.Storage)
	switch {
	case errors.Is(err, ch.ErrNoRows):
		return Entity{Kind: kind, Name: name}, false, nil
	case err != nil:
		return Entity{Kind: kind, Name: name}, false, ch.WithDefaultReason(err)
	}
	return e, true, nil
}

// CheckNameCollision returns NameCollision if an entity of the other kind has
// the name: a user and a role may share a name, even across storages, and
// the server then resolves GRANT and REVOKE grantees user-first, so grants
// meant for the role would land on the user (design 5.4, 5.5).
func CheckNameCollision(ctx context.Context, db ch.DB, kind EntityKind, name string) error {
	other, exists, err := ReadEntity(ctx, db, kind.other(), name)
	if err != nil {
		return err
	}
	if exists {
		return reason.Errorf(reason.NameCollision,
			"a %s named %q already exists (id %s, storage %s); ClickHouse resolves grantees user-first, so a user and a role must not share a name",
			other.Kind.lower(), name, other.ID, other.Storage)
	}
	return nil
}

// An EntityObservation is the outcome of ObserveEntity.
type EntityObservation struct {
	// Name is the MR's effective external name, the entity's ClickHouse
	// name.
	Name string
	// Found is set when an entity with the name exists on the server; Entity
	// is then its row (for status.atProvider.id and storage).
	Found  bool
	Entity Entity
	// Exists is what Observe reports as ResourceExists. It is false when no
	// entity exists, and while the MR is being deleted for every entity that
	// is not the MR's own.
	Exists bool
	// Orphaned is set while the MR is being deleted when the entity (or the
	// name) is left in place: a Warning event was emitted. Report
	// ResourceExists=false and send no SQL.
	Orphaned bool
	// Decision is the ownership decision, when Found.
	Decision ownership.Result
}

// ObserveEntity runs the part of Observe that User and Role share (design
// 4.2, 5.4, 5.5, 5.8), for the MR mg of kind kind whose spec.forProvider.name
// is forProviderName:
//
//  1. resolve the effective name;
//  2. check the name against the ClusterProviderConfig policy, for every
//     management policy and also while deleting (D-25);
//  3. refuse the provider's own identity (User);
//  4. read the entity: no row means it does not exist, every error is
//     returned;
//  5. apply the ownership decision table, with the create journal as the
//     evidence row 5 needs, persist claim changes (adoption, recovery) and
//     emit Adopted or CreateRecovered;
//  6. refuse a user and a role with the same name (not while deleting).
//
// While the MR is being deleted, an unresolvable name, a name outside the
// policy, the provider's own identity and every entity that is not the MR's
// own (owned, adopted or recovered) orphan the MR: a Warning event is
// emitted, Exists is false and nothing is written. This holds for every
// management policy (D-26).
//
// On errors that make the MR unavailable (design 5.10) it sets Ready=False;
// once ownership is settled it sets Ready=True. The caller fills status from
// the result even when an error is returned, and reads the kind's details
// only when Exists is true and the MR is not being deleted.
func (e *Env) ObserveEntity(ctx context.Context, mg resource.Managed, kind EntityKind, forProviderName *string) (EntityObservation, error) {
	deleting := meta.WasDeleted(mg)
	writes := ownership.PoliciesOf(mg).Writes()

	name, err := ownership.EffectiveName(mg, forProviderName)
	if err != nil {
		if deleting {
			e.LeaveInPlace(mg, ReasonOrphaned, fmt.Errorf("%w; no %s was dropped", err, kind.lower()))
			return EntityObservation{Orphaned: true}, nil
		}
		return EntityObservation{}, Unavailable(mg, err)
	}
	obs := EntityObservation{Name: name}

	// The name policy applies to every management policy, including
	// observe-only, and also while deleting: the ownership claim is only
	// annotations, which the tenant can edit, so the policy is the boundary
	// between tenants.
	if err := e.checkEntityName(mg, kind, name); err != nil {
		if deleting {
			e.LeaveInPlace(mg, ReasonOrphanedByPolicy, fmt.Errorf("%w; the %s is left in place", err, kind.lower()))
			obs.Orphaned = true
			return obs, nil
		}
		return obs, Unavailable(mg, err)
	}

	if kind == User && name == e.CurrentUser() {
		err := reason.Errorf(reason.SelfModification, "user %q is the provider's own identity and is never modified", name)
		switch {
		case deleting:
			e.LeaveInPlace(mg, ReasonOrphaned, fmt.Errorf("%w; it is left in place", err))
			obs.Orphaned = true
			return obs, nil
		case writes:
			return obs, Unavailable(mg, err)
		}
	}

	ent, found, err := ReadEntity(ctx, e.Client, kind, name)
	if err != nil {
		return obs, err
	}
	if !found {
		if !deleting {
			if err := CheckNameCollision(ctx, e.Client, kind, name); err != nil {
				return obs, Unavailable(mg, err)
			}
		}
		return obs, nil
	}
	obs.Found, obs.Entity = true, ent
	readOnly, err := e.ReadOnly(ctx, ent)
	if err != nil {
		return obs, err
	}

	key := e.createKey(kind, mg, name)
	d := ownership.Decide(mg, ownership.Input{
		Kind:            string(kind),
		Name:            name,
		ObservedID:      ent.ID,
		Storage:         ent.Storage,
		ReadOnly:        readOnly,
		Recoverable:     true,
		CreateEvidence:  e.journal().Evidence(key),
		AdoptionAllowed: e.Resolved != nil && e.Resolved.AdoptionAllowed,
	})
	obs.Decision = d
	if d.Err != nil {
		return obs, Unavailable(mg, d.Err)
	}
	if !d.Exists {
		if deleting {
			e.LeaveInPlace(mg, ReasonOrphaned, fmt.Errorf("%s %q (id %s) is not this resource's own (%s); it is left in place", kind, name, ent.ID, d.Decision))
			obs.Orphaned = true
		}
		return obs, nil
	}
	if err := ownership.Patch(ctx, e.Kube, mg, d.Set, d.Remove); err != nil {
		return obs, err
	}
	if d.Event != "" {
		e.recorder().Event(mg, event.Normal(event.Reason(d.Event), fmt.Sprintf("%s %s %q (id %s)", d.Event, kind, name, ent.ID)))
	}
	switch d.Decision {
	case ownership.Owned, ownership.Adopted, ownership.Recovered:
		e.journal().Forget(key)
	}
	obs.Exists = true
	if deleting {
		return obs, nil
	}
	if err := CheckNameCollision(ctx, e.Client, kind, name); err != nil {
		return obs, Unavailable(mg, err)
	}
	if d.Ready() {
		mg.SetConditions(xpv2.Available())
	}
	return obs, nil
}

// checkEntityName is the name check of a User or Role.
func (e *Env) checkEntityName(mg resource.Managed, kind EntityKind, name string) error {
	if kind == Role {
		return e.Policy().CheckRoleName(mg.GetNamespace(), name)
	}
	return e.Policy().CheckUserName(mg.GetNamespace(), name)
}

// CreateEntity runs stmt, the CREATE USER or CREATE ROLE statement for the
// entity named name, for the MR mg (design 4.2, 5.4, 5.5). The kind's own
// write checks (for a User: policy.CheckUserWrite and the jwt capability)
// run before it is called.
//
// Before the statement it checks that the initializer wrote a valid claim,
// that the name is allowed by the policy, that the entity is not the
// provider's own identity, that the access storage may be written from this
// endpoint (CheckWritable) and that no entity of the other kind has the
// name. It records the create in the journal. On failure it sets the
// create-outcome annotation, which crossplane-runtime persists together with
// external-create-failed: "rejected" for a ClickHouse exception (493
// becomes AlreadyExists), "unknown" when the statement may have run. On
// success it reads the UUID back and sets owned-id, which is persisted with
// external-create-succeeded.
func (e *Env) CreateEntity(ctx context.Context, mg resource.Managed, kind EntityKind, name, stmt string) error {
	if !ownership.ClaimValid(mg) {
		return reason.Errorf(reason.Internal, "the ownership claim is missing; the initializer did not run")
	}
	if err := e.checkEntityName(mg, kind, name); err != nil {
		return Unavailable(mg, err)
	}
	if kind == User && name == e.CurrentUser() {
		return Unavailable(mg, reason.Errorf(reason.SelfModification, "user %q is the provider's own identity and is never created or modified", name))
	}
	if err := e.CheckWritable(ctx); err != nil {
		return Unavailable(mg, err)
	}
	if err := CheckNameCollision(ctx, e.Client, kind, name); err != nil {
		return Unavailable(mg, err)
	}

	key := e.createKey(kind, mg, name)
	j := e.journal()
	j.Begin(key)
	if err := e.Client.Exec(ctx, stmt); err != nil {
		if ch.IsUncertain(err) {
			j.Unknown(key)
			meta.AddAnnotations(mg, map[string]string{v1alpha1.AnnotationCreateOutcome: v1alpha1.CreateOutcomeUnknown})
		} else {
			j.Rejected(key)
			meta.AddAnnotations(mg, map[string]string{v1alpha1.AnnotationCreateOutcome: v1alpha1.CreateOutcomeRejected})
		}
		return ch.WithDefaultReason(err)
	}
	ent, found, err := ReadEntity(ctx, e.Client, kind, name)
	if err == nil && !found {
		err = reason.Errorf(reason.Internal, "%s %q was created but cannot be read back", kind, name)
	}
	if err != nil {
		// The CREATE ran; the next Observe recovers the entity through the
		// journal.
		j.Unknown(key)
		meta.AddAnnotations(mg, map[string]string{v1alpha1.AnnotationCreateOutcome: v1alpha1.CreateOutcomeUnknown})
		return err
	}
	j.Succeeded(key, ent.ID)
	meta.AddAnnotations(mg, map[string]string{v1alpha1.AnnotationOwnedID: ent.ID})
	meta.RemoveAnnotations(mg, v1alpha1.AnnotationCreateOutcome)
	return nil
}

// VerifyOwned re-checks, immediately before an ALTER or DROP, that mg still
// owns the entity: the statements address it by name, so it could have been
// dropped and re-created by someone else since Observe (design 5.3, applied
// to users and roles). It checks the claim, the name policy (D-25), the
// provider's own identity, the access storage (CheckWritable), and that the
// entity's current UUID equals owned-id and it is not in users.xml. exists
// is false when the entity no longer exists: the caller then returns without
// error and the next Observe decides.
func (e *Env) VerifyOwned(ctx context.Context, mg resource.Managed, kind EntityKind, name string) (bool, error) {
	claim := ownership.ClaimOf(mg)
	if !ownership.ClaimValid(mg) || claim.OwnedID == "" {
		return false, reason.Errorf(reason.NotOwned, "%s %q is not claimed by this resource; nothing was changed", kind, name)
	}
	if err := e.checkEntityName(mg, kind, name); err != nil {
		return false, err
	}
	if kind == User && name == e.CurrentUser() {
		return false, reason.Errorf(reason.SelfModification, "user %q is the provider's own identity and is never modified", name)
	}
	if err := e.CheckWritable(ctx); err != nil {
		return false, err
	}
	ent, exists, err := ReadEntity(ctx, e.Client, kind, name)
	if err != nil || !exists {
		return false, err
	}
	readOnly, err := e.ReadOnly(ctx, ent)
	if err != nil {
		return false, err
	}
	if readOnly {
		return false, reason.Errorf(reason.ReadOnlyStorage, "%s %q is in the read-only user directory %q (such as users.xml/users.d) and cannot be managed", kind, name, ent.Storage)
	}
	if ent.ID != claim.OwnedID {
		return false, reason.Errorf(reason.Replaced, "%s %q was replaced since it was observed (id %s, now %s); nothing was changed", kind, name, claim.OwnedID, ent.ID)
	}
	return true, nil
}
