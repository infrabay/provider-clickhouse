// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	chgo "github.com/ClickHouse/clickhouse-go/v2"
	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	"github.com/golang-jwt/jwt/v5"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/crossplane/crossplane-runtime/v2/pkg/meta"
	"github.com/crossplane/crossplane-runtime/v2/pkg/reconciler/managed"
	"github.com/crossplane/crossplane-runtime/v2/pkg/resource"

	"github.com/infrabay/provider-clickhouse/apis/v1alpha1"
	ch "github.com/infrabay/provider-clickhouse/internal/clients/clickhouse"
	"github.com/infrabay/provider-clickhouse/internal/clients/clickhouse/auth"
	"github.com/infrabay/provider-clickhouse/internal/clients/clickhouse/sqlx"
	"github.com/infrabay/provider-clickhouse/internal/controller/access"
	"github.com/infrabay/provider-clickhouse/internal/controller/ownership"
	"github.com/infrabay/provider-clickhouse/internal/controller/user"
	"github.com/infrabay/provider-clickhouse/internal/reason"
)

// envUpdateFixtures rewrites the SHOW CREATE USER fixtures of the parser's
// unit tests from the servers under test.
const envUpdateFixtures = "XPCH_UPDATE_FIXTURES"

// userPolicy is accessPolicy (access_test.go): the test namespace may
// manage app_ users and roles and name the provider identity (for the
// self-modification and read-only checks), with password and JWT users
// pinned to processorTest. The identity itself can only touch app_ names.
func userPolicy() v1alpha1.ClusterPolicy {
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

// TestUser runs the User controller against every image with the minimal,
// prefix-scoped provider identity: password users (upstream and Antalya),
// JWT users (Antalya; Unsupported upstream), drift repair, ownership and
// the connection Secret.
func TestUser(t *testing.T) {
	forEachAccessImage(t, userPolicy(), func(t *testing.T, f *accessFixture) {
		t.Run("ShowCreateUserFixtures", func(t *testing.T) { testUserFixtures(t, f) })
		t.Run("PasswordScheme", func(t *testing.T) { testUserPasswordScheme(t, f) })
		t.Run("PasswordLifecycle", func(t *testing.T) { testUserPasswordLifecycle(t, f) })
		t.Run("PasswordSecretRef", func(t *testing.T) { testUserPasswordSecretRef(t, f) })
		t.Run("UncontrolledSecret", func(t *testing.T) { testUserUncontrolledSecret(t, f) })
		t.Run("Hosts", func(t *testing.T) { testUserHosts(t, f) })
		t.Run("DefaultRoles", func(t *testing.T) { testUserDefaultRoles(t, f) })
		t.Run("Ownership", func(t *testing.T) { testUserOwnership(t, f) })
		t.Run("ProviderIdentity", func(t *testing.T) { testUserIdentity(t, f) })
		t.Run("PermissionDenied", func(t *testing.T) { testUserPermissionDenied(t, f) })
		if f.family == familyAntalya {
			t.Run("JWT", func(t *testing.T) { testUserJWT(t, f) })
		} else {
			t.Run("JWTUnsupported", func(t *testing.T) { testUserJWTUnsupported(t, f) })
		}
	})
}

// --- harness ---

// userHarness drives the User external client on an access fixture.
type userHarness struct {
	*mrHarness[*v1alpha1.User]
	f   *accessFixture
	rec *eventRecorder
}

func newUserHarness(t *testing.T, f *accessFixture) *userHarness {
	t.Helper()
	rec := &eventRecorder{}
	conn := accessConnector(f, "User", user.NewExternal, nil)
	conn.Recorder = rec
	init := ownership.NewInitializer(f.kube, func(mg resource.Managed) *string { return mg.(*v1alpha1.User).Spec.ForProvider.Name })
	return &userHarness{
		mrHarness: newAccessHarness(t, f, conn, func() *v1alpha1.User { return &v1alpha1.User{} }, init),
		f:         f,
		rec:       rec,
	}
}

// createUser stores a User MR named mrName for the ClickHouse user name,
// with a generated password and the connection Secret "<mrName>-conn",
// unless opts change them.
func (h *userHarness) createUser(mrName, name string, opts ...func(*v1alpha1.User)) {
	h.t.Helper()
	u := &v1alpha1.User{ObjectMeta: metav1.ObjectMeta{Name: mrName}}
	u.Spec.ProviderConfigReference = h.f.pc
	u.Spec.ForProvider.Name = &name
	u.Spec.ForProvider.Authentication.Password = &v1alpha1.PasswordAuthentication{Generate: &v1alpha1.GeneratePassword{}}
	u.Spec.WriteConnectionSecretToReference = &xpv2.LocalSecretReference{Name: mrName + "-conn"}
	for _, o := range opts {
		o(u)
	}
	h.create(u)
}

// direct connects to the MR's config and runs fn with the external client
// and the MR, outside the reconcile loop. The harness does not persist
// status, so conditions and status.atProvider are checked this way.
func (h *userHarness) direct(mrName string, fn func(ctx context.Context, ext managed.TypedExternalClient[*v1alpha1.User], u *v1alpha1.User)) *v1alpha1.User {
	h.t.Helper()
	ctx := context.Background()
	u := h.get(mrName)
	ext, err := h.connector.Connect(ctx, u)
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = ext.Disconnect(ctx) }()
	fn(ctx, ext, u)
	return u
}

// observed runs Observe and returns the MR with the status it set.
func (h *userHarness) observed(mrName string) *v1alpha1.User {
	h.t.Helper()
	return h.direct(mrName, func(ctx context.Context, ext managed.TypedExternalClient[*v1alpha1.User], u *v1alpha1.User) {
		_, _ = ext.Observe(ctx, u)
	})
}

// events returns the reasons of the events recorded since the last call.
func (h *userHarness) events() []string {
	h.rec.mu.Lock()
	defer h.rec.mu.Unlock()
	out := []string{}
	for _, e := range h.rec.events {
		out = append(out, string(e.Reason))
	}
	h.rec.events = nil
	return out
}

// rotationToken makes the user a password user with a generated password
// and this rotation token.
func rotationToken(tok string) func(*v1alpha1.User) {
	return func(u *v1alpha1.User) {
		u.Spec.ForProvider.Authentication = v1alpha1.UserAuthentication{Password: &v1alpha1.PasswordAuthentication{Generate: &v1alpha1.GeneratePassword{RotationToken: &tok}}}
	}
}

func jwtAuth(processor, claims string) func(*v1alpha1.User) {
	return func(u *v1alpha1.User) {
		j := &v1alpha1.JWTAuthentication{Processor: processor}
		if claims != "" {
			j.Claims = &apiextensionsv1.JSON{Raw: []byte(claims)}
		}
		u.Spec.ForProvider.Authentication = v1alpha1.UserAuthentication{JWT: j}
	}
}

func policies(p ...xpv2.ManagementAction) func(*v1alpha1.User) {
	return func(u *v1alpha1.User) { u.SetManagementPolicies(p) }
}

// mustReconcile reconciles and fails unless the outcome passes check.
func (h *userHarness) mustReconcile(mrName, what string, check func(o outcome) bool) outcome {
	h.t.Helper()
	o := h.reconcile(mrName)
	if !check(o) {
		h.t.Fatalf("%s: %s", what, o)
	}
	return o
}

func created(o outcome) bool { return o.err == nil && o.created }
func updated(o outcome) bool { return o.err == nil && o.updated }
func steady(o outcome) bool {
	return o.err == nil && o.obs.ResourceExists && o.obs.ResourceUpToDate && !o.updated
}
func released(o outcome) bool { return o.err == nil && o.gone }
func failedWith(r reason.Reason) func(outcome) bool {
	return func(o outcome) bool { return reason.Is(o.err, r) && !o.created && !o.updated && !o.deleted }
}

// --- server helpers ---

// showCreateUser returns SHOW CREATE USER output, read raw through the
// driver as the admin.
func (f *accessFixture) showCreateUser(t *testing.T, name string) string {
	t.Helper()
	return f.srv.rawString(t, sqlx.ShowCreateUser(name))
}

// userRow is a user's row of system.users, as the admin reads it.
type userRow struct {
	authTypes       []string
	hostIP          []string
	hostNames       []string
	hostRegexps     []string
	hostLikes       []string
	rolesAll        uint8
	rolesList       []string
	rolesExcept     []string
	defaultDatabase string
}

