// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"testing"
	"time"

	chgo "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/crossplane/crossplane-runtime/v2/pkg/meta"
	"github.com/crossplane/crossplane-runtime/v2/pkg/resource"

	"github.com/infrabay/provider-clickhouse/apis/v1alpha1"
	ch "github.com/infrabay/provider-clickhouse/internal/clients/clickhouse"
	"github.com/infrabay/provider-clickhouse/internal/reason"
)

// ClickHouse names. The provider identities may only manage databases,
// users and roles whose names start with app_ (the README grants), and on
// Antalya JWT users of service accounts in xpch-e2e-* namespaces.
const (
	dbShop        = "app_shop"
	roleReader    = "app_reader"
	passwordUser  = "app_svc"
	foreignDB     = "app_foreign_db"
	foreignUser   = "app_foreign_user"
	foreignRole   = "app_foreign_role"
	legacyDB      = "app_legacy"
	legacyRole    = "app_legacy_role"
	jwtProcessor  = "kubernetes"
	tokenAudience = "clickhouse"
	// restartRole is the role of the restart scenario's RoleGrant;
	// restartRolePrefix starts the names of the roles it creates at once.
	restartRole       = "app_restart_role"
	restartRolePrefix = "app_restart_role_"
)

// restartRoles is the number of extra Role MRs per server that the restart
// scenario creates at once, so that creates are still running when the
// provider pod gets SIGTERM.
const restartRoles = 40

// creationGracePeriod is crossplane-runtime's default creation grace period:
// within it after a create, an external resource that Observe reports
// missing is taken as not visible yet, and the reconciler waits with backoff
// instead of creating it again.
const creationGracePeriod = 30 * time.Second

// TestEndToEnd runs every scenario against both servers:
//
//  1. Activation: the managed resource kinds stay inactive until a
//     ManagedResourceActivationPolicy activates them.
//  2. Scenarios, per server and in parallel: the lifecycle of every kind, a
//     login with what the connection Secret holds (or a service account
//     token on Antalya) with exactly the granted privileges, out-of-band
//     drift repaired within the poll interval, foreign objects with the same
//     names refused and left byte-identical, the ClusterProviderConfig
//     policy, and observe-only resources that never write.
//  3. ProviderRestart: the provider pod is deleted while new resources are
//     being created (the test checks that creates were still running);
//     ownership survives and nothing is created twice.
//  4. Deletion, per server: deleting the resources revokes and drops only
//     what the provider created.
func TestEndToEnd(t *testing.T) {
	tenants := []*tenant{newTenant(t, "upstream"), newTenant(t, "antalya")}
	if !t.Run("Activation", testActivation) {
		t.FailNow()
	}
	t.Run("Scenarios", func(t *testing.T) {
		for _, tn := range tenants {
			t.Run(tn.name, func(t *testing.T) {
				t.Parallel()
				tn.scenarios(t)
			})
		}
	})
	t.Run("ProviderRestart", func(t *testing.T) { testRestart(t, tenants) })
	t.Run("Deletion", func(t *testing.T) {
		for _, tn := range tenants {
			t.Run(tn.name, func(t *testing.T) {
				t.Parallel()
				tn.deletion(t)
			})
		}
	})
}

// tenant is one server with its tenant namespace, its ClusterProviderConfig
// and namespaced ProviderConfig, and the managed resources of the scenarios.
type tenant struct {
	name string
	ns   string
	srv  *server
	jwt  bool
	cpc  *xpv2.ProviderConfigReference
	pc   *xpv2.ProviderConfigReference
	// userName is the ClickHouse name of the User MR "app".
	userName string
	// ok is set once every scenario passed; the later phases need their
	// resources.
	ok bool

	db         *v1alpha1.Database
	role       *v1alpha1.Role
	user       *v1alpha1.User
	grant      *v1alpha1.Grant
	roleGrant  *v1alpha1.RoleGrant
	pcMRs      []resource.Managed
	foreignMRs []resource.Managed
	policyMRs  []resource.Managed
	restart    []resource.Managed
	// restartRoles are the extra Role MRs of the restart scenario.
	restartRoles []resource.Managed

	foreignObjs     [][2]string
	foreign         []string
	foreignPassword string
}

func newTenant(t *testing.T, name string) *tenant {
	tn := &tenant{
		name: name,
		ns:   "xpch-e2e-" + name,
		srv:  connectServer(t, name),
		jwt:  name == "antalya",
		cpc:  &xpv2.ProviderConfigReference{Kind: v1alpha1.ClusterProviderConfigKind, Name: "xpch-e2e-" + name},
		pc:   &xpv2.ProviderConfigReference{Kind: v1alpha1.ProviderConfigKind, Name: "tenant"},
	}
	tn.userName = passwordUser
	if tn.jwt {
		tn.userName = tn.serviceAccountUser("reporter")
	}
	return tn
}

// serviceAccountUser is the ClickHouse name of a JWT user that logs in with
// the token of a service account of the tenant namespace.
func (tn *tenant) serviceAccountUser(sa string) string {
	return "system:serviceaccount:" + tn.ns + ":" + sa
}

// identities returns the ClickHouse users of the provider's identity (the
// ClusterProviderConfig) and of the tenant's identity (the namespaced
// ProviderConfig), as hack/e2e/run.sh configures them.
func (tn *tenant) identities() (provider, tenant string) {
	if tn.jwt {
		return "system:serviceaccount:" + nsCrossplane + ":" + providerName, tn.serviceAccountUser("tenant-admin")
	}
	return providerName, "tenant-admin"
}

func (tn *tenant) meta(name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Namespace: tn.ns, Name: name}
}

func (tn *tenant) database(mrName, name string, pc *xpv2.ProviderConfigReference) *v1alpha1.Database {
	d := &v1alpha1.Database{ObjectMeta: tn.meta(mrName)}
	d.Spec.ProviderConfigReference = pc
	d.Spec.ForProvider.Name = new(name)
	return d
}

func (tn *tenant) roleMR(mrName, name string, pc *xpv2.ProviderConfigReference) *v1alpha1.Role {
	r := &v1alpha1.Role{ObjectMeta: tn.meta(mrName)}
	r.Spec.ProviderConfigReference = pc
	r.Spec.ForProvider.Name = new(name)
	return r
}

// userMR is a User with a generated password on upstream, and on Antalya a
// JWT user pinned to the "kubernetes" token processor. Its connection Secret
// is "<mrName>-clickhouse".
func (tn *tenant) userMR(mrName, name string) *v1alpha1.User {
	u := &v1alpha1.User{ObjectMeta: tn.meta(mrName)}
	u.Spec.ProviderConfigReference = tn.cpc
	u.Spec.ForProvider.Name = new(name)
	u.Spec.WriteConnectionSecretToReference = &xpv2.LocalSecretReference{Name: mrName + "-clickhouse"}
	if tn.jwt {
		u.Spec.ForProvider.Authentication.JWT = &v1alpha1.JWTAuthentication{Processor: jwtProcessor}
	} else {
		u.Spec.ForProvider.Authentication.Password = &v1alpha1.PasswordAuthentication{Generate: &v1alpha1.GeneratePassword{}}
	}
	return u
}

