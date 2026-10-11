// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	"github.com/google/go-cmp/cmp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/crossplane/crossplane-runtime/v2/pkg/meta"
	"github.com/crossplane/crossplane-runtime/v2/pkg/reconciler/managed"
	"github.com/crossplane/crossplane-runtime/v2/pkg/resource"

	"github.com/infrabay/provider-clickhouse/apis/v1alpha1"
	ch "github.com/infrabay/provider-clickhouse/internal/clients/clickhouse"
	"github.com/infrabay/provider-clickhouse/internal/controller/access"
	"github.com/infrabay/provider-clickhouse/internal/controller/ownership"
	"github.com/infrabay/provider-clickhouse/internal/controller/role"
	"github.com/infrabay/provider-clickhouse/internal/reason"
)

// Entities defined in users.d by the role fixtures (users_xml storage).
const (
	// roleFixtureYAMLRole is a users.d role inside the policy's grantableRoles (app_.+).
	roleFixtureYAMLRole = "app_yaml_reader"
	// roleFixtureYAMLSuperRole is a users.d role outside grantableRoles.
	roleFixtureYAMLSuperRole = "zz_yaml_super"
	// roleFixtureYAMLUser is a users.d user inside userNamePatterns.
	roleFixtureYAMLUser = "app_yaml_user"
)

// roleUsersD defines roleFixtureYAMLRole, roleFixtureYAMLSuperRole and
// roleFixtureYAMLUser in users.d.
func roleUsersD() string {
	return fmt.Sprintf(`roles:
  %s:
    grants:
      query:
        - "GRANT SELECT ON app_yaml.*"
  %s:
    grants:
      query:
        - "GRANT SELECT ON *.*"
users:
  %s:
    password_sha256_hex: %q
    networks: {ip: ["::/0"]}
    profile: default
    quota: default
`, roleFixtureYAMLRole, roleFixtureYAMLSuperRole, roleFixtureYAMLUser, sha256Hex(randHex(16)))
}

// forEachRoleImage runs fn for every selected image (XPCH_UPSTREAM_IMAGES,
// XPCH_ANTALYA_IMAGES), each with its own access fixture whose server also
// has roleUsersD in users.d.
func forEachRoleImage(t *testing.T, policy v1alpha1.ClusterPolicy, fn func(t *testing.T, f *accessFixture)) {
	t.Helper()
	ran := false
	for _, fam := range []struct{ name, env, def string }{
		{familyUpstream, envUpstreamImages, defaultUpstreamImages},
		{familyAntalya, envAntalyaImages, defaultAntalyaImages},
	} {
		for _, image := range imageList(fam.env, fam.def) {
			ran = true
			t.Run(image, func(t *testing.T) {
				fn(t, newAccessFixture(t, fam.name, image, policy, map[string]string{"/etc/clickhouse-server/users.d/role-fixtures.yaml": roleUsersD()}))
			})
		}
	}
	if !ran {
		t.Skip("no images selected")
	}
}

// roleHarness returns a harness that drives the Role controller's external
// client and initializer on the fixture.
func roleHarness(t *testing.T, f *accessFixture) *mrHarness[*v1alpha1.Role] {
	t.Helper()
	conn := accessConnector(f, "Role", role.NewExternal, nil)
	init := ownership.NewInitializer(f.kube, func(mg resource.Managed) *string { return mg.(*v1alpha1.Role).Spec.ForProvider.Name })
	return newAccessHarness(t, f, conn, func() *v1alpha1.Role { return &v1alpha1.Role{} }, init)
}

// newRoleMR returns a Role MR named mrName for the ClickHouse role name on
// the fixture's config.
func newRoleMR(f *accessFixture, mrName, name string) *v1alpha1.Role {
	r := &v1alpha1.Role{ObjectMeta: metav1.ObjectMeta{Name: mrName}}
	r.Spec.ProviderConfigReference = f.pc
	r.Spec.ForProvider.Name = &name
	return r
}