func (f *accessFixture) userRow(t *testing.T, name string) userRow {
	t.Helper()
	var r userRow
	var id string
	err := f.srv.admin.QueryRow(chgo.Context(context.Background(), chgo.WithParameters(chgo.Parameters{"name": ch.ParamValue(name)})), sqlx.QueryUser).
		Scan(&id, &r.authTypes, &r.hostIP, &r.hostNames, &r.hostRegexps, &r.hostLikes, &r.rolesAll, &r.rolesList, &r.rolesExcept, &r.defaultDatabase)
	if err != nil {
		t.Fatalf("system.users row of %q: %v", name, err)
	}
	return r
}

// loginPassword logs in as name with password through the provider's own
// client code and returns the error of the first statement.
func (f *accessFixture) loginPassword(t *testing.T, name, password string) error {
	t.Helper()
	return f.login(t, ch.Config{Host: f.srv.host, Port: f.srv.nativePort, Protocol: ch.ProtocolNative, Username: name, Password: password, MaxOpenConns: 1}, name)
}

// loginJWT logs in with token, read from a token file by the provider's
// own token source (which checks the audience first), and expects to be
// user name.
func (f *accessFixture) loginJWT(t *testing.T, token, name string) error {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	writeToken(t, path, token)
	src, err := auth.NewFileSource(path, auth.Checker{Audience: tokenAudience})
	if err != nil {
		t.Fatal(err)
	}
	return f.login(t, ch.Config{Host: f.srv.host, Port: f.srv.nativePort, Protocol: ch.ProtocolNative, TokenSource: src, MaxOpenConns: 1}, name)
}

func (f *accessFixture) login(t *testing.T, cfg ch.Config, want string) error {
	t.Helper()
	conn, err := ch.OpenDriver(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close() //nolint:errcheck // Test cleanup.
	info, err := ch.ProbeInfo(context.Background(), conn)
	if err != nil {
		return err
	}
	if info.CurrentUser != want {
		return fmt.Errorf("logged in as %q, want %q", info.CurrentUser, want)
	}
	return nil
}

func authFailed(err error) bool { return ch.IsCode(err, ch.CodeAuthenticationFailed) }

// identifiedQueries returns the statements with an IDENTIFIED clause that
// the MR "<kind>/<namespace>/<mrName>" sent, as system.query_log records
// them.
func (f *accessFixture) identifiedQueries(t *testing.T, mrName string) []string {
	t.Helper()
	f.srv.exec(t, "SYSTEM FLUSH LOGS")
	ctx := chgo.Context(context.Background(), chgo.WithParameters(chgo.Parameters{"c": ch.ParamValue("User/" + namespace + "/" + mrName)}))
	rows, err := f.srv.admin.Query(ctx, "SELECT query FROM system.query_log WHERE log_comment = {c:String} AND type = 'QueryFinish' AND query ILIKE '%IDENTIFIED%' ORDER BY event_time_microseconds")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close() //nolint:errcheck // Test helper.
	var out []string
	for rows.Next() {
		var q string
		if err := rows.Scan(&q); err != nil {
			t.Fatal(err)
		}
		out = append(out, q)
	}
	return out
}

// identified is IDENTIFIED WITH sha256_hash BY ... SALT ... of the test
// vector.
var identified = "IDENTIFIED WITH sha256_hash BY " + ch.QuoteString(testHash) + " SALT " + ch.QuoteString(testSalt)

// --- SHOW CREATE USER fixtures ---

// userFixtureCase creates a user for a SHOW CREATE USER fixture of the
// parser's unit tests (internal/clients/clickhouse/sqlx/testdata/user).
type userFixtureCase struct {
	name  string
	user  string
	stmts []string
	jwt   bool
}

func userFixtureCases() []userFixtureCase {
	q := ch.QuoteIdent
	return []userFixtureCase{
		{name: "password", user: "fx_password", stmts: []string{"CREATE USER fx_password " + identified}},
		{name: "hosts", user: "fx_hosts", stmts: []string{"CREATE USER fx_hosts " + identified +
			" HOST IP '10.1.2.3/8', IP '2001:db8::1/128', IP 'fd00:0::1', NAME 'Host.Example.COM'"}},
		{name: "host_none", user: "fx_host_none", stmts: []string{"CREATE USER fx_host_none " + identified + " HOST NONE"}},
		{name: "host_local", user: "fx_host_local", stmts: []string{"CREATE USER fx_host_local " + identified + " HOST LOCAL"}},
		{name: "host_regexp", user: "fx_host_regexp", stmts: []string{"CREATE USER fx_host_regexp " + identified + ` HOST REGEXP '.*\\.example\\.com'`}},
		{name: "host_like", user: "fx_host_like", stmts: []string{"CREATE USER fx_host_like " + identified + " HOST LIKE '%.example.org'"}},
		{name: "default_role", user: "fx_default_role", stmts: []string{
			"CREATE ROLE " + q("fx_ro'le"),
			"CREATE USER fx_default_role " + identified + " DEFAULT ROLE ALL EXCEPT " + q("fx_ro'le"),
		}},
		{name: "settings", user: "fx_settings", stmts: []string{"CREATE USER fx_settings " + identified + " SETTINGS max_threads = 2, PROFILE 'default'"}},
		{name: "quoted_name", user: "fx_we!rd.n@me`q'\\z", stmts: []string{"CREATE USER " + q("fx_we!rd.n@me`q'\\z") + " " + identified + ` HOST NAME 'h\'1'`}},
		{name: "no_password", user: "fx_no_password", stmts: []string{"CREATE USER fx_no_password"}},
		{name: "valid_until", user: "fx_valid_until", stmts: []string{"CREATE USER fx_valid_until " + identified + " VALID UNTIL '2031-01-01 00:00:00'"}},
		{name: "every_clause", user: "fx_every_clause", stmts: []string{"CREATE USER fx_every_clause " + identified +
			" VALID UNTIL '2031-01-01 00:00:00' HOST IP '10.0.0.0/8' DEFAULT ROLE NONE DEFAULT DATABASE fx_db SETTINGS PROFILE 'default'"}},
		{name: "added_method", user: "fx_added_method", stmts: []string{
			"CREATE USER fx_added_method " + identified,
			"ALTER USER fx_added_method ADD IDENTIFIED WITH plaintext_password BY 'second' VALID UNTIL '2032-02-02 00:00:00'",
		}},
		{name: "two_expiring", user: "fx_two_expiring", stmts: []string{
			"CREATE USER fx_two_expiring " + identified,
			"ALTER USER fx_two_expiring ADD IDENTIFIED WITH plaintext_password BY 'second'",
			"ALTER USER fx_two_expiring VALID UNTIL '2031-01-01 00:00:00'",
		}},
		{name: "jwt", user: "fx_jwt", jwt: true, stmts: []string{`CREATE USER fx_jwt IDENTIFIED WITH jwt PROCESSOR 'p\'1' CLAIMS '{"hd":"it\'s \\\\ ok"}'`}},
		{name: "jwt_bare", user: "fx_jwt_bare", jwt: true, stmts: []string{"CREATE USER fx_jwt_bare IDENTIFIED WITH jwt"}},
		{name: "jwt_clauses", user: "fx_jwt_clauses", jwt: true, stmts: []string{`CREATE USER fx_jwt_clauses IDENTIFIED WITH jwt PROCESSOR 'test' CLAIMS '{"groups":["a","b"]}'` +
			" HOST NAME 'a.example.com' DEFAULT ROLE NONE DEFAULT DATABASE fx_db SETTINGS PROFILE 'default'"}},
		{name: "jwt_two", user: "fx_jwt_two", jwt: true, stmts: []string{
			`CREATE USER fx_jwt_two IDENTIFIED WITH jwt PROCESSOR 'test' CLAIMS '{"b": 1, "a": "x y"}' VALID UNTIL '2031-01-01 00:00:00'`,
			"ALTER USER fx_jwt_two ADD IDENTIFIED WITH jwt PROCESSOR 'other'",
		}},
		{name: "jwt_and_other", user: "fx_jwt_and_other", jwt: true, stmts: []string{
			"CREATE USER fx_jwt_and_other IDENTIFIED WITH jwt PROCESSOR 'test'",
			"ALTER USER fx_jwt_and_other ADD IDENTIFIED WITH plaintext_password BY 'x'",
		}},
	}
}

// testUserFixtures reads SHOW CREATE USER output through the driver and
// compares it with the fixtures of the parser's unit tests (rewritten with
// XPCH_UPDATE_FIXTURES=1). Each statement must parse.
func testUserFixtures(t *testing.T, f *accessFixture) {
	got := map[string]string{}
	for _, c := range userFixtureCases() {
		if c.jwt && f.family != familyAntalya {
			continue
		}
		for _, s := range c.stmts {
			f.srv.exec(t, s)
		}
		stmt := f.showCreateUser(t, c.user)
		if strings.ContainsAny(stmt, "\t\n") {
			t.Fatalf("%s: SHOW CREATE USER output is not a single line: %q", c.name, stmt)
		}
		def, err := sqlx.ParseShowCreateUser(stmt)
		if err != nil || def.Name != c.user || len(def.Methods) == 0 {
			t.Errorf("%s: ParseShowCreateUser(%q) = %+v, %v", c.name, stmt, def, err)
		}
		got[c.name] = stmt
	}
	names := make([]string, 0, len(got))
	for n := range got {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		b.WriteString(n + "\t" + got[n] + "\n")
	}
	version := f.client(t).Info().Version
	path := filepath.Join("..", "..", "internal", "clients", "clickhouse", "sqlx", "testdata", "user", version+".txt")
	if os.Getenv(envUpdateFixtures) != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path) //nolint:gosec // Fixture path.
	if os.IsNotExist(err) {
		t.Skipf("no fixture for %s; run with %s=1 to record one", version, envUpdateFixtures)
	}
	if err != nil {
		t.Fatal(err)
	}
	if string(want) != b.String() {
		t.Errorf("SHOW CREATE USER output of %s differs from %s:\n got:\n%s\nwant:\n%s", version, path, b.String(), want)
	}
}

