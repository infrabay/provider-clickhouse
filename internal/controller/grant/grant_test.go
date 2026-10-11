// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package grant

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/crossplane/crossplane-runtime/v2/pkg/controller"
	"github.com/crossplane/crossplane-runtime/v2/pkg/event"
	"github.com/crossplane/crossplane-runtime/v2/pkg/gate"
	"github.com/crossplane/crossplane-runtime/v2/pkg/meta"
	"github.com/crossplane/crossplane-runtime/v2/pkg/reconciler/managed"
	"github.com/crossplane/crossplane-runtime/v2/pkg/resource"

	"github.com/infrabay/provider-clickhouse/apis/v1alpha1"
	ch "github.com/infrabay/provider-clickhouse/internal/clients/clickhouse"
	"github.com/infrabay/provider-clickhouse/internal/clients/clickhouse/fake"
	"github.com/infrabay/provider-clickhouse/internal/clients/clickhouse/sqlx"
	"github.com/infrabay/provider-clickhouse/internal/controller/access"
	"github.com/infrabay/provider-clickhouse/internal/controller/access/accesstest"
	"github.com/infrabay/provider-clickhouse/internal/controller/ownership"
	"github.com/infrabay/provider-clickhouse/internal/controller/providerconfig"
	"github.com/infrabay/provider-clickhouse/internal/reason"
)

const (
	ns       = "team-a"
	dbName   = "team-a_db"
	roleName = "team-a_reader"
	userName = "team-a_api"
	roleID   = "11111111-1111-1111-1111-111111111111"
	otherID  = "22222222-2222-2222-2222-222222222222"
	userID   = "33333333-3333-3333-3333-333333333333"
	provider = "provider" // fake.NewClient's currentUser()
)

var sharedPC = &xpv2.ProviderConfigReference{Kind: "ClusterProviderConfig", Name: "shared"}

// --- test doubles ---

type recorder struct {
	mu     sync.Mutex
	events []event.Event
}

func (r *recorder) Event(_ runtime.Object, e event.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *recorder) WithAnnotations(...string) event.Recorder { return r }

func (r *recorder) reasons() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []string{}
	for _, e := range r.events {
		out = append(out, string(e.Reason))
	}
	return out
}

func (r *recorder) messages() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var b strings.Builder
	for _, e := range r.events {
		b.WriteString(e.Message + "\n")
	}
	return b.String()
}

func kubeWith(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.SchemeBuilder.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return clientfake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
}

// sharedPolicy is the shared ClusterProviderConfig of design 3.3.
func sharedPolicy() v1alpha1.ClusterPolicy {
	return v1alpha1.ClusterPolicy{
		AllowedNamespaces:    []string{"team-.*"},
		UserNamePatterns:     []string{"{namespace}_[a-z0-9_]+"},
		RoleNamePatterns:     []string{"{namespace}_[a-z0-9_]+"},
		DatabaseNamePatterns: []string{"{namespace}_[a-z0-9_]+"},
	}
}

func resolved(t *testing.T, cp v1alpha1.ClusterPolicy) *providerconfig.Resolved {
	t.Helper()
	p, err := providerconfig.CompilePolicy(cp)
	if err != nil {
		t.Fatal(err)
	}
	return &providerconfig.Resolved{
		Kind: "ClusterProviderConfig", Name: "shared",
		CacheKey:        ch.Key{Identity: "ClusterProviderConfig//shared", Version: "v"},
		Endpoint:        providerconfig.Endpoint{Host: "clickhouse.example.com", Port: 9440, Protocol: v1alpha1.ProtocolNative, TLS: true},
		Policy:          p,
		AdoptionAllowed: p.AdoptionAllowed(),
	}
}

// --- server fixtures ---

// privileges returns system.privileges of a server version, as recorded
// through the driver in the sqlx testdata.
func privileges(t *testing.T, version string) fake.Result {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", "clients", "clickhouse", "sqlx", "testdata", "privileges", version+".tsv"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	privs, err := sqlx.ReadPrivilegesTSV(f)
	if err != nil {
		t.Fatal(err)
	}
	rows := make([][]any, 0, len(privs))
	for _, p := range privs {
		rows = append(rows, []any{p.Name, p.Aliases, p.Level, p.Parent})
	}
	return fake.Rows(rows...)
}

func columnsOf(p fake.Profile) sqlx.GrantColumns {
	return sqlx.GrantColumns{AccessObject: p.AccessObject, IsWildcard: p.IsWildcard}
}

// server is a scripted ClickHouse server of one version.
type server struct {
	t  *testing.T
	p  fake.Profile
	db *fake.DB
}

func newServer(t *testing.T, p fake.Profile) *server {
	t.Helper()
	// The capability probes are scripted first, so that a test can
	// override them.
	db := fake.NewDB().WithProfile(p).WithEntities().On(ch.QueryPrivileges, privileges(t, p.Version))
	return &server{t: t, p: p, db: db}
}

// role adds a role in local storage.
func (s *server) role(name, id string) *server {
	s.db.WithRole(name, id, ch.StorageLocalDirectory)
	return s
}

func (s *server) user(name, id string) *server {
	s.db.WithUser(name, id, ch.StorageLocalDirectory)
	return s
}

// grants scripts the system.grants rows of a grantee, and an empty SHOW
// GRANTS on servers without is_wildcard.
func (s *server) grants(role bool, name string, rows ...sqlx.GrantRow) *server {
	cols := columnsOf(s.p)
	out := make([][]any, 0, len(rows))
	for _, r := range rows {
		v := []any{r.AccessType, r.Database, r.Table, r.Column, b8(r.PartialRevoke), b8(r.GrantOption)}
		if cols.AccessObject {
			v = append(v, r.AccessObject)
		}
		if cols.IsWildcard {
			v = append(v, b8(r.Wildcard))
		}
		out = append(out, v)
	}
	s.db.OnFunc(sqlx.QueryGrants(role, cols), func(p ch.Params) fake.Result {
		if p["grantee"] != name {
			return fake.Rows()
		}
		return fake.Rows(out...)
	})
	if !s.p.IsWildcard {
		if _, ok := s.db.Queries[sqlx.ShowGrants(name)]; !ok {
			s.show(name)
		}
	}
	return s
}