// roleSnapshot returns every role, every user and every membership on the
// server, as the admin sees them, one line each: "role <name> <id>",
// "user <name> <id>" and "grant <grantee> <role> <admin> <default>". It is
// compared before and after operations that must not touch foreign entities.
func (s *server) roleSnapshot(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, q := range []string{
		"SELECT concat('role ', name, ' ', toString(id)) FROM system.roles",
		"SELECT concat('user ', name, ' ', toString(id)) FROM system.users",
		"SELECT concat('grant ', ifNull(user_name, concat('role:', ifNull(role_name, ''))), ' ', granted_role_name, ' ', " +
			"toString(with_admin_option), ' ', toString(granted_role_is_default)) FROM system.role_grants",
	} {
		out = append(out, s.rawStrings(t, q)...)
	}
	slices.Sort(out)
	return out
}

// sameRoleSnapshot fails the test if the server's access entities differ from
// before.
func sameRoleSnapshot(t *testing.T, s *server, before []string, what string) {
	t.Helper()
	if diff := cmp.Diff(before, s.roleSnapshot(t)); diff != "" {
		t.Errorf("%s changed access entities (-before +after):\n%s", what, diff)
	}
}

// TestRole runs the Role controller against every image with the minimal,
// prefix-scoped provider identity (design 5.5, 4.2, 10.2): the lifecycle,
// out-of-band drops and replacements, adoption, roles it did not create,
// name collisions with users, users.d roles and users, names outside the
// policy, forged create annotations and adversarial names. Foreign entities
// must stay untouched, which is checked by comparing system.roles,
// system.users and system.role_grants before and after.
func TestRole(t *testing.T) {
	forEachRoleImage(t, accessPolicy(), func(t *testing.T, f *accessFixture) {
		t.Run("Lifecycle", func(t *testing.T) { testRoleLifecycle(t, f) })
		t.Run("Adoption", func(t *testing.T) { testRoleAdoption(t, f) })
		t.Run("ForeignRoles", func(t *testing.T) { testRoleForeign(t, f) })
		t.Run("NameCollision", func(t *testing.T) { testRoleNameCollision(t, f) })
		t.Run("UsersXML", func(t *testing.T) { testRoleUsersXML(t, f) })
		t.Run("AdversarialName", func(t *testing.T) { testRoleAdversarialName(t, f) })
	})
}

func testRoleLifecycle(t *testing.T, f *accessFixture) {
	h := roleHarness(t, f)
	h.create(newRoleMR(f, "reader", "app_reader"))
	if o := h.reconcile("reader"); o.err != nil || !o.created {
		t.Fatalf("create: %s", o)
	}
	id, storage, ok := f.srv.entity(t, access.Role, "app_reader")
	if !ok || storage != ch.StorageLocalDirectory {
		t.Fatalf("server has %q (%s, %t)", id, storage, ok)
	}
	r := h.get("reader")
	if c := ownership.ClaimOf(r); c.OwnedID != id || c.OwnerUID != string(r.GetUID()) || c.CreateOutcome != "" {
		t.Fatalf("claim after create: %+v", c)
	}
	if meta.GetExternalName(r) != "app_reader" {
		t.Errorf("external name = %q", meta.GetExternalName(r))
	}

	o := h.reconcile("reader")
	if o.err != nil || !o.obs.ResourceExists || !o.obs.ResourceUpToDate || o.created || o.updated {
		t.Fatalf("steady state: %s", o)
	}

	// Every statement is attributable to the MR (design 5.1).
	f.srv.exec(t, "SYSTEM FLUSH LOGS")
	var n uint64
	if err := f.srv.admin.QueryRow(context.Background(),
		"SELECT count() FROM system.query_log WHERE type = 'QueryFinish' AND log_comment = 'Role/team-a/reader' AND query LIKE 'CREATE ROLE%'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("CREATE ROLE with log_comment Role/team-a/reader: %d entries in system.query_log", n)
	}

	// Dropped out of band: created again, and the new UUID recorded.
	f.srv.exec(t, "DROP ROLE app_reader")
	if o := h.reconcile("reader"); o.err != nil || !o.created {
		t.Fatalf("re-create: %s", o)
	}
	newID, _, ok := f.srv.entity(t, access.Role, "app_reader")
	if !ok || newID == id || ownership.ClaimOf(h.get("reader")).OwnedID != newID {
		t.Fatalf("re-created as %q (was %q), claim %v", newID, id, h.get("reader").GetAnnotations())
	}

	// A user granted the role keeps nothing once the role is dropped.
	f.adminUser(t, "app_reader_user")
	f.srv.exec(t, "GRANT app_reader TO app_reader_user")
	h.delete("reader")
	if o := h.reconcile("reader"); o.err != nil || !o.deleted {
		t.Fatalf("delete: %s", o)
	}
	if _, _, ok := f.srv.entity(t, access.Role, "app_reader"); ok {
		t.Fatal("the role still exists")
	}
	if g := f.srv.rawStrings(t, "SELECT granted_role_name FROM system.role_grants WHERE user_name = 'app_reader_user'"); len(g) != 0 {
		t.Errorf("DROP ROLE leaves memberships behind: %v", g)
	}
	if o := h.reconcile("reader"); o.err != nil || !o.gone {
		t.Fatalf("finalize: %s", o)
	}
}

