// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"

	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	"github.com/google/go-cmp/cmp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/crossplane/crossplane-runtime/v2/pkg/meta"

	"github.com/infrabay/provider-clickhouse/apis/v1alpha1"
	ch "github.com/infrabay/provider-clickhouse/internal/clients/clickhouse"
	"github.com/infrabay/provider-clickhouse/internal/controller/access"
	"github.com/infrabay/provider-clickhouse/internal/controller/access/accesstest"
	"github.com/infrabay/provider-clickhouse/internal/controller/providerconfig"
	"github.com/infrabay/provider-clickhouse/internal/controller/rolegrant"
	"github.com/infrabay/provider-clickhouse/internal/reason"
)

// roleGrantHarness returns a harness that drives the RoleGrant controller's
// external client on the fixture, with its reference resolver.
func roleGrantHarness(t *testing.T, f *accessFixture) *mrHarness[*v1alpha1.RoleGrant] {
	t.Helper()
	conn := accessConnector(f, "RoleGrant", rolegrant.NewExternal, rolegrant.References)
	return newAccessHarness(t, f, conn, func() *v1alpha1.RoleGrant { return &v1alpha1.RoleGrant{} }).
		withReferences(providerconfig.NewReferenceResolver(f.kube, rolegrant.ReferencesOf))
}

// rgMROpt sets a field of a RoleGrant MR.
type rgMROpt func(*v1alpha1.RoleGrantParameters)

func rgMemberOf(name string) rgMROpt {
	return func(p *v1alpha1.RoleGrantParameters) { p.MemberOf = &name }
}

func rgMemberOfRef(mr string) rgMROpt {
	return func(p *v1alpha1.RoleGrantParameters) { p.MemberOfRef = &xpv2.NamespacedReference{Name: mr} }
}

func rgToUser(name string) rgMROpt {
	return func(p *v1alpha1.RoleGrantParameters) { p.User = &name }
}

func rgToUserRef(mr string) rgMROpt {
	return func(p *v1alpha1.RoleGrantParameters) { p.UserRef = &xpv2.NamespacedReference{Name: mr} }
}

func rgToRole(name string) rgMROpt {
	return func(p *v1alpha1.RoleGrantParameters) { p.Role = &name }
}

func rgToRoleRef(mr string) rgMROpt {
	return func(p *v1alpha1.RoleGrantParameters) { p.RoleRef = &xpv2.NamespacedReference{Name: mr} }
}

func rgAdmin(v bool) rgMROpt {
	return func(p *v1alpha1.RoleGrantParameters) { p.AdminOption = &v }
}

// newRoleGrantMR returns a RoleGrant MR named mrName on the config pc.
func newRoleGrantMR(pc *xpv2.ProviderConfigReference, mrName string, opts ...rgMROpt) *v1alpha1.RoleGrant {
	g := &v1alpha1.RoleGrant{ObjectMeta: metav1.ObjectMeta{Name: mrName}}
	g.Spec.ProviderConfigReference = pc
	g.Spec.ForProvider.AdminOption = new(false)
	for _, o := range opts {
		o(&g.Spec.ForProvider)
	}
	return g
}

// rgMembership reads, as the admin, whether grantee holds role, and with
// which admin option and default flag.
func (s *server) rgMembership(t *testing.T, kind access.EntityKind, grantee, role string) (admin, isDefault, ok bool) {
	t.Helper()
	col := "user_name"
	if kind == access.Role {
		col = "role_name"
	}
	rows := s.rawStrings(t, "SELECT concat(toString(with_admin_option), toString(granted_role_is_default)) FROM system.role_grants WHERE "+
		col+" = "+ch.QuoteString(grantee)+" AND granted_role_name = "+ch.QuoteString(role))
	if len(rows) == 0 {
		return false, false, false
	}
	return rows[0][0] == '1', rows[0][1] == '1', true
}

// rgDefaultRoles returns default_roles_list of a user, as the admin sees it.
func (s *server) rgDefaultRoles(t *testing.T, user string) []string {
	t.Helper()
	return s.rawStrings(t, "SELECT arrayJoin(default_roles_list) FROM system.users WHERE name = "+ch.QuoteString(user))
}

