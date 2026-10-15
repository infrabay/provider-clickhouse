// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	"github.com/moby/moby/api/types/container"
	"github.com/testcontainers/testcontainers-go"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/infrabay/provider-clickhouse/apis/v1alpha1"
	ch "github.com/infrabay/provider-clickhouse/internal/clients/clickhouse"
	"github.com/infrabay/provider-clickhouse/internal/clients/clickhouse/auth"
	"github.com/infrabay/provider-clickhouse/internal/controller/access"
	"github.com/infrabay/provider-clickhouse/internal/controller/ownership"
	"github.com/infrabay/provider-clickhouse/internal/reason"
	"github.com/infrabay/provider-clickhouse/test/fakegcp"
)

// Google service accounts of these tests. All names are examples.
var (
	googleProvider = fakegcp.Identity{Email: "xpch-provider@example-project.iam.gserviceaccount.com", UniqueID: "100000000000000000101"}
	googleWorkload = fakegcp.Identity{Email: "app-api@example-project.iam.gserviceaccount.com", UniqueID: "100000000000000000202"}
	// googleRecreated has the workload's email and a new unique ID, as after
	// the service account was deleted and created again.
	googleRecreated = fakegcp.Identity{Email: googleWorkload.Email, UniqueID: "100000000000000000203"}
	googleAttacker  = fakegcp.Identity{Email: "attacker@other-project.iam.gserviceaccount.com", UniqueID: "100000000000000000666"}
)

const (
	// Cache lifetimes of the processor under test. Production uses
	// fakegcp.ProductionGoogleProcessor (1 h and 5 min); short values make
	// key rotation and token expiry observable in seconds. The processor is
	// otherwise rendered exactly as in production.
	googleJWKSCacheLifetime  = 10 * time.Second
	googleTokenCacheLifetime = 10 * time.Second

	// envGoogleUpstreamImages are upstream images that must fail cleanly
	// with GoogleIDToken credentials (no JWT login).
	envGoogleUpstreamImages    = "XPCH_GOOGLE_UPSTREAM_IMAGES"
	defaultGoogleUpstreamImage = "clickhouse/clickhouse-server:25.3.3.42-alpine"

	// envFakeGCPBind is the address the fake's JWKS listener binds to, so
	// that containers reach it through host.docker.internal. The default is
	// 127.0.0.1 on macOS and Windows (Docker Desktop and OrbStack forward
	// host.docker.internal to the host's loopback) and 0.0.0.0 on Linux
	// (host-gateway is the bridge address). The metadata listener, which
	// serves tokens, is always bound to 127.0.0.1.
	envFakeGCPBind = "XPCH_FAKEGCP_BIND"
	// envFakeGCPLog=1 logs the fake's requests and issued tokens (key IDs
	// and claims only) to stderr.
	envFakeGCPLog = "XPCH_FAKEGCP_LOG"
)

// googleFake is a fake metadata server for the provider (this process, via
// GCE_METADATA_HOST) and a JWKS endpoint for ClickHouse containers.
type googleFake struct {
	*fakegcp.Server
	// jwksURI is the JWKS URI as containers see it.
	jwksURI string
}

func startGoogleFake(t *testing.T) *googleFake {
	t.Helper()
	opts := fakegcp.Options{Identity: googleProvider}
	if os.Getenv(envFakeGCPLog) == "1" {
		opts.Logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	fake, err := fakegcp.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fake.Close)
	bind := os.Getenv(envFakeGCPBind)
	if bind == "" {
		bind = "127.0.0.1"
		if runtime.GOOS == "linux" {
			bind = "0.0.0.0"
		}
	}
	meta := serveHTTP(t, "127.0.0.1:0", fake.MetadataHandler())
	jwks := serveHTTP(t, net.JoinHostPort(bind, "0"), fake.JWKSHandler())
	fakegcp.UseFake(t, meta.String())
	_, port, _ := net.SplitHostPort(jwks.String())
	return &googleFake{Server: fake, jwksURI: "http://host.docker.internal:" + port + fakegcp.JWKSPath}
}

func serveHTTP(t *testing.T, addr string, h http.Handler) net.Addr {
	t.Helper()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr()
}