func (tn *tenant) scenarios(t *testing.T) {
	for _, step := range []struct {
		name string
		fn   func(*testing.T)
	}{
		{"Configs", tn.configs},
		{"ForeignObjects", tn.createForeign},
		{"Lifecycle", tn.lifecycle},
		{"Login", tn.login},
		{"Drift", tn.drift},
		{"ForeignRefused", tn.foreignRefused},
		{"Policy", tn.policy},
		{"ObserveOnly", tn.observeOnly},
	} {
		if !t.Run(step.name, step.fn) {
			return
		}
	}
	tn.ok = true
}

// configs creates the server's ClusterProviderConfig, with a closed policy
// that opens only the tenant namespace and app_ names, and the tenant's
// namespaced ProviderConfig with the tenant's own credentials.
func (tn *tenant) configs(t *testing.T) {
	port := int32(9000)
	conn := v1alpha1.ConnectionSpec{Endpoint: v1alpha1.Endpoint{Host: tn.srv.host, Port: &port}, AllowInsecureTransport: true}
	// grantableRoles is narrower than roleNamePatterns, so that the policy
	// scenario can refuse a RoleGrant of a role that the tenant may create
	// but not grant. Both servers allow the "kubernetes" processor, so that
	// a jwt user on upstream is refused by allowedAuthentication alone.
	policy := v1alpha1.ClusterPolicy{
		AllowedNamespaces:    []string{tn.ns},
		DatabaseNamePatterns: []string{"app_[a-z0-9_]+"},
		UserNamePatterns:     []string{"app_[a-z0-9_]+"},
		RoleNamePatterns:     []string{"app_[a-z0-9_]+"},
		GrantableRoles:       []string{roleReader, restartRole},
		AllowedJWTProcessors: []string{jwtProcessor},
	}
	cpc := &v1alpha1.ClusterProviderConfig{ObjectMeta: metav1.ObjectMeta{Name: tn.cpc.Name}}
	cpc.Spec.ConnectionSpec = conn
	pc := &v1alpha1.ProviderConfig{ObjectMeta: tn.meta(tn.pc.Name)}
	pc.Spec.ConnectionSpec = conn
	if tn.jwt {
		// The provider logs in with its projected service account token
		// (examples/runtime/projected-token.yaml); the tenant with a token
		// of its own service account stored in a Secret.
		policy.UserNamePatterns = append(policy.UserNamePatterns, "system:serviceaccount:{namespace}:[a-z0-9-]+")
		policy.AllowedAuthentication = []v1alpha1.AuthenticationMethod{v1alpha1.AuthenticationMethodJWT}
		cpc.Spec.Credentials = v1alpha1.ClusterCredentials{
			Source:    v1alpha1.CredentialsSourceTokenFile,
			TokenFile: &v1alpha1.TokenFileCredentials{Path: "/var/run/secrets/clickhouse/token", Audience: tokenAudience},
		}
		pc.Spec.Credentials = v1alpha1.LocalCredentials{
			Source: v1alpha1.CredentialsSourceTokenSecret,
			TokenSecret: &v1alpha1.LocalTokenSecretCredentials{
				SecretRef: xpv2.LocalSecretKeySelector{LocalSecretReference: xpv2.LocalSecretReference{Name: "clickhouse-tenant"}, Key: "token"},
				Audience:  tokenAudience,
			},
		}
	} else {
		policy.AllowedAuthentication = []v1alpha1.AuthenticationMethod{v1alpha1.AuthenticationMethodPassword}
		cpc.Spec.Credentials = v1alpha1.ClusterCredentials{
			Source: v1alpha1.CredentialsSourcePasswordSecret,
			PasswordSecret: &v1alpha1.ClusterPasswordCredentials{
				Username:          providerName,
				PasswordSecretRef: xpv2.SecretKeySelector{SecretReference: xpv2.SecretReference{Namespace: nsCrossplane, Name: "clickhouse-upstream"}, Key: "password"},
			},
		}
		pc.Spec.Credentials = v1alpha1.LocalCredentials{
			Source: v1alpha1.CredentialsSourcePasswordSecret,
			PasswordSecret: &v1alpha1.LocalPasswordCredentials{
				Username:          "tenant-admin",
				PasswordSecretRef: xpv2.LocalSecretKeySelector{LocalSecretReference: xpv2.LocalSecretReference{Name: "clickhouse-tenant"}, Key: "password"},
			},
		}
	}
	cpc.Spec.Policy = policy
	create(t, cpc)
	create(t, pc)
}

// createForeign creates, as the administrator, a database, a user and a role
// with names the tenant may use, and grants between them, before any MR
// exists.
func (tn *tenant) createForeign(t *testing.T) {
	s := tn.srv
	tn.foreignPassword = randomHex(16)
	s.exec(t, "CREATE DATABASE "+foreignDB+" COMMENT 'theirs'")
	s.execSecret(t, "CREATE USER "+foreignUser+" IDENTIFIED WITH sha256_password BY "+ch.QuoteString(tn.foreignPassword)+" SETTINGS max_threads = 3", tn.foreignPassword)
	s.exec(t, "CREATE ROLE "+foreignRole)
	s.exec(t, "GRANT SELECT ON "+foreignDB+".* TO "+foreignRole)
	s.exec(t, "GRANT "+foreignRole+" TO "+foreignUser)
	// A privilege on the database the provider will create, for another
	// grantee than the Grant's.
	s.exec(t, "GRANT INSERT ON "+dbShop+".* TO "+foreignUser)
	tn.foreignObjs = [][2]string{{kindDatabase, foreignDB}, {kindUser, foreignUser}, {kindRole, foreignRole}}
	tn.foreign = s.snapshot(t, tn.foreignObjs...)
}

// sameForeign fails the test if the foreign objects changed in any way.
func (tn *tenant) sameForeign(t *testing.T, what string) {
	t.Helper()
	if diff := cmp.Diff(tn.foreign, tn.srv.snapshot(t, tn.foreignObjs...)); diff != "" {
		t.Errorf("%s changed the foreign objects (-before +after):\n%s", what, diff)
	}
}