// TestRoleGrant runs the RoleGrant controller against every image with the
// minimal, prefix-scoped provider identity (design 5.7, 4.3, 10.2): GRANT,
// drift of the membership and of the admin option, REVOKE on delete,
// references, role grantees, users.d roles and users, foreign and ambiguous
// grantees, a grantee replaced between Observe and the GRANT, missing roles,
// the interplay with default roles per server version, and adversarial
// names. Foreign grantees must stay untouched, which is checked by comparing
// system.role_grants before and after.
func TestRoleGrant(t *testing.T) {
	forEachRoleImage(t, accessPolicy(), func(t *testing.T, f *accessFixture) {
		t.Run("Lifecycle", func(t *testing.T) { testRoleGrantLifecycle(t, f) })
		t.Run("AdminOption", func(t *testing.T) { testRoleGrantAdminOption(t, f) })
		t.Run("RoleGrantee", func(t *testing.T) { testRoleGrantToRole(t, f) })
		t.Run("UsersXML", func(t *testing.T) { testRoleGrantUsersXML(t, f) })
		t.Run("ForeignGrantees", func(t *testing.T) { testRoleGrantForeign(t, f) })
		t.Run("GranteeReplacedBeforeGrant", func(t *testing.T) { testRoleGrantReplaced(t, f) })
		t.Run("MissingRole", func(t *testing.T) { testRoleGrantMissingRole(t, f) })
		t.Run("DefaultRoles", func(t *testing.T) { testRoleGrantDefaultRoles(t, f) })
		t.Run("References", func(t *testing.T) { testRoleGrantReferences(t, f) })
		t.Run("ImportWithoutDelete", func(t *testing.T) { testRoleGrantImport(t, f) })
		t.Run("AdversarialNames", func(t *testing.T) { testRoleGrantAdversarial(t, f) })
	})
}

func testRoleGrantLifecycle(t *testing.T, f *accessFixture) {
	rh := roleHarness(t, f)
	rh.create(newRoleMR(f, "lc-reader", "app_lc_reader"))
	if o := rh.reconcile("lc-reader"); o.err != nil || !o.created {
		t.Fatalf("role: %s", o)
	}
	f.ownedUser(t, "lc-api", "app_lc_api")
	// A membership the RoleGrant does not manage.
	f.adminRole(t, "app_lc_other")
	f.srv.exec(t, "GRANT app_lc_other TO app_lc_api")

	h := roleGrantHarness(t, f)
	h.create(newRoleGrantMR(f.pc, "lc", rgMemberOfRef("lc-reader"), rgToUserRef("lc-api")))
	if o := h.reconcile("lc"); o.err != nil || !o.created {
		t.Fatalf("create: %s", o)
	}
	if p := h.get("lc").Spec.ForProvider; p.MemberOf == nil || *p.MemberOf != "app_lc_reader" || p.User == nil || *p.User != "app_lc_api" {
		t.Fatalf("references resolved to %v, %v", p.MemberOf, p.User)
	}
	if admin, def, ok := f.srv.rgMembership(t, access.User, "app_lc_api", "app_lc_reader"); !ok || admin || !def {
		t.Fatalf("membership after create: ok %t, admin %t, default %t", ok, admin, def)
	}
	o := h.reconcile("lc")
	if o.err != nil || !o.obs.ResourceExists || !o.obs.ResourceUpToDate || o.created || o.updated {
		t.Fatalf("steady state: %s", o)
	}
	rg, _, _ := observeStored(t, h, "lc")
	want := v1alpha1.RoleGrantObservation{Grantee: "user:app_lc_api", IsDefault: true}
	if diff := cmp.Diff(want, rg.Status.AtProvider); diff != "" {
		t.Errorf("status -want +got:\n%s", diff)
	}
	if c := rg.GetCondition(xpv2.TypeReady); c.Status != "True" {
		t.Errorf("Ready = %s", c.Status)
	}

	// The admin option added out of band is removed; the membership stays.
	f.srv.exec(t, "GRANT app_lc_reader TO app_lc_api WITH ADMIN OPTION")
	if o := h.reconcile("lc"); o.err != nil || !o.updated || o.obs.ResourceUpToDate {
		t.Fatalf("admin option drift: %s", o)
	}
	if admin, _, ok := f.srv.rgMembership(t, access.User, "app_lc_api", "app_lc_reader"); !ok || admin {
		t.Fatalf("after repair: ok %t, admin %t", ok, admin)
	}

	// Revoked out of band: granted again.
	f.srv.exec(t, "REVOKE app_lc_reader FROM app_lc_api")
	if o := h.reconcile("lc"); o.err != nil || !o.created {
		t.Fatalf("re-grant: %s", o)
	}

	// The admin option needs policy.allowAdminOption: nothing changes.
	h.mutate("lc", func(g *v1alpha1.RoleGrant) { g.Spec.ForProvider.AdminOption = new(true) })
	if o := h.reconcile("lc"); !reason.Is(o.err, reason.PolicyViolation) || o.updated {
		t.Fatalf("admin option without policy: %s", o)
	}
	if admin, _, _ := f.srv.rgMembership(t, access.User, "app_lc_api", "app_lc_reader"); admin {
		t.Fatal("the admin option was granted")
	}
	h.mutate("lc", func(g *v1alpha1.RoleGrant) { g.Spec.ForProvider.AdminOption = new(false) })

	// Deleting the RoleGrant revokes exactly its membership.
	h.delete("lc")
	if o := h.reconcile("lc"); o.err != nil || !o.deleted {
		t.Fatalf("delete: %s", o)
	}
	if _, _, ok := f.srv.rgMembership(t, access.User, "app_lc_api", "app_lc_reader"); ok {
		t.Fatal("the membership still exists")
	}
	if _, _, ok := f.srv.rgMembership(t, access.User, "app_lc_api", "app_lc_other"); !ok {
		t.Fatal("another membership of the grantee was revoked")
	}
	if o := h.reconcile("lc"); o.err != nil || !o.gone {
		t.Fatalf("finalize: %s", o)
	}
}