func testRoleAdoption(t *testing.T, f *accessFixture) {
	h := roleHarness(t, f)
	h.create(newRoleMR(f, "adoptee", "app_adoptee"))
	if o := h.reconcile("adoptee"); o.err != nil || !o.created {
		t.Fatalf("create: %s", o)
	}
	// An administrator replaces the role: CREATE ROLE OR REPLACE assigns a
	// new UUID (A30), so the role is no longer the MR's.
	f.adminUser(t, "app_adoptee_user")
	f.srv.exec(t, "GRANT app_adoptee TO app_adoptee_user")
	f.srv.exec(t, "CREATE ROLE OR REPLACE app_adoptee")
	replaced, _, _ := f.srv.entity(t, access.Role, "app_adoptee")
	if g := f.srv.rawStrings(t, "SELECT granted_role_name FROM system.role_grants WHERE user_name = 'app_adoptee_user'"); len(g) != 0 {
		t.Errorf("CREATE ROLE OR REPLACE keeps memberships: %v", g)
	}
	before := f.srv.roleSnapshot(t)
	o := h.reconcile("adoptee")
	if !reason.Is(o.err, reason.Replaced) || !strings.Contains(o.err.Error(), replaced) || o.created {
		t.Fatalf("replaced: %s", o)
	}
	if r, _, _ := observeStored(t, h, "adoptee"); r.GetCondition(xpv2.TypeReady).Status != "False" {
		t.Errorf("Ready = %s", r.GetCondition(xpv2.TypeReady).Status)
	}

	// Adoption needs the observed UUID, and policy.allowAdoption.
	h.mutate("adoptee", func(r *v1alpha1.Role) {
		meta.AddAnnotations(r, map[string]string{v1alpha1.AnnotationAdopt: "true"})
	})
	if o := h.reconcile("adoptee"); !reason.Is(o.err, reason.AdoptionRejected) {
		t.Fatalf("adopt \"true\": %s", o)
	}
	h.mutate("adoptee", func(r *v1alpha1.Role) {
		meta.AddAnnotations(r, map[string]string{v1alpha1.AnnotationAdopt: replaced})
	})
	if o := h.reconcile("adoptee"); !reason.Is(o.err, reason.AdoptionRejected) || !strings.Contains(o.err.Error(), "allowAdoption") {
		t.Fatalf("adopt without policy: %s", o)
	}
	sameRoleSnapshot(t, f.srv, before, "a rejected adoption")
	setRoleFixtureAdoption(t, f, true)
	defer setRoleFixtureAdoption(t, f, false)
	if o := h.reconcile("adoptee"); o.err != nil || !o.obs.ResourceExists {
		t.Fatalf("adopt: %s", o)
	}
	if c := ownership.ClaimOf(h.get("adoptee")); c.OwnedID != replaced || c.Adopt != "" {
		t.Fatalf("claim after adoption: %+v", c)
	}
	sameRoleSnapshot(t, f.srv, before, "adoption")

	// The adopted role is the MR's own: deleting the MR drops it.
	h.delete("adoptee")
	if o := h.reconcile("adoptee"); o.err != nil || !o.deleted {
		t.Fatalf("delete: %s", o)
	}
	if _, _, ok := f.srv.entity(t, access.Role, "app_adoptee"); ok {
		t.Fatal("the adopted role still exists")
	}
}