// lifecycle creates a Database, a Role, a User, a Grant of the database to
// the role and a RoleGrant of the role to the user (through references) on
// the ClusterProviderConfig, and a Database and a Role on the namespaced
// ProviderConfig, and checks the server.
func (tn *tenant) lifecycle(t *testing.T) {
	s := tn.srv
	tn.db = tn.database("shop", dbShop, tn.cpc)
	tn.db.Spec.ForProvider.Comment = new("shop v1")
	tn.role = tn.roleMR("reader", roleReader, tn.cpc)
	if tn.jwt {
		ensureServiceAccount(t, tn.ns, "reporter")
	}
	tn.user = tn.userMR("app", tn.userName)
	tn.user.Spec.ForProvider.DefaultDatabase = new(dbShop)
	tn.grant = &v1alpha1.Grant{ObjectMeta: tn.meta("reader-shop")}
	tn.grant.Spec.ProviderConfigReference = tn.cpc
	tn.grant.Spec.ForProvider = v1alpha1.GrantParameters{
		Privileges:  []string{"SELECT", "INSERT"},
		DatabaseRef: &xpv2.NamespacedReference{Name: "shop"},
		RoleRef:     &xpv2.NamespacedReference{Name: "reader"},
	}
	tn.roleGrant = &v1alpha1.RoleGrant{ObjectMeta: tn.meta("app-reader")}
	tn.roleGrant.Spec.ProviderConfigReference = tn.cpc
	tn.roleGrant.Spec.ForProvider = v1alpha1.RoleGrantParameters{
		MemberOfRef: &xpv2.NamespacedReference{Name: "reader"},
		UserRef:     &xpv2.NamespacedReference{Name: "app"},
	}
	tn.pcMRs = []resource.Managed{tn.database("scratch", "app_scratch", tn.pc), tn.roleMR("tenant-role", "app_tenant_role", tn.pc)}
	all := append([]resource.Managed{tn.db, tn.role, tn.user, tn.grant, tn.roleGrant}, tn.pcMRs...)
	for _, mg := range all {
		create(t, mg)
	}
	waitReady(t, all...)

	// The server holds exactly what the MRs describe, under the UUIDs they
	// claim.
	for _, c := range []struct {
		mg         resource.Managed
		kind, name string
	}{
		{tn.db, kindDatabase, dbShop}, {tn.role, kindRole, roleReader}, {tn.user, kindUser, tn.userName},
		{tn.pcMRs[0], kindDatabase, "app_scratch"}, {tn.pcMRs[1], kindRole, "app_tenant_role"},
	} {
		id := s.uuid(t, c.kind, c.name)
		if id == "" || id != c.mg.GetAnnotations()[v1alpha1.AnnotationOwnedID] {
			t.Errorf("%s %s: server UUID %q, owned-id %q", c.kind, c.name, id, c.mg.GetAnnotations()[v1alpha1.AnnotationOwnedID])
		}
		if c.kind != kindDatabase {
			if st := s.storage(t, c.kind, c.name); st != ch.StorageLocalDirectory {
				t.Errorf("%s %s is stored in %q, want %s", c.kind, c.name, st, ch.StorageLocalDirectory)
			}
		}
	}
	if got := s.comment(t, dbShop); got != "shop v1" {
		t.Errorf("comment of %s: %q", dbShop, got)
	}
	if tn.db.Status.AtProvider.ID != tn.db.GetAnnotations()[v1alpha1.AnnotationOwnedID] || tn.db.Spec.ForProvider.Engine == nil {
		t.Errorf("Database status %+v, late-initialized engine %+v", tn.db.Status.AtProvider, tn.db.Spec.ForProvider.Engine)
	}
	granted := []string{"INSERT " + dbShop + ".*", "SELECT " + dbShop + ".*"}
	if got := s.grantsOf(t, kindRole, roleReader); !slices.Equal(got, granted) {
		t.Errorf("grants of %s: %v, want %v", roleReader, got, granted)
	}
	if ok, admin := s.member(t, tn.userName, roleReader); !ok || admin {
		t.Errorf("membership of %s in %s: %t, admin option %t", tn.userName, roleReader, ok, admin)
	}
	if g, rg := tn.grant.Status.AtProvider, tn.roleGrant.Status.AtProvider; g.Grantee != "role:"+roleReader || g.Target != dbShop+".*" || rg.Grantee != "user:"+tn.userName {
		t.Errorf("status: Grant %+v, RoleGrant %+v", g, rg)
	}
	usages := &v1alpha1.ProviderConfigUsageList{}
	if err := kube.List(context.Background(), usages, client.InNamespace(tn.ns)); err != nil || len(usages.Items) < len(all) {
		t.Errorf("ProviderConfigUsages in %s: %d (%v), want at least %d", tn.ns, len(usages.Items), err, len(all))
	}
	// Each MR ran every statement (system.query_log, by log_comment) as the
	// identity of its own config: the provider's for the
	// ClusterProviderConfig, the tenant's for the namespaced ProviderConfig.
	// Both identities hold the same grants, so only this tells a tenant's MRs
	// running with the provider's credentials apart.
	providerID, tenantID := tn.identities()
	for _, c := range []struct {
		lc, want string
	}{
		{"Database/" + tn.ns + "/shop", providerID}, {"Role/" + tn.ns + "/reader", providerID}, {"User/" + tn.ns + "/app", providerID},
		{"Grant/" + tn.ns + "/reader-shop", providerID}, {"RoleGrant/" + tn.ns + "/app-reader", providerID},
		{"Database/" + tn.ns + "/scratch", tenantID}, {"Role/" + tn.ns + "/tenant-role", tenantID},
	} {
		if got := s.usersOf(t, c.lc); !slices.Equal(got, []string{c.want}) {
			t.Errorf("%s ran as %q, want only %q", c.lc, got, c.want)
		}
	}

	// A spec change reaches the server (comment changes need 25.8 or
	// later).
	if tn.commentChange(t) {
		get(t, tn.db)
		tn.db.Spec.ForProvider.Comment = new("shop v2")
		if err := kube.Update(context.Background(), tn.db); err != nil {
			t.Fatal(err)
		}
		eventually(t, 2*time.Minute, "comment change on the server", func() (bool, string) {
			c := s.comment(t, dbShop)
			return c == "shop v2", "comment " + c
		})
	}
	tn.sameForeign(t, "creating the MRs")
}

// commentChange reports whether the server supports database comment
// changes.
func (tn *tenant) commentChange(t *testing.T) bool {
	return tn.srv.count(t, "SELECT count() FROM system.privileges WHERE privilege = 'ALTER MODIFY DATABASE COMMENT'") > 0
}