func testRoleGrantAdminOption(t *testing.T, f *accessFixture) {
	// A second ClusterProviderConfig that allows the admin option. The
	// grantee's owner must use the same config.
	cpc := &v1alpha1.ClusterProviderConfig{}
	if err := f.kube.Get(context.Background(), client.ObjectKey{Name: f.pc.Name}, cpc); err != nil {
		t.Fatal(err)
	}
	cpc = cpc.DeepCopy()
	cpc.ObjectMeta = metav1.ObjectMeta{Name: "access-admin", UID: types.UID("cpc-access-admin")}
	cpc.Spec.Policy.AllowAdminOption = true
	if err := f.kube.Create(context.Background(), cpc); err != nil {
		t.Fatal(err)
	}
	pc := &xpv2.ProviderConfigReference{Kind: v1alpha1.ClusterProviderConfigKind, Name: cpc.Name}
	f.adminRole(t, "app_ao_reader")
	u := accesstest.OwnedUser(namespace, "ao-api", "app_ao_api", f.adminUser(t, "app_ao_api"), pc)
	if err := f.kube.Create(context.Background(), u); err != nil {
		t.Fatal(err)
	}

	h := roleGrantHarness(t, f)
	h.create(newRoleGrantMR(pc, "ao", rgMemberOf("app_ao_reader"), rgToUser("app_ao_api"), rgAdmin(true)))
	if o := h.reconcile("ao"); o.err != nil || !o.created {
		t.Fatalf("create: %s", o)
	}
	if admin, _, ok := f.srv.rgMembership(t, access.User, "app_ao_api", "app_ao_reader"); !ok || !admin {
		t.Fatalf("after create: ok %t, admin %t", ok, admin)
	}
	if g, o, err := observeStored(t, h, "ao"); err != nil || !o.ResourceUpToDate || !g.Status.AtProvider.AdminOption {
		t.Fatalf("steady state: %+v, %v, %+v", o, err, g.Status.AtProvider)
	}
	// Removing the admin option keeps the membership.
	h.mutate("ao", func(g *v1alpha1.RoleGrant) { g.Spec.ForProvider.AdminOption = new(false) })
	if o := h.reconcile("ao"); o.err != nil || !o.updated {
		t.Fatalf("remove the admin option: %s", o)
	}
	if admin, _, ok := f.srv.rgMembership(t, access.User, "app_ao_api", "app_ao_reader"); !ok || admin {
		t.Fatalf("after removal: ok %t, admin %t", ok, admin)
	}
	h.mutate("ao", func(g *v1alpha1.RoleGrant) { g.Spec.ForProvider.AdminOption = new(true) })
	if o := h.reconcile("ao"); o.err != nil || !o.updated {
		t.Fatalf("add the admin option: %s", o)
	}
	if admin, _, ok := f.srv.rgMembership(t, access.User, "app_ao_api", "app_ao_reader"); !ok || !admin {
		t.Fatalf("after adding: ok %t, admin %t", ok, admin)
	}
	h.delete("ao")
	if o := h.reconcile("ao"); o.err != nil || !o.deleted {
		t.Fatalf("delete: %s", o)
	}
	if _, _, ok := f.srv.rgMembership(t, access.User, "app_ao_api", "app_ao_reader"); ok {
		t.Fatal("the membership still exists")
	}

	// A grantee owned through the other config is not this config's.
	h.create(newRoleGrantMR(f.pc, "ao-other", rgMemberOf("app_ao_reader"), rgToUser("app_ao_api")))
	if o := h.reconcile("ao-other"); !reason.Is(o.err, reason.NotOwned) || o.created {
		t.Fatalf("grantee owned through another config: %s", o)
	}
}

