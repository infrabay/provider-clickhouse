// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	chgo "github.com/ClickHouse/clickhouse-go/v2"
	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/crossplane/crossplane-runtime/v2/pkg/meta"
	"github.com/crossplane/crossplane-runtime/v2/pkg/reconciler/managed"
	"github.com/crossplane/crossplane-runtime/v2/pkg/resource"

	"github.com/infrabay/provider-clickhouse/apis/v1alpha1"
	ch "github.com/infrabay/provider-clickhouse/internal/clients/clickhouse"
	"github.com/infrabay/provider-clickhouse/internal/clients/clickhouse/sqlx"
	"github.com/infrabay/provider-clickhouse/internal/controller/access"
	"github.com/infrabay/provider-clickhouse/internal/controller/access/accesstest"
	"github.com/infrabay/provider-clickhouse/internal/controller/grant"
	"github.com/infrabay/provider-clickhouse/internal/controller/providerconfig"
	"github.com/infrabay/provider-clickhouse/internal/reason"
)

// envRecordPrivileges makes TestGrantPrivilegeCatalogue (re)write the
// recorded catalogue of every image it runs on instead of comparing it.
const envRecordPrivileges = "XPCH_RECORD_PRIVILEGES"

// privilegesDir holds the recorded catalogues the unit tests use.
var privilegesDir = filepath.Join("..", "..", "internal", "clients", "clickhouse", "sqlx", "testdata", "privileges")

// TestGrantPrivilegeCatalogue checks that the catalogue the provider reads
// through the driver (with the minimal identity) equals the recorded
// testdata/privileges/<version>.tsv of the image, so that the unit tests of
// the privilege model run against what each server version really has
// (OQ-14). With XPCH_RECORD_PRIVILEGES=1 it records the file instead.
func TestGrantPrivilegeCatalogue(t *testing.T) {
	forEachAccessImage(t, accessPolicy(), func(t *testing.T, f *accessFixture) {
		ctx := context.Background()
		cl := f.client(t)
		privs, err := cl.Capabilities().Privileges(ctx, cl)
		if err != nil {
			t.Fatal(err)
		}
		version := cl.Info().Version
		path := filepath.Join(privilegesDir, version+".tsv")
		if os.Getenv(envRecordPrivileges) != "" {
			var b bytes.Buffer
			header := "system.privileges of ClickHouse " + version + " (" + f.image + "),\nread through the driver with clickhouse.QueryPrivileges by TestGrantPrivilegeCatalogue.\nprivilege, aliases (JSON), level, parent_group; NULL is empty."
			if err := sqlx.WritePrivilegesTSV(&b, header, privs); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil { //nolint:gosec // A test fixture in the source tree.
				t.Fatal(err)
			}
			t.Logf("recorded %d privileges into %s", len(privs), path)
			return
		}
		fh, err := os.Open(path) //nolint:gosec // Test fixture path.
		if err != nil {
			t.Fatalf("no recorded catalogue for %s (record it with %s=1): %v", version, envRecordPrivileges, err)
		}
		defer fh.Close()
		want, err := sqlx.ReadPrivilegesTSV(fh)
		if err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff(want, privs, cmpopts.EquateEmpty()); diff != "" {
			t.Errorf("system.privileges of %s differs from %s (-recorded +server):\n%s", version, path, diff)
		}
	})
}

// grantPolicy is accessPolicy with grant option allowed. Global grants stay
// forbidden; testGrantCeiling uses an unrestricted config for them.
func grantPolicy() v1alpha1.ClusterPolicy {
	p := accessPolicy()
	p.AllowGrantOption = true
	return p
}

// TestGrant runs the Grant controller against every image with the minimal,
// prefix-scoped identity, whose grant ceiling is SELECT, INSERT and SHOW
// TABLES ON app_*.* WITH GRANT OPTION (design 9.4). The scenarios follow
// design 10.2 (6): the lifecycle with drift, column grants, revoke-aware
// coverage and partial revokes, merged grants, the grant option cases of
// A17, deletion of covered Grants and of Grants with foreign privileges,
// wildcards (coverage on 26.8, Unsupported elsewhere), source privileges, the
// server-side ceiling, references, ambiguous and foreign grantees, and the
// expansion model against the server. Grants of bystanders, and of every
// grantee the provider must not touch, stay byte-identical.
func TestGrant(t *testing.T) {
	forEachAccessImage(t, grantPolicy(), func(t *testing.T, f *accessFixture) {
		g := newGrantFixture(t, f)
		bystander := g.bystander(t)
		t.Run("Lifecycle", func(t *testing.T) { testGrantLifecycle(t, g) })
		t.Run("Columns", func(t *testing.T) { testGrantColumns(t, g) })
		t.Run("Coverage", func(t *testing.T) { testGrantCoverage(t, g) })
		t.Run("Merged", func(t *testing.T) { testGrantMerged(t, g) })
		t.Run("GrantOption", func(t *testing.T) { testGrantOption(t, g) })
		t.Run("DeleteKeepsForeign", func(t *testing.T) { testGrantDeleteKeepsForeign(t, g) })
		t.Run("Wildcards", func(t *testing.T) { testGrantWildcards(t, g) })
		t.Run("Sources", func(t *testing.T) { testGrantSources(t, g) })
		t.Run("Ceiling", func(t *testing.T) { testGrantCeiling(t, g) })
		t.Run("References", func(t *testing.T) { testGrantReferences(t, g) })
		t.Run("Grantees", func(t *testing.T) { testGrantGrantees(t, g) })
		t.Run("Names", func(t *testing.T) { testGrantNames(t, g) })
		t.Run("ExpansionModel", func(t *testing.T) { testGrantExpansionModel(t, g) })
		t.Run("HiddenGrantOption", func(t *testing.T) { testGrantHiddenOption(t, g) })
		t.Run("HiddenGrantOptionModel", func(t *testing.T) { testGrantHiddenOptionModel(t, g) })
		t.Run("HiddenGrantOptionBroader", func(t *testing.T) { testGrantHiddenOptionBroader(t, g) })
		t.Run("PlanOrder", func(t *testing.T) { testGrantPlanOrder(t, g) })
		t.Run("UnrevocableForeign", func(t *testing.T) { testGrantUnrevocableForeign(t, g) })
		bystander.unchanged(t)
	})
}

// grantFixture drives Grants through the harness and the connector.
type grantFixture struct {
	*accessFixture
	conn *access.Connector[*v1alpha1.Grant]
	h    *mrHarness[*v1alpha1.Grant]
	rec  *eventRecorder
}

func newGrantFixture(t *testing.T, f *accessFixture) *grantFixture {
	t.Helper()
	conn := accessConnector(f, "Grant", grant.NewExternal, grant.References)
	rec := &eventRecorder{}
	conn.Recorder = rec
	// References are resolved with the generated resolver behind the
	// namespace check, as providerconfig.NewReferenceResolver does, but
	// persisted with an Update: crossplane-runtime persists them with a
	// server-side apply patch that names only the resolved fields, and the
	// fake API server then drops every other field of the spec (a real API
	// server keeps the fields that other managers own).
	refs := managed.ReferenceResolverFn(func(ctx context.Context, mg resource.Managed) error {
		g, ok := mg.(*v1alpha1.Grant)
		if !ok {
			return nil
		}
		if err := providerconfig.CheckReferenceNamespaces(grant.References(g), g.GetNamespace()); err != nil {
			return err
		}
		if err := g.ResolveReferences(ctx, f.kube); err != nil {
			return err
		}
		return f.kube.Update(ctx, g)
	})
	h := newAccessHarness(t, f, conn, func() *v1alpha1.Grant { return &v1alpha1.Grant{} }).withReferences(refs)
	return &grantFixture{accessFixture: f, conn: conn, h: h, rec: rec}
}