// setRoleFixtureAdoption sets policy.allowAdoption of the fixture's
// ClusterProviderConfig.
func setRoleFixtureAdoption(t *testing.T, f *accessFixture, allow bool) {
	t.Helper()
	cpc := &v1alpha1.ClusterProviderConfig{}
	if err := f.kube.Get(context.Background(), client.ObjectKey{Name: f.pc.Name}, cpc); err != nil {
		t.Fatal(err)
	}
	cpc.Spec.Policy.AllowAdoption = allow
	cpc.Generation++
	if err := f.kube.Update(context.Background(), cpc); err != nil {
		t.Fatal(err)
	}
}

func testRoleForeign(t *testing.T, f *accessFixture) {
	h := roleHarness(t, f)
	// Roles an administrator created, one of them granted to a user.
	f.adminRole(t, "app_foreign")
	f.adminRole(t, "app_victim")
	f.adminUser(t, "app_foreign_user")
	f.srv.exec(t, "GRANT app_foreign TO app_foreign_user")
	f.adminRole(t, "zz_outside")
	before := f.srv.roleSnapshot(t)

	// A Role MR for an existing role is NotOwned and never creates or
	// changes it.
	h.create(newRoleMR(f, "foreign", "app_foreign"))
	if o := h.reconcile("foreign"); !reason.Is(o.err, reason.NotOwned) || o.created {
		t.Fatalf("pre-existing role: %s", o)
	}
	// Its id is reported, so that it can be adopted.
	id, _, _ := f.srv.entity(t, access.Role, "app_foreign")
	if r, _, err := observeStored(t, h, "foreign"); r.Status.AtProvider.ID != id || !strings.Contains(err.Error(), id) {
		t.Errorf("status id = %q, error %v; want %q", r.Status.AtProvider.ID, err, id)
	}

	// A tenant forges the create annotations that ownership row 5 reads:
	// this provider process never sent that CREATE, so nothing is claimed.
	h.create(newRoleMR(f, "forger", "app_victim"))
	if o := h.reconcile("forger"); !reason.Is(o.err, reason.NotOwned) {
		t.Fatalf("victim: %s", o)
	}
	h.mutate("forger", func(r *v1alpha1.Role) {
		meta.SetExternalCreatePending(r, time.Now())
		meta.AddAnnotations(r, map[string]string{v1alpha1.AnnotationCreateOutcome: v1alpha1.CreateOutcomeUnknown})
	})
	if o := h.reconcile("forger"); !reason.Is(o.err, reason.NotOwned) || ownership.ClaimOf(h.get("forger")).OwnedID != "" {
		t.Fatalf("forged create annotations: %s, claim %v", o, h.get("forger").GetAnnotations())
	}

	// A name outside the policy is never read, even with a forged claim.
	h.create(newRoleMR(f, "outside", "zz_outside"))
	if o := h.reconcile("outside"); !reason.Is(o.err, reason.PolicyViolation) {
		t.Fatalf("outside the policy: %s", o)
	}
	outsideID, _, _ := f.srv.entity(t, access.Role, "zz_outside")
	h.mutate("outside", func(r *v1alpha1.Role) {
		meta.AddAnnotations(r, map[string]string{v1alpha1.AnnotationOwnerUID: string(r.GetUID()), v1alpha1.AnnotationOwnedID: outsideID})
	})

	// An observe-only MR reports any role inside the policy.
	obs := newRoleMR(f, "observer", "app_foreign")
	obs.SetManagementPolicies(xpv2.ManagementPolicies{xpv2.ManagementActionObserve})
	h.create(obs)
	if o := h.reconcile("observer"); o.err != nil || !o.obs.ResourceExists {
		t.Fatalf("observe-only: %s", o)
	}
	if c := ownership.ClaimOf(h.get("observer")); c.OwnerUID != "" {
		t.Errorf("an observe-only MR got a claim: %+v", c)
	}

	// Deleting the MRs leaves every role in place.
	for _, mr := range []string{"foreign", "forger", "outside", "observer"} {
		h.delete(mr)
		if o := h.reconcile(mr); o.err != nil || !o.gone || o.deleted {
			t.Fatalf("delete %s: %s", mr, o)
		}
	}
	sameRoleSnapshot(t, f.srv, before, "Role MRs for foreign roles")
}

