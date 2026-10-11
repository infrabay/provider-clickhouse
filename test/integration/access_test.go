// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	chgo "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/golang-jwt/jwt/v5"

	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	"github.com/google/go-cmp/cmp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/crossplane/crossplane-runtime/v2/pkg/event"
	"github.com/crossplane/crossplane-runtime/v2/pkg/meta"
	"github.com/crossplane/crossplane-runtime/v2/pkg/reconciler/managed"
	"github.com/crossplane/crossplane-runtime/v2/pkg/resource"

	"github.com/infrabay/provider-clickhouse/apis/v1alpha1"
	ch "github.com/infrabay/provider-clickhouse/internal/clients/clickhouse"
	"github.com/infrabay/provider-clickhouse/internal/clients/clickhouse/fake"
	"github.com/infrabay/provider-clickhouse/internal/clients/clickhouse/sqlx"
	"github.com/infrabay/provider-clickhouse/internal/controller/access"
	"github.com/infrabay/provider-clickhouse/internal/controller/ownership"
	"github.com/infrabay/provider-clickhouse/internal/controller/providerconfig"
	"github.com/infrabay/provider-clickhouse/internal/reason"
)

// accessPolicy lets the test namespace manage app_ users, roles and
// databases, and name the provider identity (for the read-only and
// self-modification checks).
func accessPolicy() v1alpha1.ClusterPolicy {
	return v1alpha1.ClusterPolicy{
		AllowedNamespaces:     []string{namespace},
		UserNamePatterns:      []string{"app_.+", regexp.QuoteMeta(providerUser), regexp.QuoteMeta(jwtProviderUser)},
		RoleNamePatterns:      []string{"app_.+"},
		GrantableRoles:        []string{"app_.+"},
		DatabaseNamePatterns:  []string{"app_.+"},
		AllowedAuthentication: []v1alpha1.AuthenticationMethod{v1alpha1.AuthenticationMethodPassword, v1alpha1.AuthenticationMethodJWT},
		AllowedJWTProcessors:  []string{processorTest},
	}
}

// knownProfile returns the capabilities verified for an image of the CI
// matrix (design section 7), if the image is one of them.
func knownProfile(image string) (fake.Profile, bool) {
	for tag, p := range map[string]fake.Profile{
		":25.3.":                     fake.Upstream253,
		":25.8.":                     fake.Upstream258,
		"clickhouse-server:26.8.":    fake.Upstream268,
		":26.3.13.20001.altinityant": fake.Antalya263,
		":26.6.4.20001.altinityant":  fake.Antalya266,
	} {
		if strings.Contains(image, tag) {
			return p, true
		}
	}
	return fake.Profile{}, false
}

// TestAccessFoundation checks the shared foundations of the access entity
// kinds against every image, with the minimal, prefix-scoped provider
// identity: capability probes, entity reads, the ownership flow of design
// 4.2 for a role (including the hardened Recovered rule), the grantee
// binding of grants, and the SQL lexer on server output read through the
// driver.
func TestAccessFoundation(t *testing.T) {
	forEachAccessImage(t, accessPolicy(), func(t *testing.T, f *accessFixture) {
		t.Run("Capabilities", func(t *testing.T) { testAccessCapabilities(t, f) })
		t.Run("Entities", func(t *testing.T) { testAccessEntities(t, f) })
		t.Run("RoleOwnership", func(t *testing.T) { testAccessRoleOwnership(t, f) })
		t.Run("GranteeBinding", func(t *testing.T) { testAccessGranteeBinding(t, f) })
		t.Run("LexServerOutput", func(t *testing.T) { testAccessLex(t, f) })
	})
}

