// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package access

import (
	"context"
	"strings"
	"testing"
	"time"

	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	"github.com/google/go-cmp/cmp"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/crossplane/crossplane-runtime/v2/pkg/meta"

	"github.com/infrabay/provider-clickhouse/apis/v1alpha1"
	ch "github.com/infrabay/provider-clickhouse/internal/clients/clickhouse"
	"github.com/infrabay/provider-clickhouse/internal/clients/clickhouse/fake"
	"github.com/infrabay/provider-clickhouse/internal/clients/clickhouse/sqlx"
	"github.com/infrabay/provider-clickhouse/internal/controller/ownership"
	"github.com/infrabay/provider-clickhouse/internal/controller/providerconfig"
	"github.com/infrabay/provider-clickhouse/internal/reason"
)

const roleName = "team-a_reader"

func TestReadEntity(t *testing.T) {
	ctx := context.Background()
	db := fake.NewDB().WithUser("a`b'c\\d", userID, ch.StorageLocalDirectory)
	e, ok, err := ReadEntity(ctx, db, User, "a`b'c\\d")
	if err != nil || !ok || e.ID != userID || e.ReadOnly() {
		t.Fatalf("ReadEntity() = %+v, %t, %v", e, ok, err)
	}
	if db.Params[0]["name"] != "a`b'c\\d" {
		t.Errorf("the name is sent as a server-side parameter, unquoted: %q", db.Params[0]["name"])
	}
	if _, ok, err := ReadEntity(ctx, db, Role, "a`b'c\\d"); ok || err != nil {
		t.Errorf("absent role: %t, %v", ok, err)
	}
	denied := fake.NewDB().On(sqlx.QueryRoleEntity, fake.Error(fake.Exception(497, "necessary to have the grant SHOW ROLES")))
	if _, ok, err := ReadEntity(ctx, denied, Role, "r"); ok || !reason.Is(err, reason.PermissionDenied) {
		t.Errorf("497 is never 'not found': %t, %v", ok, err)
	}
	broken := fake.NewDB().On(sqlx.QueryUserEntity, fake.Error(&ch.TransportError{Category: "timeout"}))
	if _, ok, err := ReadEntity(ctx, broken, User, "u"); ok || err == nil {
		t.Errorf("a transport error is never 'not found': %t, %v", ok, err)
	}
}

func TestCheckNameCollision(t *testing.T) {
	ctx := context.Background()
	db := fake.NewDB().WithUser("same", userID, ch.StorageUsersXML).WithRole("r", roleID, ch.StorageLocalDirectory)
	if err := CheckNameCollision(ctx, db, Role, "same"); !reason.Is(err, reason.NameCollision) || !strings.Contains(err.Error(), "users_xml") {
		t.Errorf("role named like a users.d user: %v", err)
	}
	if err := CheckNameCollision(ctx, db, User, "r"); !reason.Is(err, reason.NameCollision) {
		t.Errorf("user named like a role: %v", err)
	}
	if err := CheckNameCollision(ctx, db, User, "other"); err != nil {
		t.Errorf("no collision: %v", err)
	}
}

