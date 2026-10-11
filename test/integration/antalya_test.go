// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	"github.com/testcontainers/testcontainers-go"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/infrabay/provider-clickhouse/apis/v1alpha1"
	ch "github.com/infrabay/provider-clickhouse/internal/clients/clickhouse"
	"github.com/infrabay/provider-clickhouse/internal/clients/clickhouse/auth"
	"github.com/infrabay/provider-clickhouse/internal/controller/providerconfig"
	"github.com/infrabay/provider-clickhouse/internal/reason"
)

const jwtProviderUser = "provider@example.com"

// antalyaServer starts Antalya with a jwt_static_key HS256 token processor
// (random key, never committed) and a JWT provider identity in users.d that
// pins the processor and holds only the prefix-scoped Database grants. HTTPS
// is served on 8443 with a certificate from tc.
func antalyaServer(t *testing.T, image string, tc testTLS) (*server, []byte) {
	t.Helper()
	key := []byte(randHex(24))
	processors := fmt.Sprintf(`token_processors:
  test:
    type: jwt_static_key
    algo: HS256
    static_key: %q
    expected_issuer: %s
    expected_audience: %s
    username_claim: email
`, key, tokenIssuer, tokenAudience)
	usersD := fmt.Sprintf(`users:
  %q:
    jwt: {processor: test}
    networks: {ip: ["::/0"]}
    profile: default
    quota: default
    grants:
      query:
%s`, jwtProviderUser, grantsYAML(databaseGrants(true)))
	files := map[string]string{
		"/etc/clickhouse-server/config.d/token_processors.yaml":   processors,
		"/etc/clickhouse-server/users.d/provider-clickhouse.yaml": usersD,
	}
	maps.Copy(files, tc.serverFiles())
	srv := startServer(t, image, files, testcontainers.WithExposedPorts("8443/tcp"))
	if srv.httpsPort == 0 {
		t.Fatal("HTTPS port is not mapped")
	}
	return srv, key
}

// tokenFileTLSCPC is tokenFileCPC with TLS verified against the test CA.
func tokenFileTLSCPC(s *server, path string) *v1alpha1.ClusterProviderConfig {
	c := tokenFileCPC(s, path)
	c.Name, c.UID = "antalya-tls", types.UID("cpc-antalya-tls")
	c.Spec.AllowInsecureTransport = false
	c.Spec.TLS = &v1alpha1.ClusterTLS{
		TLSCommon:       v1alpha1.TLSCommon{Enabled: true},
		CACertSecretRef: &xpv2.SecretKeySelector{SecretReference: xpv2.SecretReference{Namespace: "crossplane-system", Name: "clickhouse-ca"}, Key: "ca.crt"},
	}
	return c
}

func tokenFileCPC(s *server, path string) *v1alpha1.ClusterProviderConfig {
	port := int32(s.nativePort) //nolint:gosec // A mapped port fits.
	return &v1alpha1.ClusterProviderConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "antalya", UID: types.UID("cpc-antalya")},
		Spec: v1alpha1.ClusterProviderConfigSpec{
			ConnectionSpec: v1alpha1.ConnectionSpec{
				Endpoint:               v1alpha1.Endpoint{Host: s.host, Port: &port},
				AllowInsecureTransport: true,
			},
			Credentials: v1alpha1.ClusterCredentials{
				Source:    v1alpha1.CredentialsSourceTokenFile,
				TokenFile: &v1alpha1.TokenFileCredentials{Path: path, Audience: tokenAudience},
			},
			Policy: v1alpha1.ClusterPolicy{AllowedNamespaces: []string{namespace}, DatabaseNamePatterns: []string{"app_.+"}},
		},
	}
}