func testAccessCapabilities(t *testing.T, f *accessFixture) {
	ctx := context.Background()
	cl := f.client(t)
	caps := cl.Capabilities()
	got := fake.Profile{Version: cl.Info().Version}
	var err error
	if got.JWTUsers, err = caps.JWTUsers(ctx, cl); err != nil {
		t.Fatalf("jwt probe with the minimal identity: %v", err)
	}
	if got.JWTUsers != (f.family == familyAntalya) {
		t.Errorf("JWT users on %s = %t", f.image, got.JWTUsers)
	}
	// The column probes must agree with what the admin sees: system.columns
	// would hide system.users from an identity with SHOW USERS only.
	for _, c := range []struct {
		table, column string
		probe         func(context.Context, ch.DB) (bool, error)
		dst           *bool
	}{
		{ch.TableUsers, ch.ColumnValidUntil, caps.UsersValidUntil, &got.ValidUntil},
		{ch.TableGrants, ch.ColumnAccessObject, caps.GrantsAccessObject, &got.AccessObject},
		{ch.TableGrants, ch.ColumnIsWildcard, caps.GrantsIsWildcard, &got.IsWildcard},
	} {
		v, err := c.probe(ctx, cl)
		if err != nil {
			t.Fatalf("%s.%s probe: %v", c.table, c.column, err)
		}
		*c.dst = v
		admin := f.srv.admin.Exec(ctx, ch.QuerySystemColumnProbe(c.table, c.column)) == nil
		if v != admin {
			t.Errorf("system.%s.%s: probe says %t, the admin sees %t", c.table, c.column, v, admin)
		}
	}
	if got.DBComment, err = caps.DatabaseCommentChange(ctx, cl); err != nil {
		t.Fatal(err)
	}
	if want, ok := knownProfile(f.image); ok {
		got.JWTProbeCode, got.Storage = want.JWTProbeCode, want.Storage
		want.Version = got.Version
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("capabilities of %s differ from design section 7 (-want +got):\n%s", f.image, diff)
		}
	}

	privs, err := caps.Privileges(ctx, cl)
	if err != nil {
		t.Fatal(err)
	}
	var n uint64
	if err := f.srv.admin.QueryRow(ctx, "SELECT count() FROM system.privileges").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if uint64(len(privs)) != n {
		t.Errorf("catalogue has %d rows, system.privileges %d", len(privs), n)
	}
	byName := map[string]ch.Privilege{}
	for _, p := range privs {
		byName[p.Name] = p
	}
	if p := byName["SELECT"]; p.Level != "COLUMN" || p.Parent != "ALL" {
		t.Errorf("SELECT = %+v", p)
	}
	if p := byName["SHOW USERS"]; p.Level != "GLOBAL" || len(p.Aliases) == 0 || p.Aliases[0] != "SHOW CREATE USER" {
		t.Errorf("SHOW USERS = %+v", p)
	}
	if p, ok := byName["NONE"]; !ok || p.Level != "" || p.Parent != "" {
		t.Errorf("NONE (NULL level and parent) = %+v, %t", p, ok)
	}
	if s, err := caps.AccessStorage(ctx, cl); err != nil || s != ch.StorageLocalDirectory {
		t.Errorf("AccessStorage() = %q, %v", s, err)
	}
}

func testAccessEntities(t *testing.T, f *accessFixture) {
	ctx := context.Background()
	cl := f.client(t)

	if e, ok, err := access.ReadEntity(ctx, cl, access.User, f.identity); err != nil || !ok || !e.ReadOnly() {
		t.Errorf("the users.d identity: %+v, %t, %v", e, ok, err)
	}
	uid := f.adminUser(t, "app_ent_user")
	if e, ok, err := access.ReadEntity(ctx, cl, access.User, "app_ent_user"); err != nil || !ok || e.ID != uid || e.Storage != ch.StorageLocalDirectory {
		t.Errorf("SQL user: %+v, %t, %v (admin sees %s)", e, ok, err, uid)
	}
	rid := f.adminRole(t, "app_ent_role")
	if e, ok, err := access.ReadEntity(ctx, cl, access.Role, "app_ent_role"); err != nil || !ok || e.ID != rid {
		t.Errorf("SQL role: %+v, %t, %v", e, ok, err)
	}
	if _, ok, err := access.ReadEntity(ctx, cl, access.Role, "app_absent"); ok || err != nil {
		t.Errorf("absent role: %t, %v", ok, err)
	}
	if _, ok, err := access.ReadEntity(ctx, cl, access.Role, "app_ent_user"); ok || err != nil {
		t.Errorf("a user is not a role: %t, %v", ok, err)
	}

	// A user and a role may share a name; the provider refuses that.
	f.adminRole(t, "app_twin")
	f.adminUser(t, "app_twin")
	for _, k := range []access.EntityKind{access.User, access.Role} {
		if err := access.CheckNameCollision(ctx, cl, k, "app_twin"); !reason.Is(err, reason.NameCollision) {
			t.Errorf("%s app_twin: %v, want NameCollision", k, err)
		}
	}
	if err := access.CheckNameCollision(ctx, cl, access.Role, "app_ent_role"); err != nil {
		t.Errorf("no collision: %v", err)
	}

	// Names travel as server-side parameters.
	weird := "app_we!rd.n@me`q'\\z\tü"
	wid := f.adminRole(t, weird)
	if e, ok, err := access.ReadEntity(ctx, cl, access.Role, weird); err != nil || !ok || e.ID != wid {
		t.Errorf("adversarial name: %+v, %t, %v", e, ok, err)
	}

	env := f.env(t, roleMR(f, "probe", "app_x"), nil)
	if err := env.CheckWritable(ctx); err != nil {
		t.Errorf("CheckWritable() on a single server with local_directory: %v", err)
	}
}