// --- scenarios ---

// testUserPasswordScheme checks the password scheme of design 3.5 against
// the server, with the provider's statement builder and client: the test
// vector logs in, a hash of the decoded salt bytes never does, and server
// errors of IDENTIFIED statements carry no secret.
func testUserPasswordScheme(t *testing.T, f *accessFixture) {
	ctx := context.Background()
	cl := f.client(t)
	create := func(name string, h sqlx.PasswordHash) {
		t.Helper()
		stmt, err := sqlx.CreateUserStmt(sqlx.CreateUser{Name: name, Auth: sqlx.UserAuth{Password: &h}, DefaultRole: sqlx.DefaultRole{All: true}})
		if err != nil {
			t.Fatal(err)
		}
		if err := cl.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	create("app_scheme_vector", sqlx.PasswordHash{Hash: sqlx.HashPassword(testPassword, testSalt), Salt: testSalt})
	if err := f.loginPassword(t, "app_scheme_vector", testPassword); err != nil {
		t.Fatalf("test vector login: %v", err)
	}
	decoded, err := hex.DecodeString(testSalt)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(append([]byte(testPassword), decoded...))
	create("app_scheme_bytes", sqlx.PasswordHash{Hash: hex.EncodeToString(sum[:]), Salt: testSalt})
	if err := f.loginPassword(t, "app_scheme_bytes", testPassword); !authFailed(err) {
		t.Fatalf("a hash of the decoded salt bytes must never log in (516): %v", err)
	}
	h, err := sqlx.NewPasswordHash("Fresh-Passw0rd-x")
	if err != nil {
		t.Fatal(err)
	}
	create("app_scheme_fresh", h)
	if err := f.loginPassword(t, "app_scheme_fresh", "Fresh-Passw0rd-x"); err != nil {
		t.Fatalf("fresh salt login: %v", err)
	}

	// A hash without SALT is accepted, but cannot match; the provider always
	// sends SALT.
	if err := cl.Exec(ctx, "CREATE USER app_scheme_nosalt IDENTIFIED WITH sha256_hash BY "+ch.QuoteString(testHash)); err != nil {
		t.Fatal(err)
	}
	if err := f.loginPassword(t, "app_scheme_nosalt", testPassword); !authFailed(err) {
		t.Fatalf("sha256_hash without SALT: %v", err)
	}

	// A short hash fails with 36, a malformed clause with 62; neither error
	// carries a literal.
	for stmt, code := range map[string]int32{
		"CREATE USER app_scheme_short IDENTIFIED WITH sha256_hash BY 'abcd' SALT " + ch.QuoteString(testSalt):      ch.CodeBadArguments,
		"CREATE USER app_scheme_syntax " + identified + " HOST IP '10.0.0.0/8' '10.0.0.1/8'":                       ch.CodeSyntaxError,
		"CREATE USER app_scheme_syntax2 IDENTIFIED WITH sha256_hash BY " + ch.QuoteString(testHash) + " SALT SALT": ch.CodeSyntaxError,
	} {
		err := cl.Exec(ctx, stmt)
		if !ch.IsCode(err, code) {
			t.Errorf("%q: %v, want code %d", stmt, err, code)
			continue
		}
		if msg := err.Error(); strings.Contains(msg, testHash) || strings.Contains(msg, testSalt) || strings.Contains(msg, "abcd") {
			t.Errorf("a server error of an IDENTIFIED statement leaks a literal: %s", msg)
		}
	}
}

func testUserPasswordLifecycle(t *testing.T, f *accessFixture) {
	h := newUserHarness(t, f)
	const name = "app_etl"
	h.createUser("etl", name, rotationToken("t1"))

	h.mustReconcile("etl", "create", created)
	sec := h.secret("etl")
	password := string(sec[v1alpha1.ConnectionKeyPassword])
	want := map[string]string{
		v1alpha1.ConnectionKeyUsername:      name,
		v1alpha1.ConnectionKeyEndpoint:      f.srv.host,
		v1alpha1.ConnectionKeyPort:          fmt.Sprint(f.srv.nativePort),
		v1alpha1.ConnectionKeyProtocol:      "Native",
		v1alpha1.ConnectionKeyTLS:           "false",
		v1alpha1.ConnectionKeyRotationToken: "t1",
		v1alpha1.ConnectionKeyPassword:      password,
	}
	if diff := cmpSecret(want, sec); diff != "" || len(password) != sqlx.DefaultPasswordLength {
		t.Fatalf("connection Secret after create (password of %d characters): %s", len(password), diff)
	}
	id, storage, ok := f.srv.entity(t, access.User, name)
	if !ok || storage != ch.StorageLocalDirectory || ownership.ClaimOf(h.get("etl")).OwnedID != id {
		t.Fatalf("server user %q (%s, %t), claim %v", id, storage, ok, h.get("etl").GetAnnotations())
	}
	if err := f.loginPassword(t, name, password); err != nil {
		t.Fatalf("login with the published password: %v", err)
	}
	// The server masks the hash in query_log.
	queries := f.identifiedQueries(t, "etl")
	if len(queries) == 0 {
		t.Fatal("no CREATE USER in query_log")
	}
	for _, q := range queries {
		if !strings.Contains(q, "IDENTIFIED WITH sha256_password") || strings.Contains(q, "sha256_hash") || strings.Contains(q, "SALT") || strings.Contains(q, password) {
			t.Errorf("query_log must mask the hash: %s", q)
		}
	}

	// Late-initializes defaultRoles, defaultDatabase and settingsProfile.
	o := h.mustReconcile("etl", "observe", func(o outcome) bool { return o.err == nil && o.obs.ResourceUpToDate && !o.updated })
	if !o.obs.ResourceLateInitialized {
		t.Error("the first observe late-initializes")
	}
	p := h.get("etl").Spec.ForProvider
	if p.DefaultRoles == nil || p.DefaultRoles.Policy != v1alpha1.DefaultRolesAll || p.DefaultDatabase == nil || *p.DefaultDatabase != "" || p.SettingsProfile == nil || *p.SettingsProfile != "" {
		t.Errorf("late-initialized spec: %+v", p)
	}
	h.mustReconcile("etl", "steady", steady)
	if got := h.secret("etl"); cmpSecret(want, got) != "" {
		t.Errorf("an unchanged user must not change its Secret: %s", cmpSecret(want, got))
	}
	if u := h.observed("etl"); readyStatus(u) != "True" || u.Status.AtProvider.ID != id || u.Status.AtProvider.Storage != ch.StorageLocalDirectory ||
		!slices.Equal(u.Status.AtProvider.AuthTypes, []string{"sha256_password"}) || !slices.Equal(u.Status.AtProvider.HostIP, []string{"::/0"}) ||
		u.Status.AtProvider.DefaultRoles != "ALL" {
		t.Errorf("Ready %s, status: %+v", readyStatus(u), u.Status.AtProvider)
	}

	// Rotation.
	h.events()
	h.mutate("etl", rotationToken("t2"))
	h.mustReconcile("etl", "rotate", updated)
	rotated := string(h.secret("etl")[v1alpha1.ConnectionKeyPassword])
	if rotated == password || string(h.secret("etl")[v1alpha1.ConnectionKeyRotationToken]) != "t2" {
		t.Fatalf("rotation: Secret %v", h.secret("etl"))
	}
	if diff := cmpStrings([]string{string(user.ReasonPasswordRotated)}, h.events()); diff != "" {
		t.Errorf("events: %s", diff)
	}
	if err := f.loginPassword(t, name, rotated); err != nil {
		t.Fatalf("login with the rotated password: %v", err)
	}
	if err := f.loginPassword(t, name, password); !authFailed(err) {
		t.Fatalf("the old password must stop working: %v", err)
	}
	h.mustReconcile("etl", "steady after rotation", steady)

	// A lost connection Secret: a new password, with a Warning event.
	if err := f.kube.Delete(context.Background(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "etl-conn"}}); err != nil {
		t.Fatal(err)
	}
	h.mustReconcile("etl", "regenerate", updated)
	regenerated := string(h.secret("etl")[v1alpha1.ConnectionKeyPassword])
	if regenerated == "" || regenerated == rotated || string(h.secret("etl")[v1alpha1.ConnectionKeyRotationToken]) != "t2" {
		t.Fatalf("regenerated Secret: %v", h.secret("etl"))
	}
	if diff := cmpStrings([]string{string(user.ReasonPasswordRegenerated)}, h.events()); diff != "" {
		t.Errorf("events: %s", diff)
	}
	if err := f.loginPassword(t, name, regenerated); err != nil {
		t.Fatalf("login after regeneration: %v", err)
	}

	// Drift set out of band is repaired; the password stays the recorded one.
	for _, drift := range []struct {
		what, stmt string
		check      func(r userRow, show string) bool
		// locksOut: the drift alone makes the login fail (516).
		locksOut bool
	}{
		{what: "an expiry", stmt: "ALTER USER app_etl VALID UNTIL '2031-01-01 00:00:00'", check: func(_ userRow, show string) bool { return !strings.Contains(show, "VALID UNTIL") }},
		{what: "an expired user", stmt: "ALTER USER app_etl VALID UNTIL '2020-01-01 00:00:00'", locksOut: true,
			check: func(_ userRow, show string) bool { return !strings.Contains(show, "VALID UNTIL") }},
		{what: "a second method", stmt: "ALTER USER app_etl ADD IDENTIFIED WITH plaintext_password BY 'second'", check: func(r userRow, _ string) bool {
			return slices.Equal(r.authTypes, []string{"sha256_password"})
		}},
		{what: "HOST NONE", stmt: "ALTER USER app_etl HOST NONE", locksOut: true, check: func(r userRow, _ string) bool { return slices.Equal(r.hostIP, []string{"::/0"}) }},
		{what: "HOST LIKE", stmt: "ALTER USER app_etl HOST LIKE '%.example.org'", check: func(r userRow, _ string) bool {
			return slices.Equal(r.hostIP, []string{"::/0"}) && len(r.hostLikes) == 0
		}},
		{what: "HOST REGEXP", stmt: `ALTER USER app_etl HOST REGEXP '.*\\.example\\.com'`, check: func(r userRow, _ string) bool {
			return slices.Equal(r.hostIP, []string{"::/0"}) && len(r.hostRegexps) == 0
		}},
		{what: "a default database", stmt: "ALTER USER app_etl DEFAULT DATABASE app_other", check: func(r userRow, _ string) bool { return r.defaultDatabase == "" }},
		{what: "a settings profile", stmt: "ALTER USER app_etl SETTINGS PROFILE 'default'", check: func(_ userRow, show string) bool { return !strings.Contains(show, "SETTINGS") }},
		{what: "default roles", stmt: "ALTER USER app_etl DEFAULT ROLE NONE", check: func(r userRow, _ string) bool { return r.rolesAll == 1 }},
		{what: "a no_password method", stmt: "ALTER USER app_etl IDENTIFIED WITH no_password", check: func(r userRow, _ string) bool {
			return slices.Equal(r.authTypes, []string{"sha256_password"})
		}},
	} {
		f.srv.exec(t, drift.stmt)
		if drift.locksOut {
			if err := f.loginPassword(t, name, regenerated); !authFailed(err) {
				t.Errorf("%s must lock the user out (516): %v", drift.what, err)
			}
		}
		h.mustReconcile("etl", "repair "+drift.what, updated)
		if r := f.userRow(t, name); !drift.check(r, f.showCreateUser(t, name)) {
			t.Errorf("%s not repaired: %+v, %s", drift.what, r, f.showCreateUser(t, name))
		}
		if err := f.loginPassword(t, name, regenerated); err != nil {
			t.Errorf("login after repairing %s: %v", drift.what, err)
		}
		h.mustReconcile("etl", "steady after repairing "+drift.what, steady)
	}
	if got := string(h.secret("etl")[v1alpha1.ConnectionKeyPassword]); got != regenerated {
		t.Errorf("repairs must not change the published password")
	}
	if diff := cmpStrings([]string{}, h.events()); diff != "" {
		t.Errorf("repairs emit no password events: %s", diff)
	}

	// Removing the rotation token removes its key; the password stays.
	h.mutate("etl", func(u *v1alpha1.User) { u.Spec.ForProvider.Authentication.Password.Generate.RotationToken = nil })
	h.mustReconcile("etl", "observe without a rotation token", steady)
	if sec := h.secret("etl"); string(sec[v1alpha1.ConnectionKeyPassword]) != regenerated || sec[v1alpha1.ConnectionKeyRotationToken] != nil {
		t.Errorf("Secret without a rotation token: %v", sec)
	}

	// Replaced out of band by CREATE USER OR REPLACE, which assigns a new
	// UUID (like DROP and CREATE): Replaced; deleting the MR leaves the
	// replacement alone.
	f.srv.exec(t, "CREATE USER OR REPLACE app_etl "+identified)
	newID, _, _ := f.srv.entity(t, access.User, name)
	if newID == id {
		t.Fatal("CREATE USER OR REPLACE kept the UUID")
	}
	if o := h.reconcile("etl"); !reason.Is(o.err, reason.Replaced) || !strings.Contains(o.err.Error(), newID) {
		t.Fatalf("replaced out of band: %s", o)
	}
	before := f.showCreateUser(t, name)
	h.delete("etl")
	h.mustReconcile("etl", "delete after replacement", released)
	if got, _, ok := f.srv.entity(t, access.User, name); !ok || got != newID || f.showCreateUser(t, name) != before {
		t.Fatalf("the replacement was touched: %q %t", got, ok)
	}
	if err := f.loginPassword(t, name, testPassword); err != nil {
		t.Fatalf("the replacement's password changed: %v", err)
	}

	// An owned user is dropped on delete.
	h.createUser("etl2", "app_etl2")
	h.mustReconcile("etl2", "create", created)
	h.delete("etl2")
	h.mustReconcile("etl2", "delete", func(o outcome) bool { return o.err == nil && o.deleted })
	if _, _, ok := f.srv.entity(t, access.User, "app_etl2"); ok {
		t.Fatal("the owned user still exists")
	}
	h.mustReconcile("etl2", "finalize", released)
}