func testRoleNameCollision(t *testing.T, f *accessFixture) {
	h := roleHarness(t, f)
	f.adminUser(t, "app_twin")
	before := f.srv.roleSnapshot(t)

	// ClickHouse would create the role, and grants meant for it would then
	// land on the user: the provider refuses before the CREATE.
	h.create(newRoleMR(f, "twin", "app_twin"))
	if o := h.reconcile("twin"); !reason.Is(o.err, reason.NameCollision) || o.created {
		t.Fatalf("role named like a user: %s", o)
	}
	// The same for the name of a users.d user (A20).
	h.create(newRoleMR(f, "yaml-twin", roleFixtureYAMLUser))
	if o := h.reconcile("yaml-twin"); !reason.Is(o.err, reason.NameCollision) || o.created {
		t.Fatalf("role named like a users.d user: %s", o)
	}
	sameRoleSnapshot(t, f.srv, before, "colliding Role MRs")

	// A user created after the role: the collision is reported, and the
	// MR can still delete its own role.
	h.create(newRoleMR(f, "late", "app_late"))
	if o := h.reconcile("late"); o.err != nil || !o.created {
		t.Fatalf("create: %s", o)
	}
	f.adminUser(t, "app_late")
	if o := h.reconcile("late"); !reason.Is(o.err, reason.NameCollision) {
		t.Fatalf("user created after the role: %s", o)
	}
	h.delete("late")
	if o := h.reconcile("late"); o.err != nil || !o.deleted {
		t.Fatalf("delete: %s", o)
	}
	if _, _, ok := f.srv.entity(t, access.Role, "app_late"); ok {
		t.Error("the role still exists")
	}
	if _, _, ok := f.srv.entity(t, access.User, "app_late"); !ok {
		t.Error("the same-named user was dropped")
	}
	for _, mr := range []string{"twin", "yaml-twin"} {
		h.delete(mr)
		if o := h.reconcile(mr); o.err != nil || !o.gone {
			t.Fatalf("delete %s: %s", mr, o)
		}
	}
}

func testRoleUsersXML(t *testing.T, f *accessFixture) {
	h := roleHarness(t, f)
	id, storage, ok := f.srv.entity(t, access.Role, roleFixtureYAMLRole)
	if !ok || storage != ch.StorageUsersXML {
		t.Fatalf("users.d role: %q %q %t", id, storage, ok)
	}
	before := f.srv.roleSnapshot(t)

	h.create(newRoleMR(f, "yaml", roleFixtureYAMLRole))
	if o := h.reconcile("yaml"); !reason.Is(o.err, reason.ReadOnlyStorage) || o.created {
		t.Fatalf("users.d role: %s", o)
	}
	if r, _, _ := observeStored(t, h, "yaml"); r.Status.AtProvider.ID != id || r.Status.AtProvider.Storage != ch.StorageUsersXML {
		t.Errorf("status = %+v", r.Status.AtProvider)
	}
	// Adoption cannot make it writable.
	setRoleFixtureAdoption(t, f, true)
	h.mutate("yaml", func(r *v1alpha1.Role) { meta.AddAnnotations(r, map[string]string{v1alpha1.AnnotationAdopt: id}) })
	if o := h.reconcile("yaml"); !reason.Is(o.err, reason.ReadOnlyStorage) {
		t.Fatalf("adopting a users.d role: %s", o)
	}
	setRoleFixtureAdoption(t, f, false)

	obs := newRoleMR(f, "yaml-observer", roleFixtureYAMLRole)
	obs.SetManagementPolicies(xpv2.ManagementPolicies{xpv2.ManagementActionObserve})
	h.create(obs)
	if o := h.reconcile("yaml-observer"); o.err != nil || !o.obs.ResourceExists {
		t.Fatalf("observe-only users.d role: %s", o)
	}
	if r, _, _ := observeStored(t, h, "yaml-observer"); r.Status.AtProvider.Storage != ch.StorageUsersXML || r.GetCondition(xpv2.TypeReady).Status != "True" {
		t.Errorf("status = %+v, Ready %s", r.Status.AtProvider, r.GetCondition(xpv2.TypeReady).Status)
	}

	for _, mr := range []string{"yaml", "yaml-observer"} {
		h.delete(mr)
		if o := h.reconcile(mr); o.err != nil || !o.gone || o.deleted {
			t.Fatalf("delete %s: %s", mr, o)
		}
	}
	sameRoleSnapshot(t, f.srv, before, "Role MRs for a users.d role")
}

