// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	chgo "github.com/ClickHouse/clickhouse-go/v2"
	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	"github.com/golang-jwt/jwt/v5"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/crossplane/crossplane-runtime/v2/pkg/event"
	"github.com/crossplane/crossplane-runtime/v2/pkg/reconciler/managed"
	"github.com/crossplane/crossplane-runtime/v2/pkg/resource"

	"github.com/infrabay/provider-clickhouse/apis/v1alpha1"
	ch "github.com/infrabay/provider-clickhouse/internal/clients/clickhouse"
	"github.com/infrabay/provider-clickhouse/internal/controller/access"
	"github.com/infrabay/provider-clickhouse/internal/controller/access/accesstest"
	"github.com/infrabay/provider-clickhouse/internal/controller/ownership"
	"github.com/infrabay/provider-clickhouse/internal/controller/providerconfig"
)

// Image families.
const (
	familyUpstream = "upstream"
	familyAntalya  = "antalya"
)

// The password test vector of design 3.5: a user created with this hash and
// salt logs in with testPassword.
const (
	testPassword = "Pa55word"
	testHash     = "6257310c34f98d6e96c672db8cee44e0077f4e03eb3581a7260ccd6861848188" // gitleaks:allow
	testSalt     = "00112233445566778899aabbccddeeff"
)

// Token processors configured on Antalya fixtures. The provider identity
// logs in through processorTest (username_claim email); processorTest2 has
// another key and username_claim preferred_username, for cross-processor
// tests.
const (
	processorTest  = "test"
	processorTest2 = "test2"
)

// imageList returns the images of a family: the comma-separated list in env,
// the default if env is unset, or none if env is "none". Unlike images it
// never skips the test.
func imageList(env, def string) []string {
	v := os.Getenv(env)
	if v == "" {
		v = def
	}
	if v == "none" {
		return nil
	}
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func is253(image string) bool { return strings.Contains(image, ":25.3") }

// accessGrants are the provider identity's grants for every kind (design
// 9.4), scoped to the app_ prefix: user and role DDL on app_*, the reads the
// access entity kinds need, ROLE ADMIN for RoleGrant of provider-created
// roles, delegable privileges WITH GRANT OPTION on app_*.* for Grant, and
// the Database grants (without ALTER MODIFY DATABASE COMMENT on 25.3, where
// granting it is a syntax error).
func accessGrants(image string) []string {
	g := []string{
		"GRANT CREATE USER, ALTER USER, DROP USER ON `app_`*",
		"GRANT CREATE ROLE, DROP ROLE ON `app_`*",
		"GRANT SHOW USERS, SHOW ROLES ON *.*",
		"GRANT ROLE ADMIN ON *.*",
		"GRANT SELECT ON system.user_directories",
		"GRANT SELECT ON system.role_grants",
		"GRANT SELECT ON system.grants",
		"GRANT SELECT ON system.settings_profile_elements",
		"GRANT SELECT, INSERT, SHOW TABLES ON app_*.* WITH GRANT OPTION",
	}
	return append(g, databaseGrants(!is253(image))...)
}

// accessUpstreamServer starts upstream ClickHouse with a password provider
// identity in users.d that holds accessGrants.
func accessUpstreamServer(t *testing.T, image, password string) *server {
	t.Helper()
	usersD := fmt.Sprintf(`users:
  %s:
    password_sha256_hex: %q
    networks: {ip: ["::/0"]}
    profile: default
    quota: default
    grants:
      query:
%s`, providerUser, sha256Hex(password), grantsYAML(accessGrants(image)))
	return startServer(t, image, map[string]string{"/etc/clickhouse-server/users.d/provider-clickhouse.yaml": usersD})
}

// accessAntalyaServer starts Antalya with two jwt_static_key HS256 token
// processors (random keys, never committed) and a JWT provider identity in
// users.d that pins processorTest and holds accessGrants. It returns the
// processors' keys.
func accessAntalyaServer(t *testing.T, image string) (*server, map[string][]byte) {
	t.Helper()
	keys := map[string][]byte{processorTest: []byte(randHex(24)), processorTest2: []byte(randHex(24))}
	processors := fmt.Sprintf(`token_processors:
  %s:
    type: jwt_static_key
    algo: HS256
    static_key: %q
    expected_issuer: %s
    expected_audience: %s
    username_claim: email
  %s:
    type: jwt_static_key
    algo: HS256
    static_key: %q
    expected_issuer: %s
    expected_audience: %s
    username_claim: preferred_username
`, processorTest, keys[processorTest], tokenIssuer, tokenAudience, processorTest2, keys[processorTest2], tokenIssuer, tokenAudience)
	usersD := fmt.Sprintf(`users:
  %q:
    jwt: {processor: %s}
    networks: {ip: ["::/0"]}
    profile: default
    quota: default
    grants:
      query:
%s`, jwtProviderUser, processorTest, grantsYAML(accessGrants(image)))
	srv := startServer(t, image, map[string]string{
		"/etc/clickhouse-server/config.d/token_processors.yaml":   processors,
		"/etc/clickhouse-server/users.d/provider-clickhouse.yaml": usersD,
	})
	return srv, keys
}

// mintTokenClaims signs an HS256 token with the given claims. iss, aud, iat
// (10 s in the past, for clock skew) and exp (one hour) are added unless set.
func mintTokenClaims(t *testing.T, key []byte, claims jwt.MapClaims) string {
	t.Helper()
	now := time.Now()
	c := jwt.MapClaims{"iss": tokenIssuer, "aud": tokenAudience, "iat": now.Add(-10 * time.Second).Unix(), "exp": now.Add(time.Hour).Unix()}
	for k, v := range claims {
		c[k] = v
	}
	s, err := jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// accessFixture is a server whose provider identity holds the grants of
// every access entity kind, and a fake Kubernetes API with a
// ClusterProviderConfig "access" for it. Upstream servers use a password
// identity, Antalya servers a JWT identity read from a token file.
type accessFixture struct {
	t      *testing.T
	family string
	image  string
	srv    *server
	kube   client.Client
	cache  *ch.Cache
	// journal is the create journal of this fixture's connectors and envs.
	journal *ownership.Journal
	// pc references the ClusterProviderConfig.
	pc *xpv2.ProviderConfigReference
	// identity is the provider identity's user name.
	identity string
	// jwtKeys are the keys of the token processors (Antalya only).
	jwtKeys map[string][]byte
}

// newAccessFixture starts a server of family for image and stores a
// ClusterProviderConfig "access" with policy.
func newAccessFixture(t *testing.T, family, image string, policy v1alpha1.ClusterPolicy) *accessFixture {
	t.Helper()
	f := &accessFixture{t: t, family: family, image: image, journal: ownership.NewJournal(),
		pc: &xpv2.ProviderConfigReference{Kind: v1alpha1.ClusterProviderConfigKind, Name: "access"}}
	switch family {
	case familyUpstream:
		password := randHex(16)
		f.srv = accessUpstreamServer(t, image, password)
		cpc := passwordCPC(f.srv, "access", policy)
		f.kube = newKube(t, cpc, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: "crossplane-system", Name: "provider-clickhouse"},
			Data:       map[string][]byte{"password": []byte(password)},
		})
		f.identity = providerUser
	case familyAntalya:
		f.srv, f.jwtKeys = accessAntalyaServer(t, image)
		tokenPath := filepath.Join(t.TempDir(), "token")
		writeToken(t, tokenPath, mintToken(t, f.jwtKeys[processorTest], jwtProviderUser, tokenAudience, 2*time.Hour))
		cpc := tokenFileCPC(f.srv, tokenPath)
		cpc.Name, cpc.UID = "access", types.UID("cpc-access")
		cpc.Spec.Policy = policy
		f.kube = newKube(t, cpc)
		f.identity = jwtProviderUser
	default:
		t.Fatalf("unknown image family %q", family)
	}
	f.cache = ch.NewCache(ch.OpenDriver)
	t.Cleanup(f.cache.Close)
	return f
}