func TestObserveEntity(t *testing.T) {
	cases := map[string]struct {
		mr       *v1alpha1.Role
		kind     EntityKind // default Role
		name     string     // effective name for User cases
		db       *fake.DB
		res      *providerconfig.Resolved
		evidence bool // journal a create of this MR first
		want     EntityObservation
		reason   reason.Reason
		anyErr   bool
		ready    string
		events   []string
		noQuery  bool
		check    func(t *testing.T, mr *v1alpha1.Role, kube client.Client)
	}{
		"PolicyViolation": {
			mr: newRole("team-b_reader", claimed(roleID)), db: fake.NewDB(),
			reason: reason.PolicyViolation, ready: "False", noQuery: true,
		},
		// A forged claim on a name outside the policy never leads to SQL,
		// and deletion is not blocked.
		"PolicyViolationWhileDeleting": {
			mr: newRole("team-b_reader", claimed(roleID), deleting()), db: fake.NewDB(),
			want: EntityObservation{Name: "team-b_reader", Orphaned: true}, events: []string{"OrphanedByPolicy"}, noQuery: true,
		},
		"ExternalNameConflict": {
			mr: newRole(roleName, annotated(meta.AnnotationKeyExternalName, "other")), db: fake.NewDB(),
			reason: reason.ExternalNameConflict, ready: "False", noQuery: true,
		},
		"ExternalNameConflictWhileDeleting": {
			mr: newRole(roleName, annotated(meta.AnnotationKeyExternalName, "other"), deleting()), db: fake.NewDB(),
			want: EntityObservation{Orphaned: true}, events: []string{"Orphaned"}, noQuery: true,
		},
		"NamespacedConfigHasNoNamePolicy": {
			mr: newRole("anything", claimed(roleID)), db: fake.NewDB().WithRole("anything", roleID, ch.StorageLocalDirectory),
			res:  namespacedResolved(),
			want: EntityObservation{Name: "anything", Found: true, Exists: true}, ready: "True",
		},
		"NotFound": {
			mr: newRole(roleName, claimed("")), db: fake.NewDB().WithEntities(),
			want: EntityObservation{Name: roleName},
		},
		// The collision is reported before Create could run.
		"NotFoundButUserHasTheName": {
			mr: newRole(roleName, claimed("")), db: fake.NewDB().WithUser(roleName, userID, ch.StorageLocalDirectory),
			reason: reason.NameCollision, ready: "False",
		},
		"ReadErrorIsNotNotFound": {
			mr: newRole(roleName, claimed(roleID)), db: fake.NewDB().On(sqlx.QueryRoleEntity, fake.Error(&ch.TransportError{Category: "dial failed"})),
			anyErr: true,
		},
		"Owned": {
			mr: newRole(roleName, claimed(roleID)), db: fake.NewDB().WithRole(roleName, roleID, ch.StorageLocalDirectory),
			want: EntityObservation{Name: roleName, Found: true, Exists: true}, ready: "True",
		},
		"OwnedButUserHasTheName": {
			mr: newRole(roleName, claimed(roleID)), db: fake.NewDB().WithRole(roleName, roleID, ch.StorageLocalDirectory).WithUser(roleName, userID, ch.StorageLocalDirectory),
			reason: reason.NameCollision, ready: "False",
		},
		"OwnedWhileDeleting": {
			mr: newRole(roleName, claimed(roleID), deleting()), db: fake.NewDB().WithRole(roleName, roleID, ch.StorageLocalDirectory),
			want: EntityObservation{Name: roleName, Found: true, Exists: true},
		},
		"NotOwned": {
			mr: newRole(roleName, claimed("")), db: fake.NewDB().WithRole(roleName, roleID, ch.StorageLocalDirectory),
			reason: reason.NotOwned, ready: "False",
		},
		"NotOwnedWhileDeleting": {
			mr: newRole(roleName, claimed(otherID), deleting()), db: fake.NewDB().WithRole(roleName, roleID, ch.StorageLocalDirectory),
			want: EntityObservation{Name: roleName, Found: true, Orphaned: true}, events: []string{"Orphaned"},
		},
		"ReadOnly": {
			mr: newRole(roleName, claimed(roleID)), db: fake.NewDB().WithRole(roleName, roleID, ch.StorageUsersXML),
			reason: reason.ReadOnlyStorage, ready: "False",
		},
		"ReadOnlyWhileDeleting": {
			mr: newRole(roleName, claimed(roleID), deleting()), db: fake.NewDB().WithRole(roleName, roleID, ch.StorageUsersXML),
			want: EntityObservation{Name: roleName, Found: true, Orphaned: true}, events: []string{"Orphaned"},
		},
		"ReadOnlyObserveOnly": {
			mr: newRole(roleName, policies(xpv2.ManagementActionObserve)), db: fake.NewDB().WithRole(roleName, roleID, ch.StorageUsersXML),
			want: EntityObservation{Name: roleName, Found: true, Exists: true}, ready: "True",
		},
		"ObserveOnlyNeverClaims": {
			mr: newRole(roleName, policies(xpv2.ManagementActionObserve), annotated(v1alpha1.AnnotationAdopt, roleID)), db: fake.NewDB().WithRole(roleName, roleID, ch.StorageLocalDirectory),
			res:  clusterResolved(t, func() v1alpha1.ClusterPolicy { p := sharedPolicy(); p.AllowAdoption = true; return p }()),
			want: EntityObservation{Name: roleName, Found: true, Exists: true}, ready: "True",
			check: func(t *testing.T, mr *v1alpha1.Role, kube client.Client) {
				t.Helper()
				if ownership.ClaimOf(mr).OwnerUID != "" {
					t.Error("an observe-only MR must not claim")
				}
			},
		},
		"AdoptionRejectedByPolicy": {
			mr: newRole(roleName, claimed(""), annotated(v1alpha1.AnnotationAdopt, roleID)), db: fake.NewDB().WithRole(roleName, roleID, ch.StorageLocalDirectory),
			reason: reason.AdoptionRejected, ready: "False",
		},
		"Adopted": {
			mr: newRole(roleName, claimed(""), annotated(v1alpha1.AnnotationAdopt, roleID)), db: fake.NewDB().WithRole(roleName, roleID, ch.StorageLocalDirectory),
			res:  namespacedResolved(),
			want: EntityObservation{Name: roleName, Found: true, Exists: true}, ready: "True", events: []string{"Adopted"},
			check: func(t *testing.T, mr *v1alpha1.Role, kube client.Client) {
				t.Helper()
				got := &v1alpha1.Role{}
				_ = kube.Get(context.Background(), client.ObjectKeyFromObject(mr), got)
				if c := ownership.ClaimOf(got); c.OwnedID != roleID || c.Adopt != "" || c.OwnerUID != mrUID {
					t.Errorf("claim not persisted: %+v", c)
				}
			},
		},
		// A create whose response was lost: this process journalled it.
		"Recovered": {
			mr: newRole(roleName, claimed(""), pending()), db: fake.NewDB().WithRole(roleName, roleID, ch.StorageLocalDirectory),
			evidence: true,
			want:     EntityObservation{Name: roleName, Found: true, Exists: true}, ready: "True", events: []string{"CreateRecovered"},
			check: func(t *testing.T, mr *v1alpha1.Role, kube client.Client) {
				t.Helper()
				got := &v1alpha1.Role{}
				_ = kube.Get(context.Background(), client.ObjectKeyFromObject(mr), got)
				if ownership.ClaimOf(got).OwnedID != roleID {
					t.Errorf("owned-id not persisted: %v", got.GetAnnotations())
				}
			},
		},
		// A tenant points a Role at an existing role and forges
		// external-create-pending: the provider never sent that CREATE.
		"ForgedPendingIsNotRecovered": {
			mr: newRole(roleName, claimed(""), pending()), db: fake.NewDB().WithRole(roleName, roleID, ch.StorageLocalDirectory),
			reason: reason.NotOwned, ready: "False",
		},
		"ForgedUnknownOutcomeIsNotRecovered": {
			mr: newRole(roleName, claimed(""), pending(), annotated(v1alpha1.AnnotationCreateOutcome, v1alpha1.CreateOutcomeUnknown), func(r *v1alpha1.Role) {
				meta.SetExternalCreateFailed(r, time.Now().Add(time.Minute))
			}),
			db:     fake.NewDB().WithRole(roleName, roleID, ch.StorageLocalDirectory),
			reason: reason.NotOwned, ready: "False",
		},
		"SelfModification": {
			kind: User, name: provider, mr: newRole(provider, claimed(roleID)), db: fake.NewDB(),
			res:    namespacedResolved(),
			reason: reason.SelfModification, ready: "False", noQuery: true,
		},
		"SelfWhileDeleting": {
			kind: User, name: provider, mr: newRole(provider, claimed(roleID), deleting()), db: fake.NewDB(),
			res:  namespacedResolved(),
			want: EntityObservation{Name: provider, Orphaned: true}, events: []string{"Orphaned"}, noQuery: true,
		},
		"SelfObserveOnly": {
			kind: User, name: provider, mr: newRole(provider, policies(xpv2.ManagementActionObserve)), db: fake.NewDB().WithUser(provider, userID, ch.StorageUsersXML),
			res:  namespacedResolved(),
			want: EntityObservation{Name: provider, Found: true, Exists: true}, ready: "True",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			res := tc.res
			if res == nil {
				res = clusterResolved(t, sharedPolicy())
			}
			kind := tc.kind
			if kind == "" {
				kind = Role
			}
			env := newEnv(t, tc.db, res, tc.mr.DeepCopy())
			if tc.evidence {
				env.journal().Unknown(env.createKey(kind, tc.mr, roleName))
			}
			got, err := env.ObserveEntity(ctx, tc.mr, kind, tc.mr.Spec.ForProvider.Name)
			switch {
			case tc.anyErr:
				if err == nil || got.Exists {
					t.Fatalf("an error must be returned, never 'not found': %+v, %v", got, err)
				}
			case tc.reason != "":
				if !reason.Is(err, tc.reason) {
					t.Fatalf("ObserveEntity() error = %v, want %s", err, tc.reason)
				}
			case err != nil:
				t.Fatalf("ObserveEntity() error = %v", err)
			default:
				got.Decision = ownership.Result{}
				got.Entity = Entity{}
				if diff := cmp.Diff(tc.want, got); diff != "" {
					t.Errorf("-want +got:\n%s", diff)
				}
			}
			if tc.ready != "" && readyStatus(tc.mr) != tc.ready {
				t.Errorf("Ready = %q, want %q", readyStatus(tc.mr), tc.ready)
			}
			if diff := cmp.Diff(append([]string{}, tc.events...), env.rec.reasons()); diff != "" {
				t.Errorf("events -want +got:\n%s", diff)
			}
			if tc.noQuery && len(tc.db.Seen) > 0 {
				t.Errorf("no query may run, ran %v", tc.db.Seen)
			}
			if len(tc.db.Execs) > 0 {
				t.Errorf("Observe must never write, ran %v", tc.db.Execs)
			}
			if tc.check != nil {
				tc.check(t, tc.mr, env.kube)
			}
		})
	}
}