func testUserPasswordSecretRef(t *testing.T, f *accessFixture) {
	ctx := context.Background()
	h := newUserHarness(t, f)
	// Throwaway passwords, generated per run.
	initial, second := "Initial-"+randHex(8), "Second-"+randHex(8)
	pw := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "api-pw"}, Data: map[string][]byte{"password": []byte(initial)}}
	if err := f.kube.Create(ctx, pw); err != nil {
		t.Fatal(err)
	}
	ref := func(u *v1alpha1.User) {
		u.Spec.ForProvider.Authentication.Password = &v1alpha1.PasswordAuthentication{PasswordSecretRef: &xpv2.LocalSecretKeySelector{
			LocalSecretReference: xpv2.LocalSecretReference{Name: "api-pw"}, Key: "password",
		}}
	}
	h.createUser("api", "app_api", ref)
	h.mustReconcile("api", "create", created)
	if got := string(h.secret("api")[v1alpha1.ConnectionKeyPassword]); got != initial {
		t.Fatalf("published password %q", got)
	}
	if err := f.loginPassword(t, "app_api", initial); err != nil {
		t.Fatal(err)
	}
	h.mustReconcile("api", "late-initialize", func(o outcome) bool { return o.err == nil && o.obs.ResourceUpToDate })
	h.mustReconcile("api", "steady", steady)

	// Changing the Secret's value changes the password.
	h.events()
	if err := f.kube.Get(ctx, client.ObjectKeyFromObject(pw), pw); err != nil {
		t.Fatal(err)
	}
	pw.Data["password"] = []byte(second)
	if err := f.kube.Update(ctx, pw); err != nil {
		t.Fatal(err)
	}
	h.mustReconcile("api", "rotate", updated)
	if got := string(h.secret("api")[v1alpha1.ConnectionKeyPassword]); got != second {
		t.Fatalf("published password %q", got)
	}
	if diff := cmpStrings([]string{string(user.ReasonPasswordRotated)}, h.events()); diff != "" {
		t.Errorf("events: %s", diff)
	}
	if err := f.loginPassword(t, "app_api", second); err != nil {
		t.Fatal(err)
	}
	h.mustReconcile("api", "steady after rotation", steady)

	// A missing or empty password is an error and nothing changes.
	pw.Data["password"] = nil
	if err := f.kube.Update(ctx, pw); err != nil {
		t.Fatal(err)
	}
	h.mustReconcile("api", "empty password", failedWith(reason.InvalidSpec))
	if err := f.kube.Delete(ctx, pw); err != nil {
		t.Fatal(err)
	}
	h.mustReconcile("api", "missing password Secret", failedWith(reason.DependencyMissing))
	if err := f.loginPassword(t, "app_api", second); err != nil {
		t.Fatalf("the password must not change: %v", err)
	}
}