// login connects as the User MR "app" with what its connection Secret holds:
// the generated password on upstream; on Antalya, where the Secret holds no
// secret, with a token of the user's service account. The user can do
// exactly what the Grant and the RoleGrant give it.
func (tn *tenant) login(t *testing.T) {
	s := tn.srv
	s.exec(t, "CREATE TABLE "+dbShop+".orders (id UInt64, item String) ENGINE = MergeTree ORDER BY id")
	s.exec(t, "CREATE DATABASE e2e_other")
	s.exec(t, "CREATE TABLE e2e_other.secrets (x UInt8) ENGINE = Memory")

	sec := tn.appSecret(t)
	want := map[string]string{
		v1alpha1.ConnectionKeyUsername: tn.userName,
		v1alpha1.ConnectionKeyEndpoint: s.host,
		v1alpha1.ConnectionKeyPort:     "9000",
		v1alpha1.ConnectionKeyProtocol: string(v1alpha1.ProtocolNative),
		v1alpha1.ConnectionKeyTLS:      "false",
	}
	if tn.jwt {
		want[v1alpha1.ConnectionKeyJWTProcessor] = jwtProcessor
		if _, ok := sec.Data[v1alpha1.ConnectionKeyPassword]; ok {
			t.Error("the connection Secret of a JWT user has a password key")
		}
	} else if n := len(sec.Data[v1alpha1.ConnectionKeyPassword]); n != 32 {
		t.Fatalf("the connection Secret holds a password of %d characters, want 32", n)
	}
	for k, v := range want {
		if got := string(sec.Data[k]); got != v {
			t.Errorf("connection Secret key %s = %q, want %q", k, got, v)
		}
	}
	conn, err := s.open(t, tn.appLogin(t, sec))
	if err != nil {
		t.Fatalf("login with the connection Secret: %v", err)
	}
	if who, db := whoami(t, conn); who != tn.userName || db != dbShop {
		t.Fatalf("logged in as %q in %q, want %q in %q", who, db, tn.userName, dbShop)
	}
	if err := conn.Exec(context.Background(), "INSERT INTO "+dbShop+".orders VALUES (1, 'book')"); err != nil {
		t.Errorf("INSERT: %v", err)
	}
	var n uint64
	if err := conn.QueryRow(context.Background(), "SELECT count() FROM "+dbShop+".orders").Scan(&n); err != nil || n != 1 {
		t.Errorf("SELECT: %d rows, %v", n, err)
	}
	for _, stmt := range []string{
		"SELECT count() FROM e2e_other.secrets",
		"CREATE TABLE " + dbShop + ".extra (x UInt8) ENGINE = Memory",
		"ALTER TABLE " + dbShop + ".orders DELETE WHERE 1",
		"TRUNCATE TABLE " + dbShop + ".orders",
		"DROP TABLE " + dbShop + ".orders",
		"DROP DATABASE " + dbShop,
		"GRANT SELECT ON " + dbShop + ".* TO " + foreignUser,
		"CREATE USER app_evil",
		"CREATE ROLE app_evil",
	} {
		expectDenied(t, conn, stmt)
	}

	if tn.jwt {
		// The server validates the tokens: one of a service account without
		// a user, and one of the user's account for another audience, are
		// refused.
		ensureServiceAccount(t, tn.ns, "nobody")
		for what, token := range map[string]string{
			"a token of a service account without a user": serviceAccountToken(t, tn.ns, "nobody", tokenAudience),
			"a token for another audience":                serviceAccountToken(t, tn.ns, "reporter", "not-clickhouse"),
		} {
			_, err := s.open(t, &chgo.Options{GetJWT: func(context.Context) (string, error) { return token, nil }})
			if code := exceptionCode(err); code != 516 {
				t.Errorf("%s: code %d (%v), want 516 (AUTHENTICATION_FAILED)", what, code, err)
			}
		}
	}
	tn.sameForeign(t, "the user's session")
}

// appSecret reads the connection Secret of the User MR "app".
func (tn *tenant) appSecret(t *testing.T) *corev1.Secret {
	t.Helper()
	sec := &corev1.Secret{}
	if err := kube.Get(context.Background(), client.ObjectKey{Namespace: tn.ns, Name: "app-clickhouse"}, sec); err != nil {
		t.Fatalf("connection Secret: %v", err)
	}
	return sec
}

// appLogin returns the options to log in as the User MR "app" with what its
// connection Secret holds: the username and the generated password on
// upstream; on Antalya, where the Secret holds no secret, a token of the
// user's service account.
func (tn *tenant) appLogin(t *testing.T, sec *corev1.Secret) *chgo.Options {
	t.Helper()
	if tn.jwt {
		token := serviceAccountToken(t, tn.ns, "reporter", tokenAudience)
		return &chgo.Options{GetJWT: func(context.Context) (string, error) { return token, nil }}
	}
	return &chgo.Options{Auth: chgo.Auth{Username: string(sec.Data[v1alpha1.ConnectionKeyUsername]), Password: string(sec.Data[v1alpha1.ConnectionKeyPassword])}}
}

// drift changes the provider's objects as the administrator and checks that
// each change is repaired within the poll interval, plus a margin for the
// reconcile itself, for one that was running when the change was made, and
// for slow CI machines. The time each repair took is logged.
func (tn *tenant) drift(t *testing.T) {
	s := tn.srv
	limit := pollInterval + 30*time.Second
	user := ch.QuoteIdent(tn.userName)

	granted := s.grantsOf(t, kindRole, roleReader)
	s.exec(t, "REVOKE INSERT ON "+dbShop+".* FROM "+roleReader)
	tn.repaired(t, limit, "REVOKE INSERT from the role", func() (bool, string) {
		got := s.grantsOf(t, kindRole, roleReader)
		return slices.Equal(got, granted), fmtList(got)
	})

	s.exec(t, "GRANT SHOW TABLES ON "+dbShop+".* TO "+roleReader)
	tn.repaired(t, limit, "a foreign privilege at the Grant's target", func() (bool, string) {
		got := s.grantsOf(t, kindRole, roleReader)
		return slices.Equal(got, granted), fmtList(got)
	})

	definition := s.showCreate(t, kindUser, tn.userName)
	for _, alter := range []string{"HOST NONE", "VALID UNTIL '2035-01-01 00:00:00'", "DEFAULT DATABASE e2e_other"} {
		s.exec(t, "ALTER USER "+user+" "+alter)
		tn.repaired(t, limit, "ALTER USER "+alter, func() (bool, string) {
			got := s.showCreate(t, kindUser, tn.userName)
			return got == definition, got
		})
	}

	if tn.commentChange(t) {
		want := s.comment(t, dbShop)
		s.exec(t, "ALTER DATABASE "+dbShop+" MODIFY COMMENT 'drift'")
		tn.repaired(t, limit, "ALTER DATABASE MODIFY COMMENT", func() (bool, string) {
			got := s.comment(t, dbShop)
			return got == want, "comment " + got
		})
	}

	// A revoked membership makes the RoleGrant's external resource missing.
	// Within crossplane-runtime's creation grace period after the RoleGrant
	// was created, the reconciler takes that as not visible yet and backs
	// off instead of granting again, so the repair would take longer than
	// the poll interval. The REVOKE is therefore made once that period is
	// over (usually it is by now).
	get(t, tn.roleGrant)
	if created := meta.GetExternalCreateSucceeded(tn.roleGrant); created.IsZero() {
		t.Errorf("RoleGrant %s has no %s annotation", tn.roleGrant.GetName(), meta.AnnotationKeyExternalCreateSucceeded)
	} else if wait := time.Until(created.Add(creationGracePeriod + 2*time.Second)); wait > 0 {
		t.Logf("%s: waiting %s for the creation grace period of the RoleGrant to end", tn.name, wait.Round(100*time.Millisecond))
		time.Sleep(wait)
	}
	s.exec(t, "REVOKE "+roleReader+" FROM "+user)
	tn.repaired(t, limit, "REVOKE of the role from the user", func() (bool, string) {
		ok, _ := s.member(t, tn.userName, roleReader)
		return ok, "member " + boolString(ok)
	})

	for _, mg := range []resource.Managed{tn.db, tn.role, tn.user, tn.grant, tn.roleGrant} {
		waitMR(t, mg, time.Minute, "Ready and Synced after the drift", readyAndSynced)
	}

	// Repairing the user re-applies its definition, IDENTIFIED clause
	// included (VALID UNTIL belongs to it): the user still logs in with what
	// the connection Secret holds. SHOW CREATE USER never shows the
	// password, so only a login can tell.
	conn, err := s.open(t, tn.appLogin(t, tn.appSecret(t)))
	if err != nil {
		t.Fatalf("login with the connection Secret after the drift repairs: %v", err)
	}
	if who, _ := whoami(t, conn); who != tn.userName {
		t.Errorf("logged in as %q after the drift repairs, want %q", who, tn.userName)
	}
	tn.sameForeign(t, "repairing the drift")
}

