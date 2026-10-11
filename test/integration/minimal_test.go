// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"

	chgo "github.com/ClickHouse/clickhouse-go/v2"
	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/infrabay/provider-clickhouse/apis/v1alpha1"
	ch "github.com/infrabay/provider-clickhouse/internal/clients/clickhouse"
	"github.com/infrabay/provider-clickhouse/internal/controller/ownership"
	"github.com/infrabay/provider-clickhouse/internal/reason"
)

// settingsGrant is the grant that only Users with a managed settingsProfile
// need (design 9.4).
const settingsGrant = "GRANT SELECT ON system.settings_profile_elements"

// TestMinimalGrants checks design 10.2 scenario 5 on every image. A provider
// identity defined in SQL holds exactly the grants of design 9.4 for every
// kind (accessGrants, prefix-scoped to app_), so that an administrator can
// revoke them one at a time. With all of them, a Database, a Role, two Users
// (one with Listed default roles and a managed settingsProfile, one with
// neither), a Grant and a RoleGrant converge. With each grant revoked in
// turn, resources that exist are never reported as absent, every failure is
// PermissionDenied (or DependencyMissing for a resource whose dependency
// could not be created), nothing is created twice, and everything converges
// once the grant is given back. A User that does not set settingsProfile
// needs no SELECT ON system.settings_profile_elements, and without it the
// server is denied that read at most once per capability TTL, not on every
// poll.
func TestMinimalGrants(t *testing.T) {
	ran := false
	for _, fam := range []struct{ name, env, def string }{
		{familyUpstream, envUpstreamImages, defaultUpstreamImages},
		{familyAntalya, envAntalyaImages, defaultAntalyaImages},
	} {
		for _, image := range imageList(fam.env, fam.def) {
			ran = true
			t.Run(image, func(t *testing.T) { testMinimalGrants(t, image) })
		}
	}
	if !ran {
		t.Skip("no images selected")
	}
}

// identityGrant turns a grant of accessGrants into a GRANT to the SQL
// identity; identityRevoke into the REVOKE of it.
func identityGrant(g string) string {
	what, wgo := strings.CutSuffix(strings.TrimPrefix(g, "GRANT "), " WITH GRANT OPTION")
	s := "GRANT " + what + " TO " + ch.QuoteIdent(providerUser)
	if wgo {
		s += " WITH GRANT OPTION"
	}
	return s
}

func identityRevoke(g string) string {
	what, _ := strings.CutSuffix(strings.TrimPrefix(g, "GRANT "), " WITH GRANT OPTION")
	return "REVOKE " + what + " FROM " + ch.QuoteIdent(providerUser)
}

func testMinimalGrants(t *testing.T, image string) {
	password := randHex(16)
	f := &accessFixture{t: t, family: familyUpstream, image: image, journal: ownership.NewJournal(), identity: providerUser,
		pc: &xpv2.ProviderConfigReference{Kind: v1alpha1.ClusterProviderConfigKind, Name: "access"}}
	f.srv = startServer(t, image, nil)
	f.srv.exec(t, "CREATE USER "+ch.QuoteIdent(providerUser)+" IDENTIFIED WITH sha256_password BY "+ch.QuoteString(password))
	grants := accessGrants(image)
	for _, g := range grants {
		f.srv.exec(t, identityGrant(g))
	}
	f.kube = newKube(t, passwordCPC(f.srv, "access", accessPolicy()), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "crossplane-system", Name: "provider-clickhouse"},
		Data:       map[string][]byte{"password": []byte(password)},
	})

	d := newMinimalDrivers(t, f)
	baseline := d.createSet(f, "0")
	d.converge(t, baseline, "with every grant")

	for i, g := range grants {
		t.Run(fmt.Sprintf("Without%02d", i+1), func(t *testing.T) {
			f.srv.exec(t, identityRevoke(g))
			// New clients: capability probes run again without the grant.
			d := newMinimalDrivers(t, f)
			d.neverAbsent(t, baseline, g)
			fresh := d.createSet(f, strconv.Itoa(i+1))
			d.onlyDenied(t, fresh, g)
			if g == settingsGrant {
				plain := fresh[3]
				if n := f.srv.deniedSettingsReads(t, "User/"+namespace+"/"+plain.name); n > 1 {
					t.Errorf("without %q the server was denied the settings read of %s %d times, want at most once", g, plain.name, n)
				}
			}
			f.srv.exec(t, identityGrant(g))
			d.converge(t, fresh, "after "+g+" was given back")
			d.deleteAll(t, fresh)
		})
	}
	newMinimalDrivers(t, f).deleteAll(t, baseline)
}

// minimalMR is a managed resource of TestMinimalGrants.
type minimalMR struct {
	kind, name string
	// plain is a User that sets neither settingsProfile nor defaultRoles.
	plain bool
}

func (m minimalMR) String() string { return m.kind + " " + m.name }

// minimalDrivers drive every kind on the fixture with one client cache.
type minimalDrivers struct {
	db    *harness
	role  *mrHarness[*v1alpha1.Role]
	user  *userHarness
	grant *grantFixture
	rg    *mrHarness[*v1alpha1.RoleGrant]
}

func newMinimalDrivers(t *testing.T, f *accessFixture) *minimalDrivers {
	t.Helper()
	f.cache = ch.NewCache(ch.OpenDriver)
	t.Cleanup(f.cache.Close)
	return &minimalDrivers{db: newHarness(t, f.kube, nil), role: roleHarness(t, f), user: newUserHarness(t, f), grant: newGrantFixture(t, f), rg: roleGrantHarness(t, f)}
}