// hostAccess lets a container reach the test process as host.docker.internal
// (built in on Docker Desktop and OrbStack; host-gateway on Linux).
var hostAccess = testcontainers.WithHostConfigModifier(func(hc *container.HostConfig) {
	hc.ExtraHosts = append(hc.ExtraHosts, "host.docker.internal:host-gateway")
})

// googleGrants are the provider identity's grants: user DDL on service
// account emails that start with app- and the Database grants (design 9.4).
func googleGrants() []string {
	return append([]string{
		"GRANT CREATE USER, ALTER USER, DROP USER ON `app-`*",
		"GRANT SHOW USERS, SHOW ROLES ON *.*",
		"GRANT SELECT ON system.user_directories",
	}, databaseGrants(true)...)
}

// googleAntalyaServer starts Antalya with the production-style google_jwt
// processor (jwt_dynamic_jwks, jwks_uri pointing at the fake) and the
// provider's service account as a users.d jwt user pinned to it. Token
// authentication through user_directories is not enabled: only jwt users
// can log in with a Google token.
func googleAntalyaServer(t *testing.T, image string, fake *googleFake, audience string) *server {
	t.Helper()
	p := fakegcp.GoogleProcessor{
		Name:               fakegcp.GoogleProcessorName,
		JWKSURI:            fake.jwksURI,
		Audience:           audience,
		JWKSCacheLifetime:  googleJWKSCacheLifetime,
		TokenCacheLifetime: googleTokenCacheLifetime,
	}
	usersD := fmt.Sprintf(`users:
  %q:
    jwt: {processor: %s}
    networks: {ip: ["::/0"]}
    profile: default
    quota: default
    grants:
      query:
%s`, googleProvider.Email, fakegcp.GoogleProcessorName, grantsYAML(googleGrants()))
	return startServer(t, image, map[string]string{
		"/etc/clickhouse-server/config.d/token_processors.yaml":   p.YAML(),
		"/etc/clickhouse-server/users.d/provider-clickhouse.yaml": usersD,
	}, hostAccess)
}

// googleCPC is a ClusterProviderConfig with GoogleIDToken credentials and no
// key: Application Default Credentials, the GKE Workload Identity path.
func googleCPC(s *server, name, audience string) *v1alpha1.ClusterProviderConfig {
	port := int32(s.nativePort) //nolint:gosec // A mapped port fits.
	return &v1alpha1.ClusterProviderConfig{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID("cpc-" + name)},
		Spec: v1alpha1.ClusterProviderConfigSpec{
			ConnectionSpec: v1alpha1.ConnectionSpec{
				Endpoint:               v1alpha1.Endpoint{Host: s.host, Port: &port},
				AllowInsecureTransport: true,
			},
			Credentials: v1alpha1.ClusterCredentials{
				Source:        v1alpha1.CredentialsSourceGoogleIDToken,
				GoogleIDToken: &v1alpha1.ClusterGoogleIDTokenCredentials{Audience: audience},
			},
			Policy: v1alpha1.ClusterPolicy{
				AllowedNamespaces:     []string{namespace},
				DatabaseNamePatterns:  []string{"app_.+"},
				UserNamePatterns:      []string{`app-[a-z0-9-]+@example-project\.iam\.gserviceaccount\.com`},
				AllowedAuthentication: []v1alpha1.AuthenticationMethod{v1alpha1.AuthenticationMethodJWT},
				AllowedJWTProcessors:  []string{fakegcp.GoogleProcessorName},
			},
		},
	}
}