func (tn *tenant) repaired(t *testing.T, limit time.Duration, what string, cond func() (bool, string)) {
	t.Helper()
	start := time.Now()
	eventually(t, limit, what+" repaired", cond)
	t.Logf("%s: %s repaired after %s (poll interval %s)", tn.name, what, time.Since(start).Round(100*time.Millisecond), pollInterval)
}

// foreignRefused creates MRs for the foreign database, user and role, and a
// Grant and a RoleGrant that would change the foreign user's privileges:
// all are refused with NotOwned, and the foreign objects stay byte-identical.
func (tn *tenant) foreignRefused(t *testing.T) {
	db := tn.database("foreign-db", foreignDB, tn.cpc)
	db.Spec.ForProvider.Comment = new("mine")
	user := tn.userMR("foreign-user", foreignUser)
	grant := &v1alpha1.Grant{ObjectMeta: tn.meta("foreign-grant")}
	grant.Spec.ProviderConfigReference = tn.cpc
	grant.Spec.ForProvider = v1alpha1.GrantParameters{Privileges: []string{"SELECT", "INSERT"}, Database: new(dbShop), User: new(foreignUser)}
	rg := &v1alpha1.RoleGrant{ObjectMeta: tn.meta("foreign-rolegrant")}
	rg.Spec.ProviderConfigReference = tn.cpc
	rg.Spec.ForProvider = v1alpha1.RoleGrantParameters{MemberOfRef: &xpv2.NamespacedReference{Name: "reader"}, User: new(foreignUser)}
	tn.foreignMRs = []resource.Managed{db, tn.roleMR("foreign-role", foreignRole, tn.cpc), user, grant, rg}
	for _, mg := range tn.foreignMRs {
		create(t, mg)
	}
	for _, mg := range tn.foreignMRs {
		waitMR(t, mg, 2*time.Minute, "refused with NotOwned", refusedWith(reason.NotOwned))
	}
	// Several polls must not change anything either. No MR records the UUID
	// of a foreign object (a Database MR holds the random UUID it would
	// create the database with, design 4.2; User and Role MRs hold none).
	time.Sleep(2 * pollInterval)
	foreignIDs := map[string]bool{}
	for _, o := range tn.foreignObjs {
		foreignIDs[tn.srv.uuid(t, o[0], o[1])] = true
	}
	for _, mg := range tn.foreignMRs {
		get(t, mg)
		if !refusedWith(reason.NotOwned)(mg) {
			t.Errorf("%T %s: %s", mg, mg.GetName(), conditions(mg))
		}
		id, ok := mg.GetAnnotations()[v1alpha1.AnnotationOwnedID]
		switch {
		case foreignIDs[id]:
			t.Errorf("%T %s claims the foreign object %s", mg, mg.GetName(), id)
		case ok && !isDatabase(mg):
			t.Errorf("%T %s has owned-id %q although it created nothing", mg, mg.GetName(), id)
		}
	}
	tn.sameForeign(t, "MRs pointing at the foreign objects")
}

// policy checks the ClusterProviderConfig policy: a database, a role and a
// user name outside the allowed patterns (on Antalya the user of another
// namespace's service account), a namespace outside allowedNamespaces, an
// authentication method the policy does not allow, and a RoleGrant of a role
// outside grantableRoles (one the tenant owns and may name) are refused with
// PolicyViolation, and nothing reaches the server.
func (tn *tenant) policy(t *testing.T) {
	s := tn.srv
	outside := tn.database("outside", "other_db", tn.cpc)
	outsideRole := tn.roleMR("outside-role", "other_role", tn.cpc)
	outsideUserName := "other_user"
	if tn.jwt {
		outsideUserName = "system:serviceaccount:xpch-e2e-upstream:intruder"
	}
	outsideUser := tn.userMR("outside-user", outsideUserName)
	intruder := tn.roleMR("intruder-"+tn.name, "app_outsider_role", tn.cpc)
	intruder.Namespace = nsOutsider
	wrongAuth := tn.userMR("wrong-auth", "app_wrong_auth")
	if tn.jwt {
		wrongAuth.Spec.ForProvider.Authentication = v1alpha1.UserAuthentication{Password: &v1alpha1.PasswordAuthentication{Generate: &v1alpha1.GeneratePassword{}}}
	} else {
		// The "kubernetes" processor is allowed on upstream too: only
		// allowedAuthentication refuses this user.
		wrongAuth.Spec.ForProvider.Authentication = v1alpha1.UserAuthentication{JWT: &v1alpha1.JWTAuthentication{Processor: jwtProcessor}}
	}
	ungrantable := &v1alpha1.RoleGrant{ObjectMeta: tn.meta("ungrantable")}
	ungrantable.Spec.ProviderConfigReference = tn.cpc
	ungrantable.Spec.ForProvider = v1alpha1.RoleGrantParameters{MemberOf: new("app_tenant_role"), UserRef: &xpv2.NamespacedReference{Name: "app"}}
	tn.policyMRs = []resource.Managed{outside, outsideRole, outsideUser, intruder, wrongAuth, ungrantable}
	for _, mg := range tn.policyMRs {
		create(t, mg)
	}
	for _, mg := range tn.policyMRs {
		waitMR(t, mg, 2*time.Minute, "refused with PolicyViolation", refusedWith(reason.PolicyViolation))
	}
	for _, c := range [][2]string{{kindDatabase, "other_db"}, {kindRole, "other_role"}, {kindUser, outsideUserName}, {kindRole, "app_outsider_role"}, {kindUser, "app_wrong_auth"}} {
		if id := s.uuid(t, c[0], c[1]); id != "" {
			t.Errorf("%s %s exists although the policy refuses it", c[0], c[1])
		}
	}
	if ok, _ := s.member(t, tn.userName, "app_tenant_role"); ok {
		t.Errorf("%s holds app_tenant_role although the policy does not let RoleGrant grant it", tn.userName)
	}
	for _, lc := range []string{
		"Database/" + tn.ns + "/outside", "Role/" + tn.ns + "/outside-role", "User/" + tn.ns + "/outside-user",
		"Role/" + nsOutsider + "/intruder-" + tn.name, "User/" + tn.ns + "/wrong-auth", "RoleGrant/" + tn.ns + "/ungrantable",
	} {
		if n := s.statements(t, lc, false); n != 0 {
			t.Errorf("%s ran %d statements that change something", lc, n)
		}
	}
}