// newGrant stores a Grant MR that uses the fixture's config.
func (g *grantFixture) newGrant(name string, p v1alpha1.GrantParameters, policies ...xpv2.ManagementAction) {
	g.t.Helper()
	mr := &v1alpha1.Grant{ObjectMeta: metav1.ObjectMeta{Name: name}}
	mr.Spec.ProviderConfigReference = g.pc
	mr.Spec.ForProvider = p
	if len(policies) > 0 {
		mr.SetManagementPolicies(policies)
	}
	g.h.create(mr)
}

// observe runs Connect and Observe on the stored MR and returns the MR with
// the status and conditions Observe set (the harness does not persist
// status).
func (g *grantFixture) observe(t *testing.T, name string) (*v1alpha1.Grant, managed.ExternalObservation, error) {
	t.Helper()
	ctx := context.Background()
	mr := g.h.get(name)
	ext, err := g.conn.Connect(ctx, mr)
	if err != nil {
		return mr, managed.ExternalObservation{}, err
	}
	defer func() { _ = ext.Disconnect(ctx) }()
	obs, err := ext.Observe(ctx, mr)
	return mr, obs, err
}

// mustReconcile runs a reconcile and fails the test on an error.
func (g *grantFixture) mustReconcile(t *testing.T, name string) outcome {
	t.Helper()
	o := g.h.reconcile(name)
	if o.err != nil {
		t.Fatalf("reconcile %s: %s", name, o)
	}
	return o
}

// upToDate reconciles and requires the Grant to exist and be up to date,
// without writes.
func (g *grantFixture) upToDate(t *testing.T, name string) {
	t.Helper()
	if o := g.mustReconcile(t, name); !o.obs.ResourceExists || !o.obs.ResourceUpToDate || o.created || o.updated {
		t.Fatalf("%s is not up to date: %s", name, o)
	}
}

// deleteGrant deletes the MR and reconciles until it is gone. It returns
// whether Delete ran.
func (g *grantFixture) deleteGrant(t *testing.T, name string) bool {
	t.Helper()
	g.h.delete(name)
	deleted := false
	for range 3 {
		o := g.mustReconcile(t, name)
		deleted = deleted || o.deleted
		if o.gone {
			return deleted
		}
	}
	t.Fatalf("%s was not released", name)
	return false
}