// testUserUncontrolledSecret: a Secret of the connection type that exists
// before the User and that nothing controls is refused (SecretConflict)
// before crossplane-runtime's publisher, which publishes the connection
// details before Create, could take it over. Its password never becomes the
// user's: the user is created only once the Secret is gone, with a password
// of its own.
func testUserUncontrolledSecret(t *testing.T, f *accessFixture) {
	ctx := context.Background()
	h := newUserHarness(t, f)
	const planted = "Planted-Passw0rd-0123456789abcdef"
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "planted-conn"},
		Type:       resource.SecretTypeConnection,
		Data:       map[string][]byte{v1alpha1.ConnectionKeyPassword: []byte(planted)},
	}
	if err := f.kube.Create(ctx, sec); err != nil {
		t.Fatal(err)
	}
	h.createUser("planted", "app_planted", func(u *v1alpha1.User) { u.Spec.WriteConnectionSecretToReference.Name = "planted-conn" })
	h.mustReconcile("planted", "uncontrolled Secret", failedWith(reason.SecretConflict))
	if _, _, ok := f.srv.entity(t, access.User, "app_planted"); ok {
		t.Fatal("the user was created")
	}
	got := &corev1.Secret{}
	if err := f.kube.Get(ctx, client.ObjectKeyFromObject(sec), got); err != nil {
		t.Fatal(err)
	}
	if c := metav1.GetControllerOf(got); c != nil || string(got.Data[v1alpha1.ConnectionKeyPassword]) != planted {
		t.Fatalf("the Secret was taken over: controller %v", c)
	}
	if u := h.observed("planted"); readyStatus(u) != "False" {
		t.Errorf("Ready = %s, want False", readyStatus(u))
	}

	if err := f.kube.Delete(ctx, got); err != nil {
		t.Fatal(err)
	}
	h.mustReconcile("planted", "create", created)
	pw := string(h.secret("planted")[v1alpha1.ConnectionKeyPassword])
	if pw == "" || pw == planted {
		t.Fatal("the user must get a password of its own")
	}
	if err := f.loginPassword(t, "app_planted", pw); err != nil {
		t.Errorf("login with the published password: %v", err)
	}
	if err := f.loginPassword(t, "app_planted", planted); !authFailed(err) {
		t.Errorf("login with the planted password: %v, want 516", err)
	}
	h.delete("planted")
	h.mustReconcile("planted", "delete", func(o outcome) bool { return o.err == nil && o.deleted })
}

func testUserHosts(t *testing.T, f *accessFixture) {
	h := newUserHarness(t, f)
	h.createUser("hosts", "app_hosts", func(u *v1alpha1.User) {
		u.Spec.ForProvider.HostIP = []string{"10.1.2.3/8", "2001:db8::1/128", "fd00:0::1", "192.168.7.9/255.255.255.0"}
		u.Spec.ForProvider.HostNames = []string{"Host.Example.COM"}
		u.Spec.ForProvider.DefaultDatabase = new("")
	})
	h.mustReconcile("hosts", "create", created)
	r := f.userRow(t, "app_hosts")
	if !sameSet(r.hostIP, []string{"10.0.0.0/8", "2001:db8::1", "fd00::1", "192.168.7.0/24"}) || !slices.Equal(r.hostNames, []string{"Host.Example.COM"}) {
		t.Fatalf("hosts as stored: %+v", r)
	}
	// The server's normalisation matches the provider's: no drift.
	h.mustReconcile("hosts", "observe", func(o outcome) bool { return o.err == nil && o.obs.ResourceUpToDate && !o.updated })
	h.mustReconcile("hosts", "steady", steady)
	for _, drift := range []string{
		"ALTER USER app_hosts HOST NONE",
		"ALTER USER app_hosts HOST LIKE '%.example.org'",
		`ALTER USER app_hosts HOST REGEXP '.*\\.example\\.com'`,
		"ALTER USER app_hosts HOST ANY",
		"ALTER USER app_hosts HOST IP '10.0.0.0/8', '2001:db8::1', 'fd00::1', '192.168.7.0/24', NAME 'host.example.com'",
		"ALTER USER app_hosts HOST IP '10.0.0.0/8', '2001:db8::1', 'fd00::1', NAME 'Host.Example.COM'",
	} {
		f.srv.exec(t, drift)
		h.mustReconcile("hosts", "repair "+drift, updated)
		if r := f.userRow(t, "app_hosts"); !sameSet(r.hostIP, []string{"10.0.0.0/8", "2001:db8::1", "fd00::1", "192.168.7.0/24"}) ||
			!slices.Equal(r.hostNames, []string{"Host.Example.COM"}) || len(r.hostLikes)+len(r.hostRegexps) > 0 {
			t.Errorf("after %q: %+v", drift, r)
		}
		h.mustReconcile("hosts", "steady after "+drift, steady)
	}

	// The full range with host names would lose the names.
	h.mutate("hosts", func(u *v1alpha1.User) { u.Spec.ForProvider.HostIP = []string{"0.0.0.0/0"} })
	h.mustReconcile("hosts", "full range with names", failedWith(reason.InvalidSpec))
	h.mutate("hosts", func(u *v1alpha1.User) { u.Spec.ForProvider.HostNames = nil })
	h.mustReconcile("hosts", "full range", updated)
	if r := f.userRow(t, "app_hosts"); !slices.Equal(r.hostIP, []string{"::/0"}) || len(r.hostNames) > 0 {
		t.Errorf("HOST ANY: %+v", r)
	}
	h.mustReconcile("hosts", "steady with ANY", steady)

	// The server stores loopback addresses and localhost in any case as HOST
	// LOCAL (host_names = ['localhost']), and prints the IPv4-mapped zero
	// prefix as '::ffff:/96'. Both compare equal, so the user converges.
	h.createUser("local", "app_local", func(u *v1alpha1.User) {
		u.Spec.ForProvider.HostIP = []string{"127.0.0.1", "::1", "127.1.0.0/16", "::ffff:0:0/96"}
		u.Spec.ForProvider.HostNames = []string{"LOCALHOST"}
		u.Spec.ForProvider.DefaultDatabase = new("")
	})
	h.mustReconcile("local", "create", created)
	local := func(what string) {
		t.Helper()
		if r := f.userRow(t, "app_local"); !slices.Equal(r.hostIP, []string{"::ffff:/96"}) || !slices.Equal(r.hostNames, []string{"localhost"}) {
			t.Fatalf("%s: hosts as stored: %+v", what, r)
		}
	}
	local("create")
	h.mustReconcile("local", "observe", func(o outcome) bool { return o.err == nil && o.obs.ResourceUpToDate && !o.updated })
	h.mustReconcile("local", "steady", steady)
	f.srv.exec(t, "ALTER USER app_local HOST NAME 'localhost.example.com'")
	h.mustReconcile("local", "repair", updated)
	local("repair")
	h.mustReconcile("local", "steady after repair", steady)
	// An address the server prints without its last 24 bits could never be
	// compared.
	h.mutate("local", func(u *v1alpha1.User) { u.Spec.ForProvider.HostIP = []string{"::ffff:0.1.2.3"} })
	h.mustReconcile("local", "lossy address", failedWith(reason.InvalidSpec))
	local("refused")

	// The server folds only ASCII letters: a name that Unicode folding alone
	// makes localhost (U+017F, long s) is an ordinary host name, never HOST
	// LOCAL, and HOST LOCAL set out of band is drift.
	fold := "localhoſt"
	h.createUser("fold", "app_fold", func(u *v1alpha1.User) {
		u.Spec.ForProvider.HostNames = []string{fold}
		u.Spec.ForProvider.DefaultDatabase = new("")
	})
	h.mustReconcile("fold", "create", created)
	folded := func(what string) {
		t.Helper()
		if r := f.userRow(t, "app_fold"); len(r.hostIP) > 0 || !slices.Equal(r.hostNames, []string{fold}) {
			t.Fatalf("%s: hosts as stored: %+v", what, r)
		}
	}
	folded("create")
	h.mustReconcile("fold", "steady", steady)
	f.srv.exec(t, "ALTER USER app_fold HOST LOCAL")
	h.mustReconcile("fold", "repair HOST LOCAL", updated)
	folded("repair")
	h.mustReconcile("fold", "steady after repair", steady)
}