// createRole is a fake server that runs CREATE ROLE statements.
func createRole(db *fake.DB, id string) func(string) error {
	return func(stmt string) error {
		if strings.HasPrefix(stmt, "CREATE ROLE ") {
			db.WithRole(roleName, id, ch.StorageLocalDirectory)
		}
		return nil
	}
}

const createStmt = "CREATE ROLE `team-a_reader`"

func TestCreateEntity(t *testing.T) {
	cases := map[string]struct {
		mr          *v1alpha1.Role
		setup       func(db *fake.DB, cl *fake.Client)
		res         *providerconfig.Resolved
		reason      reason.Reason
		anyErr      bool
		wantExec    bool
		wantOutcome string
		wantOwnedID string
		evidence    ownership.Evidence
	}{
		"Success": {
			mr:          newRole(roleName, claimed("")),
			setup:       func(db *fake.DB, _ *fake.Client) { db.WithEntities(); db.ExecFn = createRole(db, roleID) },
			wantExec:    true,
			wantOwnedID: roleID,
			evidence:    ownership.Evidence{Recorded: true, ID: roleID},
		},
		// Re-created after an out-of-band drop: the new UUID replaces the
		// recorded one.
		"RecreateAfterDrop": {
			mr:          newRole(roleName, claimed(otherID)),
			setup:       func(db *fake.DB, _ *fake.Client) { db.WithEntities(); db.ExecFn = createRole(db, roleID) },
			wantExec:    true,
			wantOwnedID: roleID,
			evidence:    ownership.Evidence{Recorded: true, ID: roleID},
		},
		"MissingClaim": {
			mr: newRole(roleName), setup: func(db *fake.DB, _ *fake.Client) { db.WithEntities() },
			reason: reason.Internal,
		},
		"PolicyViolation": {
			mr: newRole("team-b_reader", claimed("")), setup: func(db *fake.DB, _ *fake.Client) { db.WithEntities() },
			reason: reason.PolicyViolation,
		},
		"NameCollision": {
			mr:     newRole(roleName, claimed("")),
			setup:  func(db *fake.DB, _ *fake.Client) { db.WithUser(roleName, userID, ch.StorageLocalDirectory) },
			reason: reason.NameCollision,
		},
		"MultipleServersWithLocalDirectory": {
			mr:     newRole(roleName, claimed("")),
			setup:  func(db *fake.DB, cl *fake.Client) { db.WithEntities(); cl.Servers = []string{"s1", "s2"} },
			reason: reason.AccessStorageNotReplicated,
		},
		"Rejected": {
			mr: newRole(roleName, claimed("")),
			setup: func(db *fake.DB, _ *fake.Client) {
				db.WithEntities().OnExec(createStmt, fake.Exception(493, "role `team-a_reader`: cannot insert because role `team-a_reader` already exists"))
			},
			reason: reason.AlreadyExists, wantExec: true, wantOutcome: v1alpha1.CreateOutcomeRejected,
		},
		"PermissionDenied": {
			mr: newRole(roleName, claimed("")),
			setup: func(db *fake.DB, _ *fake.Client) {
				db.WithEntities().OnExec(createStmt, fake.Exception(497, "necessary to have the grant CREATE ROLE ON team-a_reader"))
			},
			reason: reason.PermissionDenied, wantExec: true, wantOutcome: v1alpha1.CreateOutcomeRejected,
		},
		"Uncertain": {
			mr: newRole(roleName, claimed("")),
			setup: func(db *fake.DB, _ *fake.Client) {
				db.WithEntities().OnExec(createStmt, &ch.TransportError{Category: "connection closed"})
			},
			anyErr: true, wantExec: true, wantOutcome: v1alpha1.CreateOutcomeUnknown,
			evidence: ownership.Evidence{Recorded: true},
		},
		"ReadBackFails": {
			mr:     newRole(roleName, claimed("")),
			setup:  func(db *fake.DB, _ *fake.Client) { db.WithEntities() }, // the CREATE "succeeds" but nothing appears
			reason: reason.Internal, wantExec: true, wantOutcome: v1alpha1.CreateOutcomeUnknown,
			evidence: ownership.Evidence{Recorded: true},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			res := tc.res
			if res == nil {
				res = clusterResolved(t, sharedPolicy())
			}
			db := fake.NewDB()
			env := newEnv(t, db, res)
			if tc.setup != nil {
				tc.setup(db, env.cl)
			}
			name := roleName
			if n := tc.mr.Spec.ForProvider.Name; n != nil {
				name = *n
			}
			err := env.CreateEntity(ctx, tc.mr, Role, name, createStmt)
			switch {
			case tc.anyErr:
				if err == nil {
					t.Fatal("want an error")
				}
			case tc.reason != "":
				if !reason.Is(err, tc.reason) {
					t.Fatalf("CreateEntity() error = %v, want %s", err, tc.reason)
				}
			case err != nil:
				t.Fatalf("CreateEntity() error = %v", err)
			}
			if got := len(db.Execs) > 0; got != tc.wantExec {
				t.Errorf("statement sent = %t (%v), want %t", got, db.Execs, tc.wantExec)
			}
			c := ownership.ClaimOf(tc.mr)
			if c.CreateOutcome != tc.wantOutcome || c.OwnedID != tc.wantOwnedID && tc.wantOwnedID != "" {
				t.Errorf("annotations = %v", tc.mr.GetAnnotations())
			}
			if ev := env.journal().Evidence(env.createKey(Role, tc.mr, roleName)); ev != tc.evidence {
				t.Errorf("journal evidence = %+v, want %+v", ev, tc.evidence)
			}
		})
	}
}