// googleLogin logs in with token through the provider's own client code and
// expects to be user want.
func googleLogin(s *server, token, want string) error {
	conn, err := ch.OpenDriver(ch.Config{Host: s.host, Port: s.nativePort, Protocol: ch.ProtocolNative, TokenSource: auth.Static{Value: token}, MaxOpenConns: 1})
	if err != nil {
		return err
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

// mint signs a token for id with the right audience.
func (f *googleFake) mint(t *testing.T, id fakegcp.Identity, audience string, opts ...func(*fakegcp.MintOptions)) string {
	t.Helper()
	o := fakegcp.MintOptions{Audience: audience, Identity: &id}
	for _, fn := range opts {
		fn(&o)
	}
	tok, err := f.Mint(o)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// cleanError fails unless err is a single line without anything that looks
// like a JWT (base64url of a JSON object starts with "eyJ").
func cleanError(t *testing.T, what string, err error) {
	t.Helper()
	msg := err.Error()
	if strings.Contains(msg, "eyJ") {
		t.Errorf("%s: the error contains a token: %s", what, msg)
	}
	if strings.ContainsAny(msg, "\r\n") {
		t.Errorf("%s: the error spans several lines: %q", what, msg)
	}
	if len(msg) > 1024 {
		t.Errorf("%s: the error is %d bytes long", what, len(msg))
	}
}

// identityFetches returns how many ID tokens the provider requested.
func (f *googleFake) identityFetches() int { return f.Count(fakegcp.IdentityPath) }

// jwksFetches returns how many times ClickHouse fetched the JWKS, and how
// many of those failed.
func (f *googleFake) jwksFetches() (all, failed int) {
	for _, r := range f.Requests() {
		if r.Path == fakegcp.JWKSPath {
			all++
			if r.Status != http.StatusOK {
				failed++
			}
		}
	}
	return all, failed
}

// awaitJWKSFetch makes logins that ClickHouse must check against its JWKS
// (tokens for another audience, which are never cached) until it fetches the
// JWKS again, that is until its cached set expired. Right afterwards the
// cached set is fresh for about googleJWKSCacheLifetime.
func (f *googleFake) awaitJWKSFetch(t *testing.T, s *server) {
	t.Helper()
	before, _ := f.jwksFetches()
	deadline := time.Now().Add(googleJWKSCacheLifetime + 15*time.Second)
	for time.Now().Before(deadline) {
		probe := f.mint(t, googleProvider, "https://not-this-clickhouse.example.com")
		_ = googleLogin(s, probe, googleProvider.Email)
		if n, _ := f.jwksFetches(); n > before {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatal("ClickHouse did not fetch the JWKS again")
}

func TestGoogleIDTokenAntalya(t *testing.T) {
	for _, image := range images(t, envAntalyaImages, defaultAntalyaImages) {
		t.Run(image, func(t *testing.T) { testGoogleIDTokenAntalya(t, image) })
	}
}

func testGoogleIDTokenAntalya(t *testing.T, image string) {
	fake := startGoogleFake(t)
	audience := "https://clickhouse-" + randHex(4) + ".example.com"
	srv := googleAntalyaServer(t, image, fake, audience)
	short := googleCPC(srv, "google-short", audience)
	short.Spec.Pool = &v1alpha1.Pool{ConnMaxLifetime: &metav1.Duration{Duration: 2 * time.Second}}
	kube := newKube(t, googleCPC(srv, "google", audience), short)
	providerCPC := googleCPC(srv, "google", audience)

	// connect acquires a client through a new harness: a new cache, a new
	// GoogleSource and so a fresh token.
	connect := func(t *testing.T) (ch.Client, error) {
		t.Helper()
		return newHarness(t, kube, nil).connectFor(providerCPC)
	}
	mustConnect := func(t *testing.T, what string) {
		t.Helper()
		cl, err := connect(t)
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		defer cl.Close() //nolint:errcheck // Test cleanup.
		if got := cl.Info().CurrentUser; got != googleProvider.Email {
			t.Fatalf("%s: currentUser() = %q", what, got)
		}
	}
	// mustFail516 expects a connection to fail with 516 after the provider
	// fetched a token, was refused, and fetched a fresh one (design 6.4).
	mustFail516 := func(t *testing.T, what string) error {
		t.Helper()
		before := fake.identityFetches()
		cl, err := connect(t)
		if err == nil {
			_ = cl.Close()
			t.Fatalf("%s: the provider logged in", what)
		}
		if !reason.Is(err, reason.AuthenticationFailed) || !ch.IsAuthenticationFailed(err) {
			t.Fatalf("%s: %v, want AuthenticationFailed from a 516", what, err)
		}
		cleanError(t, what, err)
		if n := fake.identityFetches() - before; n != 2 {
			t.Errorf("%s: %d ID token requests, want 2 (one token, then a fresh one after the 516)", what, n)
		}
		return err
	}

	t.Run("ProviderLogsInAsServiceAccount", func(t *testing.T) {
		before := len(fake.Requests())
		cl, err := connect(t)
		if err != nil {
			t.Fatal(err)
		}
		defer cl.Close() //nolint:errcheck // Test cleanup.
		if got := cl.Info().CurrentUser; got != googleProvider.Email {
			t.Fatalf("currentUser() = %q, want %q", got, googleProvider.Email)
		}
		if ok, err := cl.Capabilities().JWTUsers(context.Background(), cl); err != nil || !ok {
			t.Fatalf("JWT users capability = %t, %v", ok, err)
		}
		// The provider asked the metadata server for exactly one
		// format=full token with the configured audience, and for nothing
		// else than the project ID; ClickHouse fetched the JWKS.
		ids := 0
		for _, r := range fake.Requests()[before:] {
			switch r.Path {
			case fakegcp.IdentityPath:
				ids++
				q, _ := url.ParseQuery(r.RawQuery)
				if q.Get("audience") != audience || q.Get("format") != "full" {
					t.Errorf("identity query %q", r.RawQuery)
				}
			case fakegcp.MetadataPrefix + "project/project-id", fakegcp.JWKSPath:
			default:
				t.Errorf("unexpected request %s %s", r.Method, r.Path)
			}
		}
		if ids != 1 {
			t.Errorf("%d ID token requests, want 1", ids)
		}
		if n, _ := fake.jwksFetches(); n == 0 {
			t.Error("ClickHouse never fetched the JWKS")
		}
		is := fake.Issued()
		if last := is[len(is)-1]; last.Kid != fake.ActiveKID() || last.Email != googleProvider.Email || last.Audience != audience {
			t.Errorf("issued %+v", last)
		}
	})

	t.Run("DatabaseLifecycle", func(t *testing.T) {
		h := newHarness(t, kube, nil)
		h.createMR("google-db", v1alpha1.DatabaseParameters{Name: new("app_google"), Comment: new("v1")}, "ClusterProviderConfig", "google")
		if o := h.reconcile("google-db"); o.err != nil || !o.created {
			t.Fatalf("create: %s", o)
		}
		id, _, ok := srv.database(t, "app_google")
		if !ok || id != h.get("google-db").GetAnnotations()[v1alpha1.AnnotationOwnedID] {
			t.Fatalf("server uuid %q (exists %t)", id, ok)
		}
		if o := h.reconcile("google-db"); o.err != nil || !o.obs.ResourceExists || !o.obs.ResourceUpToDate {
			t.Fatalf("observe: %s", o)
		}
		h.delete("google-db")
		if o := h.reconcile("google-db"); o.err != nil || !o.deleted {
			t.Fatalf("delete: %s", o)
		}
		if o := h.reconcile("google-db"); o.err != nil || !o.gone {
			t.Fatalf("finalize: %s", o)
		}
		if _, _, ok := srv.database(t, "app_google"); ok {
			t.Fatal("database still exists")
		}
	})

	t.Run("JWTUserForAServiceAccount", func(t *testing.T) {
		f := &accessFixture{t: t, family: familyAntalya, image: image, srv: srv, kube: kube,
			cache: ch.NewCache(ch.OpenDriver), journal: ownership.NewJournal(),
			pc:       &xpv2.ProviderConfigReference{Kind: v1alpha1.ClusterProviderConfigKind, Name: "google"},
			identity: googleProvider.Email}
		t.Cleanup(f.cache.Close)
		h := newUserHarness(t, f)
		// Pin the service account's unique ID: an email can be reused by a
		// new service account after the old one was deleted.
		claims := `{"sub":"` + googleWorkload.UniqueID + `"}`
		h.createUser("app-api", googleWorkload.Email, jwtAuth(fakegcp.GoogleProcessorName, claims))
		h.mustReconcile("app-api", "create", created)
		want := "CREATE USER `" + googleWorkload.Email + "` IDENTIFIED WITH jwt PROCESSOR 'google_jwt' CLAIMS '" + claims + "'"
		if show := f.showCreateUser(t, googleWorkload.Email); show != want {
			t.Fatalf("SHOW CREATE USER:\n%s\nwant:\n%s", show, want)
		}
		h.mustReconcile("app-api", "steady", steady)

		// The workload logs in with its own Google ID token.
		if err := googleLogin(srv, fake.mint(t, googleWorkload, audience), googleWorkload.Email); err != nil {
			t.Fatalf("workload login: %v", err)
		}
		// Refused: a recreated service account with the same email, an
		// attacker's service account (valid Google signature, right
		// audience, not a ClickHouse user), the workload's token for
		// another audience, and its token signed by an unknown key.
		for what, tok := range map[string]string{
			"recreated service account": fake.mint(t, googleRecreated, audience),
			"attacker service account":  fake.mint(t, googleAttacker, audience),
			"another audience":          fake.mint(t, googleWorkload, "https://other-clickhouse.example.com"),
			"unknown key":               fake.mint(t, googleWorkload, audience, func(o *fakegcp.MintOptions) { o.Fault = fakegcp.FaultUnknownKid }),
		} {
			if err := googleLogin(srv, tok, googleWorkload.Email); !authFailed(err) {
				t.Errorf("%s: %v, want 516", what, err)
			}
		}

		h.delete("app-api")
		h.mustReconcile("app-api", "delete", func(o outcome) bool { return o.err == nil && o.deleted })
		h.mustReconcile("app-api", "finalize", released)
		if _, _, ok := srv.entity(t, access.User, googleWorkload.Email); ok {
			t.Fatal("the user still exists")
		}
		if err := googleLogin(srv, fake.mint(t, googleWorkload, audience), googleWorkload.Email); !authFailed(err) {
			t.Fatalf("login of a deleted user: %v, want 516", err)
		}
	})

	t.Run("FaultyTokensAreRejected", func(t *testing.T) {
		for _, f := range fakegcp.Faults() {
			t.Run(string(f), func(t *testing.T) {
				if err := fake.SetFault(f, 0); err != nil {
					t.Fatal(err)
				}
				defer fake.Reset()
				err := mustFail516(t, string(f))
				t.Logf("%s: %v", f, err)
			})
		}
		// Every rejection came from ClickHouse; a good token still works.
		mustConnect(t, "after the faulty tokens")
	})

	t.Run("AttackerServiceAccount", func(t *testing.T) {
		// The provider's metadata server serves another service account:
		// a valid Google token with the right audience, for an email that is
		// not a ClickHouse user. user_directories/token is not enabled, so
		// nothing creates a user for it.
		if err := fake.SetIdentity(googleAttacker); err != nil {
			t.Fatal(err)
		}
		defer fake.Reset()
		mustFail516(t, "attacker")
		if _, _, ok := srv.entity(t, access.User, googleAttacker.Email); ok {
			t.Fatal("a user was created for the attacker")
		}
	})

	t.Run("ClockSkew", func(t *testing.T) {
		defer fake.Reset()
		fake.SetClockOffset(30 * time.Second)
		mustConnect(t, "metadata clock 30 s ahead (within the 60 s leeway)")
		fake.SetClockOffset(-30 * time.Second)
		mustConnect(t, "metadata clock 30 s behind")
		fake.SetClockOffset(2 * time.Minute)
		mustFail516(t, "metadata clock 2 min ahead: iat in the future")
		fake.SetClockOffset(-62 * time.Minute)
		mustFail516(t, "metadata clock 62 min behind: expired")
	})

	t.Run("KeyRotation", func(t *testing.T) {
		defer fake.Reset()
		initial := fake.ActiveKID()

		// Google publishes a key before it signs with it. Once ClickHouse
		// has fetched the set that contains it, the switch is seamless.
		next, err := fake.AddKey()
		if err != nil {
			t.Fatal(err)
		}
		fake.awaitJWKSFetch(t, srv)
		if err := fake.Activate(next); err != nil {
			t.Fatal(err)
		}
		before := fake.identityFetches()
		mustConnect(t, "key published ahead of use")
		if n := fake.identityFetches() - before; n != 1 {
			t.Errorf("%d ID token requests, want 1 (no 516 retry)", n)
		}

		// A key used at once is unknown until the cached set expires:
		// ClickHouse does not refetch the JWKS for an unknown kid.
		fake.awaitJWKSFetch(t, srv)
		rotated, err := fake.Rotate()
		if err != nil {
			t.Fatal(err)
		}
		mustFail516(t, "key used before ClickHouse fetched it")
		deadline := time.Now().Add(googleJWKSCacheLifetime + 15*time.Second)
		for {
			cl, err := connect(t)
			if err == nil {
				_ = cl.Close()
				break
			}
			if !authFailed(err) || time.Now().After(deadline) {
				t.Fatalf("rotated key never accepted: %v", err)
			}
			time.Sleep(time.Second)
		}
		t.Logf("key %s accepted after the JWKS cache expired", rotated)

		// Retired keys stop working once the cached set expires.
		for _, kid := range []string{initial, next} {
			if err := fake.Retire(kid); err != nil {
				t.Fatal(err)
			}
		}
		fake.awaitJWKSFetch(t, srv)
		old := fake.mint(t, googleProvider, audience, func(o *fakegcp.MintOptions) { o.KID = initial })
		if err := googleLogin(srv, old, googleProvider.Email); !authFailed(err) {
			t.Fatalf("token of a retired key: %v, want 516", err)
		}
		if err := googleLogin(srv, fake.mint(t, googleProvider, audience), googleProvider.Email); err != nil {
			t.Fatalf("token of the active key: %v", err)
		}
	})

	t.Run("ShortLivedTokenIsReplacedVia516", func(t *testing.T) {
		// A token issued 60 s ago with a 5 s lifetime: ClickHouse accepts it
		// (60 s leeway) for about 5 more seconds. The Google library caches
		// metadata ID tokens for 55 minutes whatever their exp, so the
		// provider keeps sending it on new connections until ClickHouse
		// refuses it; the 516 retry then fetches a fresh token.
		if err := fake.SetTTL(5 * time.Second); err != nil {
			t.Fatal(err)
		}
		fake.SetClockOffset(-60 * time.Second)
		defer fake.Reset()

		h := newHarness(t, kube, nil)
		h.createMR("google-short", v1alpha1.DatabaseParameters{Name: new("app_google_short")}, "ClusterProviderConfig", "google-short")
		before := fake.identityFetches()
		if o := h.reconcile("google-short"); o.err != nil || !o.created {
			t.Fatalf("create: %s", o)
		}
		if n := fake.identityFetches() - before; n != 1 {
			t.Fatalf("%d ID token requests for the create, want 1", n)
		}
		// Past the token's validity, ClickHouse's token cache and the
		// pool's connMaxLifetime (2 s): the next statement dials again.
		time.Sleep(googleTokenCacheLifetime + 3*time.Second)
		if n := fake.identityFetches() - before; n != 1 {
			t.Fatalf("the provider refreshed proactively (%d requests); the test assumes it does not", n)
		}
		if o := h.reconcile("google-short"); o.err != nil || !o.obs.ResourceExists {
			t.Fatalf("observe with an expired cached token: %s", o)
		}
		if n := fake.identityFetches() - before; n != 2 {
			t.Fatalf("%d ID token requests, want 2: the 516 retry fetches exactly one fresh token", n)
		}
		fake.Reset()
		h.delete("google-short")
		if o := h.reconcile("google-short"); o.err != nil || !o.deleted {
			t.Fatalf("delete: %s", o)
		}
	})

	t.Run("ServerTokenCacheIgnoresExp", func(t *testing.T) {
		// ClickHouse caches an accepted token for token_cache_lifetime (at
		// most 1 h), not until its exp: a token that expires after it was
		// accepted keeps working until the cache entry expires. This is
		// why the production processor sets token_cache_lifetime to 5 min.
		defer fake.Reset()
		fake.SetClockOffset(-60 * time.Second)
		if err := fake.SetTTL(3 * time.Second); err != nil {
			t.Fatal(err)
		}
		tok := fake.mint(t, googleProvider, audience) // accepted for about 3 s more
		if err := googleLogin(srv, tok, googleProvider.Email); err != nil {
			t.Fatalf("login with a token that is still valid: %v", err)
		}
		time.Sleep(6 * time.Second) // expired, but within the 10 s token cache
		if err := googleLogin(srv, tok, googleProvider.Email); err != nil {
			t.Fatalf("the cached, now expired token: %v (ClickHouse may now bound its token cache by exp; update the docs)", err)
		}
		// The same exp as the cached token (expired about 3 s ago, beyond the
		// leeway) but another iat, so another token: refused.
		fake.SetClockOffset(-67 * time.Second)
		if err := fake.SetTTL(4 * time.Second); err != nil {
			t.Fatal(err)
		}
		if err := googleLogin(srv, fake.mint(t, googleProvider, audience), googleProvider.Email); !authFailed(err) {
			t.Fatalf("an expired token that was never accepted: %v, want 516", err)
		}
	})

	t.Run("MetadataOutage", func(t *testing.T) {
		defer fake.Reset()
		for i, tc := range []struct {
			mode fakegcp.OutageMode
			want []string // one of
		}{
			{fakegcp.Outage500, []string{"compute: Received 500"}},
			{fakegcp.OutageForbidden, []string{"compute: Received 403"}},
			{fakegcp.OutageTimeout, []string{"Client.Timeout", "deadline exceeded"}},
		} {
			// A new harness: the cached pool of a previous one would not
			// need a token.
			h := newHarness(t, kube, nil)
			mr, db := fmt.Sprintf("google-outage-%d", i), fmt.Sprintf("app_google_outage_%d", i)
			h.createMR(mr, v1alpha1.DatabaseParameters{Name: new(db)}, "ClusterProviderConfig", "google")
			if err := fake.SetOutage(fakegcp.TargetIdentity, tc.mode, 0); err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			o := h.reconcile(mr)
			took := time.Since(start)
			if !reason.Is(o.err, reason.AuthenticationFailed) || ch.IsAuthenticationFailed(o.err) || o.created {
				t.Fatalf("%s: %s, want AuthenticationFailed from the token source (no 516)", tc.mode, o)
			}
			var te *ch.TransportError
			if !errors.As(o.err, &te) || te.Category != "token source failed" ||
				!slices.ContainsFunc(tc.want, func(s string) bool { return strings.Contains(o.err.Error(), s) }) {
				t.Errorf("%s: %v", tc.mode, o.err)
			}
			cleanError(t, string(tc.mode), o.err)
			if strings.Contains(o.err.Error(), "fakegcp") {
				t.Errorf("%s: the error carries more than the first line of the response: %v", tc.mode, o.err)
			}
			t.Logf("%s after %s: %v", tc.mode, took.Round(100*time.Millisecond), o.err)
			if err := fake.SetOutage(fakegcp.TargetIdentity, fakegcp.OutageNone, 0); err != nil {
				t.Fatal(err)
			}
			if o := h.reconcile(mr); o.err != nil || !o.created {
				t.Fatalf("%s: after recovery: %s", tc.mode, o)
			}
		}
	})

	t.Run("SlowMetadataServer", func(t *testing.T) {
		// Two requests hang until the metadata client's 5 s timeout; it
		// retries and gets the token on the third. The token is fetched
		// after the TCP connection to ClickHouse is open (GetJWT), so the
		// dial takes about 10 s although the dial timeout is 5 s; ClickHouse
		// keeps the connection open meanwhile (also with three hangs, about
		// 15 s, when this was written).
		defer fake.Reset()
		if err := fake.SetOutage(fakegcp.TargetIdentity, fakegcp.OutageTimeout, 2); err != nil {
			t.Fatal(err)
		}
		before := fake.identityFetches()
		start := time.Now()
		cl, err := connect(t)
		took := time.Since(start)
		if err != nil {
			t.Fatalf("connect with a slow metadata server: %v", err)
		}
		_ = cl.Close()
		if n := fake.identityFetches() - before; n != 3 || took < 9*time.Second {
			t.Errorf("connect took %s with %d ID token requests, want about 10 s and 3", took, n)
		}
		t.Logf("slow metadata server: connected after %s", took.Round(100*time.Millisecond))
	})

	t.Run("JWKSOutage", func(t *testing.T) {
		// googleapis.com unreachable from ClickHouse once its cached set
		// expired: the login that triggers the refetch fails, and the stale
		// set is used until the next attempt after jwks_cache_lifetime.
		defer fake.Reset()
		if err := fake.SetOutage(fakegcp.TargetJWKS, fakegcp.Outage500, 0); err != nil {
			t.Fatal(err)
		}
		// Valid tokens that ClickHouse has not seen (each with another
		// lifetime, so not in its token cache) need the JWKS.
		fresh := func(i int) string {
			if err := fake.SetTTL(time.Hour + time.Duration(i)*time.Second); err != nil {
				t.Fatal(err)
			}
			return fake.mint(t, googleProvider, audience)
		}
		_, failedBefore := fake.jwksFetches()
		deadline := time.Now().Add(googleJWKSCacheLifetime + 15*time.Second)
		var triggering error
		for i := 0; ; i++ {
			err := googleLogin(srv, fresh(i), googleProvider.Email)
			if _, failed := fake.jwksFetches(); failed > failedBefore {
				triggering = err
				break
			}
			if err != nil {
				t.Fatalf("login before the cached JWKS expired: %v", err)
			}
			if time.Now().After(deadline) {
				t.Fatal("ClickHouse did not try to fetch the JWKS")
			}
			time.Sleep(500 * time.Millisecond)
		}
		if !authFailed(triggering) {
			t.Errorf("the login that triggered the failed JWKS fetch: %v, want 516", triggering)
		}
		if err := googleLogin(srv, fresh(1000), googleProvider.Email); err != nil {
			t.Fatalf("login with the stale JWKS during the outage: %v", err)
		}
	})

	t.Run("NewAuthLibrary", func(t *testing.T) {
		// cloud.google.com/go/auth instead of golang.org/x/oauth2 (opt-in
		// today, the default later).
		t.Setenv("GOOGLE_API_GO_EXPERIMENTAL_ENABLE_NEW_AUTH_LIB", "true")
		before := len(fake.Requests())
		mustConnect(t, "new auth library")
		var full bool
		for _, r := range fake.Requests()[before:] {
			if r.Path == fakegcp.IdentityPath {
				q, _ := url.ParseQuery(r.RawQuery)
				full = q.Get("format") == "full" && q.Get("audience") == audience
			}
		}
		if !full {
			t.Error("the new auth library did not request a format=full token")
		}
	})
}

// TestGoogleIDTokenUpstream: upstream ClickHouse has no JWT login. With
// GoogleIDToken credentials the provider fails cleanly with
// AuthenticationFailed after one retry, and never sees a user.
func TestGoogleIDTokenUpstream(t *testing.T) {
	for _, image := range images(t, envGoogleUpstreamImages, defaultGoogleUpstreamImage) {
		t.Run(image, func(t *testing.T) {
			fake := startGoogleFake(t)
			audience := "https://clickhouse-" + randHex(4) + ".example.com"
			srv := startServer(t, image, nil)
			kube := newKube(t, googleCPC(srv, "google", audience))
			h := newHarness(t, kube, nil)
			h.createMR("upstream", v1alpha1.DatabaseParameters{Name: new("app_upstream")}, "ClusterProviderConfig", "google")
			before := fake.identityFetches()
			o := h.reconcile("upstream")
			if !reason.Is(o.err, reason.AuthenticationFailed) || !ch.IsAuthenticationFailed(o.err) || o.created {
				t.Fatalf("reconcile on %s: %s, want AuthenticationFailed (516)", image, o)
			}
			cleanError(t, image, o.err)
			if n := fake.identityFetches() - before; n != 2 {
				t.Errorf("%d ID token requests, want 2", n)
			}
			if _, _, ok := srv.database(t, "app_upstream"); ok {
				t.Error("the database was created")
			}
			t.Logf("%s: %v", image, o.err)
		})
	}
}