// createSet stores one resource of every kind, named with suffix s, in the
// order in which they depend on each other.
func (d *minimalDrivers) createSet(f *accessFixture, s string) []minimalMR {
	db, role, user, plain := "app_mg_db"+s, "app_mg_role"+s, "app_mg_user"+s, "app_mg_plain"+s
	d.db.createMR("db"+s, v1alpha1.DatabaseParameters{Name: &db, Comment: new("minimal grants")}, v1alpha1.ClusterProviderConfigKind, f.pc.Name)
	d.role.create(newRoleMR(f, "role"+s, role))
	d.user.createUser("user"+s, user, func(u *v1alpha1.User) {
		u.Spec.ForProvider.SettingsProfile = new("default")
		u.Spec.ForProvider.DefaultRoles = &v1alpha1.DefaultRoles{Policy: v1alpha1.DefaultRolesListed, Roles: []string{role}}
	})
	d.user.createUser("plain"+s, plain)
	p := roleGrantee(db, "SELECT")
	p.Role = new(role)
	d.grant.newGrant("grant"+s, p)
	d.rg.create(newRoleGrantMR(f.pc, "rg"+s, rgMemberOf(role), rgToUser(user)))
	return []minimalMR{{kind: "Database", name: "db" + s}, {kind: "Role", name: "role" + s}, {kind: "User", name: "user" + s},
		{kind: "User", name: "plain" + s, plain: true}, {kind: "Grant", name: "grant" + s}, {kind: "RoleGrant", name: "rg" + s}}
}

func (d *minimalDrivers) reconcile(m minimalMR) outcome {
	switch m.kind {
	case "Database":
		return d.db.reconcile(m.name)
	case "Role":
		return d.role.reconcile(m.name)
	case "User":
		return d.user.reconcile(m.name)
	case "Grant":
		return d.grant.h.reconcile(m.name)
	}
	return d.rg.reconcile(m.name)
}

func (d *minimalDrivers) delete(m minimalMR) {
	switch m.kind {
	case "Database":
		d.db.delete(m.name)
	case "Role":
		d.role.delete(m.name)
	case "User":
		d.user.delete(m.name)
	case "Grant":
		d.grant.h.delete(m.name)
	default:
		d.rg.delete(m.name)
	}
}

func steadyOutcome(o outcome) bool {
	return o.err == nil && o.obs.ResourceExists && o.obs.ResourceUpToDate && !o.created && !o.updated
}

// converge reconciles the set until every resource exists and is up to date
// without writes.
func (d *minimalDrivers) converge(t *testing.T, set []minimalMR, what string) {
	t.Helper()
	var last []string
	for range 6 {
		last = nil
		for _, m := range set {
			if o := d.reconcile(m); !steadyOutcome(o) {
				last = append(last, fmt.Sprintf("%s: %s", m, o))
			}
		}
		if len(last) == 0 {
			return
		}
	}
	t.Fatalf("%s: not converged:\n%s", what, strings.Join(last, "\n"))
}

// neverAbsent reconciles resources whose objects exist without grant g:
// none is reported as absent or created again, and every failure is
// PermissionDenied.
func (d *minimalDrivers) neverAbsent(t *testing.T, set []minimalMR, g string) {
	t.Helper()
	for range 2 {
		for _, m := range set {
			o := d.reconcile(m)
			switch {
			case o.err == nil && !o.obs.ResourceExists, o.created:
				t.Errorf("without %q, %s was reported as absent: %s", g, m, o)
			case o.err != nil && !reason.Is(o.err, reason.PermissionDenied):
				t.Errorf("without %q, %s failed otherwise than with PermissionDenied: %s", g, m, o)
			}
		}
	}
}

// onlyDenied reconciles new resources without grant g: every failure is
// PermissionDenied or DependencyMissing, and none is created twice. A User
// that sets no settingsProfile needs no settings grant.
func (d *minimalDrivers) onlyDenied(t *testing.T, set []minimalMR, g string) {
	t.Helper()
	created := map[string]bool{}
	for round := range 3 {
		for _, m := range set {
			o := d.reconcile(m)
			if o.created && created[m.name] {
				t.Errorf("without %q, %s was created twice", g, m)
			}
			created[m.name] = created[m.name] || o.created
			if o.err != nil && !reason.Is(o.err, reason.PermissionDenied) && !reason.Is(o.err, reason.DependencyMissing) {
				t.Errorf("without %q, %s failed otherwise than with PermissionDenied or DependencyMissing: %s", g, m, o)
			}
			if g == settingsGrant && m.plain && round > 0 && !steadyOutcome(o) {
				t.Errorf("without %q, %s needs no settings grant: %s", g, m, o)
			}
		}
	}
}

// deleteAll deletes the set in reverse order and reconciles until every
// resource is gone.
func (d *minimalDrivers) deleteAll(t *testing.T, set []minimalMR) {
	t.Helper()
	for i := len(set) - 1; i >= 0; i-- {
		m := set[i]
		d.delete(m)
		gone := false
		var o outcome
		for range 3 {
			if o = d.reconcile(m); o.gone {
				gone = true
				break
			}
		}
		if !gone {
			t.Fatalf("%s was not released: %s", m, o)
		}
	}
}

// deniedSettingsReads counts the reads of system.settings_profile_elements
// that the server refused for the MR whose statements carry logComment.
func (s *server) deniedSettingsReads(t *testing.T, logComment string) uint64 {
	t.Helper()
	s.exec(t, "SYSTEM FLUSH LOGS")
	var n uint64
	ctx := chgo.Context(context.Background(), chgo.WithParameters(chgo.Parameters{"c": ch.ParamValue(logComment)}))
	if err := s.admin.QueryRow(ctx, "SELECT count() FROM system.query_log WHERE log_comment = {c:String} AND type IN ('ExceptionBeforeStart', 'ExceptionWhileProcessing') AND query LIKE '%settings_profile_elements%'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