// renamedUserDirectories renames both user directories: system.users.storage
// and system.roles.storage hold a directory's name, so users.d entities then
// report the storage cfg_users and SQL entities sql_users [verified on
// 25.3.3].
const renamedUserDirectories = `<clickhouse>
  <user_directories replace="replace">
    <users_xml><name>cfg_users</name><path>users.xml</path></users_xml>
    <local_directory><name>sql_users</name><path>/var/lib/clickhouse/access/</path></local_directory>
  </user_directories>
</clickhouse>
`

// TestRenamedUserDirectories checks, on every image, that entities of
// users.xml/users.d are never written when the configuration renames the
// users_xml directory: a Role MR cannot adopt a users.d role and is released
// on deletion without a DROP, a User MR claiming a users.d user changes
// nothing, and a Grant to that user grants nothing. Roles the provider
// creates are stored in the renamed SQL directory and managed as usual.
func TestRenamedUserDirectories(t *testing.T) {
	policy := accessPolicy()
	policy.AllowAdoption = true
	ran := false
	for _, fam := range []struct{ name, env, def string }{
		{familyUpstream, envUpstreamImages, defaultUpstreamImages},
		{familyAntalya, envAntalyaImages, defaultAntalyaImages},
	} {
		for _, image := range imageList(fam.env, fam.def) {
			ran = true
			t.Run(image, func(t *testing.T) {
				testRenamedUserDirectories(t, newAccessFixture(t, fam.name, image, policy, map[string]string{
					"/etc/clickhouse-server/users.d/role-fixtures.yaml":    roleUsersD(),
					"/etc/clickhouse-server/config.d/user-directories.xml": renamedUserDirectories,
				}))
			})
		}
	}
	if !ran {
		t.Skip("no images selected")
	}
}