// show scripts SHOW GRANTS FOR name.
func (s *server) show(name string, lines ...string) *server {
	out := make([][]any, 0, len(lines))
	for _, l := range lines {
		out = append(out, []any{l})
	}
	s.db.On(sqlx.ShowGrants(name), fake.Rows(out...))
	return s
}

func b8(b bool) uint8 {
	if b {
		return 1
	}
	return 0
}

// gr builds a row of system.grants: object is "*.*", "db.*", "db.t" or
// "db.t(c)"; flags may contain p (partial revoke), g (grant option) and w
// (wildcard).
func gr(access, object, flags string) sqlx.GrantRow {
	r := sqlx.GrantRow{AccessType: access, PartialRevoke: strings.Contains(flags, "p"), GrantOption: strings.Contains(flags, "g"), Wildcard: strings.Contains(flags, "w")}
	if object == "*.*" {
		return r
	}
	db, rest, _ := strings.Cut(object, ".")
	r.Database = db
	switch i := strings.IndexByte(rest, '('); {
	case rest == "*":
	case i >= 0:
		r.Table, r.Column = rest[:i], strings.TrimSuffix(rest[i+1:], ")")
	default:
		r.Table = rest
	}
	return r
}

// --- MR fixtures ---

type grantOpt func(*v1alpha1.Grant)

func newGrant(opts ...grantOpt) *v1alpha1.Grant {
	g := &v1alpha1.Grant{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "reader-db", UID: "grant-uid"}}
	g.Spec.ProviderConfigReference = sharedPC
	g.Spec.ForProvider.Privileges = []string{"SELECT"}
	g.Spec.ForProvider.Database = new(dbName)
	g.Spec.ForProvider.Role = new(roleName)
	for _, o := range opts {
		o(g)
	}
	return g
}

func privs(p ...string) grantOpt {
	return func(g *v1alpha1.Grant) { g.Spec.ForProvider.Privileges = p }
}

func database(d string) grantOpt {
	return func(g *v1alpha1.Grant) { g.Spec.ForProvider.Database = new(d) }
}

func table(tb string, cols ...string) grantOpt {
	return func(g *v1alpha1.Grant) {
		g.Spec.ForProvider.Table = new(tb)
		g.Spec.ForProvider.Columns = cols
	}
}

func toUser(u string) grantOpt {
	return func(g *v1alpha1.Grant) { g.Spec.ForProvider.Role, g.Spec.ForProvider.User = nil, new(u) }
}

func withGrantOption() grantOpt {
	return func(g *v1alpha1.Grant) { g.Spec.ForProvider.WithGrantOption = new(true) }
}

func deleting() grantOpt {
	return func(g *v1alpha1.Grant) {
		now := metav1.NewTime(time.Now())
		g.SetDeletionTimestamp(&now)
		g.SetFinalizers([]string{"finalizer.managedresource.crossplane.io"})
	}
}

func policies(p ...xpv2.ManagementAction) grantOpt {
	return func(g *v1alpha1.Grant) { g.SetManagementPolicies(p) }
}

func readyStatus(g *v1alpha1.Grant) xpv2.Condition { return g.GetCondition(xpv2.TypeReady) }

// testExternal is an External over a scripted server.
type testExternal struct {
	*External
	cl  *fake.Client
	rec *recorder
}

func newTestExternal(t *testing.T, s *server, res *providerconfig.Resolved, objs ...client.Object) *testExternal {
	t.Helper()
	cl := fake.NewClient(s.db)
	cl.ServerInfo.Version = s.p.Version
	kube := kubeWith(t, objs...)
	rec := &recorder{}
	env := &access.Env{Client: cl, Resolved: res, Kube: kube, Reader: kube, APIReader: kube, Recorder: rec, Journal: ownership.NewJournal()}
	ext, ok := NewExternal(env).(*External)
	if !ok {
		t.Fatal("NewExternal() is not an *External")
	}
	return &testExternal{External: ext, cl: cl, rec: rec}
}

func ownedRole() *v1alpha1.Role {
	return accesstest.OwnedRole(ns, "reader", roleName, roleID, sharedPC)
}
func ownedUser() *v1alpha1.User { return accesstest.OwnedUser(ns, "api", userName, userID, sharedPC) }

// --- Observe ---