// observeOnly observes a database and a role that the administrator created:
// the MRs become Ready and report the objects' UUIDs, never claim them, never
// write (system.query_log) and leave them in place when deleted.
func (tn *tenant) observeOnly(t *testing.T) {
	s := tn.srv
	s.exec(t, "CREATE DATABASE "+legacyDB+" COMMENT 'theirs'")
	s.exec(t, "CREATE ROLE "+legacyRole)
	objs := [][2]string{{kindDatabase, legacyDB}, {kindRole, legacyRole}}
	before := s.snapshot(t, objs...)

	db := tn.database("legacy-db", legacyDB, tn.cpc)
	db.Spec.ManagementPolicies = xpv2.ManagementPolicies{xpv2.ManagementActionObserve}
	db.Spec.ForProvider.Comment = new("mine")
	db.Spec.ForProvider.ForceDrop = new(true)
	role := tn.roleMR("legacy-role", legacyRole, tn.cpc)
	role.Spec.ManagementPolicies = xpv2.ManagementPolicies{xpv2.ManagementActionObserve}
	create(t, db)
	create(t, role)
	waitMR(t, db, 2*time.Minute, "observed", func(mg resource.Managed) bool {
		return mg.GetCondition(xpv2.TypeReady).Status == corev1.ConditionTrue && mg.(*v1alpha1.Database).Status.AtProvider.ID == s.uuid(t, kindDatabase, legacyDB)
	})
	waitMR(t, role, 2*time.Minute, "observed", func(mg resource.Managed) bool {
		return mg.GetCondition(xpv2.TypeReady).Status == corev1.ConditionTrue && mg.(*v1alpha1.Role).Status.AtProvider.ID == s.uuid(t, kindRole, legacyRole)
	})
	time.Sleep(2*pollInterval + 2*time.Second)
	for _, mg := range []resource.Managed{db, role} {
		get(t, mg)
		if _, ok := mg.GetAnnotations()[v1alpha1.AnnotationOwnedID]; ok {
			t.Errorf("observe-only %T %s claimed the object", mg, mg.GetName())
		}
	}
	if diff := cmp.Diff(before, s.snapshot(t, objs...)); diff != "" {
		t.Errorf("observe-only MRs changed the objects (-before +after):\n%s", diff)
	}
	deleteAndWait(t, 2*time.Minute, db, role)
	if diff := cmp.Diff(before, s.snapshot(t, objs...)); diff != "" {
		t.Errorf("deleting observe-only MRs changed the objects (-before +after):\n%s", diff)
	}
	for _, lc := range []string{"Database/" + tn.ns + "/legacy-db", "Role/" + tn.ns + "/legacy-role"} {
		if n := s.statements(t, lc, false); n != 0 {
			t.Errorf("observe-only %s ran %d statements that change something", lc, n)
		}
	}
}

// batch2 returns new MRs of every kind for the restart scenario, and
// restartRoles extra Role MRs.
func (tn *tenant) batch2(t *testing.T) (batch, roles []resource.Managed) {
	name := "app_restart_user"
	if tn.jwt {
		ensureServiceAccount(t, tn.ns, "restarter")
		name = tn.serviceAccountUser("restarter")
	}
	grant := &v1alpha1.Grant{ObjectMeta: tn.meta("restart")}
	grant.Spec.ProviderConfigReference = tn.cpc
	grant.Spec.ForProvider = v1alpha1.GrantParameters{
		Privileges:  []string{"SELECT"},
		DatabaseRef: &xpv2.NamespacedReference{Name: "restart"},
		RoleRef:     &xpv2.NamespacedReference{Name: "restart"},
	}
	rg := &v1alpha1.RoleGrant{ObjectMeta: tn.meta("restart")}
	rg.Spec.ProviderConfigReference = tn.cpc
	rg.Spec.ForProvider = v1alpha1.RoleGrantParameters{
		MemberOfRef: &xpv2.NamespacedReference{Name: "restart"},
		UserRef:     &xpv2.NamespacedReference{Name: "restart"},
	}
	for i := range restartRoles {
		roles = append(roles, tn.roleMR(fmt.Sprintf("restart-%02d", i), fmt.Sprintf("%s%02d", restartRolePrefix, i), tn.cpc))
	}
	return []resource.Managed{tn.database("restart", "app_restart", tn.cpc), tn.roleMR("restart", restartRole, tn.cpc), tn.userMR("restart", name), grant, rg}, roles
}

// owned are the MRs of a tenant that own a ClickHouse object, with its kind
// and name.
type ownedMR struct {
	mg         resource.Managed
	kind, name string
	id         string
}

func (tn *tenant) ownedMRs() []ownedMR {
	out := []ownedMR{{mg: tn.db, kind: kindDatabase, name: dbShop}, {mg: tn.role, kind: kindRole, name: roleReader}, {mg: tn.user, kind: kindUser, name: tn.userName}}
	if len(tn.restart) == 5 {
		out = append(out,
			ownedMR{mg: tn.restart[0], kind: kindDatabase, name: "app_restart"},
			ownedMR{mg: tn.restart[1], kind: kindRole, name: restartRole},
			ownedMR{mg: tn.restart[2], kind: kindUser, name: *tn.restart[2].(*v1alpha1.User).Spec.ForProvider.Name})
	}
	for _, mg := range tn.restartRoles {
		out = append(out, ownedMR{mg: mg, kind: kindRole, name: *mg.(*v1alpha1.Role).Spec.ForProvider.Name})
	}
	return out
}