// forEachAccessImage runs fn in a subtest for every upstream image and then
// every Antalya image (XPCH_UPSTREAM_IMAGES, XPCH_ANTALYA_IMAGES), each with
// its own fixture.
func forEachAccessImage(t *testing.T, policy v1alpha1.ClusterPolicy, fn func(t *testing.T, f *accessFixture)) {
	t.Helper()
	ran := false
	for _, fam := range []struct{ name, env, def string }{
		{familyUpstream, envUpstreamImages, defaultUpstreamImages},
		{familyAntalya, envAntalyaImages, defaultAntalyaImages},
	} {
		for _, image := range imageList(fam.env, fam.def) {
			ran = true
			t.Run(image, func(t *testing.T) { fn(t, newAccessFixture(t, fam.name, image, policy)) })
		}
	}
	if !ran {
		t.Skip("no images selected")
	}
}

// resolver returns the fixture's config resolver.
func (f *accessFixture) resolver() providerconfig.Resolver { return newResolver(f.kube, nil) }

// resolve resolves the config of mg, which must reference the fixture's
// config (or another config in the fixture's Kubernetes API).
func (f *accessFixture) resolve(t *testing.T, mg resource.ModernManaged) *providerconfig.Resolved {
	t.Helper()
	res, err := f.resolver().Resolve(context.Background(), mg)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// client returns a ClickHouse client of the provider identity, closed when
// the test ends.
func (f *accessFixture) client(t *testing.T) ch.Client {
	t.Helper()
	probe := &v1alpha1.Role{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "probe"}}
	probe.Spec.ProviderConfigReference = f.pc
	res := f.resolve(t, probe)
	cl, err := f.cache.Acquire(context.Background(), res.CacheKey, res.Config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cl.Close() })
	return cl
}