func testRoleGrantToRole(t *testing.T, f *accessFixture) {
	f.adminRole(t, "app_tr_reader")
	f.ownedRole(t, "tr-writer", "app_tr_writer")
	h := roleGrantHarness(t, f)
	h.create(newRoleGrantMR(f.pc, "tr", rgMemberOf("app_tr_reader"), rgToRoleRef("tr-writer")))
	if o := h.reconcile("tr"); o.err != nil || !o.created {
		t.Fatalf("create: %s", o)
	}
	if _, _, ok := f.srv.rgMembership(t, access.Role, "app_tr_writer", "app_tr_reader"); !ok {
		t.Fatal("the role grantee is not a member")
	}
	if g, o, err := observeStored(t, h, "tr"); err != nil || !o.ResourceUpToDate || g.Status.AtProvider.Grantee != "role:app_tr_writer" {
		t.Fatalf("steady state: %+v, %v, %+v", o, err, g.Status.AtProvider)
	}
	// A role granted to itself is refused: 26.8 rejects it, older servers
	// store it.
	h.create(newRoleGrantMR(f.pc, "self", rgMemberOf("app_tr_writer"), rgToRole("app_tr_writer")))
	if o := h.reconcile("self"); !reason.Is(o.err, reason.InvalidSpec) || o.created {
		t.Fatalf("self grant: %s", o)
	}
	h.delete("tr")
	if o := h.reconcile("tr"); o.err != nil || !o.deleted {
		t.Fatalf("delete: %s", o)
	}
	if _, _, ok := f.srv.rgMembership(t, access.Role, "app_tr_writer", "app_tr_reader"); ok {
		t.Fatal("the membership still exists")
	}
	h.delete("self")
	if o := h.reconcile("self"); o.err != nil || !o.gone {
		t.Fatalf("delete self: %s", o)
	}
}