// grantsOf returns the rows of system.grants of a role (or user) as the
// admin sees them, as "<access_type> <object>[ p][ g]", sorted.
func (s *server) grantsOf(t *testing.T, role bool, name string) []string {
	t.Helper()
	col := "user_name"
	if role {
		col = "role_name"
	}
	rows, err := s.admin.Query(chgo.Context(context.Background(), chgo.WithParameters(chgo.Parameters{"n": ch.ParamValue(name)})),
		"SELECT toString(access_type), ifNull(database, ''), ifNull(table, ''), ifNull(column, ''), is_partial_revoke, grant_option FROM system.grants WHERE "+col+" = {n:String}")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close() //nolint:errcheck // Test helper.
	var out []string
	for rows.Next() {
		var access, db, tbl, column string
		var revoke, option uint8
		if err := rows.Scan(&access, &db, &tbl, &column, &revoke, &option); err != nil {
			t.Fatal(err)
		}
		obj := "*.*"
		switch {
		case column != "":
			obj = db + "." + tbl + "(" + column + ")"
		case tbl != "":
			obj = db + "." + tbl
		case db != "":
			obj = db + ".*"
		}
		s := access + " " + obj
		if revoke == 1 {
			s += " p"
		}
		if option == 1 {
			s += " g"
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	slices.Sort(out)
	return out
}

func (g *grantFixture) roleGrants(t *testing.T, name string) []string {
	t.Helper()
	return g.srv.grantsOf(t, true, name)
}

func (g *grantFixture) expectRoleGrants(t *testing.T, name string, want ...string) {
	t.Helper()
	slices.Sort(want)
	if diff := cmp.Diff(want, g.roleGrants(t, name), cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("grants of role %s -want +got:\n%s", name, diff)
	}
}

func (g *grantFixture) showGrants(t *testing.T, name string) []string {
	t.Helper()
	return g.srv.rawStrings(t, "SHOW GRANTS FOR "+ch.QuoteIdent(name))
}

// snapshot is the SHOW GRANTS output of grantees that the provider must not
// touch.
type snapshot struct {
	g      *grantFixture
	before map[string][]string
}

func (g *grantFixture) snapshot(t *testing.T, names ...string) *snapshot {
	t.Helper()
	s := &snapshot{g: g, before: map[string][]string{}}
	for _, n := range names {
		s.before[n] = g.showGrants(t, n)
	}
	return s
}

func (s *snapshot) unchanged(t *testing.T) {
	t.Helper()
	for n, before := range s.before {
		if diff := cmp.Diff(before, s.g.showGrants(t, n)); diff != "" {
			t.Errorf("SHOW GRANTS FOR %s changed (-before +after):\n%s", n, diff)
		}
	}
}

// bystander creates a foreign role and a foreign user with grants on the
// targets the scenarios use, which must stay byte-identical.
func (g *grantFixture) bystander(t *testing.T) *snapshot {
	t.Helper()
	g.adminRole(t, "app_bystander")
	g.adminUser(t, "app_bystander_user")
	for _, stmt := range []string{
		"GRANT SELECT, INSERT ON app_db1.* TO app_bystander",
		"GRANT SELECT(a) ON app_db2.t TO app_bystander WITH GRANT OPTION",
		"GRANT SELECT ON app_db3.* TO app_bystander",
		"REVOKE SELECT ON app_db3.t FROM app_bystander",
		"GRANT SELECT ON app_db5.t TO app_bystander_user WITH GRANT OPTION",
		"GRANT SHOW TABLES ON *.* TO app_bystander_user",
	} {
		g.srv.exec(t, stmt)
	}
	return g.snapshot(t, "app_bystander", "app_bystander_user")
}

func roleGrantee(db string, privileges ...string) v1alpha1.GrantParameters {
	return v1alpha1.GrantParameters{Privileges: privileges, Database: new(db)}
}

func testGrantLifecycle(t *testing.T, g *grantFixture) {
	g.ownedRole(t, "reader1", "app_g1")
	p := roleGrantee("app_db1", "SELECT", "insert")
	p.Role = new("app_g1")
	g.newGrant("g1", p)

	if _, obs, err := g.observe(t, "g1"); err != nil || obs.ResourceExists {
		t.Fatalf("before create: %+v, %v", obs, err)
	}
	if o := g.mustReconcile(t, "g1"); !o.created {
		t.Fatalf("create: %s", o)
	}
	g.expectRoleGrants(t, "app_g1", "INSERT app_db1.*", "SELECT app_db1.*")
	g.upToDate(t, "g1")
	mr, obs, err := g.observe(t, "g1")
	if err != nil || !obs.ResourceUpToDate || mr.GetCondition(xpv2.TypeReady).Status != corev1.ConditionTrue {
		t.Fatalf("observe: %+v, %v, %+v", obs, err, mr.GetCondition(xpv2.TypeReady))
	}
	want := v1alpha1.GrantObservation{Grantee: "role:app_g1", Target: "app_db1.*", Privileges: []string{"INSERT", "SELECT"}}
	if diff := cmp.Diff(want, mr.Status.AtProvider); diff != "" {
		t.Errorf("status -want +got:\n%s", diff)
	}

	// Drift: a desired privilege revoked and a foreign one added at the
	// target, out of band.
	g.srv.exec(t, "REVOKE INSERT ON app_db1.* FROM app_g1")
	g.srv.exec(t, "GRANT SHOW TABLES ON app_db1.* TO app_g1")
	mr, obs, err = g.observe(t, "g1")
	if err != nil || !obs.ResourceExists || obs.ResourceUpToDate || mr.GetCondition(xpv2.TypeReady).Status != corev1.ConditionFalse {
		t.Fatalf("drift: %+v, %v, %+v", obs, err, mr.GetCondition(xpv2.TypeReady))
	}
	if o := g.mustReconcile(t, "g1"); !o.updated {
		t.Fatalf("update: %s", o)
	}
	g.expectRoleGrants(t, "app_g1", "INSERT app_db1.*", "SELECT app_db1.*")
	g.upToDate(t, "g1")

	if !g.deleteGrant(t, "g1") {
		t.Error("Delete did not run")
	}
	g.expectRoleGrants(t, "app_g1")
}

func testGrantColumns(t *testing.T, g *grantFixture) {
	g.ownedRole(t, "reader2", "app_g2")
	p := roleGrantee("app_db2", "SELECT")
	p.Table, p.Columns, p.Role = new("t"), []string{"b", "a"}, new("app_g2")
	g.newGrant("g2", p)
	if o := g.mustReconcile(t, "g2"); !o.created {
		t.Fatalf("create: %s", o)
	}
	g.expectRoleGrants(t, "app_g2", "SELECT app_db2.t(a)", "SELECT app_db2.t(b)")
	g.upToDate(t, "g2")

	// A foreign column privilege at the target is revoked; one on another
	// column is not.
	g.srv.exec(t, "REVOKE SELECT(a) ON app_db2.t FROM app_g2")
	g.srv.exec(t, "GRANT INSERT(b), INSERT(c) ON app_db2.t TO app_g2")
	if o := g.mustReconcile(t, "g2"); !o.updated {
		t.Fatalf("update: %s", o)
	}
	g.expectRoleGrants(t, "app_g2", "INSERT app_db2.t(c)", "SELECT app_db2.t(a)", "SELECT app_db2.t(b)")
	g.upToDate(t, "g2")

	g.deleteGrant(t, "g2")
	g.expectRoleGrants(t, "app_g2", "INSERT app_db2.t(c)")
}

// testGrantCoverage covers revoke-aware coverage (D-11): a partial revoke at
// the target is repaired by GRANT, a Grant covered by a broader grant is up
// to date and is released on deletion without revoking anything, and the
// provider never creates a partial revoke.
func testGrantCoverage(t *testing.T, g *grantFixture) {
	g.ownedRole(t, "reader3", "app_g3")
	g.srv.exec(t, "GRANT SELECT ON app_db3.* TO app_g3")
	g.srv.exec(t, "REVOKE SELECT ON app_db3.t FROM app_g3")
	g.expectRoleGrants(t, "app_g3", "SELECT app_db3.*", "SELECT app_db3.t p")

	narrow := roleGrantee("app_db3", "SELECT")
	narrow.Table, narrow.Role = new("t"), new("app_g3")
	g.newGrant("g3t", narrow)
	if _, obs, err := g.observe(t, "g3t"); err != nil || obs.ResourceExists {
		t.Fatalf("a partial revoke at the target: %+v, %v", obs, err)
	}
	if o := g.mustReconcile(t, "g3t"); !o.created {
		t.Fatalf("create: %s", o)
	}
	// GRANT at the target removed the partial revoke; no narrow row is
	// stored under the database row.
	g.expectRoleGrants(t, "app_g3", "SELECT app_db3.*")
	g.upToDate(t, "g3t")

	// The database-wide grant was made out of band; a Grant for it takes it
	// over and is up to date at once.
	broad := roleGrantee("app_db3", "SELECT")
	broad.Role = new("app_g3")
	g.newGrant("g3d", broad)
	g.upToDate(t, "g3d")

	// The narrow Grant is covered: deleting it revokes nothing.
	if g.deleteGrant(t, "g3t") {
		t.Error("deleting a covered Grant must not revoke")
	}
	g.expectRoleGrants(t, "app_g3", "SELECT app_db3.*")
	g.deleteGrant(t, "g3d")
	g.expectRoleGrants(t, "app_g3")
}

// testGrantMerged: the server merges a narrow grant into a broader one; the
// narrow Grant stays up to date and is released without revoking.
func testGrantMerged(t *testing.T, g *grantFixture) {
	g.ownedRole(t, "reader4", "app_g4")
	narrow := roleGrantee("app_db4", "SELECT")
	narrow.Table, narrow.Role = new("t"), new("app_g4")
	g.newGrant("g4t", narrow)
	g.mustReconcile(t, "g4t")
	g.expectRoleGrants(t, "app_g4", "SELECT app_db4.t")

	broad := roleGrantee("app_db4", "SELECT")
	broad.Role = new("app_g4")
	g.newGrant("g4d", broad)
	if o := g.mustReconcile(t, "g4d"); !o.created {
		t.Fatalf("create: %s", o)
	}
	g.expectRoleGrants(t, "app_g4", "SELECT app_db4.*")
	g.upToDate(t, "g4t")
	g.upToDate(t, "g4d")

	if g.deleteGrant(t, "g4t") {
		t.Error("deleting an absorbed Grant must not revoke")
	}
	g.expectRoleGrants(t, "app_g4", "SELECT app_db4.*")
	g.deleteGrant(t, "g4d")
	g.expectRoleGrants(t, "app_g4")
}

// testGrantOption covers the grant option cases of A17.
func testGrantOption(t *testing.T, g *grantFixture) {
	g.ownedRole(t, "reader5", "app_g5")
	g.srv.exec(t, "GRANT SELECT ON app_db5.* TO app_g5")

	// Re-granting with option under a broader row without it stores a
	// separate narrow row.
	p := roleGrantee("app_db5", "SELECT")
	p.Table, p.Role, p.WithGrantOption = new("t"), new("app_g5"), new(true)
	g.newGrant("g5", p)
	if o := g.mustReconcile(t, "g5"); !o.updated {
		t.Fatalf("grant option: %s", o)
	}
	g.expectRoleGrants(t, "app_g5", "SELECT app_db5.*", "SELECT app_db5.t g")
	g.upToDate(t, "g5")

	// Dropping the option removes the narrow row cleanly.
	g.h.mutate("g5", func(mr *v1alpha1.Grant) { mr.Spec.ForProvider.WithGrantOption = new(false) })
	if o := g.mustReconcile(t, "g5"); !o.updated {
		t.Fatalf("drop the option: %s", o)
	}
	g.expectRoleGrants(t, "app_g5", "SELECT app_db5.*")
	g.upToDate(t, "g5")

	// An option a broader row holds is left alone: REVOKE GRANT OPTION FOR
	// at the target would create a partial revoke.
	g.srv.exec(t, "GRANT SELECT ON app_db5.* TO app_g5 WITH GRANT OPTION")
	g.upToDate(t, "g5")
	g.expectRoleGrants(t, "app_g5", "SELECT app_db5.* g")

	// A partial revoke of the option at the target: a plain GRANT would keep
	// it, GRANT ... WITH GRANT OPTION removes it.
	g.srv.exec(t, "REVOKE GRANT OPTION FOR SELECT ON app_db5.t FROM app_g5")
	g.expectRoleGrants(t, "app_g5", "SELECT app_db5.* g", "SELECT app_db5.t p g")
	g.upToDate(t, "g5")
	g.h.mutate("g5", func(mr *v1alpha1.Grant) { mr.Spec.ForProvider.WithGrantOption = new(true) })
	if o := g.mustReconcile(t, "g5"); !o.updated {
		t.Fatalf("repair the option: %s", o)
	}
	g.expectRoleGrants(t, "app_g5", "SELECT app_db5.* g")
	g.upToDate(t, "g5")

	// Covered by the broader row with option: released without revoking.
	if g.deleteGrant(t, "g5") {
		t.Error("deleting a covered Grant must not revoke")
	}
	g.expectRoleGrants(t, "app_g5", "SELECT app_db5.* g")
}

// testGrantDeleteKeepsForeign: Delete revokes only the desired privileges;
// a foreign privilege at the same target stays, and the MR is released.
func testGrantDeleteKeepsForeign(t *testing.T, g *grantFixture) {
	g.ownedRole(t, "reader6", "app_g6")
	p := roleGrantee("app_db6", "SELECT")
	p.Role = new("app_g6")
	g.newGrant("g6", p)
	g.mustReconcile(t, "g6")
	g.srv.exec(t, "GRANT INSERT ON app_db6.* TO app_g6")
	if !g.deleteGrant(t, "g6") {
		t.Error("Delete did not run")
	}
	g.expectRoleGrants(t, "app_g6", "INSERT app_db6.*")

	// A Grant whose target holds only foreign privileges is released at
	// once.
	g.newGrant("g6b", p)
	if g.deleteGrant(t, "g6b") {
		t.Error("nothing of this Grant was granted, nothing may be revoked")
	}
	g.expectRoleGrants(t, "app_g6", "INSERT app_db6.*")
}

// testGrantWildcards: on 26.8 a wildcard grant covers the target; elsewhere
// a grantee with wildcard grants is Unsupported and is never written, also
// while deleting.
func testGrantWildcards(t *testing.T, g *grantFixture) {
	g.ownedRole(t, "reader7", "app_g7")
	g.srv.exec(t, "GRANT SELECT ON app_w*.* TO app_g7")
	snap := g.snapshot(t, "app_g7")
	p := roleGrantee("app_wx", "SELECT")
	p.Role = new("app_g7")
	g.newGrant("g7", p)

	cl := g.client(t)
	isWildcard, err := cl.Capabilities().GrantsIsWildcard(context.Background(), cl)
	if err != nil {
		t.Fatal(err)
	}
	mr, obs, err := g.observe(t, "g7")
	if isWildcard {
		if err != nil || !obs.ResourceExists || !obs.ResourceUpToDate {
			t.Fatalf("a wildcard grant covers the target on %s: %+v, %v", g.image, obs, err)
		}
		g.upToDate(t, "g7")
	} else {
		if !reason.Is(err, reason.Unsupported) || !strings.Contains(err.Error(), "app_w*.*") || mr.GetCondition(xpv2.TypeReady).Status != corev1.ConditionFalse {
			t.Fatalf("wildcard grants without is_wildcard on %s: %v, %+v", g.image, err, mr.GetCondition(xpv2.TypeReady))
		}
		if o := g.h.reconcile("g7"); !reason.Is(o.err, reason.Unsupported) || o.created || o.updated {
			t.Fatalf("reconcile: %s", o)
		}
	}
	before := len(g.rec.reasons())
	if g.deleteGrant(t, "g7") {
		t.Error("deleting a Grant covered by a wildcard must not revoke")
	}
	if !isWildcard && !slices.Contains(g.rec.reasons()[before:], "Orphaned") {
		t.Errorf("events %v lack Orphaned", g.rec.reasons()[before:])
	}
	snap.unchanged(t)
}

// testGrantSources: source privileges are rejected in a spec and never
// counted or revoked; prefix-scoped user DDL is ignored too.
func testGrantSources(t *testing.T, g *grantFixture) {
	g.ownedRole(t, "reader8", "app_g8")
	g.srv.exec(t, "GRANT SELECT ON *.* TO app_g8")
	g.srv.exec(t, "GRANT S3, URL ON *.* TO app_g8")
	g.srv.exec(t, "GRANT CREATE USER ON app_svc_* TO app_g8")
	snap := g.snapshot(t, "app_g8")

	// An observe-only global Grant sees SELECT and nothing else at *.*.
	p := roleGrantee("*", "SELECT")
	p.Role = new("app_g8")
	g.newGrant("g8", p, xpv2.ManagementActionObserve)
	mr, obs, err := g.observe(t, "g8")
	if err != nil || !obs.ResourceExists || !obs.ResourceUpToDate {
		t.Fatalf("observe-only global Grant: %+v, %v", obs, err)
	}
	if diff := cmp.Diff([]string{"SELECT"}, mr.Status.AtProvider.Privileges); diff != "" {
		t.Errorf("status privileges -want +got:\n%s", diff)
	}
	g.deleteGrant(t, "g8")

	// A source privilege in a spec is InvalidSpec before any write.
	s3 := roleGrantee("app_db8", "SELECT", "S3")
	s3.Role = new("app_g8")
	g.newGrant("g8s", s3)
	if o := g.h.reconcile("g8s"); !reason.Is(o.err, reason.InvalidSpec) || o.created {
		t.Fatalf("source privilege: %s", o)
	}
	g.deleteGrant(t, "g8s")
	snap.unchanged(t)
}

// testGrantCeiling: what the policy and the provider refuse never reaches the
// server, and the server refuses what the identity may not grant (497).
func testGrantCeiling(t *testing.T, g *grantFixture) {
	ctx := context.Background()
	g.ownedRole(t, "reader9", "app_g9")
	snap := g.snapshot(t, "app_g9")
	cases := map[string]struct {
		p      v1alpha1.GrantParameters
		reason reason.Reason
	}{
		// Server-side ceiling: the identity holds ALTER UPDATE without grant
		// option nowhere.
		"NotDelegable": {p: roleGrantee("app_db9", "ALTER UPDATE"), reason: reason.PermissionDenied},
		// Provider-side: a privilege at the wrong level (509 on the server).
		"WrongLevel": {p: func() v1alpha1.GrantParameters {
			p := roleGrantee("app_db9", "CREATE DATABASE")
			p.Table = new("t")
			return p
		}(), reason: reason.InvalidSpec},
		"OutsidePolicy": {p: roleGrantee("zz_db9", "SELECT"), reason: reason.PolicyViolation},
		"Global":        {p: roleGrantee("*", "SELECT"), reason: reason.PolicyViolation},
	}
	for name, tc := range cases {
		tc.p.Role = new("app_g9")
		mrName := "g9-" + strings.ToLower(name)
		g.newGrant(mrName, tc.p)
		if o := g.h.reconcile(mrName); !reason.Is(o.err, tc.reason) || o.created || o.updated {
			t.Errorf("%s: %s, want %s", name, o, tc.reason)
		}
		g.deleteGrant(t, mrName)
	}

	// Through an unrestricted config the policy allows a target outside
	// app_*, and global grants; the server's ceiling still refuses them.
	open := &v1alpha1.ClusterProviderConfig{}
	if err := g.kube.Get(ctx, client.ObjectKey{Name: g.pc.Name}, open); err != nil {
		t.Fatal(err)
	}
	open = open.DeepCopy()
	open.ObjectMeta = metav1.ObjectMeta{Name: "access-grant-open", UID: types.UID("cpc-access-grant-open")}
	open.Spec.Policy = v1alpha1.ClusterPolicy{Unrestricted: true}
	if err := g.kube.Create(ctx, open); err != nil {
		t.Fatal(err)
	}
	openPC := &xpv2.ProviderConfigReference{Kind: v1alpha1.ClusterProviderConfigKind, Name: open.Name}
	id, _, _ := g.srv.entity(t, access.Role, "app_g9")
	if err := g.kube.Create(ctx, accesstest.OwnedRole(namespace, "reader9-open", "app_g9", id, openPC)); err != nil {
		t.Fatal(err)
	}
	for name, db := range map[string]string{"outside": "zz_db9", "global": "*"} {
		mr := &v1alpha1.Grant{ObjectMeta: metav1.ObjectMeta{Name: "g9-open-" + name}}
		mr.Spec.ProviderConfigReference = openPC
		mr.Spec.ForProvider = roleGrantee(db, "SELECT")
		mr.Spec.ForProvider.Role = new("app_g9")
		g.h.create(mr)
		if o := g.h.reconcile(mr.Name); !reason.Is(o.err, reason.PermissionDenied) || o.created {
			t.Errorf("%s through an unrestricted config: %s, want PermissionDenied (497)", name, o)
		}
		g.deleteGrant(t, mr.Name)
	}
	snap.unchanged(t)
}

// testGrantReferences: databaseRef and roleRef resolve to the referenced MRs'
// ClickHouse names; a referenced MR of another config is refused.
func testGrantReferences(t *testing.T, g *grantFixture) {
	ctx := context.Background()
	g.ownedRole(t, "reader10", "app_g10")
	db := &v1alpha1.Database{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "analytics"}}
	db.Spec.ProviderConfigReference = g.pc
	db.Spec.ForProvider.Name = new("app_db10")
	meta.SetExternalName(db, "app_db10")
	other := db.DeepCopy()
	other.Name = "other"
	other.Spec.ProviderConfigReference = &xpv2.ProviderConfigReference{Kind: v1alpha1.ClusterProviderConfigKind, Name: "elsewhere"}
	for _, o := range []client.Object{db, other} {
		if err := g.kube.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	p := v1alpha1.GrantParameters{Privileges: []string{"SELECT"}, DatabaseRef: &xpv2.NamespacedReference{Name: "analytics"}, RoleRef: &xpv2.NamespacedReference{Name: "reader10"}}
	g.newGrant("g10", p)
	if o := g.mustReconcile(t, "g10"); !o.created {
		t.Fatalf("create with references: %s", o)
	}
	g.expectRoleGrants(t, "app_g10", "SELECT app_db10.*")
	g.deleteGrant(t, "g10")
	g.expectRoleGrants(t, "app_g10")

	p.DatabaseRef = &xpv2.NamespacedReference{Name: "other"}
	g.newGrant("g10-other", p)
	if o := g.h.reconcile("g10-other"); !reason.Is(o.err, reason.InvalidSpec) || o.created {
		t.Fatalf("a referenced MR of another config: %s", o)
	}
	g.deleteGrant(t, "g10-other")
	g.expectRoleGrants(t, "app_g10")
}

// testGrantGrantees: a user grantee; and grantees the provider must never
// write: ambiguous, foreign, replaced out of band, claimed by a copied
// manifest, and its own identity.
func testGrantGrantees(t *testing.T, g *grantFixture) {
	ctx := context.Background()
	g.ownedUser(t, "api11", "app_u11")
	p := roleGrantee("app_db11", "SELECT")
	p.User = new("app_u11")
	g.newGrant("g11", p)
	if o := g.mustReconcile(t, "g11"); !o.created {
		t.Fatalf("user grantee: %s", o)
	}
	if diff := cmp.Diff([]string{"SELECT app_db11.*"}, g.srv.grantsOf(t, false, "app_u11")); diff != "" {
		t.Errorf("grants of user app_u11: %s", diff)
	}
	g.deleteGrant(t, "g11")

	// A role shadowed by a user of the same name.
	g.ownedRole(t, "twin", "app_twin11")
	g.adminUser(t, "app_twin11")
	g.srv.exec(t, "GRANT SELECT ON app_db11.* TO app_twin11")
	foreignRole := "app_foreign11"
	g.adminRole(t, foreignRole)
	g.srv.exec(t, "GRANT SELECT, INSERT ON app_db11.* TO "+foreignRole)
	replaced := "app_replaced11"
	g.ownedRole(t, "replaced", replaced)
	g.srv.exec(t, "DROP ROLE "+replaced)
	g.adminRole(t, replaced)
	g.srv.exec(t, "GRANT INSERT ON app_db11.* TO "+replaced)
	copied := "app_copied11"
	cid := g.adminRole(t, copied)
	copiedMR := accesstest.OwnedRole(namespace, "copied", copied, cid, g.pc)
	meta.AddAnnotations(copiedMR, map[string]string{v1alpha1.AnnotationOwnerUID: "uid-of-the-original"})
	if err := g.kube.Create(ctx, copiedMR); err != nil {
		t.Fatal(err)
	}
	snap := g.snapshot(t, "app_twin11", foreignRole, replaced, copied, g.identity)

	cases := map[string]struct {
		p      v1alpha1.GrantParameters
		reason reason.Reason
	}{
		"Ambiguous":      {p: v1alpha1.GrantParameters{Role: new("app_twin11")}, reason: reason.AmbiguousGrantee},
		"Foreign":        {p: v1alpha1.GrantParameters{Role: new(foreignRole)}, reason: reason.NotOwned},
		"ReplacedOOB":    {p: v1alpha1.GrantParameters{Role: new(replaced)}, reason: reason.NotOwned},
		"CopiedManifest": {p: v1alpha1.GrantParameters{Role: new(copied)}, reason: reason.NotOwned},
		"OwnIdentity":    {p: v1alpha1.GrantParameters{User: new(g.identity)}, reason: reason.ReadOnlyStorage},
	}
	for name, tc := range cases {
		tc.p.Privileges, tc.p.Database = []string{"SELECT"}, new("app_db11")
		mrName := "g11-" + strings.ToLower(name)
		g.newGrant(mrName, tc.p)
		if o := g.h.reconcile(mrName); !reason.Is(o.err, tc.reason) || o.created || o.updated {
			t.Errorf("%s: %s, want %s", name, o, tc.reason)
		}
		before := len(g.rec.reasons())
		if g.deleteGrant(t, mrName) {
			t.Errorf("%s: Delete ran for a grantee the provider must not write", name)
		}
		if !slices.Contains(g.rec.reasons()[before:], "Orphaned") {
			t.Errorf("%s: events %v lack Orphaned", name, g.rec.reasons()[before:])
		}
	}
	snap.unchanged(t)
}

// testGrantNames sends adversarial names through every part of a Grant:
// grantee, database, table and columns, with '@', '.', quotes, a backslash,
// a backtick, '*', a space and Unicode. A '*' inside a name is quoted and
// therefore literal: it is neither a wildcard nor reported as one.
func testGrantNames(t *testing.T, g *grantFixture) {
	role := "app_r@le.'q`\\ü*"
	db := "app_d@b.'q`\\ü*"
	table := "t.'`\\*ü"
	cols := []string{"c 1", "c'2`\\*"}
	g.ownedRole(t, "weird", role)
	p := roleGrantee(db, "SELECT", "INSERT")
	p.Table, p.Columns, p.Role = new(table), cols, new(role)
	g.newGrant("g12", p)
	if o := g.mustReconcile(t, "g12"); !o.created {
		t.Fatalf("create: %s", o)
	}
	var want []string
	for _, priv := range []string{"INSERT", "SELECT"} {
		for _, c := range cols {
			want = append(want, priv+" "+db+"."+table+"("+c+")")
		}
	}
	g.expectRoleGrants(t, role, want...)
	g.upToDate(t, "g12")
	mr, _, err := g.observe(t, "g12")
	if err != nil || mr.Status.AtProvider.Target != db+"."+table+"(c 1, c'2`\\*)" {
		t.Errorf("status target %q, %v", mr.Status.AtProvider.Target, err)
	}
	g.deleteGrant(t, "g12")
	g.expectRoleGrants(t, role)
}

// testGrantExpansionModel compares the expansion model with what the server
// stores for GRANTs made by the admin, read back with the provider's own
// catalogue (design 5.6, OQ-14).
func testGrantExpansionModel(t *testing.T, g *grantFixture) {
	ctx := context.Background()
	cl := g.client(t)
	privs, err := cl.Capabilities().Privileges(ctx, cl)
	if err != nil {
		t.Fatal(err)
	}
	cat, err := sqlx.NewCatalogue(privs)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		privilege string
		on        string
		level     sqlx.TargetLevel
	}{
		{"ALL", "app_x.*", sqlx.TargetDatabase},
		{"ALL", "app_x.t", sqlx.TargetTable},
		{"ALTER", "app_x.*", sqlx.TargetDatabase},
		{"CREATE", "app_x.*", sqlx.TargetDatabase},
		{"DROP", "app_x.*", sqlx.TargetDatabase},
		{"SYSTEM", "app_x.*", sqlx.TargetDatabase},
		{"SHOW", "app_x.t", sqlx.TargetTable},
		{"dictGet", "app_x.t", sqlx.TargetTable},
		{"SELECT", "app_x.t(a)", sqlx.TargetColumn},
		{"SHOW", "*.*", sqlx.TargetGlobal},
		{"SYSTEM", "*.*", sqlx.TargetGlobal},
		{"INTROSPECTION", "*.*", sqlx.TargetGlobal},
		{"CREATE", "*.*", sqlx.TargetGlobal},
		{"DROP", "*.*", sqlx.TargetGlobal},
		{"ALTER", "*.*", sqlx.TargetGlobal},
	}
	for i, tc := range cases {
		role := "app_model" + strings.Repeat("x", i)
		g.adminRole(t, role)
		priv := tc.privilege
		on := tc.on
		if tc.level == sqlx.TargetColumn {
			priv, on = tc.privilege+"(a)", "app_x.t"
		}
		g.srv.exec(t, "GRANT "+priv+" ON "+on+" TO "+role)
		want, err := cat.Expand([]string{tc.privilege}, tc.level)
		if err != nil {
			t.Fatalf("%s ON %s: %v", tc.privilege, tc.on, err)
		}
		got := sqlx.PrivilegeSet{}
		for _, row := range g.roleGrants(t, role) {
			access := row[:strings.LastIndex(row, " ")]
			for _, l := range cat.RowFlags(access, tc.level) {
				got[l] = struct{}{}
			}
		}
		if diff := cmp.Diff(want.Sorted(), got.Sorted()); diff != "" {
			t.Errorf("GRANT %s ON %s on %s (-model +server):\n%s", tc.privilege, tc.on, g.image, diff)
		}

		// The statements the provider builds from the expansion parse on
		// the server (every privilege name is written unquoted) and store
		// the same flags; revoking them leaves nothing.
		target := sqlx.GrantTarget{Database: "*"}
		switch tc.level {
		case sqlx.TargetDatabase:
			target = sqlx.GrantTarget{Database: "app_x"}
		case sqlx.TargetTable:
			target = sqlx.GrantTarget{Database: "app_x", Table: "t"}
		case sqlx.TargetColumn:
			target = sqlx.GrantTarget{Database: "app_x", Table: "t", Columns: []string{"a"}}
		}
		built := role + "_built"
		g.adminRole(t, built)
		stmt, err := sqlx.GrantStmt(want.Sorted(), target, built, false)
		if err != nil {
			t.Fatal(err)
		}
		g.srv.exec(t, stmt)
		if diff := cmp.Diff(g.roleGrants(t, role), g.roleGrants(t, built)); diff != "" {
			t.Errorf("%s stores other rows than GRANT %s ON %s (-admin +built):\n%s", stmt, tc.privilege, tc.on, diff)
		}
		revs := make([]sqlx.Revocation, 0, len(want))
		for _, l := range want.Sorted() {
			revs = append(revs, sqlx.Revocation{Privilege: l, Columns: target.Columns})
		}
		revoke, err := sqlx.RevokeStmt(revs, target, built, false)
		if err != nil {
			t.Fatal(err)
		}
		g.srv.exec(t, revoke)
		g.expectRoleGrants(t, built)
	}
	// ALL on *.* is rejected by the model; the admin cannot grant it either
	// (it lacks NAMED COLLECTION ADMIN).
	if _, err := cat.Expand([]string{"ALL"}, sqlx.TargetGlobal); err == nil {
		t.Error("ALL on *.* must be rejected")
	}
}

// delegates reports whether a user holding only the role can grant priv ON
// on to another user, that is whether the role holds priv WITH GRANT OPTION
// there. The grant it makes is revoked again.
func (g *grantFixture) delegates(t *testing.T, role, priv, on string) bool {
	t.Helper()
	member, other := role+"_member", role+"_other"
	if _, _, ok := g.srv.entity(t, access.User, member); !ok {
		g.srv.exec(t, "CREATE USER "+member+" IDENTIFIED WITH sha256_hash BY "+ch.QuoteString(testHash)+" SALT "+ch.QuoteString(testSalt)+" DEFAULT ROLE ALL")
		g.srv.exec(t, "GRANT "+role+" TO "+member)
		g.srv.exec(t, "CREATE USER "+other+" IDENTIFIED WITH sha256_hash BY "+ch.QuoteString(testHash)+" SALT "+ch.QuoteString(testSalt))
	}
	conn, err := ch.OpenDriver(ch.Config{Host: g.srv.host, Port: g.srv.nativePort, Protocol: ch.ProtocolNative, Username: member, Password: testPassword, MaxOpenConns: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close() //nolint:errcheck // Test cleanup.
	err = conn.Exec(context.Background(), "GRANT "+priv+" ON "+on+" TO "+other)
	if err == nil {
		g.srv.exec(t, "REVOKE "+priv+" ON "+on+" FROM "+other)
		return true
	}
	if !ch.IsCode(err, ch.CodeAccessDenied) {
		t.Fatalf("delegation as %s: %v", member, err)
	}
	return false
}

// testGrantHiddenOption: the server lists a grant option on app_db12.events
// nowhere while the role also holds SELECT ON app_db12.* and a grant on
// app_db12.events_raw [verified on every supported version]. The Grant with
// withGrantOption grants it once and records it, instead of granting again
// on every reconcile; turning the option off and deleting the Grant revoke
// it, although no row shows it (D-35). Delegation as a member of the role
// proves what the rows do not show.
func testGrantHiddenOption(t *testing.T, g *grantFixture) {
	g.ownedRole(t, "reader12", "app_g12")
	db := roleGrantee("app_db12", "SELECT")
	db.Role = new("app_g12")
	g.newGrant("g12-db", db)
	g.mustReconcile(t, "g12-db")
	raw := roleGrantee("app_db12", "INSERT")
	raw.Table, raw.Role = new("events_raw"), new("app_g12")
	g.newGrant("g12-raw", raw)
	g.mustReconcile(t, "g12-raw")

	p := roleGrantee("app_db12", "SELECT")
	p.Table, p.Role, p.WithGrantOption = new("events"), new("app_g12"), new(true)
	g.newGrant("g12", p)
	if o := g.mustReconcile(t, "g12"); !o.updated {
		t.Fatalf("grant the option: %s", o)
	}
	g.expectRoleGrants(t, "app_g12", "SELECT app_db12.*", "INSERT app_db12.events_raw")
	if !g.delegates(t, "app_g12", "SELECT", "app_db12.events") {
		t.Fatal("the role must hold the grant option that the server does not list")
	}
	if ann := g.h.get("g12").GetAnnotations()[v1alpha1.AnnotationGrantOption]; !strings.Contains(ann, `"privileges":["SELECT"]`) {
		t.Fatalf("record = %q", ann)
	}
	g.upToDate(t, "g12")
	g.upToDate(t, "g12")

	// Turned off: revoked although no row shows it.
	g.h.mutate("g12", func(mr *v1alpha1.Grant) { mr.Spec.ForProvider.WithGrantOption = new(false) })
	if o := g.mustReconcile(t, "g12"); !o.updated {
		t.Fatalf("turn the option off: %s", o)
	}
	if g.delegates(t, "app_g12", "SELECT", "app_db12.events") {
		t.Error("the grant option must be revoked")
	}
	g.upToDate(t, "g12")
	g.expectRoleGrants(t, "app_g12", "SELECT app_db12.*", "INSERT app_db12.events_raw")

	// Deleted: revoked, then released.
	g.h.mutate("g12", func(mr *v1alpha1.Grant) { mr.Spec.ForProvider.WithGrantOption = new(true) })
	if o := g.mustReconcile(t, "g12"); !o.updated {
		t.Fatalf("turn the option on again: %s", o)
	}
	if !g.delegates(t, "app_g12", "SELECT", "app_db12.events") {
		t.Fatal("the role must hold the grant option again")
	}
	if !g.deleteGrant(t, "g12") {
		t.Error("Delete must revoke the grant option of record")
	}
	if g.delegates(t, "app_g12", "SELECT", "app_db12.events") {
		t.Error("the grant option must be revoked when the Grant is deleted")
	}
	g.expectRoleGrants(t, "app_g12", "SELECT app_db12.*", "INSERT app_db12.events_raw")
	g.deleteGrant(t, "g12-raw")
	g.deleteGrant(t, "g12-db")
	g.expectRoleGrants(t, "app_g12")
}

// testGrantHiddenOptionBroader: the server hides the grant option at
// app_db15.events (a sibling events_raw), and another Grant of the role
// turns the option on at app_db15.* and off again. Its REVOKE GRANT OPTION
// FOR also removes the option at app_db15.events, as delegation shows. The
// Grant on app_db15.events forgets its record while the broader option is
// shown, also without a statement, so it grants its own option again once
// the broader one is gone; the same when it turns its option off under the
// broader one and on again after that is gone (D-35).
func testGrantHiddenOptionBroader(t *testing.T, g *grantFixture) {
	role := "app_g15"
	g.ownedRole(t, "reader15", role)
	raw := roleGrantee("app_db15", "INSERT")
	raw.Table, raw.Role = new("events_raw"), new(role)
	g.newGrant("g15-raw", raw)
	g.mustReconcile(t, "g15-raw")
	db := roleGrantee("app_db15", "SELECT")
	db.Role = new(role)
	g.newGrant("g15-db", db)
	g.mustReconcile(t, "g15-db")
	p := roleGrantee("app_db15", "SELECT")
	p.Table, p.Role, p.WithGrantOption = new("events"), new(role), new(true)
	g.newGrant("g15", p)
	if o := g.mustReconcile(t, "g15"); !o.updated {
		t.Fatalf("grant the option: %s", o)
	}
	setOption := func(name string, on bool) {
		t.Helper()
		g.h.mutate(name, func(mr *v1alpha1.Grant) { mr.Spec.ForProvider.WithGrantOption = new(on) })
		g.mustReconcile(t, name)
	}
	record := func() string { return g.h.get("g15").GetAnnotations()[v1alpha1.AnnotationGrantOption] }
	delegates := func() bool { return g.delegates(t, role, "SELECT", "app_db15.events") }
	if !delegates() || record() == "" {
		t.Fatalf("the role must hold the option, recorded: %q", record())
	}

	// The broader option is shown: the record is forgotten.
	setOption("g15-db", true)
	g.upToDate(t, "g15")
	if record() != "" {
		t.Errorf("the record of an option that a broader shown one covers must be forgotten: %q", record())
	}
	// It is revoked, and the option at the target with it.
	setOption("g15-db", false)
	if delegates() {
		t.Fatal("REVOKE GRANT OPTION FOR SELECT ON app_db15.* must remove the option below it")
	}
	if o := g.mustReconcile(t, "g15"); !o.updated || !delegates() {
		t.Fatalf("the option must be granted again: %s", o)
	}
	g.upToDate(t, "g15")

	// Turned off under the broader option, which is then revoked; turned on
	// again.
	setOption("g15-db", true)
	g.h.mutate("g15", func(mr *v1alpha1.Grant) { mr.Spec.ForProvider.WithGrantOption = new(false) })
	g.upToDate(t, "g15")
	if record() != "" {
		t.Errorf("the record must be dropped although no statement ran: %q", record())
	}
	setOption("g15-db", false)
	g.h.mutate("g15", func(mr *v1alpha1.Grant) { mr.Spec.ForProvider.WithGrantOption = new(true) })
	if o := g.mustReconcile(t, "g15"); !o.updated || !delegates() {
		t.Fatalf("turned on again: %s", o)
	}
	g.upToDate(t, "g15")

	for _, n := range []string{"g15", "g15-db", "g15-raw"} {
		g.deleteGrant(t, n)
	}
	if delegates() {
		t.Error("the option must be revoked with the Grant")
	}
	g.expectRoleGrants(t, role)
}

// testGrantUnrevocableForeign: a foreign privilege at the target that the
// identity cannot revoke (it holds ALTER UPDATE nowhere WITH GRANT OPTION)
// does not keep the desired privilege from being granted. The refused
// REVOKE changes nothing and is reported as PermissionDenied naming the
// grant the identity lacks; once an administrator revokes the privilege,
// the Grant is up to date (design 9.4).
func testGrantUnrevocableForeign(t *testing.T, g *grantFixture) {
	g.ownedRole(t, "reader14", "app_g14")
	g.srv.exec(t, "GRANT ALTER UPDATE ON app_db14.* TO app_g14")
	p := roleGrantee("app_db14", "SELECT")
	p.Role = new("app_g14")
	g.newGrant("g14", p)
	for i := range 2 {
		o := g.h.reconcile("g14")
		if !reason.Is(o.err, reason.PermissionDenied) || !strings.Contains(o.err.Error(), "ALTER UPDATE ON app_db14.* WITH GRANT OPTION") {
			t.Fatalf("reconcile %d: %s", i+1, o)
		}
		g.expectRoleGrants(t, "app_g14", "ALTER UPDATE app_db14.*", "SELECT app_db14.*")
	}
	// The role holds what the Grant asks for: Ready, but not up to date.
	mr, obs, err := g.observe(t, "g14")
	if err != nil || !obs.ResourceExists || obs.ResourceUpToDate || mr.GetCondition(xpv2.TypeReady).Status != corev1.ConditionTrue {
		t.Fatalf("observe: %+v, %v, %+v", obs, err, mr.GetCondition(xpv2.TypeReady))
	}
	g.srv.exec(t, "REVOKE ALTER UPDATE ON app_db14.* FROM app_g14")
	g.upToDate(t, "g14")
	g.deleteGrant(t, "g14")
	g.expectRoleGrants(t, "app_g14")
}

// testGrantHiddenOptionModel checks the analysis against the server: for
// grant options the server does not list, it must report that the server
// may hide them (sqlx.Analysis.Hidden). The statements are run by the
// administrator; the shapes are those of design 5.6.
func testGrantHiddenOptionModel(t *testing.T, g *grantFixture) {
	ctx := context.Background()
	cl := g.client(t)
	privs, err := cl.Capabilities().Privileges(ctx, cl)
	if err != nil {
		t.Fatal(err)
	}
	cat, err := sqlx.NewCatalogue(privs)
	if err != nil {
		t.Fatal(err)
	}
	cols := sqlx.GrantColumns{}
	if cols.AccessObject, err = cl.Capabilities().GrantsAccessObject(ctx, cl); err != nil {
		t.Fatal(err)
	}
	if cols.IsWildcard, err = cl.Capabilities().GrantsIsWildcard(ctx, cl); err != nil {
		t.Fatal(err)
	}
	events := sqlx.GrantTarget{Database: "app_m", Table: "events"}
	teamA := sqlx.GrantTarget{Database: "app_ma"}
	for i, tc := range []struct {
		stmts  []string
		target sqlx.GrantTarget
		hidden bool // the server lists no grant option row at the target
	}{
		{[]string{"GRANT SELECT ON app_m.* TO %s", "GRANT SELECT ON app_m.events TO %s WITH GRANT OPTION", "GRANT INSERT ON app_m.events_raw TO %s"}, events, true},
		{[]string{"GRANT SELECT ON app_m.* TO %s", "GRANT SELECT ON app_m.events TO %s WITH GRANT OPTION", "GRANT INSERT ON app_m.evil TO %s"}, events, true},
		{[]string{"GRANT SELECT ON app_m.* TO %s", "GRANT SELECT ON app_m.events TO %s WITH GRANT OPTION", "GRANT INSERT ON app_m.evil TO %s", "GRANT INSERT ON app_m.evx TO %s"}, events, false},
		{[]string{"GRANT SELECT ON app_m.* TO %s", "GRANT SELECT ON app_m.events TO %s WITH GRANT OPTION", "GRANT INSERT ON app_m.x TO %s"}, events, false},
		{[]string{"GRANT SELECT ON *.* TO %s", "GRANT SELECT ON app_ma.* TO %s WITH GRANT OPTION", "GRANT INSERT ON app_mb.* TO %s"}, teamA, true},
		{[]string{"GRANT SELECT ON *.* TO %s", "GRANT SELECT ON app_ma.* TO %s WITH GRANT OPTION"}, teamA, false},
	} {
		role := "app_hm" + strings.Repeat("x", i)
		g.adminRole(t, role)
		for _, stmt := range tc.stmts {
			g.srv.exec(t, strings.ReplaceAll(stmt, "%s", role))
		}
		rows := g.rows(t, role, cols)
		desired, err := cat.Expand([]string{"SELECT"}, tc.target.Level())
		if err != nil {
			t.Fatal(err)
		}
		a := sqlx.Analyze(cat, tc.target, desired, rows)
		listed := false
		for _, r := range rows {
			if r.GrantOption && !r.PartialRevoke && r.Database == tc.target.Database && r.Table == tc.target.Table {
				listed = true
			}
		}
		if listed == tc.hidden {
			t.Errorf("%v on %s: the server lists the option: %t (SHOW GRANTS %v)", tc.stmts, g.image, listed, g.showGrants(t, role))
		}
		if !listed && !a.Hidden() {
			t.Errorf("%v on %s: the server does not list the option, but the analysis says it would (rows %+v)", tc.stmts, g.image, rows)
		}
	}
}

// rows reads a role's rows of system.grants as the provider does.
func (g *grantFixture) rows(t *testing.T, role string, cols sqlx.GrantColumns) []sqlx.GrantRow {
	t.Helper()
	var out []sqlx.GrantRow
	cl := g.client(t)
	err := cl.Query(context.Background(), sqlx.QueryGrants(true, cols), ch.Params{"grantee": role}, func(r ch.Row) error {
		row, err := sqlx.ScanGrantRow(r, cols)
		if err != nil {
			return err
		}
		out = append(out, row)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// testGrantPlanOrder runs the provider's plans as the administrator (the
// minimal identity cannot grant global privileges): a REVOKE of a
// privilege also removes the flags of its children, so the plan revokes
// first and then grants every desired flag the REVOKE removed, and the
// target converges in one step. CREATE ARBITRARY TEMPORARY TABLE carries a
// flag of its own besides that of its only child CREATE TEMPORARY TABLE.
func testGrantPlanOrder(t *testing.T, g *grantFixture) {
	ctx := context.Background()
	cl := g.client(t)
	privs, err := cl.Capabilities().Privileges(ctx, cl)
	if err != nil {
		t.Fatal(err)
	}
	cat, err := sqlx.NewCatalogue(privs)
	if err != nil {
		t.Fatal(err)
	}
	cols := sqlx.GrantColumns{}
	if cols.AccessObject, err = cl.Capabilities().GrantsAccessObject(ctx, cl); err != nil {
		t.Fatal(err)
	}
	if cols.IsWildcard, err = cl.Capabilities().GrantsIsWildcard(ctx, cl); err != nil {
		t.Fatal(err)
	}
	global := sqlx.GrantTarget{Database: "*"}
	for i, tc := range []struct {
		foreign string
		desired string
		wgo     bool
	}{
		{"GRANT CREATE ARBITRARY TEMPORARY TABLE ON *.* TO %s", "CREATE TEMPORARY TABLE", false},
		{"GRANT CREATE ARBITRARY TEMPORARY TABLE ON *.* TO %s WITH GRANT OPTION", "CREATE TEMPORARY TABLE", true},
		{"GRANT CREATE ARBITRARY TEMPORARY TABLE ON *.* TO %s WITH GRANT OPTION", "CREATE TEMPORARY TABLE", false},
		{"GRANT CREATE ON *.* TO %s", "CREATE TEMPORARY TABLE", false},
	} {
		role := "app_po" + strings.Repeat("x", i)
		g.adminRole(t, role)
		g.srv.exec(t, strings.ReplaceAll(tc.foreign, "%s", role))
		desired, err := cat.Expand([]string{tc.desired}, sqlx.TargetGlobal)
		if err != nil {
			t.Fatal(err)
		}
		a := sqlx.Analyze(cat, global, desired, g.rows(t, role, cols))
		stmts, err := a.Apply(tc.wgo, nil).Statements(global, role)
		if err != nil {
			t.Fatal(err)
		}
		for _, stmt := range stmts {
			g.srv.exec(t, stmt)
		}
		after := sqlx.Analyze(cat, global, desired, g.rows(t, role, cols))
		if next, _ := after.Apply(tc.wgo, nil).Statements(global, role); len(next) > 0 || !after.Effective() {
			t.Errorf("%s, then %v: missing %v, next plan %v (SHOW GRANTS %v)", tc.foreign, stmts, after.Missing(), next, g.showGrants(t, role))
		}
	}
}