func TestObserve(t *testing.T) {
	transport := &ch.TransportError{Category: "dial failed", Message: "connection refused"}
	type tc struct {
		mr          *v1alpha1.Grant
		server      func(t *testing.T) *server
		objs        []client.Object
		policy      *v1alpha1.ClusterPolicy
		want        managed.ExternalObservation
		wantReason  reason.Reason
		wantAnyErr  bool
		wantReady   xpv2.Condition
		wantEvents  []string
		wantNoQuery bool
		wantStatus  *v1alpha1.GrantObservation
		wantNoShow  bool
	}
	up := func(rows ...sqlx.GrantRow) func(t *testing.T) *server {
		return func(t *testing.T) *server {
			return newServer(t, fake.Upstream253).role(roleName, roleID).grants(true, roleName, rows...)
		}
	}
	ready := xpv2.Available()
	unavailable := xpv2.Condition{Type: xpv2.TypeReady, Status: corev1.ConditionFalse}
	cases := map[string]tc{
		"DatabaseUnresolved": {
			mr: newGrant(func(g *v1alpha1.Grant) { g.Spec.ForProvider.Database = nil }), server: up(),
			wantReason: reason.DependencyMissing, wantReady: unavailable, wantNoQuery: true,
		},
		// Nothing was resolved, so nothing was granted.
		"DatabaseUnresolvedWhileDeleting": {
			mr: newGrant(deleting(), func(g *v1alpha1.Grant) { g.Spec.ForProvider.Database = nil }), server: up(),
			wantEvents: []string{"Orphaned"}, wantNoQuery: true,
		},
		"GranteeUnresolved": {
			mr: newGrant(func(g *v1alpha1.Grant) { g.Spec.ForProvider.Role = nil }), server: up(),
			wantReason: reason.DependencyMissing, wantNoQuery: true,
		},
		"TargetOutsidePolicy": {
			mr: newGrant(database("team-b_db")), server: up(),
			wantReason: reason.PolicyViolation, wantReady: unavailable, wantNoQuery: true,
		},
		// D-25: a target outside the policy is never read or revoked, even
		// with a forged owner of the grantee.
		"TargetOutsidePolicyWhileDeleting": {
			mr: newGrant(database("team-b_db"), deleting()), server: up(gr("SELECT", "team-b_db.*", "")), objs: []client.Object{ownedRole()},
			wantEvents: []string{"OrphanedByPolicy"}, wantNoQuery: true,
		},
		"GranteeOutsidePolicyWhileDeleting": {
			mr: newGrant(deleting(), func(g *v1alpha1.Grant) { g.Spec.ForProvider.Role = new("team-b_reader") }), server: up(),
			wantEvents: []string{"OrphanedByPolicy"}, wantNoQuery: true,
		},
		"GlobalTargetNeedsAllowGlobalGrants": {
			mr: newGrant(database("*")), server: up(), objs: []client.Object{ownedRole()},
			wantReason: reason.PolicyViolation, wantReady: unavailable,
		},
		"GlobalTargetObserveOnly": {
			mr:     newGrant(database("*"), policies(xpv2.ManagementActionObserve)),
			server: up(gr("SELECT", "*.*", "")),
			want:   managed.ExternalObservation{ResourceExists: true, ResourceUpToDate: true}, wantReady: ready,
		},
		"GrantOptionNotAllowed": {
			mr: newGrant(withGrantOption()), server: up(), objs: []client.Object{ownedRole()},
			wantReason: reason.PolicyViolation, wantReady: unavailable,
		},
		"GranteeNotOwned": {
			mr: newGrant(), server: up(gr("SELECT", dbName+".*", "")),
			wantReason: reason.NotOwned, wantReady: unavailable,
		},
		// The role was dropped and re-created by someone else: the owner's
		// claim names the old UUID.
		"GranteeReplaced": {
			mr: newGrant(),
			server: func(t *testing.T) *server {
				return newServer(t, fake.Upstream253).role(roleName, otherID).grants(true, roleName)
			},
			objs: []client.Object{ownedRole()}, wantReason: reason.NotOwned,
		},
		"GranteeMissing": {
			mr: newGrant(), server: func(t *testing.T) *server { return newServer(t, fake.Upstream253) }, objs: []client.Object{ownedRole()},
			wantReason: reason.DependencyMissing, wantReady: unavailable,
		},
		"GranteeMissingWhileDeleting": {
			mr: newGrant(deleting()), server: func(t *testing.T) *server { return newServer(t, fake.Upstream253) }, objs: []client.Object{ownedRole()},
		},
		"Ambiguous": {
			mr:     newGrant(),
			server: func(t *testing.T) *server { return up()(t).user(roleName, userID) }, objs: []client.Object{ownedRole()},
			wantReason: reason.AmbiguousGrantee, wantReady: unavailable,
		},
		"AmbiguousWhileDeleting": {
			mr:     newGrant(deleting()),
			server: func(t *testing.T) *server { return up(gr("SELECT", dbName+".*", ""))(t).user(roleName, userID) }, objs: []client.Object{ownedRole()},
			wantEvents: []string{"Orphaned"},
		},
		"SelfAsGrantee": {
			mr: newGrant(toUser(provider)),
			server: func(t *testing.T) *server {
				return newServer(t, fake.Upstream253).user(provider, userID).grants(false, provider)
			},
			policy: &v1alpha1.ClusterPolicy{Unrestricted: true}, wantReason: reason.SelfModification,
		},
		"Absent": {
			mr: newGrant(), server: up(), objs: []client.Object{ownedRole()},
			want:       managed.ExternalObservation{ResourceExists: false},
			wantStatus: &v1alpha1.GrantObservation{Grantee: "role:" + roleName, Target: dbName + ".*"},
		},
		"UpToDate": {
			mr: newGrant(privs("select", "INSERT")), server: up(gr("SELECT", dbName+".*", ""), gr("INSERT", dbName+".*", "")), objs: []client.Object{ownedRole()},
			want: managed.ExternalObservation{ResourceExists: true, ResourceUpToDate: true}, wantReady: ready,
			wantStatus: &v1alpha1.GrantObservation{Grantee: "role:" + roleName, Target: dbName + ".*", Privileges: []string{"INSERT", "SELECT"}},
		},
		"UserGrantee": {
			mr: newGrant(toUser(userName)),
			server: func(t *testing.T) *server {
				return newServer(t, fake.Upstream253).user(userName, userID).grants(false, userName, gr("SELECT", dbName+".*", ""))
			},
			objs: []client.Object{ownedUser()},
			want: managed.ExternalObservation{ResourceExists: true, ResourceUpToDate: true}, wantReady: ready,
		},
		"PrivilegeMissing": {
			mr: newGrant(privs("SELECT", "INSERT")), server: up(gr("SELECT", dbName+".*", "")), objs: []client.Object{ownedRole()},
			want: managed.ExternalObservation{ResourceExists: true, ResourceUpToDate: false}, wantReady: unavailable,
		},
		"ForeignPrivilegeAtTarget": {
			mr: newGrant(), server: up(gr("SELECT", dbName+".*", ""), gr("INSERT", dbName+".*", "")), objs: []client.Object{ownedRole()},
			want: managed.ExternalObservation{ResourceExists: true, ResourceUpToDate: false}, wantReady: ready,
		},
		"CoveredByBroaderGrant": {
			mr: newGrant(table("events")), server: up(gr("SELECT", dbName+".*", "")), objs: []client.Object{ownedRole()},
			want: managed.ExternalObservation{ResourceExists: true, ResourceUpToDate: true}, wantReady: ready,
			wantStatus: &v1alpha1.GrantObservation{Grantee: "role:" + roleName, Target: dbName + ".events"},
		},
		"PartialRevokeAtTarget": {
			mr: newGrant(table("events")), server: up(gr("SELECT", dbName+".events", "p"), gr("SELECT", dbName+".*", "")), objs: []client.Object{ownedRole()},
			want: managed.ExternalObservation{ResourceExists: false},
		},
		"InvalidPrivilege": {
			mr: newGrant(privs("FROBNICATE")), server: up(), objs: []client.Object{ownedRole()},
			wantReason: reason.InvalidSpec, wantReady: unavailable,
		},
		"SourcePrivilege": {
			mr: newGrant(privs("SELECT", "S3")), server: up(), objs: []client.Object{ownedRole()},
			wantReason: reason.InvalidSpec, wantReady: unavailable,
		},
		"WrongLevel": {
			mr: newGrant(privs("CREATE DATABASE"), table("events")), server: up(), objs: []client.Object{ownedRole()},
			wantReason: reason.InvalidSpec,
		},
		"InvalidPrivilegeWhileDeleting": {
			mr: newGrant(privs("FROBNICATE"), deleting()), server: up(), objs: []client.Object{ownedRole()},
			wantEvents: []string{"Orphaned"},
		},
		// D-10: without is_wildcard, a wildcard grant looks like a literal
		// one; the grantee's grants are not managed.
		"WildcardWithoutIsWildcard": {
			mr: newGrant(),
			server: func(t *testing.T) *server {
				return up(gr("SELECT", "team-a_*.*", ""))(t).show(roleName, "GRANT SELECT ON `team-a_`*.* TO `team-a_reader`")
			},
			objs: []client.Object{ownedRole()}, wantReason: reason.Unsupported, wantReady: unavailable,
		},
		"WildcardWithoutIsWildcardWhileDeleting": {
			mr: newGrant(deleting()),
			server: func(t *testing.T) *server {
				return up(gr("SELECT", dbName+".*", ""))(t).show(roleName, "GRANT SELECT ON team_*.* TO `team-a_reader`")
			},
			objs: []client.Object{ownedRole()}, wantEvents: []string{"Orphaned"},
		},
		// 26.8 has is_wildcard: the wildcard covers the target, and SHOW
		// GRANTS is not read.
		"WildcardCovers268": {
			mr: newGrant(database("team-a_x")),
			server: func(t *testing.T) *server {
				return newServer(t, fake.Upstream268).role(roleName, roleID).grants(true, roleName, gr("SELECT", "team-a_.*", "w"))
			},
			objs: []client.Object{ownedRole()},
			want: managed.ExternalObservation{ResourceExists: true, ResourceUpToDate: true}, wantReady: ready, wantNoShow: true,
		},
		"Antalya266Rows": {
			mr: newGrant(privs("ALTER")),
			server: func(t *testing.T) *server {
				return newServer(t, fake.Antalya266).role(roleName, roleID).grants(true, roleName, gr("ALTER", dbName+".*", ""),
					func() sqlx.GrantRow { r := gr("READ", "*.*", ""); r.AccessObject = "S3"; return r }())
			},
			objs: []client.Object{ownedRole()},
			want: managed.ExternalObservation{ResourceExists: true, ResourceUpToDate: true}, wantReady: ready,
		},
		"GrantsReadDenied": {
			mr: newGrant(),
			server: func(t *testing.T) *server {
				s := newServer(t, fake.Upstream253).role(roleName, roleID)
				s.db.On(sqlx.QueryGrants(true, sqlx.GrantColumns{}), fake.Error(fake.Exception(497, "necessary to have the grant SELECT ON system.grants")))
				return s
			},
			objs: []client.Object{ownedRole()}, wantReason: reason.PermissionDenied,
		},
		"ShowGrantsDenied": {
			mr: newGrant(),
			server: func(t *testing.T) *server {
				s := up()(t)
				s.db.On(sqlx.ShowGrants(roleName), fake.Error(fake.Exception(497, "necessary to have the grant SHOW ROLES")))
				return s
			},
			objs: []client.Object{ownedRole()}, wantReason: reason.PermissionDenied,
		},
		"TransportErrorIsNotNotFound": {
			mr: newGrant(),
			server: func(t *testing.T) *server {
				s := newServer(t, fake.Upstream253).role(roleName, roleID)
				s.db.On(sqlx.QueryGrants(true, sqlx.GrantColumns{}), fake.Error(transport))
				return s
			},
			objs: []client.Object{ownedRole()}, wantAnyErr: true,
		},
		"CapabilityProbeFails": {
			mr: newGrant(),
			server: func(t *testing.T) *server {
				s := up()(t)
				s.db.On(ch.QuerySystemColumnProbe(ch.TableGrants, ch.ColumnAccessObject), fake.Error(fake.Exception(497, "necessary to have the grant SELECT ON system.grants")))
				return s
			},
			objs: []client.Object{ownedRole()}, wantReason: reason.PermissionDenied,
		},
		"CatalogueFails": {
			mr: newGrant(),
			server: func(t *testing.T) *server {
				s := up()(t)
				s.db.On(ch.QueryPrivileges, fake.Error(transport))
				return s
			},
			objs: []client.Object{ownedRole()}, wantAnyErr: true,
		},
		// While deleting, the Grant exists only while Delete has something
		// to revoke.
		"ExactWhileDeleting": {
			mr: newGrant(deleting()), server: up(gr("SELECT", dbName+".*", "")), objs: []client.Object{ownedRole()},
			want: managed.ExternalObservation{ResourceExists: true},
		},
		"CoveredWhileDeleting": {
			mr: newGrant(deleting(), table("events")), server: up(gr("SELECT", dbName+".*", "")), objs: []client.Object{ownedRole()},
			want: managed.ExternalObservation{ResourceExists: false}, wantEvents: []string{},
		},
		"ForeignOnlyWhileDeleting": {
			mr: newGrant(deleting()), server: up(gr("INSERT", dbName+".*", "")), objs: []client.Object{ownedRole()},
			want: managed.ExternalObservation{ResourceExists: false},
		},
		// D-26: without Delete nothing is ever revoked.
		"NoDeletePolicyWhileDeleting": {
			mr:     newGrant(deleting(), policies(xpv2.ManagementActionObserve, xpv2.ManagementActionCreate, xpv2.ManagementActionUpdate)),
			server: up(gr("SELECT", dbName+".*", "")), objs: []client.Object{ownedRole()},
			wantEvents: []string{"Orphaned"}, wantNoQuery: true,
		},
		// Observe-only Grants may report the grants of any grantee that the
		// name policy allows (D-30).
		"ObserveOnlyNotOwned": {
			mr:     newGrant(policies(xpv2.ManagementActionObserve), privs("SELECT", "INSERT")),
			server: up(gr("SELECT", dbName+".*", "")),
			want:   managed.ExternalObservation{ResourceExists: true, ResourceUpToDate: false}, wantReady: unavailable,
		},
		"ObserveOnlyGlobalWithSources": {
			mr:     newGrant(policies(xpv2.ManagementActionObserve), database("*")),
			server: up(gr("SELECT", "*.*", ""), gr("S3", "*.*", ""), gr("CREATE USER", "*.*", "")),
			want:   managed.ExternalObservation{ResourceExists: true, ResourceUpToDate: true}, wantReady: ready,
			wantStatus: &v1alpha1.GrantObservation{Grantee: "role:" + roleName, Target: "*.*", Privileges: []string{"SELECT"}},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s := tc.server(t)
			cp := sharedPolicy()
			if tc.policy != nil {
				cp = *tc.policy
			}
			e := newTestExternal(t, s, resolved(t, cp), append([]client.Object{tc.mr.DeepCopy()}, tc.objs...)...)
			got, err := e.Observe(context.Background(), tc.mr)
			switch {
			case tc.wantAnyErr:
				if err == nil || got.ResourceExists {
					t.Fatalf("an error must be returned, never 'not found': %+v, %v", got, err)
				}
			case tc.wantReason != "":
				if !reason.Is(err, tc.wantReason) {
					t.Fatalf("Observe() error = %v, want %s", err, tc.wantReason)
				}
			case err != nil:
				t.Fatalf("Observe() error = %v", err)
			default:
				if diff := cmp.Diff(tc.want, got); diff != "" {
					t.Errorf("Observe() -want +got:\n%s", diff)
				}
			}
			if tc.wantReady.Type != "" {
				r := readyStatus(tc.mr)
				if r.Status != tc.wantReady.Status || (tc.wantReady.Reason != "" && r.Reason != tc.wantReady.Reason) {
					t.Errorf("Ready = %s/%s, want %s/%s", r.Status, r.Reason, tc.wantReady.Status, tc.wantReady.Reason)
				}
			}
			if tc.wantEvents != nil {
				if diff := cmp.Diff(tc.wantEvents, e.rec.reasons()); diff != "" {
					t.Errorf("events -want +got:\n%s", diff)
				}
			}
			if tc.wantStatus != nil {
				if diff := cmp.Diff(*tc.wantStatus, tc.mr.Status.AtProvider, cmpopts.EquateEmpty()); diff != "" {
					t.Errorf("status -want +got:\n%s", diff)
				}
			}
			if tc.wantNoQuery {
				for _, q := range s.db.Seen {
					if q != ch.QueryPrivileges && !strings.Contains(q, "system.user_directories") {
						t.Errorf("no query may run, ran %q", q)
					}
				}
			}
			if tc.wantNoShow && slices.Contains(s.db.Seen, sqlx.ShowGrants(roleName)) {
				t.Error("SHOW GRANTS must not be read on a server with is_wildcard")
			}
			if len(s.db.Execs) > 0 {
				t.Errorf("Observe must never write, ran %v", s.db.Execs)
			}
		})
	}
}