func testRenamedUserDirectories(t *testing.T, f *accessFixture) {
	roleID, storage, ok := f.srv.entity(t, access.Role, roleFixtureYAMLRole)
	if !ok || storage != "cfg_users" {
		t.Fatalf("users.d role: %q %q %t", roleID, storage, ok)
	}
	userID, storage, ok := f.srv.entity(t, access.User, roleFixtureYAMLUser)
	if !ok || storage != "cfg_users" {
		t.Fatalf("users.d user: %q %q %t", userID, storage, ok)
	}
	before := f.srv.roleSnapshot(t)
	userBefore := f.showCreateUser(t, roleFixtureYAMLUser)
	grantsBefore := f.srv.rawStrings(t, "SHOW GRANTS FOR "+ch.QuoteIdent(roleFixtureYAMLUser))

	// Adoption cannot make a users.d role writable, whatever its directory
	// is called; deleting the MR, even with a claim on the role, releases it
	// without a DROP.
	h := roleHarness(t, f)
	r := newRoleMR(f, "cfg", roleFixtureYAMLRole)
	meta.AddAnnotations(r, map[string]string{v1alpha1.AnnotationAdopt: roleID})
	h.create(r)
	if o := h.reconcile("cfg"); !reason.Is(o.err, reason.ReadOnlyStorage) || o.created {
		t.Fatalf("adopting a users.d role: %s", o)
	}
	if c := ownership.ClaimOf(h.get("cfg")); c.OwnedID != "" {
		t.Fatalf("a users.d role was claimed: %+v", c)
	}
	h.mutate("cfg", func(r *v1alpha1.Role) {
		meta.RemoveAnnotations(r, v1alpha1.AnnotationAdopt)
		meta.AddAnnotations(r, map[string]string{v1alpha1.AnnotationOwnerUID: string(r.GetUID()), v1alpha1.AnnotationOwnedID: roleID})
	})
	h.delete("cfg")
	if o := h.reconcile("cfg"); o.err != nil || !o.gone || o.deleted {
		t.Fatalf("delete: %s", o)
	}

	// A User MR claiming the users.d user changes nothing.
	uh := newUserHarness(t, f)
	uh.createUser("cfg-user", roleFixtureYAMLUser, func(u *v1alpha1.User) {
		u.Spec.ForProvider.HostIP = []string{"10.0.0.0/8"}
	})
	uh.mutate("cfg-user", func(u *v1alpha1.User) {
		meta.AddAnnotations(u, map[string]string{v1alpha1.AnnotationOwnerUID: string(u.GetUID()), v1alpha1.AnnotationOwnedID: userID})
	})
	uh.mustReconcile("cfg-user", "users.d user", failedWith(reason.ReadOnlyStorage))

	// Nor does a Grant to it.
	g := newGrantFixture(t, f)
	p := roleGrantee("app_cfg", "SELECT")
	p.User = new(roleFixtureYAMLUser)
	g.newGrant("cfg-grant", p)
	if o := g.h.reconcile("cfg-grant"); !reason.Is(o.err, reason.ReadOnlyStorage) || o.created || o.updated {
		t.Fatalf("grant to a users.d user: %s", o)
	}

	// The SQL directory under another name is managed as usual.
	h.create(newRoleMR(f, "sql", "app_sql_role"))
	if o := h.reconcile("sql"); o.err != nil || !o.created {
		t.Fatalf("create: %s", o)
	}
	if _, storage, ok := f.srv.entity(t, access.Role, "app_sql_role"); !ok || storage != "sql_users" {
		t.Fatalf("created role: %q %t", storage, ok)
	}
	if o := h.reconcile("sql"); o.err != nil || !o.obs.ResourceExists || !o.obs.ResourceUpToDate || o.created {
		t.Fatalf("steady state: %s", o)
	}
	h.delete("sql")
	if o := h.reconcile("sql"); o.err != nil || !o.deleted {
		t.Fatalf("delete: %s", o)
	}

	sameRoleSnapshot(t, f.srv, before, "MRs for users.d entities in a renamed directory")
	if got := f.showCreateUser(t, roleFixtureYAMLUser); got != userBefore {
		t.Errorf("the users.d user changed: %s, was %s", got, userBefore)
	}
	if diff := cmp.Diff(grantsBefore, f.srv.rawStrings(t, "SHOW GRANTS FOR "+ch.QuoteIdent(roleFixtureYAMLUser))); diff != "" {
		t.Errorf("grants of the users.d user changed (-before +after):\n%s", diff)
	}
}

func testRoleAdversarialName(t *testing.T, f *accessFixture) {
	h := roleHarness(t, f)
	name := "app_r`'\\.ü@*\tx"
	mr := newRoleMR(f, "weird", name)
	h.create(mr)
	if o := h.reconcile("weird"); o.err != nil || !o.created {
		t.Fatalf("create: %s", o)
	}
	id, _, ok := f.srv.entity(t, access.Role, name)
	if !ok || ownership.ClaimOf(h.get("weird")).OwnedID != id {
		t.Fatalf("server %q (%t), claim %v", id, ok, h.get("weird").GetAnnotations())
	}
	if o := h.reconcile("weird"); o.err != nil || !o.obs.ResourceUpToDate {
		t.Fatalf("observe: %s", o)
	}
	h.delete("weird")
	if o := h.reconcile("weird"); o.err != nil || !o.deleted {
		t.Fatalf("delete: %s", o)
	}
	if _, _, ok := f.srv.entity(t, access.Role, name); ok {
		t.Fatal("the role still exists")
	}
}

// observeStored connects and runs Observe on the stored MR name, as the first
// half of a reconcile, and returns the MR with the status and conditions
// that Observe set: the harness does not persist status.
func observeStored[T resource.ModernManaged](t *testing.T, h *mrHarness[T], name string) (T, managed.ExternalObservation, error) {
	t.Helper()
	ctx := context.Background()
	obj := h.get(name)
	ext, err := h.connector.Connect(ctx, obj)
	if err != nil {
		return obj, managed.ExternalObservation{}, err
	}
	defer func() { _ = ext.Disconnect(ctx) }()
	obs, err := ext.Observe(ctx, obj)
	return obj, obs, err
}