// testRestart deletes the provider pod while new MRs are being created and
// checks that creates were running when it got SIGTERM: the old pod ran
// creates after its deletion and left some to the new pod. A graceful
// shutdown must let the running creates record their claims (design 5.9).
// Once the new pod runs, every MR is Ready, no claim and no object changed,
// and every object was created exactly once.
func testRestart(t *testing.T, tenants []*tenant) {
	var active []*tenant
	for _, tn := range tenants {
		if tn.ok {
			active = append(active, tn)
		}
	}
	if len(active) == 0 {
		t.Skip("the scenarios failed")
	}
	before := map[*tenant][]ownedMR{}
	for _, tn := range active {
		for _, o := range tn.ownedMRs() {
			get(t, o.mg)
			o.id = o.mg.GetAnnotations()[v1alpha1.AnnotationOwnedID]
			if o.id == "" || o.id != tn.srv.uuid(t, o.kind, o.name) {
				t.Fatalf("%s %s: owned-id %q before the restart", o.kind, o.name, o.id)
			}
			before[tn] = append(before[tn], o)
		}
	}
	revisions := &metav1.PartialObjectMetadataList{}
	revisions.SetGroupVersionKind(xppkgRevisionListGVK)
	if err := kube.List(context.Background(), revisions, client.MatchingLabels{"pkg.crossplane.io/package": providerName}); err != nil {
		t.Fatal(err)
	}

	// The extra roles of both servers first, then the other MRs. The pod is
	// deleted as soon as the first extra role exists on a server, while the
	// provider is creating the others.
	for _, tn := range active {
		tn.restart, tn.restartRoles = tn.batch2(t)
	}
	deleted := make(chan podDeletion, 1)
	go func() { deleted <- deleteProviderOnFirstCreate(active) }()
	for i := range restartRoles {
		for _, tn := range active {
			create(t, tn.restartRoles[i])
		}
	}
	for _, tn := range active {
		for _, mg := range tn.restart {
			create(t, mg)
		}
	}
	del := <-deleted
	if del.err != nil {
		t.Fatal(del.err)
	}
	t.Logf("deleted provider pod(s) %v once the first role of the scenario existed", del.pods)
	waitNewProviderPod(t, del.uids)

	var oldAfter, fresh uint64
	for _, tn := range active {
		t.Run(tn.name, func(t *testing.T) {
			s := tn.srv
			waitReady(t, append(slices.Clone(tn.restart), tn.restartRoles...)...)
			for _, o := range before[tn] {
				waitMR(t, o.mg, time.Minute, "Ready and Synced after the restart", readyAndSynced)
				if id := o.mg.GetAnnotations()[v1alpha1.AnnotationOwnedID]; id != o.id {
					t.Errorf("%s %s: owned-id changed from %q to %q", o.kind, o.name, o.id, id)
				}
				if id := s.uuid(t, o.kind, o.name); id != o.id {
					t.Errorf("%s %s: server UUID changed from %q to %q", o.kind, o.name, o.id, id)
				}
			}
			for _, o := range tn.ownedMRs() {
				get(t, o.mg)
				if id := s.uuid(t, o.kind, o.name); id == "" || id != o.mg.GetAnnotations()[v1alpha1.AnnotationOwnedID] {
					t.Errorf("%s %s: server UUID %q, owned-id %q", o.kind, o.name, id, o.mg.GetAnnotations()[v1alpha1.AnnotationOwnedID])
				}
				kind := map[string]string{kindDatabase: "Database", kindRole: "Role", kindUser: "User"}[o.kind]
				if n := s.creates(t, kind+"/"+tn.ns+"/"+o.mg.GetName()); n != 1 {
					t.Errorf("%s %s was created %d times, want once", o.kind, o.name, n)
				}
			}
			if got := s.grantsOf(t, kindRole, restartRole); !slices.Equal(got, []string{"SELECT app_restart.*"}) {
				t.Errorf("grants of %s: %v", restartRole, got)
			}
			user := *tn.restart[2].(*v1alpha1.User).Spec.ForProvider.Name
			if ok, _ := s.member(t, user, restartRole); !ok {
				t.Errorf("%s is not a member of %s", user, restartRole)
			}
			tn.sameForeign(t, "the provider restart")

			// Who created the extra roles, by the client host name (the pod
			// name) that system.query_log records.
			s.exec(t, "SYSTEM FLUSH LOGS")
			q := "SELECT count() FROM system.query_log WHERE type = 'QueryFinish' AND startsWith(log_comment, ?) AND match(query, '^\\\\s*(?i:CREATE ROLE)\\\\s') AND "
			lc := "Role/" + tn.ns + "/restart-"
			oldBefore := s.count(t, q+"has(?, client_hostname) AND event_time_microseconds < toDateTime64(?, 6)", lc, del.pods, del.at[tn])
			after := s.count(t, q+"has(?, client_hostname) AND event_time_microseconds >= toDateTime64(?, 6)", lc, del.pods, del.at[tn])
			byNew := s.count(t, q+"NOT has(?, client_hostname)", lc, del.pods)
			t.Logf("%s: the old pod created %d of the %d roles before its deletion and %d after it, the new pod %d", tn.name, oldBefore, restartRoles, after, byNew)
			oldAfter += after
			fresh += byNew
		})
	}
	// The scenario is only meaningful if the shutdown interrupted the
	// creates: the old pod was still creating after its deletion, and the
	// new pod had roles left to create.
	if oldAfter == 0 || fresh == 0 {
		t.Errorf("the provider pod did not get SIGTERM while creates were running (old pod after its deletion: %d, new pod: %d); "+
			"the scenario did not exercise the graceful shutdown", oldAfter, fresh)
	}

	after := &metav1.PartialObjectMetadataList{}
	after.SetGroupVersionKind(xppkgRevisionListGVK)
	if err := kube.List(context.Background(), after, client.MatchingLabels{"pkg.crossplane.io/package": providerName}); err != nil {
		t.Fatal(err)
	}
	if len(after.Items) != len(revisions.Items) {
		t.Errorf("provider revisions: %d before the restart, %d after", len(revisions.Items), len(after.Items))
	}
}

// podDeletion records the deletion of the provider's pods: their names and
// UIDs, and each server's clock just before the deletion.
type podDeletion struct {
	pods []string
	uids map[string]bool
	at   map[*tenant]string
	err  error
}