// TestObserveMessages checks the condition messages that name what is
// missing or unsupported.
func TestObserveMessages(t *testing.T) {
	s := newServer(t, fake.Upstream253).role(roleName, roleID).grants(true, roleName)
	e := newTestExternal(t, s, resolved(t, sharedPolicy()), ownedRole())
	mr := newGrant(privs("ALL"))
	s.grants(true, roleName, gr("SELECT", dbName+".*", ""))
	if _, err := e.Observe(context.Background(), mr); err != nil {
		t.Fatal(err)
	}
	msg := readyStatus(mr).Message
	if !strings.Contains(msg, "role:"+roleName+" does not hold ALTER ADD COLUMN") || !strings.Contains(msg, "more on "+dbName+".*") {
		t.Errorf("Ready message = %q", msg)
	}

	s = newServer(t, fake.Upstream253).role(roleName, roleID).grants(true, roleName).show(roleName, "GRANT SELECT ON app_*.* TO r")
	e = newTestExternal(t, s, resolved(t, sharedPolicy()), ownedRole())
	_, err := e.Observe(context.Background(), newGrant())
	if !reason.Is(err, reason.Unsupported) || !strings.Contains(err.Error(), "ON app_*.*") || !strings.Contains(err.Error(), "25.3.3.42") {
		t.Errorf("wildcard error = %v", err)
	}
}