func writeToken(t *testing.T, path, token string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAntalyaTokenFile(t *testing.T) {
	for _, image := range images(t, envAntalyaImages, defaultAntalyaImages) {
		t.Run(image, func(t *testing.T) { testAntalyaTokenFile(t, image) })
	}
}

func testAntalyaTokenFile(t *testing.T, image string) {
	tc := newTestTLS(t)
	srv, key := antalyaServer(t, image, tc)
	tokenPath := filepath.Join(t.TempDir(), "token")
	writeToken(t, tokenPath, mintToken(t, key, jwtProviderUser, tokenAudience, time.Hour))
	kube := newKube(t, tokenFileCPC(srv, tokenPath), tokenFileTLSCPC(srv, tokenPath),
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "crossplane-system", Name: "clickhouse-ca"}, Data: map[string][]byte{"ca.crt": []byte(tc.caPEM)}})

	// lifecycle runs a Database through create, observe, drift repair and
	// delete with the given harness and config.
	lifecycle := func(t *testing.T, h *harness, pc, mr, dbName string) {
		t.Helper()
		h.createMR(mr, v1alpha1.DatabaseParameters{Name: new(dbName), Comment: new("v1")}, "ClusterProviderConfig", pc)
		if o := h.reconcile(mr); o.err != nil || !o.created {
			t.Fatalf("create: %s", o)
		}
		id, _, ok := srv.database(t, dbName)
		if !ok || id != h.get(mr).GetAnnotations()[v1alpha1.AnnotationOwnedID] {
			t.Fatalf("server uuid %q (exists %t)", id, ok)
		}
		if o := h.reconcile(mr); o.err != nil || !o.obs.ResourceExists || !o.obs.ResourceUpToDate {
			t.Fatalf("observe: %s", o)
		}
		srv.exec(t, "ALTER DATABASE "+ch.QuoteIdent(dbName)+" MODIFY COMMENT 'drifted'")
		if o := h.reconcile(mr); o.err != nil || !o.updated {
			t.Fatalf("drift: %s", o)
		}
		if _, c, _ := srv.database(t, dbName); c != "v1" {
			t.Fatalf("comment after repair = %q", c)
		}
		h.delete(mr)
		if o := h.reconcile(mr); o.err != nil || !o.deleted {
			t.Fatalf("delete: %s", o)
		}
		if _, _, ok := srv.database(t, dbName); ok {
			t.Fatal("database still exists")
		}
		if o := h.reconcile(mr); o.err != nil || !o.gone {
			t.Fatalf("finalize: %s", o)
		}
	}

	t.Run("IdentityIsTheJWTUser", func(t *testing.T) {
		h := newHarness(t, kube, nil)
		cl, err := h.connectFor(tokenFileCPC(srv, tokenPath))
		if err != nil {
			t.Fatal(err)
		}
		defer cl.Close() //nolint:errcheck // Test cleanup.
		if got := cl.Info().CurrentUser; got != jwtProviderUser {
			t.Fatalf("currentUser() = %q, want %q", got, jwtProviderUser)
		}
		ok, err := cl.Capabilities().JWTUsers(context.Background(), cl)
		if err != nil || !ok {
			t.Fatalf("JWT users capability on %s = %t, %v", image, ok, err)
		}
	})

	t.Run("NativeDatabaseLifecycle", func(t *testing.T) {
		lifecycle(t, newHarness(t, kube, nil), "antalya", "native", "app_native")
	})

	t.Run("HTTPDatabaseLifecycle", func(t *testing.T) {
		// HTTP is not selectable through the v1alpha1 API; it is exercised at
		// the client layer: Bearer token over POST, no GetJWT.
		toHTTP := func(r *providerconfig.Resolved) {
			r.Config.Protocol = ch.ProtocolHTTP
			r.Config.Port = srv.httpPort
			r.CacheKey.Version += ";http"
		}
		h := newHarness(t, kube, toHTTP)
		cl, err := h.connectFor(tokenFileCPC(srv, tokenPath))
		if err != nil {
			t.Fatal(err)
		}
		if got := cl.Info().CurrentUser; got != jwtProviderUser {
			t.Fatalf("currentUser() over HTTP = %q, want %q (never 'default')", got, jwtProviderUser)
		}
		_ = cl.Close()
		lifecycle(t, h, "antalya", "http", "app_http")
	})

	t.Run("HTTPSDatabaseLifecycle", func(t *testing.T) {
		// Over HTTPS clickhouse-go fills in user "default" and asks for
		// certificate authentication; the bearer transport must strip both,
		// or the server rejects every request.
		toHTTPS := func(r *providerconfig.Resolved) {
			r.Config.Protocol = ch.ProtocolHTTP
			r.Config.Port = srv.httpsPort
			r.CacheKey.Version += ";https"
		}
		h := newHarness(t, kube, toHTTPS)
		cl, err := h.connectFor(tokenFileTLSCPC(srv, tokenPath))
		if err != nil {
			t.Fatal(err)
		}
		if got := cl.Info().CurrentUser; got != jwtProviderUser {
			t.Fatalf("currentUser() over HTTPS = %q, want %q", got, jwtProviderUser)
		}
		_ = cl.Close()
		lifecycle(t, h, "antalya-tls", "https", "app_https")
	})

	t.Run("TokenFileIsRereadOnNewConnections", func(t *testing.T) {
		// A token with the wrong audience never leaves the provider.
		writeToken(t, tokenPath, mintToken(t, key, jwtProviderUser, "kubernetes", time.Hour))
		h := newHarness(t, kube, nil)
		h.createMR("rotate", v1alpha1.DatabaseParameters{Name: new("app_rotate")}, "ClusterProviderConfig", "antalya")
		if o := h.reconcile("rotate"); !reason.Is(o.err, reason.InvalidConfig) {
			t.Fatalf("wrong audience: %s, want InvalidConfig", o)
		}
		// A token signed with another key reaches the server and fails with
		// 516 after one retry.
		writeToken(t, tokenPath, mintToken(t, []byte("not-the-key"), jwtProviderUser, tokenAudience, time.Hour))
		if o := newHarness(t, kube, nil).reconcile("rotate"); !reason.Is(o.err, reason.AuthenticationFailed) {
			t.Fatalf("bad signature: %s, want AuthenticationFailed", o)
		}
		// The rotated file is picked up by the next connection.
		writeToken(t, tokenPath, mintToken(t, key, jwtProviderUser, tokenAudience, time.Hour))
		if o := newHarness(t, kube, nil).reconcile("rotate"); o.err != nil || !o.created {
			t.Fatalf("after rotation: %s", o)
		}
	})

	t.Run("RetryOnceOn516WithFreshToken", func(t *testing.T) {
		good := mintToken(t, key, jwtProviderUser, tokenAudience, time.Hour)
		bad := mintToken(t, []byte("stale-key"), jwtProviderUser, tokenAudience, time.Hour)
		ts := &scriptedTokens{first: bad, then: good}
		h := newHarness(t, kube, func(r *providerconfig.Resolved) {
			r.Config.TokenSource = ts
			r.CacheKey.Version += ";scripted"
		})
		h.createMR("retry", v1alpha1.DatabaseParameters{Name: new("app_retry")}, "ClusterProviderConfig", "antalya")
		if o := h.reconcile("retry"); o.err != nil || !o.created {
			t.Fatalf("reconcile with one stale token: %s", o)
		}
		if n := ts.invalidations(); n != 1 {
			t.Fatalf("invalidated %d times, want exactly 1", n)
		}
	})
}

// connectFor acquires a client for a ClusterProviderConfig through the
// harness's resolver and cache.
func (h *harness) connectFor(cpc *v1alpha1.ClusterProviderConfig) (ch.Client, error) {
	d := &v1alpha1.Database{}
	d.SetNamespace(namespace)
	d.SetName("probe")
	d.Spec.ProviderConfigReference = &xpv2.ProviderConfigReference{Kind: v1alpha1.ClusterProviderConfigKind, Name: cpc.Name}
	res, err := h.connector.Resolver.Resolve(context.Background(), d)
	if err != nil {
		return nil, err
	}
	return h.cache.Acquire(context.Background(), res.CacheKey, res.Config)
}

// scriptedTokens returns first until it is invalidated, then then.
type scriptedTokens struct {
	first, then string

	mu          sync.Mutex
	invalidated int
}

func (s *scriptedTokens) Token(context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.invalidated == 0 {
		return s.first, nil
	}
	return s.then, nil
}

func (s *scriptedTokens) Invalidate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.invalidated++
}

func (s *scriptedTokens) invalidations() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.invalidated
}

var _ auth.TokenSource = &scriptedTokens{}