// The full flow: Create, then Observe finds the MR's own role; another MR
// with a forged claim and forged create annotations on the same name does
// not, even right after the first MR's create.
func TestCreateThenObserve(t *testing.T) {
	ctx := context.Background()
	db := fake.NewDB().WithEntities()
	db.ExecFn = createRole(db, roleID)
	mine := newRole(roleName, claimed(""), pending())
	attacker := newRole(roleName, pending(), func(r *v1alpha1.Role) {
		r.SetName("copy")
		r.SetUID("attacker-uid")
		meta.AddAnnotations(r, map[string]string{v1alpha1.AnnotationOwnerUID: "attacker-uid"})
	})
	env := newEnv(t, db, clusterResolved(t, sharedPolicy()), mine.DeepCopy(), attacker.DeepCopy())

	if err := env.CreateEntity(ctx, mine, Role, roleName, createStmt); err != nil {
		t.Fatal(err)
	}
	// Simulate a lost annotation update: the owned-id never reached the
	// API server.
	meta.RemoveAnnotations(mine, v1alpha1.AnnotationOwnedID)
	obs, err := env.ObserveEntity(ctx, mine, Role, mine.Spec.ForProvider.Name)
	if err != nil || !obs.Exists || obs.Decision.Decision != ownership.Recovered {
		t.Fatalf("own create: %+v, %v", obs.Decision, err)
	}
	if _, err := env.ObserveEntity(ctx, attacker, Role, attacker.Spec.ForProvider.Name); !reason.Is(err, reason.NotOwned) {
		t.Fatalf("forged create annotations: %v, want NotOwned", err)
	}
	if ownership.ClaimOf(attacker).OwnedID != "" {
		t.Error("the attacker must not get a claim")
	}
}