// roleMR returns a Role MR with a valid owner-uid claim and no owned-id, as
// the initializer leaves it, stored in the fixture's Kubernetes API.
func roleMR(f *accessFixture, mrName, name string) *v1alpha1.Role {
	f.t.Helper()
	r := &v1alpha1.Role{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: mrName, UID: types.UID("uid-" + mrName + "-" + randHex(3))}}
	r.Spec.ProviderConfigReference = f.pc
	r.Spec.ForProvider.Name = &name
	meta.SetExternalName(r, name)
	meta.AddAnnotations(r, map[string]string{v1alpha1.AnnotationOwnerUID: string(r.GetUID())})
	if err := f.kube.Create(context.Background(), r); err != nil {
		f.t.Fatal(err)
	}
	return r
}

func getRole(t *testing.T, f *accessFixture, mrName string) *v1alpha1.Role {
	t.Helper()
	r := &v1alpha1.Role{}
	if err := f.kube.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: mrName}, r); err != nil {
		t.Fatal(err)
	}
	return r
}

func testAccessRoleOwnership(t *testing.T, f *accessFixture) {
	const name = "app_owned"
	ctx := context.Background()
	mine := roleMR(f, "mine", name)
	rec := &eventRecorder{}
	env := f.env(t, mine, rec)
	sqlCtx := access.SQLContext(ctx, "Role", mine)

	obs, err := env.ObserveEntity(sqlCtx, mine, access.Role, mine.Spec.ForProvider.Name)
	if err != nil || obs.Exists || obs.Found {
		t.Fatalf("before create: %+v, %v", obs, err)
	}
	meta.SetExternalCreatePending(mine, time.Now())
	if err := env.CreateEntity(sqlCtx, mine, access.Role, name, "CREATE ROLE "+ch.QuoteIdent(name)); err != nil {
		t.Fatalf("CreateEntity() = %v", err)
	}
	id, storage, ok := f.srv.entity(t, access.Role, name)
	if !ok || ownership.ClaimOf(mine).OwnedID != id || storage != ch.StorageLocalDirectory {
		t.Fatalf("server has %q (%s, %t), owned-id %q", id, storage, ok, ownership.ClaimOf(mine).OwnedID)
	}

	// The annotation update after the create was lost: the journal of this
	// process recognises the role, and only this exact UUID.
	meta.RemoveAnnotations(mine, v1alpha1.AnnotationOwnedID)
	obs, err = env.ObserveEntity(sqlCtx, mine, access.Role, mine.Spec.ForProvider.Name)
	if err != nil || !obs.Exists || obs.Decision.Decision != ownership.Recovered {
		t.Fatalf("lost response: %+v, %v", obs.Decision, err)
	}
	if got := ownership.ClaimOf(getRole(t, f, "mine")).OwnedID; got != id {
		t.Errorf("recovered claim not persisted: %q", got)
	}
	if diff := cmp.Diff([]string{"CreateRecovered"}, rec.reasons()); diff != "" {
		t.Errorf("events: %s", diff)
	}
	obs, err = env.ObserveEntity(sqlCtx, mine, access.Role, mine.Spec.ForProvider.Name)
	if err != nil || obs.Decision.Decision != ownership.Owned || !obs.Exists {
		t.Fatalf("owned: %+v, %v", obs.Decision, err)
	}
	if ok, err := env.VerifyOwned(sqlCtx, mine, access.Role, name); err != nil || !ok {
		t.Fatalf("VerifyOwned() = %t, %v", ok, err)
	}

	// Another MR points at the same role and forges every annotation that
	// row 5 reads: it gets nothing, and deleting it leaves the role alone.
	attacker := roleMR(f, "attacker", name)
	meta.SetExternalCreatePending(attacker, time.Now())
	meta.AddAnnotations(attacker, map[string]string{v1alpha1.AnnotationCreateOutcome: v1alpha1.CreateOutcomeUnknown})
	aenv := f.env(t, attacker, nil)
	if _, err := aenv.ObserveEntity(sqlCtx, attacker, access.Role, attacker.Spec.ForProvider.Name); !reason.Is(err, reason.NotOwned) {
		t.Fatalf("forged create annotations: %v, want NotOwned", err)
	}
	if ok, err := aenv.VerifyOwned(sqlCtx, attacker, access.Role, name); ok || !reason.Is(err, reason.NotOwned) {
		t.Fatalf("VerifyOwned() for the attacker = %t, %v", ok, err)
	}
	now := metav1.Now()
	attacker.SetDeletionTimestamp(&now)
	if obs, err := aenv.ObserveEntity(sqlCtx, attacker, access.Role, attacker.Spec.ForProvider.Name); err != nil || obs.Exists || !obs.Orphaned {
		t.Fatalf("deleting the attacker: %+v, %v", obs, err)
	}

	// Dropped and re-created out of band: the UUID changed.
	f.srv.exec(t, "DROP ROLE "+ch.QuoteIdent(name))
	newID := f.adminRole(t, name)
	meta.SetExternalCreateSucceeded(mine, time.Now())
	if _, err := env.ObserveEntity(sqlCtx, mine, access.Role, mine.Spec.ForProvider.Name); !reason.Is(err, reason.Replaced) || !strings.Contains(err.Error(), newID) {
		t.Fatalf("replaced: %v", err)
	}
	if ok, err := env.VerifyOwned(sqlCtx, mine, access.Role, name); ok || !reason.Is(err, reason.Replaced) {
		t.Fatalf("VerifyOwned() after replacement = %t, %v", ok, err)
	}
	// While deleting, the replacement is left in place.
	mineDeleting := mine.DeepCopy()
	mineDeleting.SetDeletionTimestamp(&now)
	if obs, err := env.ObserveEntity(sqlCtx, mineDeleting, access.Role, mineDeleting.Spec.ForProvider.Name); err != nil || obs.Exists || !obs.Orphaned {
		t.Fatalf("deleting after replacement: %+v, %v", obs, err)
	}
	if got, _, ok := f.srv.entity(t, access.Role, name); !ok || got != newID {
		t.Fatalf("the replacement was touched: %q %t", got, ok)
	}

	// The server-side grants are the ceiling: a name outside the identity's
	// prefix fails with 497 even through an unrestricted config.
	open := &v1alpha1.ClusterProviderConfig{}
	if err := f.kube.Get(ctx, client.ObjectKey{Name: f.pc.Name}, open); err != nil {
		t.Fatal(err)
	}
	open = open.DeepCopy()
	open.ObjectMeta = metav1.ObjectMeta{Name: "access-open", UID: types.UID("cpc-access-open")}
	open.Spec.Policy = v1alpha1.ClusterPolicy{Unrestricted: true}
	if err := f.kube.Create(ctx, open); err != nil {
		t.Fatal(err)
	}
	outside := roleMR(f, "outside", "zz_role")
	outside.Spec.ProviderConfigReference = &xpv2.ProviderConfigReference{Kind: v1alpha1.ClusterProviderConfigKind, Name: open.Name}
	oenv := f.env(t, outside, nil)
	if err := oenv.CreateEntity(sqlCtx, outside, access.Role, "zz_role", "CREATE ROLE `zz_role`"); !reason.Is(err, reason.PermissionDenied) {
		t.Fatalf("outside the identity's prefix: %v, want PermissionDenied", err)
	}
	if got := ownership.ClaimOf(outside).CreateOutcome; got != v1alpha1.CreateOutcomeRejected {
		t.Errorf("create-outcome = %q, want rejected", got)
	}
}