// --- Create and Update ---

func TestApply(t *testing.T) {
	type tc struct {
		mr         *v1alpha1.Grant
		server     func(t *testing.T) *server
		objs       []client.Object
		policy     *v1alpha1.ClusterPolicy
		multi      bool
		execErr    map[string]error
		change     func(s *server)
		wantStmts  []string
		wantReason reason.Reason
	}
	up := func(rows ...sqlx.GrantRow) func(t *testing.T) *server {
		return func(t *testing.T) *server {
			return newServer(t, fake.Upstream253).role(roleName, roleID).grants(true, roleName, rows...)
		}
	}
	grantSelect := "GRANT SELECT ON `team-a_db`.* TO `team-a_reader`"
	cases := map[string]tc{
		"Grants": {
			mr: newGrant(), server: up(), objs: []client.Object{ownedRole()},
			wantStmts: []string{grantSelect},
		},
		"GrantsAndRevokesForeign": {
			mr: newGrant(privs("SELECT", "SHOW TABLES")), server: up(gr("SELECT", dbName+".*", ""), gr("INSERT", dbName+".*", "")), objs: []client.Object{ownedRole()},
			wantStmts: []string{"GRANT SHOW TABLES ON `team-a_db`.* TO `team-a_reader`", "REVOKE INSERT ON `team-a_db`.* FROM `team-a_reader`"},
		},
		"Columns": {
			mr: newGrant(privs("SELECT", "INSERT"), table("t", "b", "a")), server: up(gr("SELECT", dbName+".t(a)", "")), objs: []client.Object{ownedRole()},
			wantStmts: []string{"GRANT INSERT(`a`, `b`), SELECT(`a`, `b`) ON `team-a_db`.`t` TO `team-a_reader`"},
		},
		"WithGrantOption": {
			mr: newGrant(withGrantOption(), table("t")), server: up(gr("SELECT", dbName+".*", "")), objs: []client.Object{ownedRole()},
			policy:    &v1alpha1.ClusterPolicy{AllowedNamespaces: []string{"team-.*"}, RoleNamePatterns: []string{"{namespace}_[a-z0-9_]+"}, DatabaseNamePatterns: []string{"{namespace}_[a-z0-9_]+"}, AllowGrantOption: true},
			wantStmts: []string{"GRANT SELECT ON `team-a_db`.`t` TO `team-a_reader` WITH GRANT OPTION"},
		},
		"DropsGrantOption": {
			mr: newGrant(), server: up(gr("SELECT", dbName+".*", "g")), objs: []client.Object{ownedRole()},
			wantStmts: []string{"REVOKE GRANT OPTION FOR SELECT ON `team-a_db`.* FROM `team-a_reader`"},
		},
		"UserGrantee": {
			mr: newGrant(toUser(userName)),
			server: func(t *testing.T) *server {
				return newServer(t, fake.Upstream253).user(userName, userID).grants(false, userName)
			},
			objs: []client.Object{ownedUser()}, wantStmts: []string{"GRANT SELECT ON `team-a_db`.* TO `team-a_api`"},
		},
		"NothingToDo": {
			mr: newGrant(), server: up(gr("SELECT", dbName+".*", "")), objs: []client.Object{ownedRole()},
		},
		// The role was dropped and re-created between Observe and the write.
		"ReplacedSinceObserve": {
			mr: newGrant(), server: up(), objs: []client.Object{ownedRole()},
			change:     func(s *server) { s.db.WithRole(roleName, otherID, ch.StorageLocalDirectory) },
			wantReason: reason.NotOwned,
		},
		"VanishedSinceObserve": {
			mr: newGrant(), server: up(), objs: []client.Object{ownedRole()},
			change:     func(s *server) { s.db.RemoveRole(roleName) },
			wantReason: reason.DependencyMissing,
		},
		"ShadowedSinceObserve": {
			mr: newGrant(), server: up(), objs: []client.Object{ownedRole()},
			change:     func(s *server) { s.db.WithUser(roleName, userID, ch.StorageLocalDirectory) },
			wantReason: reason.AmbiguousGrantee,
		},
		"WildcardSinceObserve": {
			mr: newGrant(), server: up(), objs: []client.Object{ownedRole()},
			change:     func(s *server) { s.show(roleName, "GRANT SELECT ON team_*.* TO `team-a_reader`") },
			wantReason: reason.Unsupported,
		},
		"NotWritable": {
			mr: newGrant(), server: up(), objs: []client.Object{ownedRole()}, multi: true,
			wantReason: reason.AccessStorageNotReplicated,
		},
		// The server-side ceiling: the identity lacks the privilege WITH
		// GRANT OPTION.
		"ServerCeiling": {
			mr: newGrant(privs("ALTER UPDATE")), server: up(), objs: []client.Object{ownedRole()},
			execErr:    map[string]error{"GRANT ALTER UPDATE ON `team-a_db`.* TO `team-a_reader`": fake.Exception(497, "Not enough privileges. To execute this query, it's necessary to have the grant ALTER UPDATE ON `team-a_db`.* WITH GRANT OPTION")},
			wantStmts:  []string{"GRANT ALTER UPDATE ON `team-a_db`.* TO `team-a_reader`"},
			wantReason: reason.PermissionDenied,
		},
		"InvalidGrant": {
			mr: newGrant(), server: up(), objs: []client.Object{ownedRole()},
			execErr:    map[string]error{grantSelect: fake.Exception(509, "SELECT cannot be granted on the database level")},
			wantStmts:  []string{grantSelect},
			wantReason: reason.InvalidSpec,
		},
		"GranteeGoneOnExec": {
			mr: newGrant(), server: up(), objs: []client.Object{ownedRole()},
			execErr:    map[string]error{grantSelect: fake.Exception(511, "There is no role `team-a_reader` in user directories")},
			wantStmts:  []string{grantSelect},
			wantReason: reason.DependencyMissing,
		},
	}
	for name, tc := range cases {
		for _, op := range []string{"Create", "Update"} {
			t.Run(name+"/"+op, func(t *testing.T) {
				s := tc.server(t)
				cp := sharedPolicy()
				if tc.policy != nil {
					cp = *tc.policy
				}
				e := newTestExternal(t, s, resolved(t, cp), append([]client.Object{tc.mr.DeepCopy()}, tc.objs...)...)
				if _, err := e.Observe(context.Background(), tc.mr); err != nil {
					t.Fatalf("Observe() = %v", err)
				}
				if tc.multi {
					e.cl.Servers = []string{"s1", "s2"}
				}
				if tc.change != nil {
					tc.change(s)
				}
				for stmt, err := range tc.execErr {
					s.db.OnExec(stmt, err)
				}
				var err error
				if op == "Create" {
					_, err = e.Create(context.Background(), tc.mr)
				} else {
					_, err = e.Update(context.Background(), tc.mr)
				}
				if tc.wantReason != "" {
					if !reason.Is(err, tc.wantReason) {
						t.Fatalf("%s() error = %v, want %s", op, err, tc.wantReason)
					}
				} else if err != nil {
					t.Fatalf("%s() error = %v", op, err)
				}
				if diff := cmp.Diff(tc.wantStmts, s.db.Execs, cmpopts.EquateEmpty()); diff != "" {
					t.Errorf("statements -want +got:\n%s", diff)
				}
			})
		}
	}
}