func testRoleGrantUsersXML(t *testing.T, f *accessFixture) {
	f.ownedUser(t, "x-api", "app_x_api")
	h := roleGrantHarness(t, f)

	// A users.d role listed in grantableRoles is granted to an SQL user and
	// becomes one of its default roles (DEFAULT ROLE ALL).
	h.create(newRoleGrantMR(f.pc, "yaml", rgMemberOf(roleFixtureYAMLRole), rgToUser("app_x_api")))
	if o := h.reconcile("yaml"); o.err != nil || !o.created {
		t.Fatalf("users.d role: %s", o)
	}
	if _, def, ok := f.srv.rgMembership(t, access.User, "app_x_api", roleFixtureYAMLRole); !ok || !def {
		t.Fatalf("users.d role membership: ok %t, default %t", ok, def)
	}
	if g, o, err := observeStored(t, h, "yaml"); err != nil || !o.ResourceUpToDate || !g.Status.AtProvider.IsDefault {
		t.Fatalf("steady state: %+v, %v, %+v", o, err, g.Status.AtProvider)
	}

	f.adminRole(t, "app_x_reader")
	before := f.srv.roleSnapshot(t)
	// A users.d role outside grantableRoles is never granted.
	h.create(newRoleGrantMR(f.pc, "super", rgMemberOf(roleFixtureYAMLSuperRole), rgToUser("app_x_api")))
	if o := h.reconcile("super"); !reason.Is(o.err, reason.PolicyViolation) || o.created {
		t.Fatalf("users.d role outside grantableRoles: %s", o)
	}
	// users.d users, including the provider identity, are never written.
	h.create(newRoleGrantMR(f.pc, "to-yaml-user", rgMemberOf(roleFixtureYAMLRole), rgToUser(roleFixtureYAMLUser)))
	if o := h.reconcile("to-yaml-user"); !reason.Is(o.err, reason.ReadOnlyStorage) || o.created {
		t.Fatalf("users.d user grantee: %s", o)
	}
	h.create(newRoleGrantMR(f.pc, "to-me", rgMemberOf(roleFixtureYAMLRole), rgToUser(f.identity)))
	if o := h.reconcile("to-me"); !reason.Is(o.err, reason.ReadOnlyStorage) || o.created {
		t.Fatalf("the provider identity as grantee: %s", o)
	}
	// A users.d role as grantee is read-only too.
	h.create(newRoleGrantMR(f.pc, "to-yaml-role", rgMemberOf("app_x_reader"), rgToRole(roleFixtureYAMLRole)))
	if o := h.reconcile("to-yaml-role"); !reason.Is(o.err, reason.ReadOnlyStorage) || o.created {
		t.Fatalf("users.d role grantee: %s", o)
	}
	for _, mr := range []string{"super", "to-yaml-user", "to-me", "to-yaml-role"} {
		h.delete(mr)
		if o := h.reconcile(mr); o.err != nil || !o.gone || o.deleted {
			t.Fatalf("delete %s: %s", mr, o)
		}
	}
	sameRoleSnapshot(t, f.srv, before, "RoleGrants for users.d entities")

	h.delete("yaml")
	if o := h.reconcile("yaml"); o.err != nil || !o.deleted {
		t.Fatalf("delete: %s", o)
	}
	if _, _, ok := f.srv.rgMembership(t, access.User, "app_x_api", roleFixtureYAMLRole); ok {
		t.Fatal("the users.d role was not revoked")
	}
}