func testAccessGranteeBinding(t *testing.T, f *accessFixture) {
	ctx := context.Background()
	f.ownedRole(t, "reader", "app_reader")
	grant := func() *v1alpha1.Grant {
		g := &v1alpha1.Grant{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "g", UID: "grant-uid"}}
		g.Spec.ProviderConfigReference = f.pc
		g.Spec.ForProvider.Privileges = []string{"SELECT"}
		g.Spec.ForProvider.Database = new("app_db")
		return g
	}
	g := grant()
	env := f.env(t, g, nil)

	b, err := env.BindGrantee(ctx, g, access.Grantee{Kind: access.Role, Name: "app_reader"})
	if err != nil || !b.Exists || b.Owner != "Role team-a/reader" {
		t.Fatalf("owned role grantee: %+v, %v", b, err)
	}
	f.adminRole(t, "app_foreign")
	if _, err := env.BindGrantee(ctx, grant(), access.Grantee{Kind: access.Role, Name: "app_foreign"}); !reason.Is(err, reason.NotOwned) {
		t.Errorf("a role the provider did not create: %v, want NotOwned", err)
	}
	if _, err := env.BindGrantee(ctx, grant(), access.Grantee{Kind: access.User, Name: f.identity}); !reason.Is(err, reason.ReadOnlyStorage) {
		t.Errorf("the users.d identity as grantee: %v, want ReadOnlyStorage", err)
	}
	f.ownedRole(t, "twin", "app_twin_role")
	f.adminUser(t, "app_twin_role")
	if _, err := env.BindGrantee(ctx, grant(), access.Grantee{Kind: access.Role, Name: "app_twin_role"}); !reason.Is(err, reason.AmbiguousGrantee) {
		t.Errorf("a role shadowed by a user: %v, want AmbiguousGrantee", err)
	}
	if _, err := env.BindGrantee(ctx, grant(), access.Grantee{Kind: access.Role, Name: "app_missing"}); !reason.Is(err, reason.DependencyMissing) {
		t.Errorf("a missing grantee: %v, want DependencyMissing", err)
	}

	// Immediately before a write the binding is checked again: a role
	// replaced out of band is no longer owned.
	if ok, err := env.RebindGrantee(ctx, g, b); err != nil || !ok {
		t.Fatalf("RebindGrantee() = %t, %v", ok, err)
	}
	f.srv.exec(t, "DROP ROLE app_reader")
	f.adminRole(t, "app_reader")
	if ok, err := env.RebindGrantee(ctx, g, b); ok || !reason.Is(err, reason.NotOwned) {
		t.Fatalf("RebindGrantee() after replacement = %t, %v", ok, err)
	}
}