// env returns the access.Env that the connector would build for mg: the
// provider identity's client, mg's resolved config, the fixture's Kubernetes
// API and journal. rec may be nil.
func (f *accessFixture) env(t *testing.T, mg resource.ModernManaged, rec event.Recorder) *access.Env {
	t.Helper()
	res := f.resolve(t, mg)
	cl, err := f.cache.Acquire(context.Background(), res.CacheKey, res.Config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cl.Close() })
	if rec == nil {
		rec = event.NewNopRecorder()
	}
	return &access.Env{Client: cl, Resolved: res, Kube: f.kube, Reader: f.kube, APIReader: f.kube, Recorder: rec, Journal: f.journal}
}

// accessConnector returns the connector of an access entity kind wired to
// the fixture: its resolver, client cache, Kubernetes API and journal.
func accessConnector[T resource.ModernManaged](f *accessFixture, kind string, newClient func(*access.Env) managed.TypedExternalClient[T], refs func(T) []providerconfig.Reference) *access.Connector[T] {
	return &access.Connector[T]{
		Kind:       kind,
		Kube:       f.kube,
		Reader:     f.kube,
		APIReader:  f.kube,
		Usage:      nopTracker{},
		Resolver:   f.resolver(),
		Cache:      f.cache,
		Journal:    f.journal,
		References: refs,
		NewClient:  newClient,
	}
}

// newAccessHarness returns an MR harness for an access entity kind on the
// fixture's Kubernetes API.
func newAccessHarness[T resource.ModernManaged](t *testing.T, f *accessFixture, connector managed.TypedExternalConnector[T], newObj func() T, inits ...managed.Initializer) *mrHarness[T] {
	t.Helper()
	return newMRHarness(t, f.kube, connector, newObj, inits...)
}

// adminUser creates a password user (the test vector) as the admin and
// returns its UUID.
func (f *accessFixture) adminUser(t *testing.T, name string) string {
	t.Helper()
	f.srv.exec(t, "CREATE USER "+ch.QuoteIdent(name)+" IDENTIFIED WITH sha256_hash BY "+ch.QuoteString(testHash)+" SALT "+ch.QuoteString(testSalt))
	id, _, ok := f.srv.entity(t, access.User, name)
	if !ok {
		t.Fatalf("user %q was not created", name)
	}
	return id
}

// adminRole creates a role as the admin and returns its UUID.
func (f *accessFixture) adminRole(t *testing.T, name string) string {
	t.Helper()
	f.srv.exec(t, "CREATE ROLE "+ch.QuoteIdent(name))
	id, _, ok := f.srv.entity(t, access.Role, name)
	if !ok {
		t.Fatalf("role %q was not created", name)
	}
	return id
}

// ownedUser creates a user as the admin and stores a User MR named mrName
// that holds a valid claim on it, as after its Create or an adoption, so
// that a Grant or RoleGrant may use it as a grantee.
func (f *accessFixture) ownedUser(t *testing.T, mrName, name string) *v1alpha1.User {
	t.Helper()
	u := accesstest.OwnedUser(namespace, mrName, name, f.adminUser(t, name), f.pc)
	if err := f.kube.Create(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	return u
}

// ownedRole creates a role as the admin and stores a Role MR named mrName
// that holds a valid claim on it.
func (f *accessFixture) ownedRole(t *testing.T, mrName, name string) *v1alpha1.Role {
	t.Helper()
	r := accesstest.OwnedRole(namespace, mrName, name, f.adminRole(t, name), f.pc)
	if err := f.kube.Create(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	return r
}

// entity reads a user's or role's UUID and storage as the admin.
func (s *server) entity(t *testing.T, kind access.EntityKind, name string) (id, storage string, ok bool) {
	t.Helper()
	table := "users"
	if kind == access.Role {
		table = "roles"
	}
	err := s.admin.QueryRow(chgo.Context(context.Background(), chgo.WithParameters(chgo.Parameters{"n": ch.ParamValue(name)})),
		"SELECT toString(id), storage FROM system."+table+" WHERE name = {n:String}").Scan(&id, &storage)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return "", "", false
		}
		t.Fatal(err)
	}
	return id, storage, true
}

// rawString runs a query as the admin and returns the first column of the
// first row exactly as the driver returns it, e.g. SHOW CREATE USER output.
func (s *server) rawString(t *testing.T, query string) string {
	t.Helper()
	var v string
	if err := s.admin.QueryRow(context.Background(), query).Scan(&v); err != nil {
		t.Fatalf("admin %q: %v", query, err)
	}
	return v
}

// rawStrings runs a query as the admin and returns the first column of every
// row, e.g. SHOW GRANTS output.
func (s *server) rawStrings(t *testing.T, query string) []string {
	t.Helper()
	rows, err := s.admin.Query(context.Background(), query)
	if err != nil {
		t.Fatalf("admin %q: %v", query, err)
	}
	defer rows.Close() //nolint:errcheck // Test helper.
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}