// deleteProviderOnFirstCreate waits until a role of the restart scenario
// exists on one of the servers, then reads each server's clock and deletes
// the provider's pods. It runs in its own goroutine, so it returns errors
// instead of failing the test.
func deleteProviderOnFirstCreate(active []*tenant) podDeletion {
	ctx := context.Background()
	deadline := time.Now().Add(2 * time.Minute)
	for created := false; !created; {
		for _, tn := range active {
			var n uint64
			if err := tn.srv.admin.QueryRow(ctx, "SELECT count() FROM system.roles WHERE startsWith(name, ?)", restartRolePrefix).Scan(&n); err != nil {
				return podDeletion{err: fmt.Errorf("%s: %w", tn.name, err)}
			}
			created = created || n > 0
		}
		if !created {
			if time.Now().After(deadline) {
				return podDeletion{err: errors.New("no role of the restart scenario was created within 2 minutes")}
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	d := podDeletion{uids: map[string]bool{}, at: map[*tenant]string{}}
	for _, tn := range active {
		var now string
		if err := tn.srv.admin.QueryRow(ctx, "SELECT toString(now64(6))").Scan(&now); err != nil {
			return podDeletion{err: fmt.Errorf("%s: %w", tn.name, err)}
		}
		d.at[tn] = now
	}
	pods, err := clientset.CoreV1().Pods(nsCrossplane).List(ctx, metav1.ListOptions{LabelSelector: "pkg.crossplane.io/provider=" + providerName})
	if err != nil || len(pods.Items) == 0 {
		return podDeletion{err: fmt.Errorf("provider pods: %d, %v", len(pods.Items), err)}
	}
	for _, p := range pods.Items {
		if err := clientset.CoreV1().Pods(nsCrossplane).Delete(ctx, p.Name, metav1.DeleteOptions{}); err != nil {
			return podDeletion{err: err}
		}
		d.pods = append(d.pods, p.Name)
		d.uids[string(p.UID)] = true
	}
	return d
}

// waitNewProviderPod waits until the deleted pods are gone and a new pod is
// ready.
func waitNewProviderPod(t *testing.T, old map[string]bool) {
	t.Helper()
	selector := "pkg.crossplane.io/provider=" + providerName
	eventually(t, 3*time.Minute, "a new provider pod", func() (bool, string) {
		pods, err := clientset.CoreV1().Pods(nsCrossplane).List(context.Background(), metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			return false, err.Error()
		}
		ready, stale := 0, 0
		for _, p := range pods.Items {
			switch {
			case old[string(p.UID)]:
				stale++
			case podReady(&p):
				ready++
			}
		}
		return ready == 1 && stale == 0, strconv.Itoa(ready) + " new ready, " + strconv.Itoa(stale) + " old"
	})
}

// deletion deletes every MR and checks that exactly what the provider
// created is revoked and dropped.
func (tn *tenant) deletion(t *testing.T) {
	if !tn.ok {
		t.Skip("the scenarios failed")
	}
	s := tn.srv

	// The Grant and the RoleGrant revoke what they granted; the user and the
	// role stay.
	deleteAndWait(t, 2*time.Minute, tn.grant, tn.roleGrant)
	if got := s.grantsOf(t, kindRole, roleReader); len(got) != 0 {
		t.Errorf("grants of %s after deleting the Grant: %v", roleReader, got)
	}
	if ok, _ := s.member(t, tn.userName, roleReader); ok {
		t.Errorf("%s is still a member of %s after deleting the RoleGrant", tn.userName, roleReader)
	}
	if s.uuid(t, kindUser, tn.userName) == "" || s.uuid(t, kindRole, roleReader) == "" {
		t.Error("deleting the Grant or the RoleGrant dropped the user or the role")
	}
	tn.sameForeign(t, "deleting the Grant and the RoleGrant")

	// A database with a table is not dropped (the identity has no DROP
	// TABLE either); once the table is gone it is.
	id := s.uuid(t, kindDatabase, dbShop)
	if err := kube.Delete(context.Background(), tn.db); err != nil {
		t.Fatal(err)
	}
	waitMR(t, tn.db, 2*time.Minute, "refused with DatabaseNotEmpty", refusedWith(reason.DatabaseNotEmpty))
	if got := s.uuid(t, kindDatabase, dbShop); got != id {
		t.Fatalf("%s: UUID %q after the refused deletion, want %q", dbShop, got, id)
	}
	s.exec(t, "DROP TABLE "+dbShop+".orders SYNC")
	deleteAndWait(t, 4*time.Minute, tn.db)

	owned := append([]resource.Managed{tn.user, tn.role}, tn.pcMRs...)
	owned = append(owned, tn.restart...)
	owned = append(owned, tn.restartRoles...)
	deleteAndWait(t, 3*time.Minute, objects(owned...)...)
	for _, c := range [][2]string{
		{kindDatabase, dbShop}, {kindRole, roleReader}, {kindUser, tn.userName}, {kindDatabase, "app_scratch"}, {kindRole, "app_tenant_role"},
		{kindDatabase, "app_restart"}, {kindRole, restartRole}, {kindUser, "app_restart_user"}, {kindUser, tn.serviceAccountUser("restarter")},
	} {
		if id := s.uuid(t, c[0], c[1]); id != "" {
			t.Errorf("%s %s still exists", c[0], c[1])
		}
	}

	// MRs that never owned anything leave the objects alone.
	deleteAndWait(t, 2*time.Minute, objects(append(tn.foreignMRs, tn.policyMRs...)...)...)
	tn.sameForeign(t, "deleting all MRs")
	want := []string{"DATABASE " + foreignDB, "DATABASE " + legacyDB, "ROLE " + foreignRole, "ROLE " + legacyRole, "USER " + foreignUser}
	if got := s.names(t, "app_"); !slices.Equal(got, want) {
		t.Errorf("app_ objects after deleting every MR: %v, want %v", got, want)
	}
	// Only the tenant's identity (users.d, Antalya) is left of the service
	// account users.
	var identities []string
	if tn.jwt {
		identities = []string{"USER " + tn.serviceAccountUser("tenant-admin")}
	}
	if got := s.names(t, tn.serviceAccountUser("")); !slices.Equal(got, identities) {
		t.Errorf("service account users left: %v, want %v", got, identities)
	}
	// The foreign user still logs in with its own password.
	conn, err := s.open(t, &chgo.Options{Auth: chgo.Auth{Username: foreignUser, Password: tn.foreignPassword}})
	if err != nil {
		t.Fatalf("the foreign user cannot log in with its own password: %v", err)
	}
	if who, _ := whoami(t, conn); who != foreignUser {
		t.Errorf("logged in as %q", who)
	}
	expectSelect(t, conn, "SELECT count() FROM system.one")

	// With no MR left, the configs can be deleted (their usages are gone).
	deleteAndWait(t, 2*time.Minute,
		&v1alpha1.ProviderConfig{ObjectMeta: tn.meta(tn.pc.Name)},
		&v1alpha1.ClusterProviderConfig{ObjectMeta: metav1.ObjectMeta{Name: tn.cpc.Name}})
}

func expectSelect(t *testing.T, conn driver.Conn, query string) {
	t.Helper()
	var n uint64
	if err := conn.QueryRow(context.Background(), query).Scan(&n); err != nil {
		t.Errorf("%s: %v", query, err)
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// xppkgRevisionListGVK lists ProviderRevisions as metadata only.
var xppkgRevisionListGVK = schema.GroupVersionKind{Group: "pkg.crossplane.io", Version: "v1", Kind: "ProviderRevisionList"}

func isDatabase(mg resource.Managed) bool {
	_, ok := mg.(*v1alpha1.Database)
	return ok
}