func TestApplyRequiresBinding(t *testing.T) {
	s := newServer(t, fake.Upstream253).role(roleName, roleID).grants(true, roleName)
	e := newTestExternal(t, s, resolved(t, sharedPolicy()), ownedRole())
	if _, err := e.Create(context.Background(), newGrant()); !reason.Is(err, reason.Internal) {
		t.Fatalf("Create() without Observe = %v, want Internal", err)
	}
	if _, err := e.Delete(context.Background(), newGrant(deleting())); !reason.Is(err, reason.Internal) {
		t.Fatalf("Delete() without Observe = %v, want Internal", err)
	}
	// The binding is for one grantee only.
	if _, err := e.Observe(context.Background(), newGrant()); err != nil {
		t.Fatal(err)
	}
	other := newGrant(toUser(userName))
	if _, err := e.Update(context.Background(), other); !reason.Is(err, reason.Internal) {
		t.Fatalf("Update() of another grantee = %v, want Internal", err)
	}
	if len(s.db.Execs) > 0 {
		t.Errorf("nothing may be written: %v", s.db.Execs)
	}
}

// TestApplyHonoursManagementPolicies: an observe-only Grant binds its
// grantee without an ownership check, so Create and Update refuse to write
// for it even if crossplane-runtime called them (D-26).
func TestApplyHonoursManagementPolicies(t *testing.T) {
	ctx := context.Background()
	s := newServer(t, fake.Upstream253).role(roleName, roleID).grants(true, roleName)
	observeOnly := newGrant(policies(xpv2.ManagementActionObserve))
	e := newTestExternal(t, s, resolved(t, sharedPolicy()), observeOnly.DeepCopy())
	if _, err := e.Observe(ctx, observeOnly); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Create(ctx, observeOnly); !reason.Is(err, reason.Internal) {
		t.Errorf("Create() of an observe-only Grant = %v, want Internal", err)
	}
	if _, err := e.Update(ctx, observeOnly); !reason.Is(err, reason.Internal) {
		t.Errorf("Update() of an observe-only Grant = %v, want Internal", err)
	}

	createOnly := newGrant(policies(xpv2.ManagementActionObserve, xpv2.ManagementActionCreate))
	e = newTestExternal(t, s, resolved(t, sharedPolicy()), createOnly.DeepCopy(), ownedRole())
	if _, err := e.Observe(ctx, createOnly); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Update(ctx, createOnly); !reason.Is(err, reason.Internal) {
		t.Errorf("Update() without the Update policy = %v, want Internal", err)
	}
	if len(s.db.Execs) > 0 {
		t.Errorf("nothing may be written: %v", s.db.Execs)
	}
	if _, err := e.Create(ctx, createOnly); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]string{"GRANT SELECT ON `team-a_db`.* TO `team-a_reader`"}, s.db.Execs); diff != "" {
		t.Errorf("Create() -want +got:\n%s", diff)
	}
}