func testRoleGrantForeign(t *testing.T, f *accessFixture) {
	// Entities an administrator created, with memberships.
	f.adminRole(t, "app_fg_reader")
	f.adminRole(t, "app_fg_other")
	f.adminUser(t, "app_fg_user")
	f.srv.exec(t, "GRANT app_fg_reader TO app_fg_user WITH ADMIN OPTION")
	// A role shadowed by a same-named user: GRANT and REVOKE would hit the
	// user. The role MR owns the role, but the binding is ambiguous.
	f.ownedRole(t, "fg-twin", "app_fg_twin")
	f.adminUser(t, "app_fg_twin")
	f.srv.exec(t, "GRANT app_fg_reader TO app_fg_twin")
	// A User MR whose claim was copied from another MR.
	copied := accesstest.OwnedUser(namespace, "fg-copy", "app_fg_user", "00000000-0000-0000-0000-000000000001", f.pc)
	meta.AddAnnotations(copied, map[string]string{v1alpha1.AnnotationOwnerUID: "someone-else"})
	if err := f.kube.Create(context.Background(), copied); err != nil {
		t.Fatal(err)
	}
	before := f.srv.roleSnapshot(t)

	h := roleGrantHarness(t, f)
	for _, c := range []struct {
		mr   string
		opts []rgMROpt
		want reason.Reason
	}{
		// Revoking the admin option of a foreign user's membership.
		{"fg-reader", []rgMROpt{rgMemberOf("app_fg_reader"), rgToUser("app_fg_user")}, reason.NotOwned},
		// Granting another role to it.
		{"fg-other", []rgMROpt{rgMemberOf("app_fg_other"), rgToUser("app_fg_user")}, reason.NotOwned},
		{"fg-twin", []rgMROpt{rgMemberOf("app_fg_other"), rgToRole("app_fg_twin")}, reason.AmbiguousGrantee},
		{"fg-twin-reader", []rgMROpt{rgMemberOf("app_fg_reader"), rgToRole("app_fg_twin")}, reason.AmbiguousGrantee},
		{"fg-missing", []rgMROpt{rgMemberOf("app_fg_other"), rgToUser("app_fg_nobody")}, reason.DependencyMissing},
		{"fg-outside", []rgMROpt{rgMemberOf("app_fg_other"), rgToUser("zz_user")}, reason.PolicyViolation},
	} {
		h.create(newRoleGrantMR(f.pc, c.mr, c.opts...))
		if o := h.reconcile(c.mr); !reason.Is(o.err, c.want) || o.created || o.updated {
			t.Fatalf("%s: %s, want %s", c.mr, o, c.want)
		}
	}
	// An observe-only RoleGrant reports a foreign membership.
	obs := newRoleGrantMR(f.pc, "fg-observer", rgMemberOf("app_fg_reader"), rgToUser("app_fg_user"))
	obs.SetManagementPolicies(xpv2.ManagementPolicies{xpv2.ManagementActionObserve})
	h.create(obs)
	if o := h.reconcile("fg-observer"); o.err != nil || !o.obs.ResourceExists || o.obs.ResourceUpToDate {
		t.Fatalf("observe-only: %s", o)
	}
	if g, _, _ := observeStored(t, h, "fg-observer"); !g.Status.AtProvider.AdminOption || g.Status.AtProvider.Grantee != "user:app_fg_user" {
		t.Errorf("observe-only status: %+v", g.Status.AtProvider)
	}

	// Deleting every MR revokes nothing.
	for _, mr := range []string{"fg-reader", "fg-other", "fg-twin", "fg-twin-reader", "fg-missing", "fg-outside", "fg-observer"} {
		h.delete(mr)
		if o := h.reconcile(mr); o.err != nil || !o.gone || o.deleted {
			t.Fatalf("delete %s: %s", mr, o)
		}
	}
	sameRoleSnapshot(t, f.srv, before, "RoleGrants for foreign grantees")
}

// testRoleGrantReplaced replaces the grantee between Observe and the GRANT
// of one reconcile: the re-check before the write refuses it, and the new
// user gets nothing.
func testRoleGrantReplaced(t *testing.T, f *accessFixture) {
	ctx := context.Background()
	f.adminRole(t, "app_rp_reader")
	f.ownedUser(t, "rp-api", "app_rp_api")
	mr := newRoleGrantMR(f.pc, "rp", rgMemberOf("app_rp_reader"), rgToUser("app_rp_api"))
	mr.SetNamespace(namespace)
	mr.SetUID("uid-rp")
	if err := f.kube.Create(ctx, mr); err != nil {
		t.Fatal(err)
	}
	ext, err := accessConnector(f, "RoleGrant", rolegrant.NewExternal, rolegrant.References).Connect(ctx, mr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ext.Disconnect(ctx) }()
	if obs, err := ext.Observe(ctx, mr); err != nil || obs.ResourceExists {
		t.Fatalf("Observe() = %+v, %v", obs, err)
	}
	f.srv.exec(t, "DROP USER app_rp_api")
	f.adminUser(t, "app_rp_api")
	before := f.srv.roleSnapshot(t)
	if _, err := ext.Create(ctx, mr); !reason.Is(err, reason.NotOwned) {
		t.Fatalf("Create() after the grantee was replaced = %v, want NotOwned", err)
	}
	sameRoleSnapshot(t, f.srv, before, "a GRANT to a replaced grantee")
}