func testUserDefaultRoles(t *testing.T, f *accessFixture) {
	h := newUserHarness(t, f)
	for _, r := range []string{"app_r1", "app_r2", "app_r3"} {
		f.adminRole(t, r)
	}
	roles := func(policy string, rs ...string) func(*v1alpha1.User) {
		return func(u *v1alpha1.User) {
			u.Spec.ForProvider.DefaultRoles = &v1alpha1.DefaultRoles{Policy: policy, Roles: rs}
		}
	}

	// Listed: created with NONE, then filled as the roles are granted.
	h.createUser("listed", "app_listed", roles(v1alpha1.DefaultRolesListed, "app_r1", "app_r2"))
	h.mustReconcile("listed", "create", created)
	if r := f.userRow(t, "app_listed"); r.rolesAll != 0 || len(r.rolesList) > 0 {
		t.Fatalf("a Listed user is created with DEFAULT ROLE NONE: %+v", r)
	}
	h.events()
	h.mustReconcile("listed", "nothing granted", func(o outcome) bool { return o.err == nil && o.obs.ResourceUpToDate })
	if ev := h.events(); !slices.Contains(ev, string(user.ReasonDefaultRoleNotGranted)) {
		t.Errorf("events: %v", ev)
	}
	f.srv.exec(t, "GRANT app_r1 TO app_listed")
	h.mustReconcile("listed", "app_r1 granted", updated)
	if r := f.userRow(t, "app_listed"); r.rolesAll != 0 || !slices.Equal(r.rolesList, []string{"app_r1"}) {
		t.Fatalf("after granting app_r1: %+v", r)
	}
	f.srv.exec(t, "GRANT app_r2 TO app_listed")
	h.mustReconcile("listed", "app_r2 granted", updated)
	if r := f.userRow(t, "app_listed"); !sameSet(r.rolesList, []string{"app_r1", "app_r2"}) {
		t.Fatalf("after granting app_r2: %+v", r)
	}
	h.mustReconcile("listed", "steady", steady)
	// 25.3 keeps a revoked role in default_roles_list, 26.x drops it; both
	// converge to the desired roles that are granted.
	f.srv.exec(t, "REVOKE app_r1 FROM app_listed")
	h.reconcile("listed")
	if r := f.userRow(t, "app_listed"); !slices.Equal(r.rolesList, []string{"app_r2"}) {
		t.Fatalf("after revoking app_r1: %+v", r)
	}
	h.mustReconcile("listed", "steady after revoke", steady)

	// AllExcept is set at creation; None too; drift is repaired.
	h.createUser("except", "app_except", roles(v1alpha1.DefaultRolesAllExcept, "app_r3"))
	h.mustReconcile("except", "create", created)
	if r := f.userRow(t, "app_except"); r.rolesAll != 1 || !slices.Equal(r.rolesExcept, []string{"app_r3"}) {
		t.Fatalf("AllExcept: %+v", r)
	}
	h.createUser("none", "app_none", roles(v1alpha1.DefaultRolesNone))
	h.mustReconcile("none", "create", created)
	h.mustReconcile("none", "observe", func(o outcome) bool { return o.err == nil && o.obs.ResourceUpToDate })
	f.srv.exec(t, "ALTER USER app_none DEFAULT ROLE ALL")
	h.mustReconcile("none", "repair", updated)
	if r := f.userRow(t, "app_none"); r.rolesAll != 0 || len(r.rolesList) > 0 {
		t.Fatalf("None after repair: %+v", r)
	}

	// An excepted role that does not exist, and a role outside the policy.
	h.createUser("missing", "app_missing_role", roles(v1alpha1.DefaultRolesAllExcept, "app_nosuch"))
	h.mustReconcile("missing", "unknown role", failedWith(reason.DependencyMissing))
	h.createUser("outside", "app_outside_role", roles(v1alpha1.DefaultRolesListed, "zz_role"))
	h.mustReconcile("outside", "role outside grantableRoles", failedWith(reason.PolicyViolation))
	for _, n := range []string{"app_missing_role", "app_outside_role"} {
		if _, _, ok := f.srv.entity(t, access.User, n); ok {
			t.Errorf("%s was created", n)
		}
	}
}