func testAccessLex(t *testing.T, f *accessFixture) {
	f.adminUser(t, "app_lex")
	f.srv.exec(t, "ALTER USER app_lex VALID UNTIL '2031-01-01 00:00:00'")
	f.srv.exec(t, "ALTER USER app_lex HOST IP '10.1.2.3/8', NAME 'Host.Example.COM' DEFAULT DATABASE app_db SETTINGS PROFILE 'default'")
	stmt := f.srv.rawString(t, "SHOW CREATE USER app_lex")
	toks, err := sqlx.Lex(stmt)
	if err != nil {
		t.Fatalf("Lex(%q): %v", stmt, err)
	}
	var strs, idents []string
	for _, tk := range toks {
		switch tk.Kind {
		case sqlx.TokenString:
			strs = append(strs, tk.Value)
		case sqlx.TokenQuotedIdent:
			idents = append(idents, tk.Value)
		}
	}
	for _, want := range []string{"2031-01-01 00:00:00", "10.0.0.0/8", "Host.Example.COM"} {
		if !contains(strs, want) {
			t.Errorf("SHOW CREATE USER %q: string literals %q lack %q", stmt, strs, want)
		}
	}
	if !contains(idents, "default") {
		t.Errorf("SHOW CREATE USER %q prints the settings profile as an identifier; got %q", stmt, idents)
	}

	if f.family == familyAntalya {
		f.srv.exec(t, `CREATE USER app_lex_jwt IDENTIFIED WITH jwt PROCESSOR 'test' CLAIMS '{"hd":"it\'s \\\\ ok"}'`)
		toks, err := sqlx.Lex(f.srv.rawString(t, "SHOW CREATE USER app_lex_jwt"))
		if err != nil {
			t.Fatal(err)
		}
		var lits []string
		for _, tk := range toks {
			if tk.Kind == sqlx.TokenString {
				lits = append(lits, tk.Value)
			}
		}
		if diff := cmp.Diff([]string{"test", `{"hd":"it's \\ ok"}`}, lits); diff != "" {
			t.Errorf("jwt literals: %s", diff)
		}
	}

	f.adminRole(t, "app_lex_role")
	f.srv.exec(t, "GRANT SELECT ON app_*.* TO app_lex_role")
	// A literal '*' in a name; not under app_*, which would absorb it.
	f.srv.exec(t, "GRANT SELECT ON `zz_odd*`.* TO app_lex_role")
	wildcards := map[bool]int{}
	for _, line := range f.srv.rawStrings(t, "SHOW GRANTS FOR app_lex_role") {
		toks, err := sqlx.Lex(line)
		if err != nil {
			t.Fatalf("Lex(%q): %v", line, err)
		}
		w := false
		for i := 1; i < len(toks); i++ {
			if toks[i].IsPunct('*') && toks[i].Adjacent(toks[i-1]) && toks[i-1].IsIdent() {
				w = true
			}
		}
		wildcards[w]++
	}
	if wildcards[true] != 1 || wildcards[false] != 1 {
		t.Errorf("SHOW GRANTS lines with and without a wildcard: %v, want one each", wildcards)
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// eventRecorder records the reasons of the events it is given.
type eventRecorder struct {
	mu     sync.Mutex
	events []event.Event
}

func (r *eventRecorder) Event(_ runtime.Object, e event.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *eventRecorder) WithAnnotations(...string) event.Recorder { return r }

func (r *eventRecorder) reasons() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []string{}
	for _, e := range r.events {
		out = append(out, string(e.Reason))
	}
	return out
}

// probeRole is a minimal Role external client built only on the shared
// foundations. It is not the Role controller: it exists so that the harness
// and the foundations are checked together through the reconcile loop.
type probeRole struct{ *access.Env }

func (p *probeRole) Observe(ctx context.Context, cr *v1alpha1.Role) (managed.ExternalObservation, error) {
	obs, err := p.ObserveEntity(access.SQLContext(ctx, "Role", cr), cr, access.Role, cr.Spec.ForProvider.Name)
	cr.Status.AtProvider = v1alpha1.RoleObservation{ID: obs.Entity.ID, Storage: obs.Entity.Storage}
	if err != nil || !obs.Exists {
		return managed.ExternalObservation{}, err
	}
	return managed.ExternalObservation{ResourceExists: true, ResourceUpToDate: true,
		ConnectionDetails: managed.ConnectionDetails{"role": []byte(obs.Name)}}, nil
}

func (p *probeRole) Create(ctx context.Context, cr *v1alpha1.Role) (managed.ExternalCreation, error) {
	name, err := ownership.EffectiveName(cr, cr.Spec.ForProvider.Name)
	if err != nil {
		return managed.ExternalCreation{}, err
	}
	return managed.ExternalCreation{}, p.CreateEntity(access.SQLContext(ctx, "Role", cr), cr, access.Role, name, "CREATE ROLE "+ch.QuoteIdent(name))
}

func (p *probeRole) Update(context.Context, *v1alpha1.Role) (managed.ExternalUpdate, error) {
	return managed.ExternalUpdate{}, nil
}

func (p *probeRole) Delete(ctx context.Context, cr *v1alpha1.Role) (managed.ExternalDelete, error) {
	name, err := ownership.EffectiveName(cr, cr.Spec.ForProvider.Name)
	if err != nil {
		return managed.ExternalDelete{}, err
	}
	ctx = access.SQLContext(ctx, "Role", cr)
	ok, err := p.VerifyOwned(ctx, cr, access.Role, name)
	if err != nil || !ok {
		return managed.ExternalDelete{}, err
	}
	return managed.ExternalDelete{}, access.IgnoreGone(p.Client.Exec(ctx, "DROP ROLE IF EXISTS "+ch.QuoteIdent(name)))
}

// probeGrant is a minimal Grant external client that only binds its role
// grantee.
type probeGrant struct{ *access.Env }

func (p *probeGrant) Observe(ctx context.Context, cr *v1alpha1.Grant) (managed.ExternalObservation, error) {
	if cr.Spec.ForProvider.Role == nil {
		return managed.ExternalObservation{}, errors.New("role is not resolved")
	}
	b, err := p.BindGrantee(ctx, cr, access.Grantee{Kind: access.Role, Name: *cr.Spec.ForProvider.Role})
	cr.Status.AtProvider.Grantee = b.Grantee.String()
	return managed.ExternalObservation{ResourceExists: b.Exists, ResourceUpToDate: true}, err
}

func (p *probeGrant) Create(context.Context, *v1alpha1.Grant) (managed.ExternalCreation, error) {
	return managed.ExternalCreation{}, errors.New("not implemented")
}

func (p *probeGrant) Update(context.Context, *v1alpha1.Grant) (managed.ExternalUpdate, error) {
	return managed.ExternalUpdate{}, nil
}

func (p *probeGrant) Delete(context.Context, *v1alpha1.Grant) (managed.ExternalDelete, error) {
	return managed.ExternalDelete{}, nil
}

func grantRefs(g *v1alpha1.Grant) []providerconfig.Reference {
	p := g.Spec.ForProvider
	return []providerconfig.Reference{
		{Field: "databaseRef", Ref: p.DatabaseRef, Selector: p.DatabaseSelector, New: func() resource.ModernManaged { return &v1alpha1.Database{} }},
		{Field: "userRef", Ref: p.UserRef, Selector: p.UserSelector, New: func() resource.ModernManaged { return &v1alpha1.User{} }},
		{Field: "roleRef", Ref: p.RoleRef, Selector: p.RoleSelector, New: func() resource.ModernManaged { return &v1alpha1.Role{} }},
	}
}

// TestAccessHarness drives minimal access entity clients through the
// harness on every image: the ownership initializer, create annotations,
// connection Secret publishing, reference resolution, deletion of an owned
// and of a foreign role, and (Antalya) JWT login through both token
// processors.
func TestAccessHarness(t *testing.T) {
	forEachAccessImage(t, accessPolicy(), func(t *testing.T, f *accessFixture) {
		conn := accessConnector(f, "Role", func(env *access.Env) managed.TypedExternalClient[*v1alpha1.Role] { return &probeRole{env} }, nil)
		init := ownership.NewInitializer(f.kube, func(mg resource.Managed) *string { return mg.(*v1alpha1.Role).Spec.ForProvider.Name })
		h := newAccessHarness(t, f, conn, func() *v1alpha1.Role { return &v1alpha1.Role{} }, init)

		mine := &v1alpha1.Role{ObjectMeta: metav1.ObjectMeta{Name: "probe"}}
		mine.Spec.ProviderConfigReference = f.pc
		mine.Spec.ForProvider.Name = new("app_probe")
		mine.Spec.WriteConnectionSecretToReference = &xpv2.LocalSecretReference{Name: "probe-conn"}
		h.create(mine)
		if o := h.reconcile("probe"); o.err != nil || !o.created {
			t.Fatalf("create: %s", o)
		}
		id, _, ok := f.srv.entity(t, access.Role, "app_probe")
		if !ok || ownership.ClaimOf(h.get("probe")).OwnedID != id {
			t.Fatalf("server %q (%t), claim %v", id, ok, h.get("probe").GetAnnotations())
		}
		if o := h.reconcile("probe"); o.err != nil || !o.obs.ResourceExists || !o.obs.ResourceUpToDate {
			t.Fatalf("observe: %s", o)
		}
		if got := string(h.secret("probe")["role"]); got != "app_probe" {
			t.Errorf("connection Secret: %q", got)
		}

		f.adminRole(t, "app_theirs")
		theirs := &v1alpha1.Role{ObjectMeta: metav1.ObjectMeta{Name: "theirs"}}
		theirs.Spec.ProviderConfigReference = f.pc
		theirs.Spec.ForProvider.Name = new("app_theirs")
		h.create(theirs)
		if o := h.reconcile("theirs"); !reason.Is(o.err, reason.NotOwned) || o.created {
			t.Fatalf("pre-existing role: %s", o)
		}
		h.delete("theirs")
		if o := h.reconcile("theirs"); o.err != nil || !o.gone || o.deleted {
			t.Fatalf("delete a foreign role's MR: %s", o)
		}
		if _, _, ok := f.srv.entity(t, access.Role, "app_theirs"); !ok {
			t.Fatal("a foreign role was dropped")
		}

		h.delete("probe")
		if o := h.reconcile("probe"); o.err != nil || !o.deleted {
			t.Fatalf("delete: %s", o)
		}
		if _, _, ok := f.srv.entity(t, access.Role, "app_probe"); ok {
			t.Fatal("the owned role still exists")
		}
		if o := h.reconcile("probe"); o.err != nil || !o.gone {
			t.Fatalf("finalize: %s", o)
		}

		// References resolve to the referenced MR's external name.
		f.ownedRole(t, "reader", "app_ref_reader")
		gconn := accessConnector(f, "Grant", func(env *access.Env) managed.TypedExternalClient[*v1alpha1.Grant] { return &probeGrant{env} }, grantRefs)
		gh := newAccessHarness(t, f, gconn, func() *v1alpha1.Grant { return &v1alpha1.Grant{} }).
			withReferences(providerconfig.NewReferenceResolver(f.kube, func(mg resource.Managed) []providerconfig.Reference { return grantRefs(mg.(*v1alpha1.Grant)) }))
		g := &v1alpha1.Grant{ObjectMeta: metav1.ObjectMeta{Name: "g"}}
		g.Spec.ProviderConfigReference = f.pc
		g.Spec.ForProvider.Privileges = []string{"SELECT"}
		g.Spec.ForProvider.Database = new("app_db")
		g.Spec.ForProvider.RoleRef = &xpv2.NamespacedReference{Name: "reader"}
		gh.create(g)
		if o := gh.reconcile("g"); o.err != nil || !o.obs.ResourceExists {
			t.Fatalf("grant with roleRef: %s", o)
		}
		if r := gh.get("g").Spec.ForProvider.Role; r == nil || *r != "app_ref_reader" {
			t.Fatalf("roleRef resolved to %v", r)
		}

		// An owned user is a valid grantee.
		f.ownedUser(t, "api", "app_api")
		env := f.env(t, g, nil)
		if b, err := env.BindGrantee(context.Background(), g, access.Grantee{Kind: access.User, Name: "app_api"}); err != nil || b.Owner != "User team-a/api" {
			t.Fatalf("owned user grantee: %+v, %v", b, err)
		}

		if f.family == familyAntalya {
			testJWTLogin(t, f)
		}
	})
}

// testJWTLogin checks that both token processors of the Antalya fixture
// accept tokens for users pinned to them.
func testJWTLogin(t *testing.T, f *accessFixture) {
	f.srv.exec(t, "CREATE USER app_jwt1 IDENTIFIED WITH jwt PROCESSOR '"+processorTest+"'")
	f.srv.exec(t, "CREATE USER app_jwt2 IDENTIFIED WITH jwt PROCESSOR '"+processorTest2+"'")
	for user, token := range map[string]string{
		"app_jwt1": mintTokenClaims(t, f.jwtKeys[processorTest], jwt.MapClaims{"email": "app_jwt1"}),
		"app_jwt2": mintTokenClaims(t, f.jwtKeys[processorTest2], jwt.MapClaims{"preferred_username": "app_jwt2"}),
	} {
		conn, err := chgo.Open(&chgo.Options{
			Addr:   []string{f.srv.host + ":" + strconv.Itoa(f.srv.nativePort)},
			GetJWT: func(context.Context) (string, error) { return token, nil },
		})
		if err != nil {
			t.Fatal(err)
		}
		var got string
		err = conn.QueryRow(context.Background(), "SELECT currentUser()").Scan(&got)
		_ = conn.Close()
		if err != nil || got != user {
			t.Errorf("JWT login as %s: %q, %v", user, got, err)
		}
	}
}