func testRoleGrantMissingRole(t *testing.T, f *accessFixture) {
	f.ownedUser(t, "mr-api", "app_mr_api")
	rh := roleHarness(t, f)
	rh.create(newRoleMR(f, "mr-reader", "app_mr_reader"))
	if o := rh.reconcile("mr-reader"); o.err != nil || !o.created {
		t.Fatalf("role: %s", o)
	}
	h := roleGrantHarness(t, f)
	h.create(newRoleGrantMR(f.pc, "mr-none", rgMemberOf("app_mr_nosuch"), rgToUser("app_mr_api")))
	if o := h.reconcile("mr-none"); !reason.Is(o.err, reason.DependencyMissing) || o.created {
		t.Fatalf("missing role: %s", o)
	}
	h.create(newRoleGrantMR(f.pc, "mr", rgMemberOfRef("mr-reader"), rgToUser("app_mr_api")))
	if o := h.reconcile("mr"); o.err != nil || !o.created {
		t.Fatalf("create: %s", o)
	}
	// The Role MR is deleted first: DROP ROLE removes the membership too.
	rh.delete("mr-reader")
	if o := rh.reconcile("mr-reader"); o.err != nil || !o.deleted {
		t.Fatalf("delete the role: %s", o)
	}
	if o := h.reconcile("mr"); !reason.Is(o.err, reason.DependencyMissing) {
		t.Fatalf("after the role was dropped: %s", o)
	}
	for _, mr := range []string{"mr", "mr-none"} {
		h.delete(mr)
		if o := h.reconcile(mr); o.err != nil || !o.gone || o.deleted {
			t.Fatalf("delete %s: %s", mr, o)
		}
	}
}

// testRoleGrantDefaultRoles checks what a RoleGrant does to its grantee's
// default roles: nothing itself. DEFAULT ROLE ALL users get the role as a
// default role, users with a list do not; REVOKE removes the role from the
// list on every version except 25.3, where a stale entry stays and makes a
// later GRANT default again (A15).
func testRoleGrantDefaultRoles(t *testing.T, f *accessFixture) {
	f.adminRole(t, "app_dr_reader")
	f.srv.exec(t, "CREATE USER app_dr_listed IDENTIFIED WITH sha256_hash BY "+ch.QuoteString(testHash)+" SALT "+ch.QuoteString(testSalt)+" DEFAULT ROLE NONE")
	id, _, _ := f.srv.entity(t, access.User, "app_dr_listed")
	if err := f.kube.Create(context.Background(), accesstest.OwnedUser(namespace, "dr-listed", "app_dr_listed", id, f.pc)); err != nil {
		t.Fatal(err)
	}
	h := roleGrantHarness(t, f)
	h.create(newRoleGrantMR(f.pc, "dr", rgMemberOf("app_dr_reader"), rgToUser("app_dr_listed")))
	if o := h.reconcile("dr"); o.err != nil || !o.created {
		t.Fatalf("create: %s", o)
	}
	if g, o, err := observeStored(t, h, "dr"); err != nil || !o.ResourceUpToDate || g.Status.AtProvider.IsDefault {
		t.Fatalf("a role granted to a DEFAULT ROLE NONE user is not default: %+v, %v, %+v", o, err, g.Status.AtProvider)
	}
	if got := f.srv.rgDefaultRoles(t, "app_dr_listed"); len(got) != 0 {
		t.Fatalf("the RoleGrant changed the default roles: %v", got)
	}
	// User.defaultRoles (Listed) makes it default; the RoleGrant reports it.
	f.srv.exec(t, "ALTER USER app_dr_listed DEFAULT ROLE app_dr_reader")
	if g, o, err := observeStored(t, h, "dr"); err != nil || !o.ResourceUpToDate || !g.Status.AtProvider.IsDefault {
		t.Fatalf("default after ALTER USER: %+v, %v, %+v", o, err, g.Status.AtProvider)
	}

	h.delete("dr")
	if o := h.reconcile("dr"); o.err != nil || !o.deleted {
		t.Fatalf("delete: %s", o)
	}
	stale := is253(f.image)
	if got := f.srv.rgDefaultRoles(t, "app_dr_listed"); (len(got) == 1) != stale {
		t.Errorf("default_roles_list after REVOKE on %s = %v; a stale entry is expected only on 25.3", f.image, got)
	}
	// Granted again: default again only through the stale entry (25.3).
	if o := h.reconcile("dr"); o.err != nil || !o.gone {
		t.Fatalf("finalize: %s", o)
	}
	h.create(newRoleGrantMR(f.pc, "dr2", rgMemberOf("app_dr_reader"), rgToUser("app_dr_listed")))
	if o := h.reconcile("dr2"); o.err != nil || !o.created {
		t.Fatalf("grant again: %s", o)
	}
	if _, def, ok := f.srv.rgMembership(t, access.User, "app_dr_listed", "app_dr_reader"); !ok || def != stale {
		t.Errorf("is default after GRANT again on %s = %t (member %t); want %t", f.image, def, ok, stale)
	}
}