func testUserOwnership(t *testing.T, f *accessFixture) {
	ctx := context.Background()
	h := newUserHarness(t, f)

	// A pre-existing user with the MR's name is never touched.
	f.srv.exec(t, "CREATE USER app_foreign "+identified+" HOST ANY DEFAULT ROLE NONE SETTINGS PROFILE 'default'")
	foreignID, _, _ := f.srv.entity(t, access.User, "app_foreign")
	before := f.showCreateUser(t, "app_foreign")
	h.createUser("foreign", "app_foreign")
	if o := h.reconcile("foreign"); !reason.Is(o.err, reason.NotOwned) || !strings.Contains(o.err.Error(), foreignID) || o.created || o.updated {
		t.Fatalf("pre-existing user: %s", o)
	}
	if u := h.observed("foreign"); readyStatus(u) != "False" || u.Status.AtProvider.ID != foreignID {
		t.Errorf("Ready %s, status %+v", readyStatus(u), u.Status.AtProvider)
	}
	// Forged create annotations claim nothing: this process never sent that
	// CREATE.
	h.mutate("foreign", func(u *v1alpha1.User) {
		meta.SetExternalCreatePending(u, time.Now())
		meta.AddAnnotations(u, map[string]string{v1alpha1.AnnotationCreateOutcome: v1alpha1.CreateOutcomeUnknown})
	})
	if o := h.reconcile("foreign"); !reason.Is(o.err, reason.NotOwned) {
		t.Fatalf("forged create annotations: %s", o)
	}
	// Adoption is not allowed by this config.
	h.mutate("foreign", func(u *v1alpha1.User) { meta.AddAnnotations(u, map[string]string{v1alpha1.AnnotationAdopt: foreignID}) })
	if o := h.reconcile("foreign"); !reason.Is(o.err, reason.AdoptionRejected) {
		t.Fatalf("adoption without allowAdoption: %s", o)
	}
	h.delete("foreign")
	h.mustReconcile("foreign", "delete", released)
	if id, _, ok := f.srv.entity(t, access.User, "app_foreign"); !ok || id != foreignID || f.showCreateUser(t, "app_foreign") != before {
		t.Fatalf("the pre-existing user changed:\nbefore: %s\nafter:  %s", before, f.showCreateUser(t, "app_foreign"))
	}
	if err := f.loginPassword(t, "app_foreign", testPassword); err != nil {
		t.Fatalf("the pre-existing user's password changed: %v", err)
	}

	// A user and a role must not share a name.
	f.adminRole(t, "app_twin_user")
	h.createUser("twin", "app_twin_user")
	h.mustReconcile("twin", "name collision", failedWith(reason.NameCollision))
	if _, _, ok := f.srv.entity(t, access.User, "app_twin_user"); ok {
		t.Fatal("a user named like a role was created")
	}

	// A name outside the policy.
	h.createUser("zz-name", "zz_user")
	h.mustReconcile("zz-name", "outside the policy", failedWith(reason.PolicyViolation))

	// A create whose response was lost is recovered through the journal of
	// this process.
	h.createUser("lost", "app_lost")
	h.mustReconcile("lost", "create", created)
	lostID, _, _ := f.srv.entity(t, access.User, "app_lost")
	h.mutate("lost", func(u *v1alpha1.User) {
		meta.RemoveAnnotations(u, v1alpha1.AnnotationOwnedID, meta.AnnotationKeyExternalCreateSucceeded)
		meta.SetExternalCreatePending(u, time.Now())
	})
	h.events()
	h.mustReconcile("lost", "recover", func(o outcome) bool { return o.err == nil && o.obs.ResourceExists })
	if got := ownership.ClaimOf(h.get("lost")).OwnedID; got != lostID {
		t.Fatalf("recovered owned-id %q, want %q", got, lostID)
	}
	if ev := h.events(); !slices.Contains(ev, string(access.ReasonCreateRecovered)) {
		t.Errorf("events: %v", ev)
	}

	// Adoption by UUID through a config that allows it takes the user over:
	// with no recorded password a new one is generated and published.
	cpc := &v1alpha1.ClusterProviderConfig{}
	if err := f.kube.Get(ctx, client.ObjectKey{Name: f.pc.Name}, cpc); err != nil {
		t.Fatal(err)
	}
	cpc = cpc.DeepCopy()
	cpc.ObjectMeta = metav1.ObjectMeta{Name: "access-adopt", UID: types.UID("cpc-access-adopt")}
	cpc.Spec.Policy.AllowAdoption = true
	if err := f.kube.Create(ctx, cpc); err != nil {
		t.Fatal(err)
	}
	adopteeID := f.adminUser(t, "app_adoptee")
	h.createUser("adoptee", "app_adoptee", func(u *v1alpha1.User) {
		u.Spec.ProviderConfigReference = &xpv2.ProviderConfigReference{Kind: v1alpha1.ClusterProviderConfigKind, Name: cpc.Name}
		meta.AddAnnotations(u, map[string]string{v1alpha1.AnnotationAdopt: adopteeID})
	})
	h.events()
	h.mustReconcile("adoptee", "adopt", updated)
	if c := ownership.ClaimOf(h.get("adoptee")); c.OwnedID != adopteeID || c.Adopt != "" {
		t.Fatalf("claim after adoption: %+v", c)
	}
	if ev := h.events(); !slices.Contains(ev, string(access.ReasonAdopted)) || !slices.Contains(ev, string(user.ReasonPasswordRegenerated)) {
		t.Errorf("events: %v", ev)
	}
	if err := f.loginPassword(t, "app_adoptee", string(h.secret("adoptee")[v1alpha1.ConnectionKeyPassword])); err != nil {
		t.Fatalf("login after adoption: %v", err)
	}
	h.mustReconcile("adoptee", "steady after adoption", steady)
}

// testUserPermissionDenied: through an unrestricted config, a User outside
// the identity's server-side prefix reaches the server, which refuses its
// CREATE USER (497). The statement carries an IDENTIFIED clause, so every
// literal is redacted from the server's error; the error still names the
// grant the identity lacks, and it holds no hash or salt (design 5.1, 5.2).
func testUserPermissionDenied(t *testing.T, f *accessFixture) {
	ctx := context.Background()
	cpc := &v1alpha1.ClusterProviderConfig{}
	if err := f.kube.Get(ctx, client.ObjectKey{Name: f.pc.Name}, cpc); err != nil {
		t.Fatal(err)
	}
	cpc = cpc.DeepCopy()
	cpc.ObjectMeta = metav1.ObjectMeta{Name: "access-open-user", UID: types.UID("cpc-access-open-user")}
	cpc.Spec.Policy = v1alpha1.ClusterPolicy{Unrestricted: true}
	if err := f.kube.Create(ctx, cpc); err != nil {
		t.Fatal(err)
	}
	h := newUserHarness(t, f)
	h.createUser("outside-prefix", "zz_outside_prefix", func(u *v1alpha1.User) {
		u.Spec.ProviderConfigReference = &xpv2.ProviderConfigReference{Kind: v1alpha1.ClusterProviderConfigKind, Name: cpc.Name}
	})
	o := h.reconcile("outside-prefix")
	if !reason.Is(o.err, reason.PermissionDenied) || o.created {
		t.Fatalf("create outside the prefix: %s", o)
	}
	if msg := o.err.Error(); !strings.Contains(msg, "necessary to have the grant CREATE USER ON") || regexp.MustCompile(`[0-9a-fA-F]{32}`).MatchString(msg) {
		t.Errorf("the error must name the missing grant and hold no hash or salt: %s", msg)
	}
	if _, _, ok := f.srv.entity(t, access.User, "zz_outside_prefix"); ok {
		t.Fatal("the user was created")
	}
	h.delete("outside-prefix")
	h.mustReconcile("outside-prefix", "delete", released)
}

// testUserIdentity: the provider's own identity, defined in users.d, is
// never modified, and an observe-only MR only reports it.
func testUserIdentity(t *testing.T, f *accessFixture) {
	h := newUserHarness(t, f)
	before := f.showCreateUser(t, f.identity)
	h.createUser("self", f.identity)
	h.mustReconcile("self", "own identity", failedWith(reason.SelfModification))
	h.createUser("self-observe", f.identity, policies(xpv2.ManagementActionObserve))
	h.mustReconcile("self-observe", "observe the identity", func(o outcome) bool { return o.err == nil && o.obs.ResourceExists && !o.updated })
	if obs := h.observed("self-observe"); readyStatus(obs) != "True" || obs.Status.AtProvider.Storage != ch.StorageUsersXML || ownership.ClaimOf(obs).OwnerUID != "" {
		t.Errorf("observe-only identity: Ready %s, %+v, %v", readyStatus(obs), obs.Status.AtProvider, obs.GetAnnotations())
	}
	if self := h.observed("self"); readyStatus(self) != "False" {
		t.Errorf("own identity: Ready %s", readyStatus(self))
	}
	h.delete("self")
	h.mustReconcile("self", "delete", released)
	h.delete("self-observe")
	h.mustReconcile("self-observe", "delete observe-only", released)
	if got := f.showCreateUser(t, f.identity); got != before {
		t.Fatalf("the provider identity changed:\nbefore: %s\nafter:  %s", before, got)
	}
	if _, storage, _ := f.srv.entity(t, access.User, f.identity); storage != ch.StorageUsersXML {
		t.Fatalf("identity storage %q", storage)
	}
}