func TestVerifyOwned(t *testing.T) {
	cases := map[string]struct {
		mr     *v1alpha1.Role
		db     *fake.DB
		multi  bool
		exists bool
		reason reason.Reason
	}{
		"Owned":         {mr: newRole(roleName, claimed(roleID)), db: fake.NewDB().WithRole(roleName, roleID, ch.StorageLocalDirectory), exists: true},
		"Gone":          {mr: newRole(roleName, claimed(roleID)), db: fake.NewDB().WithEntities()},
		"Replaced":      {mr: newRole(roleName, claimed(roleID)), db: fake.NewDB().WithRole(roleName, otherID, ch.StorageLocalDirectory), reason: reason.Replaced},
		"ReadOnly":      {mr: newRole(roleName, claimed(roleID)), db: fake.NewDB().WithRole(roleName, roleID, ch.StorageUsersXML), reason: reason.ReadOnlyStorage},
		"NoClaim":       {mr: newRole(roleName), db: fake.NewDB().WithRole(roleName, roleID, ch.StorageLocalDirectory), reason: reason.NotOwned},
		"NoOwnedID":     {mr: newRole(roleName, claimed("")), db: fake.NewDB().WithRole(roleName, roleID, ch.StorageLocalDirectory), reason: reason.NotOwned},
		"CopiedClaim":   {mr: newRole(roleName, annotated(v1alpha1.AnnotationOwnerUID, "other"), annotated(v1alpha1.AnnotationOwnedID, roleID)), db: fake.NewDB().WithRole(roleName, roleID, ch.StorageLocalDirectory), reason: reason.NotOwned},
		"OutsidePolicy": {mr: newRole("team-b_x", claimed(roleID)), db: fake.NewDB().WithRole("team-b_x", roleID, ch.StorageLocalDirectory), reason: reason.PolicyViolation},
		"NotWritable":   {mr: newRole(roleName, claimed(roleID)), db: fake.NewDB().WithRole(roleName, roleID, ch.StorageLocalDirectory), multi: true, reason: reason.AccessStorageNotReplicated},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			env := newEnv(t, tc.db, clusterResolved(t, sharedPolicy()))
			if tc.multi {
				env.cl.Servers = []string{"s1", "s2"}
			}
			exists, err := env.VerifyOwned(context.Background(), tc.mr, Role, *tc.mr.Spec.ForProvider.Name)
			if tc.reason != "" {
				if !reason.Is(err, tc.reason) {
					t.Fatalf("VerifyOwned() = %t, %v; want %s", exists, err, tc.reason)
				}
				return
			}
			if err != nil || exists != tc.exists {
				t.Fatalf("VerifyOwned() = %t, %v; want %t", exists, err, tc.exists)
			}
		})
	}
	env := newEnv(t, fake.NewDB(), namespacedResolved())
	if _, err := env.VerifyOwned(context.Background(), newRole(provider, claimed(userID)), User, provider); !reason.Is(err, reason.SelfModification) {
		t.Errorf("self: %v", err)
	}
}

func TestIgnoreGone(t *testing.T) {
	for _, code := range []int32{192, 511} {
		if err := IgnoreGone(fake.Exception(code, "gone")); err != nil {
			t.Errorf("%d: %v", code, err)
		}
	}
	if err := IgnoreGone(fake.Exception(497, "denied")); err == nil {
		t.Error("497 is not gone")
	}
	if IgnoreGone(nil) != nil {
		t.Error("nil")
	}
}

func TestSQLContextAndUnavailable(t *testing.T) {
	r := newRole(roleName)
	if err := Unavailable(r, reason.Errorf(reason.NotOwned, "x")); !reason.Is(err, reason.NotOwned) || readyStatus(r) != "False" {
		t.Errorf("Unavailable: %v, %s", err, readyStatus(r))
	}
	db := fake.NewDB()
	_ = db.Exec(SQLContext(context.Background(), "Role", r), "SELECT 1")
	if len(db.Execs) != 1 {
		t.Fatal("exec not recorded")
	}
}