func testRoleGrantReferences(t *testing.T, f *accessFixture) {
	f.adminRole(t, "app_ref_role")
	f.ownedUser(t, "ref-api", "app_ref_api")
	// A Role MR on another config.
	other := accesstest.OwnedRole(namespace, "ref-elsewhere", "app_ref_role", "00000000-0000-0000-0000-000000000002",
		&xpv2.ProviderConfigReference{Kind: v1alpha1.ProviderConfigKind, Name: "elsewhere"})
	if err := f.kube.Create(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	before := f.srv.roleSnapshot(t)
	h := roleGrantHarness(t, f)

	// A referenced MR must use the same ProviderConfig.
	h.create(newRoleGrantMR(f.pc, "ref-other-config", rgMemberOfRef("ref-elsewhere"), rgToUserRef("ref-api")))
	if o := h.reconcile("ref-other-config"); !reason.Is(o.err, reason.InvalidSpec) || !strings.Contains(o.err.Error(), "memberOfRef") || o.created {
		t.Fatalf("reference to another config: %s", o)
	}
	// References never leave the namespace (CEL rejects them too).
	cross := newRoleGrantMR(f.pc, "ref-cross", rgMemberOf("app_ref_role"))
	cross.Spec.ForProvider.UserRef = &xpv2.NamespacedReference{Name: "ref-api", Namespace: "team-b"}
	h.create(cross)
	if o := h.reconcile("ref-cross"); !reason.Is(o.err, reason.InvalidSpec) || o.created {
		t.Fatalf("cross-namespace reference: %s", o)
	}
	if p := h.get("ref-cross").Spec.ForProvider; p.User != nil {
		t.Errorf("the cross-namespace reference was resolved: %q", *p.User)
	}
	sameRoleSnapshot(t, f.srv, before, "invalid references")
}

// testRoleGrantImport manages an existing membership without Delete in the
// management policies: deleting the MR leaves it in place (design 9.6).
func testRoleGrantImport(t *testing.T, f *accessFixture) {
	f.adminRole(t, "app_im_reader")
	f.ownedUser(t, "im-api", "app_im_api")
	f.srv.exec(t, "GRANT app_im_reader TO app_im_api")
	h := roleGrantHarness(t, f)
	mr := newRoleGrantMR(f.pc, "im", rgMemberOf("app_im_reader"), rgToUser("app_im_api"))
	mr.SetManagementPolicies(xpv2.ManagementPolicies{xpv2.ManagementActionObserve, xpv2.ManagementActionCreate, xpv2.ManagementActionUpdate})
	h.create(mr)
	if o := h.reconcile("im"); o.err != nil || !o.obs.ResourceExists || o.created {
		t.Fatalf("import: %s", o)
	}
	h.delete("im")
	if o := h.reconcile("im"); o.err != nil || !o.gone || o.deleted {
		t.Fatalf("delete: %s", o)
	}
	if _, _, ok := f.srv.rgMembership(t, access.User, "app_im_api", "app_im_reader"); !ok {
		t.Fatal("the imported membership was revoked")
	}
}

func testRoleGrantAdversarial(t *testing.T, f *accessFixture) {
	role, user := "app_r`'\\.ü@* x", "app_u`'\\.ü@*\tx"
	f.adminRole(t, role)
	f.ownedUser(t, "adv-user", user)
	h := roleGrantHarness(t, f)
	h.create(newRoleGrantMR(f.pc, "adv", rgMemberOf(role), rgToUser(user)))
	if o := h.reconcile("adv"); o.err != nil || !o.created {
		t.Fatalf("create: %s", o)
	}
	if _, _, ok := f.srv.rgMembership(t, access.User, user, role); !ok {
		t.Fatal("not granted")
	}
	if o := h.reconcile("adv"); o.err != nil || !o.obs.ResourceUpToDate {
		t.Fatalf("observe: %s", o)
	}
	h.delete("adv")
	if o := h.reconcile("adv"); o.err != nil || !o.deleted {
		t.Fatalf("delete: %s", o)
	}
	if _, _, ok := f.srv.rgMembership(t, access.User, user, role); ok {
		t.Fatal("not revoked")
	}
}