func testUserJWT(t *testing.T, f *accessFixture) {
	h := newUserHarness(t, f)
	const name = "app_jwt_api"
	h.createUser("jwt-api", name, jwtAuth(processorTest, `{"hd": "example.com"}`))
	h.mustReconcile("jwt-api", "create", created)
	want := map[string]string{
		v1alpha1.ConnectionKeyUsername:     name,
		v1alpha1.ConnectionKeyEndpoint:     f.srv.host,
		v1alpha1.ConnectionKeyPort:         fmt.Sprint(f.srv.nativePort),
		v1alpha1.ConnectionKeyProtocol:     "Native",
		v1alpha1.ConnectionKeyTLS:          "false",
		v1alpha1.ConnectionKeyJWTProcessor: processorTest,
	}
	if diff := cmpSecret(want, h.secret("jwt-api")); diff != "" {
		t.Fatalf("connection Secret: %s", diff)
	}
	if show := f.showCreateUser(t, name); !strings.Contains(show, `IDENTIFIED WITH jwt PROCESSOR 'test' CLAIMS '{"hd":"example.com"}'`) {
		t.Fatalf("SHOW CREATE USER: %s", show)
	}
	h.mustReconcile("jwt-api", "observe", func(o outcome) bool { return o.err == nil && o.obs.ResourceUpToDate && !o.updated })
	h.mustReconcile("jwt-api", "steady", steady)
	if u := h.observed("jwt-api"); readyStatus(u) != "True" || u.Status.AtProvider.JWTProcessor != processorTest || !slices.Equal(u.Status.AtProvider.AuthTypes, []string{"jwt"}) {
		t.Errorf("Ready %s, status: %+v", readyStatus(u), u.Status.AtProvider)
	}

	// Log in with HS256 tokens through the provider's own client code.
	good := mintTokenClaims(t, f.jwtKeys[processorTest], jwt.MapClaims{"email": name, "hd": "example.com"})
	if err := f.loginJWT(t, good, name); err != nil {
		t.Fatalf("login with a matching token: %v", err)
	}
	if err := f.loginJWT(t, mintTokenClaims(t, f.jwtKeys[processorTest], jwt.MapClaims{"email": name, "hd": "other.org"}), name); !authFailed(err) {
		t.Fatalf("a token without the claims must fail: %v", err)
	}
	// The processor is pinned: a token of the other processor whose
	// username claim is the user's name is refused.
	if err := f.loginJWT(t, mintTokenClaims(t, f.jwtKeys[processorTest2], jwt.MapClaims{"preferred_username": name, "hd": "example.com"}), name); !authFailed(err) {
		t.Fatalf("a token of another processor must fail: %v", err)
	}

	// Drift of CLAIMS, PROCESSOR, the method and the expiry is repaired;
	// claims that differ only in formatting are not drift.
	f.srv.exec(t, `ALTER USER app_jwt_api IDENTIFIED WITH jwt PROCESSOR 'test' CLAIMS '{ "hd" : "example.com" }'`)
	h.mustReconcile("jwt-api", "claims formatting", steady)
	for _, drift := range []string{
		`ALTER USER app_jwt_api IDENTIFIED WITH jwt PROCESSOR 'test' CLAIMS '{"hd":"other.org"}'`,
		`ALTER USER app_jwt_api IDENTIFIED WITH jwt PROCESSOR 'test'`,
		`ALTER USER app_jwt_api IDENTIFIED WITH jwt PROCESSOR 'test2' CLAIMS '{"hd":"example.com"}'`,
		`ALTER USER app_jwt_api IDENTIFIED WITH jwt CLAIMS '{"hd":"example.com"}'`,
		`ALTER USER app_jwt_api VALID UNTIL '2031-01-01 00:00:00'`,
		`ALTER USER app_jwt_api ADD IDENTIFIED WITH jwt PROCESSOR 'test2'`,
		"ALTER USER app_jwt_api " + identified,
	} {
		f.srv.exec(t, drift)
		h.mustReconcile("jwt-api", "repair "+drift, updated)
		if show := f.showCreateUser(t, name); show != `CREATE USER app_jwt_api IDENTIFIED WITH jwt PROCESSOR 'test' CLAIMS '{"hd":"example.com"}'` {
			t.Errorf("after %q: %s", drift, show)
		}
		h.mustReconcile("jwt-api", "steady after "+drift, steady)
	}
	if err := f.loginJWT(t, good, name); err != nil {
		t.Fatalf("login after the repairs: %v", err)
	}

	// Switching a password user to jwt removes the password from its Secret.
	h.createUser("switch", "app_jwt_switch", rotationToken("t1"))
	h.mustReconcile("switch", "create", created)
	old := string(h.secret("switch")[v1alpha1.ConnectionKeyPassword])
	h.mutate("switch", jwtAuth(processorTest, ""))
	h.mustReconcile("switch", "switch to jwt", updated)
	sec := h.secret("switch")
	if _, ok := sec[v1alpha1.ConnectionKeyPassword]; ok || sec[v1alpha1.ConnectionKeyRotationToken] != nil || string(sec[v1alpha1.ConnectionKeyJWTProcessor]) != processorTest {
		t.Fatalf("Secret after the switch to jwt: %v", sec)
	}
	if err := f.loginPassword(t, "app_jwt_switch", old); !authFailed(err) {
		t.Fatalf("the old password must stop working: %v", err)
	}
	h.mustReconcile("switch", "steady as jwt", steady)
	// And back: a new password is generated, the processor key goes.
	h.mutate("switch", rotationToken("t2"))
	h.mustReconcile("switch", "switch to password", updated)
	sec = h.secret("switch")
	if _, ok := sec[v1alpha1.ConnectionKeyJWTProcessor]; ok || len(sec[v1alpha1.ConnectionKeyPassword]) == 0 {
		t.Fatalf("Secret after the switch to password: %v", sec)
	}
	if err := f.loginPassword(t, "app_jwt_switch", string(sec[v1alpha1.ConnectionKeyPassword])); err != nil {
		t.Fatal(err)
	}

	// The processor must be allowed by the policy and be a valid name.
	h.createUser("jwt-other", "app_jwt_other", jwtAuth(processorTest2, ""))
	h.mustReconcile("jwt-other", "processor outside the policy", failedWith(reason.PolicyViolation))
	h.createUser("jwt-bad", "app_jwt_bad", jwtAuth("bad.name", ""))
	h.mustReconcile("jwt-bad", "invalid processor name", failedWith(reason.InvalidSpec))
	for _, n := range []string{"app_jwt_other", "app_jwt_bad"} {
		if _, _, ok := f.srv.entity(t, access.User, n); ok {
			t.Errorf("%s was created", n)
		}
	}

	// A pre-existing jwt user is never taken over.
	f.srv.exec(t, "CREATE USER app_jwt_foreign IDENTIFIED WITH jwt PROCESSOR 'test2' HOST IP '10.0.0.0/8'")
	before := f.showCreateUser(t, "app_jwt_foreign")
	h.createUser("jwt-foreign", "app_jwt_foreign", jwtAuth(processorTest, `{"hd":"example.com"}`))
	h.mustReconcile("jwt-foreign", "pre-existing jwt user", failedWith(reason.NotOwned))
	h.delete("jwt-foreign")
	h.mustReconcile("jwt-foreign", "delete", released)
	if got := f.showCreateUser(t, "app_jwt_foreign"); got != before {
		t.Fatalf("the pre-existing jwt user changed:\nbefore: %s\nafter:  %s", before, got)
	}
}

// testUserJWTUnsupported: upstream servers do not support jwt users; the
// MR is Unsupported and no SQL is sent.
func testUserJWTUnsupported(t *testing.T, f *accessFixture) {
	h := newUserHarness(t, f)
	h.createUser("jwt", "app_jwt_upstream", jwtAuth(processorTest, ""))
	h.mustReconcile("jwt", "jwt user upstream", failedWith(reason.Unsupported))
	u := h.direct("jwt", func(ctx context.Context, ext managed.TypedExternalClient[*v1alpha1.User], u *v1alpha1.User) {
		if _, err := ext.Create(ctx, u); !reason.Is(err, reason.Unsupported) || !strings.Contains(err.Error(), "requires Altinity Antalya") {
			t.Errorf("Create() = %v", err)
		}
	})
	if readyStatus(u) != "False" {
		t.Errorf("Ready = %s", readyStatus(u))
	}
	if _, _, ok := f.srv.entity(t, access.User, "app_jwt_upstream"); ok {
		t.Fatal("a jwt user was created upstream")
	}
}

// --- comparisons ---

func cmpSecret(want map[string]string, got map[string][]byte) string {
	g := map[string]string{}
	for k, v := range got {
		g[k] = string(v)
	}
	var diffs []string
	for k, v := range want {
		if g[k] != v {
			diffs = append(diffs, fmt.Sprintf("%s: got %q, want %q", k, g[k], v))
		}
	}
	for k := range g {
		if _, ok := want[k]; !ok {
			diffs = append(diffs, fmt.Sprintf("unexpected key %s", k))
		}
	}
	sort.Strings(diffs)
	return strings.Join(diffs, "; ")
}

func readyStatus(u *v1alpha1.User) string { return string(u.GetCondition(xpv2.TypeReady).Status) }

func cmpStrings(want, got []string) string {
	if slices.Equal(want, got) {
		return ""
	}
	return fmt.Sprintf("got %q, want %q", got, want)
}

func sameSet(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}