func TestApplyRechecksPolicy(t *testing.T) {
	s := newServer(t, fake.Upstream253).role(roleName, roleID).grants(true, roleName)
	e := newTestExternal(t, s, resolved(t, sharedPolicy()), ownedRole())
	mr := newGrant()
	if _, err := e.Observe(context.Background(), mr); err != nil {
		t.Fatal(err)
	}
	mr.Spec.ForProvider.Database = new("team-b_db")
	if _, err := e.Create(context.Background(), mr); !reason.Is(err, reason.PolicyViolation) {
		t.Fatalf("Create() = %v, want PolicyViolation", err)
	}
	mr.Spec.ForProvider.Database = new(dbName)
	mr.Spec.ForProvider.WithGrantOption = new(true)
	if _, err := e.Update(context.Background(), mr); !reason.Is(err, reason.PolicyViolation) {
		t.Fatalf("Update() = %v, want PolicyViolation", err)
	}
	mr.Spec.ForProvider.Database = nil
	if _, err := e.Update(context.Background(), mr); !reason.Is(err, reason.DependencyMissing) {
		t.Fatalf("Update() = %v, want DependencyMissing", err)
	}
	if len(s.db.Execs) > 0 {
		t.Errorf("nothing may be written: %v", s.db.Execs)
	}
}

// --- Delete ---

func TestDelete(t *testing.T) {
	type tc struct {
		mr         *v1alpha1.Grant
		server     func(t *testing.T) *server
		objs       []client.Object
		change     func(s *server, mr *v1alpha1.Grant)
		execErr    map[string]error
		wantStmts  []string
		wantReason reason.Reason
	}
	up := func(rows ...sqlx.GrantRow) func(t *testing.T) *server {
		return func(t *testing.T) *server {
			return newServer(t, fake.Upstream253).role(roleName, roleID).grants(true, roleName, rows...)
		}
	}
	revokeSelect := "REVOKE SELECT ON `team-a_db`.* FROM `team-a_reader`"
	cases := map[string]tc{
		"RevokesWhatItOwns": {
			mr: newGrant(deleting()), server: up(gr("SELECT", dbName+".*", ""), gr("INSERT", dbName+".*", "")), objs: []client.Object{ownedRole()},
			wantStmts: []string{revokeSelect},
		},
		"NarrowOptionRowUnderBroaderGrant": {
			mr:     newGrant(deleting(), table("t")),
			server: up(gr("SELECT", dbName+".*", ""), gr("SELECT", dbName+".t", "g")), objs: []client.Object{ownedRole()},
			wantStmts: []string{"REVOKE GRANT OPTION FOR SELECT ON `team-a_db`.`t` FROM `team-a_reader`"},
		},
		"Columns": {
			mr:     newGrant(deleting(), privs("SELECT", "INSERT"), table("t", "a", "b")),
			server: up(gr("SELECT", dbName+".t(a)", ""), gr("INSERT", dbName+".t(b)", ""), gr("UPDATE", dbName+".t(a)", "")), objs: []client.Object{ownedRole()},
			wantStmts: []string{"REVOKE INSERT(`b`), SELECT(`a`) ON `team-a_db`.`t` FROM `team-a_reader`"},
		},
		"CoveredSinceObserve": {
			mr: newGrant(deleting()), server: up(gr("SELECT", dbName+".*", "")), objs: []client.Object{ownedRole()},
			change: func(s *server, _ *v1alpha1.Grant) {
				s.grants(true, roleName, gr("SELECT", "*.*", ""))
			},
		},
		"GranteeGone": {
			mr: newGrant(deleting()), server: up(gr("SELECT", dbName+".*", "")), objs: []client.Object{ownedRole()},
			change: func(s *server, _ *v1alpha1.Grant) { s.db.RemoveRole(roleName) },
		},
		"GranteeGoneOnExec": {
			mr: newGrant(deleting()), server: up(gr("SELECT", dbName+".*", "")), objs: []client.Object{ownedRole()},
			execErr:   map[string]error{revokeSelect: fake.Exception(511, "There is no role")},
			wantStmts: []string{revokeSelect},
		},
		"Replaced": {
			mr: newGrant(deleting()), server: up(gr("SELECT", dbName+".*", "")), objs: []client.Object{ownedRole()},
			change:     func(s *server, _ *v1alpha1.Grant) { s.db.WithRole(roleName, otherID, ch.StorageLocalDirectory) },
			wantReason: reason.NotOwned,
		},
		"OutsidePolicy": {
			mr: newGrant(deleting()), server: up(gr("SELECT", dbName+".*", "")), objs: []client.Object{ownedRole()},
			change:     func(_ *server, mr *v1alpha1.Grant) { mr.Spec.ForProvider.Database = new("team-b_db") },
			wantReason: reason.PolicyViolation,
		},
		"ServerCeiling": {
			mr: newGrant(deleting()), server: up(gr("SELECT", dbName+".*", "")), objs: []client.Object{ownedRole()},
			execErr:    map[string]error{revokeSelect: fake.Exception(497, "Not enough privileges")},
			wantStmts:  []string{revokeSelect},
			wantReason: reason.PermissionDenied,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s := tc.server(t)
			e := newTestExternal(t, s, resolved(t, sharedPolicy()), append([]client.Object{tc.mr.DeepCopy()}, tc.objs...)...)
			obs, err := e.Observe(context.Background(), tc.mr)
			if err != nil || !obs.ResourceExists {
				t.Fatalf("Observe() = %+v, %v", obs, err)
			}
			if tc.change != nil {
				tc.change(s, tc.mr)
			}
			for stmt, err := range tc.execErr {
				s.db.OnExec(stmt, err)
			}
			_, err = e.Delete(context.Background(), tc.mr)
			if tc.wantReason != "" {
				if !reason.Is(err, tc.wantReason) {
					t.Fatalf("Delete() error = %v, want %s", err, tc.wantReason)
				}
			} else if err != nil {
				t.Fatalf("Delete() error = %v", err)
			}
			if diff := cmp.Diff(tc.wantStmts, s.db.Execs, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("statements -want +got:\n%s", diff)
			}
			if c := readyStatus(tc.mr); c.Reason != xpv2.ReasonDeleting {
				t.Errorf("Ready reason = %q, want Deleting", c.Reason)
			}
		})
	}
}

// --- wiring ---

func TestReferences(t *testing.T) {
	g := newGrant(func(g *v1alpha1.Grant) {
		g.Spec.ForProvider.DatabaseRef = &xpv2.NamespacedReference{Name: "db"}
		g.Spec.ForProvider.UserSelector = &xpv2.NamespacedSelector{MatchLabels: map[string]string{"a": "b"}}
		g.Spec.ForProvider.RoleRef = &xpv2.NamespacedReference{Name: "r"}
	})
	refs := References(g)
	if len(refs) != 3 || refs[0].Field != "databaseRef" || refs[0].Ref.Name != "db" || refs[1].Selector == nil || refs[2].Ref.Name != "r" {
		t.Fatalf("References() = %+v", refs)
	}
	if _, ok := refs[0].New().(*v1alpha1.Database); !ok {
		t.Errorf("databaseRef: New() = %T", refs[0].New())
	}
	if _, ok := refs[1].New().(*v1alpha1.User); !ok {
		t.Errorf("userRef: New() = %T", refs[1].New())
	}
	if _, ok := refs[2].New().(*v1alpha1.Role); !ok {
		t.Errorf("roleRef: New() = %T", refs[2].New())
	}
	if referencesOf(&v1alpha1.Role{}) != nil {
		t.Error("referencesOf() of another kind must be nil")
	}
	if len(referencesOf(g)) != 3 {
		t.Error("referencesOf() of a Grant")
	}
}

// TestSetupGated: the controller is registered with safe-start and is not
// set up before the Grant CRD is installed (Setup would need a manager).
func TestSetupGated(t *testing.T) {
	o := controller.Options{Gate: new(gate.Gate[schema.GroupVersionKind])}
	if err := SetupGated(nil, o, access.Dependencies{}); err != nil {
		t.Fatal(err)
	}
}

type nopTracker struct{}

func (nopTracker) Track(context.Context, resource.ModernManaged) error { return nil }

type resolverFn func() (*providerconfig.Resolved, error)

func (f resolverFn) Resolve(context.Context, resource.ModernManaged) (*providerconfig.Resolved, error) {
	return f()
}

type cacheFn func() (ch.Client, error)

func (f cacheFn) Acquire(context.Context, ch.Key, ch.Config) (ch.Client, error) { return f() }

// TestConnector checks that the access connector builds Grant clients and
// rejects a reference to an MR of another ProviderConfig.
func TestConnector(t *testing.T) {
	db := &v1alpha1.Database{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "db"}}
	db.Spec.ProviderConfigReference = &xpv2.ProviderConfigReference{Kind: "ClusterProviderConfig", Name: "other"}
	kube := kubeWith(t, db)
	cl := fake.NewClient(fake.NewDB())
	c := &access.Connector[*v1alpha1.Grant]{
		Kind: kind, Kube: kube, Usage: nopTracker{},
		Resolver:  resolverFn(func() (*providerconfig.Resolved, error) { return resolved(t, sharedPolicy()), nil }),
		Cache:     cacheFn(func() (ch.Client, error) { return cl, nil }),
		NewClient: NewExternal, References: References,
	}
	ext, err := c.Connect(context.Background(), newGrant())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ext.(*External); !ok {
		t.Fatalf("Connect() = %T", ext)
	}
	if err := ext.Disconnect(context.Background()); err != nil || cl.Closed != 1 {
		t.Errorf("Disconnect() = %v, closed %d", err, cl.Closed)
	}
	mr := newGrant(func(g *v1alpha1.Grant) { g.Spec.ForProvider.DatabaseRef = &xpv2.NamespacedReference{Name: "db"} })
	if _, err := c.Connect(context.Background(), mr); !reason.Is(err, reason.InvalidSpec) {
		t.Errorf("a reference to an MR of another ProviderConfig: %v, want InvalidSpec", err)
	}
}

// TestForgedOwnerClaim: a claim copied onto the grantee's Role from another
// MR (owner-uid is not the Role's UID) does not make the role owned, so
// deleting the Grant revokes nothing.
func TestForgedOwnerClaim(t *testing.T) {
	forged := ownedRole()
	meta.AddAnnotations(forged, map[string]string{v1alpha1.AnnotationOwnerUID: "someone-else"})
	s := newServer(t, fake.Upstream253).role(roleName, roleID).grants(true, roleName, gr("SELECT", dbName+".*", ""))
	mr := newGrant(deleting())
	e := newTestExternal(t, s, resolved(t, sharedPolicy()), mr.DeepCopy(), forged)
	obs, err := e.Observe(context.Background(), mr)
	if err != nil || obs.ResourceExists {
		t.Fatalf("Observe() = %+v, %v", obs, err)
	}
	if diff := cmp.Diff([]string{"Orphaned"}, e.rec.reasons()); diff != "" {
		t.Errorf("events: %s", diff)
	}
	if !strings.Contains(e.rec.messages(), "left in place") {
		t.Errorf("event message: %q", e.rec.messages())
	}
	if _, err := e.Delete(context.Background(), mr); !reason.Is(err, reason.Internal) {
		t.Errorf("Delete() of an unbound grantee = %v, want Internal", err)
	}
	if len(s.db.Execs) > 0 {
		t.Errorf("nothing may be revoked: %v", s.db.Execs)
	}
}
