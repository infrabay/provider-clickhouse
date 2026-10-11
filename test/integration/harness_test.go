// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

//go:build integration

// Package integration runs the provider's external clients against real
// ClickHouse containers: upstream ClickHouse with a password identity, and
// Altinity Antalya with a JWT identity (jwt_static_key HS256 processor) over
// the native protocol and HTTP.
//
// Containers are named xpch-it-<test>-<random> and removed when the test
// ends. Testcontainers' reaper is disabled (its container name does not
// follow that rule). Run with "make test-integration".
package integration

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	chgo "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	"github.com/golang-jwt/jwt/v5"
	"github.com/testcontainers/testcontainers-go"
	tcclickhouse "github.com/testcontainers/testcontainers-go/modules/clickhouse"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/crossplane/crossplane-runtime/v2/pkg/meta"
	"github.com/crossplane/crossplane-runtime/v2/pkg/reconciler/managed"
	"github.com/crossplane/crossplane-runtime/v2/pkg/resource"

	"github.com/infrabay/provider-clickhouse/apis/v1alpha1"
	ch "github.com/infrabay/provider-clickhouse/internal/clients/clickhouse"
	"github.com/infrabay/provider-clickhouse/internal/controller/database"
	"github.com/infrabay/provider-clickhouse/internal/controller/ownership"
	"github.com/infrabay/provider-clickhouse/internal/controller/providerconfig"
)

// Images under test. Override with comma-separated lists.
const (
	envUpstreamImages = "XPCH_UPSTREAM_IMAGES"
	envAntalyaImages  = "XPCH_ANTALYA_IMAGES"

	defaultUpstreamImages = "clickhouse/clickhouse-server:26.8.23.6"
	defaultAntalyaImages  = "altinity/clickhouse-server:26.6.4.20001.altinityantalya"

	adminUser = "admin"
)

func TestMain(m *testing.M) {
	// The reaper container is named reaper_<session>, which breaks the
	// container naming rule; every test removes its own containers instead.
	if os.Getenv("TESTCONTAINERS_RYUK_DISABLED") == "" {
		_ = os.Setenv("TESTCONTAINERS_RYUK_DISABLED", "true")
	}
	os.Exit(m.Run())
}

// images returns the images to test: the comma-separated list in env, the
// default if env is unset, or none if env is "none".
func images(t *testing.T, env, def string) []string {
	t.Helper()
	v := os.Getenv(env)
	if v == "" {
		v = def
	}
	if v == "none" {
		t.Skipf("%s=none", env)
	}
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

var nonName = regexp.MustCompile(`[^a-z0-9-]+`)

func containerName(t *testing.T) string {
	n := nonName.ReplaceAllString(strings.ToLower(t.Name()), "-")
	n = strings.Trim(n, "-")
	if len(n) > 40 {
		n = n[len(n)-40:]
	}
	return "xpch-it-" + strings.Trim(n, "-") + "-" + randHex(3)
}

// server is a running ClickHouse container with an admin connection for
// out-of-band changes.
type server struct {
	image      string
	host       string
	nativePort int
	httpPort   int
	httpsPort  int // 0 unless 8443/tcp is exposed
	admin      driver.Conn
}

// startServer starts a container. files maps container paths to contents;
// opts customize the container further (networks, extra ports).
func startServer(t *testing.T, image string, files map[string]string, opts ...testcontainers.ContainerCustomizer) *server {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	var cf []testcontainers.ContainerFile
	for path, content := range files {
		host := filepath.Join(dir, filepath.Base(path))
		if err := os.WriteFile(host, []byte(content), 0o644); err != nil { //nolint:gosec // Readable by the container user.
			t.Fatal(err)
		}
		cf = append(cf, testcontainers.ContainerFile{HostFilePath: host, ContainerFilePath: path, FileMode: 0o644})
	}
	adminPassword := randHex(16)
	c, err := tcclickhouse.Run(ctx, image, append([]testcontainers.ContainerCustomizer{
		tcclickhouse.WithUsername(adminUser),
		tcclickhouse.WithPassword(adminPassword),
		testcontainers.WithEnv(map[string]string{"CLICKHOUSE_DEFAULT_ACCESS_MANAGEMENT": "1"}),
		testcontainers.WithName(containerName(t)),
		testcontainers.WithFiles(cf...),
	}, opts...)...)
	if c != nil {
		t.Cleanup(func() {
			if err := testcontainers.TerminateContainer(c); err != nil {
				t.Logf("cannot remove container: %v", err)
			}
		})
	}
	if err != nil {
		t.Fatalf("cannot start %s: %v", image, err)
	}
	host, err := c.Host(ctx)
	if err != nil {
		t.Fatal(err)
	}
	np, err := c.MappedPort(ctx, "9000/tcp")
	if err != nil {
		t.Fatal(err)
	}
	hp, err := c.MappedPort(ctx, "8123/tcp")
	if err != nil {
		t.Fatal(err)
	}
	s := &server{image: image, host: host, nativePort: int(np.Num()), httpPort: int(hp.Num())}
	if sp, err := c.MappedPort(ctx, "8443/tcp"); err == nil {
		s.httpsPort = int(sp.Num())
	}
	s.admin, err = chgo.Open(&chgo.Options{
		Addr: []string{fmt.Sprintf("%s:%d", host, s.nativePort)},
		Auth: chgo.Auth{Username: adminUser, Password: adminPassword},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.admin.Close() })
	// Wait until the native port accepts the admin.
	deadline := time.Now().Add(60 * time.Second)
	for {
		if err = s.admin.Ping(ctx); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server not ready: %v", err)
		}
		time.Sleep(500 * time.Millisecond)
	}
	return s
}

// exec runs admin SQL.
func (s *server) exec(t *testing.T, stmt string) {
	t.Helper()
	if err := s.admin.Exec(context.Background(), stmt); err != nil {
		t.Fatalf("admin %q: %v", stmt, err)
	}
}

// database returns a database's uuid and comment, and whether it exists.
func (s *server) database(t *testing.T, name string) (string, string, bool) {
	t.Helper()
	var id, comment string
	err := s.admin.QueryRow(chgo.Context(context.Background(), chgo.WithParameters(chgo.Parameters{"n": ch.ParamValue(name)})),
		"SELECT toString(uuid), comment FROM system.databases WHERE name = {n:String}").Scan(&id, &comment)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return "", "", false
		}
		t.Fatal(err)
	}
	return id, comment, true
}

// commentChangeSupported reports whether the server has
// ALTER MODIFY DATABASE COMMENT (25.8 and later).
func (s *server) commentChangeSupported(t *testing.T) bool {
	t.Helper()
	var n uint64
	if err := s.admin.QueryRow(context.Background(), "SELECT count() FROM system.privileges WHERE privilege = 'ALTER MODIFY DATABASE COMMENT'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

// databaseGrants are the minimal grants for Database MRs, scoped to the app_
// prefix (design 9.4). The comment privilege does not exist on 25.3.
func databaseGrants(withComment bool) []string {
	g := []string{"GRANT CREATE DATABASE, DROP DATABASE, SHOW TABLES ON app_*.*"}
	if withComment {
		g = append(g, "GRANT ALTER MODIFY DATABASE COMMENT ON app_*.*")
	}
	return g
}

func grantsYAML(grants []string) string {
	var b strings.Builder
	for _, g := range grants {
		fmt.Fprintf(&b, "        - %q\n", g)
	}
	return b.String()
}

// --- Kubernetes side ---

const namespace = "team-a"

// newKube returns a fake Kubernetes API with the provider's types, in which
// the status of every managed resource kind is a subresource, as in a real
// API server.
func newKube(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.SchemeBuilder.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return clientfake.NewClientBuilder().WithScheme(s).WithObjects(objs...).
		WithStatusSubresource(&v1alpha1.Database{}, &v1alpha1.User{}, &v1alpha1.Role{}, &v1alpha1.Grant{}, &v1alpha1.RoleGrant{}).Build()
}

type nopTracker struct{}

func (nopTracker) Track(context.Context, resource.ModernManaged) error { return nil }

// resolveHook adjusts a resolved config, e.g. to switch to HTTP.
type resolveHook func(*providerconfig.Resolved)

type hookedResolver struct {
	r    providerconfig.Resolver
	hook resolveHook
}

func (h hookedResolver) Resolve(ctx context.Context, mg resource.ModernManaged) (*providerconfig.Resolved, error) {
	res, err := h.r.Resolve(ctx, mg)
	if err == nil && h.hook != nil && !res.NamespaceDenied {
		h.hook(res)
	}
	return res, err
}

// newResolver returns the resolver the tests use: the real API resolver
// over the fake Kubernetes API, with namespaced ProviderConfigs enabled.
func newResolver(kube client.Client, hook resolveHook) providerconfig.Resolver {
	return hookedResolver{r: &providerconfig.APIResolver{Kube: kube, APIReader: kube, Flags: providerconfig.Flags{EnableNamespacedProviderConfigs: true}}, hook: hook}
}

// outcome of one reconcile.
type outcome struct {
	obs     managed.ExternalObservation
	err     error
	created bool
	updated bool
	deleted bool
	gone    bool // the MR's finalizer was released
	// details are the connection details of the last Observe, Create or
	// Update of the reconcile, as published to the connection Secret.
	details managed.ConnectionDetails
}

func (o outcome) String() string {
	return fmt.Sprintf("exists=%t upToDate=%t lateInit=%t created=%t updated=%t deleted=%t gone=%t err=%v",
		o.obs.ResourceExists, o.obs.ResourceUpToDate, o.obs.ResourceLateInitialized, o.created, o.updated, o.deleted, o.gone, o.err)
}

const finalizer = "finalizer.managedresource.crossplane.io"

// mrHarness drives the external client of one managed resource kind the way
// crossplane-runtime's managed reconciler does, against a fake Kubernetes
// API: initializers, reference resolution, Observe, then Create, Update or
// Delete, the critical create annotations and the connection Secret. Every
// kind's integration tests use it; see harness for Database and
// accessFixture for the access entity kinds.
type mrHarness[T resource.ModernManaged] struct {
	t         *testing.T
	kube      client.Client
	connector managed.TypedExternalConnector[T]
	inits     []managed.Initializer
	resolver  managed.ReferenceResolver
	newObj    func() T
	publisher *managed.APILocalSecretPublisher
}

// newMRHarness returns a harness for the kind that newObj creates empty
// objects of, connected through connector. inits are the kind's
// initializers (none for Grant and RoleGrant).
func newMRHarness[T resource.ModernManaged](t *testing.T, kube client.Client, connector managed.TypedExternalConnector[T], newObj func() T, inits ...managed.Initializer) *mrHarness[T] {
	t.Helper()
	return &mrHarness[T]{
		t: t, kube: kube, connector: connector, inits: inits, newObj: newObj,
		publisher: managed.NewAPILocalSecretPublisher(kube, kube.Scheme()),
	}
}

// withReferences makes reconcile resolve references before Observe (not
// while deleting), as crossplane-runtime does.
func (h *mrHarness[T]) withReferences(r managed.ReferenceResolver) *mrHarness[T] {
	h.resolver = r
	return h
}

// create stores an MR with the managed resource finalizer, so that deleting
// it sets a deletion timestamp, as for a real MR. The namespace defaults to
// the test namespace. The fake API server assigns no UIDs; a real one always
// does, so create assigns a random one.
func (h *mrHarness[T]) create(obj T) {
	h.t.Helper()
	if obj.GetNamespace() == "" {
		obj.SetNamespace(namespace)
	}
	obj.SetUID(types.UID("uid-" + obj.GetName() + "-" + randHex(4)))
	obj.SetFinalizers([]string{finalizer})
	if err := h.kube.Create(context.Background(), obj); err != nil {
		h.t.Fatal(err)
	}
}

func (h *mrHarness[T]) get(name string) T {
	h.t.Helper()
	obj := h.newObj()
	if err := h.kube.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: name}, obj); err != nil {
		h.t.Fatal(err)
	}
	gvk, err := apiutil.GVKForObject(obj, h.kube.Scheme())
	if err != nil {
		h.t.Fatal(err)
	}
	obj.GetObjectKind().SetGroupVersionKind(gvk)
	return obj
}

func (h *mrHarness[T]) mutate(name string, fn func(T)) {
	h.t.Helper()
	obj := h.get(name)
	fn(obj)
	if err := h.kube.Update(context.Background(), obj); err != nil {
		h.t.Fatal(err)
	}
}

func (h *mrHarness[T]) delete(name string) {
	h.t.Helper()
	if err := h.kube.Delete(context.Background(), h.get(name)); err != nil {
		h.t.Fatal(err)
	}
}

// secret returns the MR's connection Secret data, or nil if there is none.
func (h *mrHarness[T]) secret(name string) map[string][]byte {
	h.t.Helper()
	ref := h.get(name).GetWriteConnectionSecretToReference()
	if ref == nil {
		return nil
	}
	s := &corev1.Secret{}
	err := h.kube.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: ref.Name}, s)
	if kerrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		h.t.Fatal(err)
	}
	return s.Data
}

func (h *mrHarness[T]) publish(ctx context.Context, obj T, d managed.ConnectionDetails, o *outcome) {
	h.t.Helper()
	o.details = d
	if _, err := h.publisher.PublishConnection(ctx, obj, d); err != nil {
		h.t.Fatalf("cannot publish connection details: %v", err)
	}
}

// reconcile runs one reconcile: initialize, resolve references, connect,
// observe, publish connection details, then create, update or delete as the
// managed reconciler would. Like crossplane-runtime with management
// policies, it releases a deleted MR without Observe when the policies do
// not include Delete, and calls Update only when they include Update.
func (h *mrHarness[T]) reconcile(name string) outcome {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	obj := h.get(name)
	var o outcome
	if meta.WasDeleted(obj) && !ownership.PoliciesOf(obj).Delete() {
		meta.RemoveFinalizer(obj, finalizer)
		if err := h.kube.Update(ctx, obj); err != nil {
			h.t.Fatal(err)
		}
		o.gone = true
		return o
	}
	for _, i := range h.inits {
		if err := i.Initialize(ctx, obj); err != nil {
			o.err = err
			return o
		}
	}
	if h.resolver != nil && !meta.WasDeleted(obj) {
		if err := h.resolver.ResolveReferences(ctx, obj); err != nil {
			o.err = err
			return o
		}
	}
	ext, err := h.connector.Connect(ctx, obj)
	if err != nil {
		o.err = err
		return o
	}
	defer func() { _ = ext.Disconnect(ctx) }()

	o.obs, o.err = ext.Observe(ctx, obj)
	if o.err != nil {
		return o
	}
	pol := ownership.PoliciesOf(obj)
	if meta.WasDeleted(obj) {
		if o.obs.ResourceExists {
			_, o.err = ext.Delete(ctx, obj)
			o.deleted = o.err == nil
			return o
		}
		meta.RemoveFinalizer(obj, finalizer)
		if err := h.kube.Update(ctx, obj); err != nil {
			h.t.Fatal(err)
		}
		o.gone = true
		return o
	}
	h.publish(ctx, obj, o.obs.ConnectionDetails, &o)
	if !o.obs.ResourceExists {
		if !pol.Create() {
			return o
		}
		meta.SetExternalCreatePending(obj, time.Now())
		if err := h.kube.Update(ctx, obj); err != nil {
			h.t.Fatal(err)
		}
		var c managed.ExternalCreation
		c, o.err = ext.Create(ctx, obj)
		if o.err != nil {
			meta.SetExternalCreateFailed(obj, time.Now())
		} else {
			meta.SetExternalCreateSucceeded(obj, time.Now())
			o.created = true
		}
		// The critical annotation update persists every annotation that
		// Create set in memory (owned-id, create-outcome).
		if err := h.kube.Update(ctx, obj); err != nil {
			h.t.Fatal(err)
		}
		if o.err == nil {
			h.publish(ctx, obj, c.ConnectionDetails, &o)
		}
		return o
	}
	if o.obs.ResourceLateInitialized && pol.LateInitialize() {
		if err := h.kube.Update(ctx, obj); err != nil {
			h.t.Fatal(err)
		}
	}
	if !o.obs.ResourceUpToDate && pol.Update() {
		var u managed.ExternalUpdate
		u, o.err = ext.Update(ctx, obj)
		o.updated = o.err == nil
		if o.err == nil {
			h.publish(ctx, obj, u.ConnectionDetails, &o)
		}
	}
	return o
}

// harness drives the Database external client.
type harness struct {
	*mrHarness[*v1alpha1.Database]
	connector *database.Connector
	cache     *ch.Cache
}

func newHarness(t *testing.T, kube client.Client, hook resolveHook) *harness {
	t.Helper()
	cache := ch.NewCache(ch.OpenDriver)
	t.Cleanup(cache.Close)
	conn := &database.Connector{
		Kube:     kube,
		Usage:    nopTracker{},
		Resolver: newResolver(kube, hook),
		Cache:    cache,
	}
	init := ownership.NewInitializer(kube, func(mg resource.Managed) *string {
		return mg.(*v1alpha1.Database).Spec.ForProvider.Name
	}, ownership.WithPreassignedID())
	return &harness{
		mrHarness: newMRHarness[*v1alpha1.Database](t, kube, conn, func() *v1alpha1.Database { return &v1alpha1.Database{} }, init),
		connector: conn,
		cache:     cache,
	}
}

// createMR stores a Database MR.
func (h *harness) createMR(name string, p v1alpha1.DatabaseParameters, pcKind, pcName string) {
	h.t.Helper()
	d := &v1alpha1.Database{}
	d.SetName(name)
	d.Spec.ForProvider = p
	d.Spec.ProviderConfigReference = &xpv2.ProviderConfigReference{Kind: pcKind, Name: pcName}
	h.create(d)
}

// --- tokens ---

const (
	tokenIssuer   = "https://issuer.example.com"
	tokenAudience = "clickhouse-it"
)

func mintToken(t *testing.T, key []byte, email, audience string, ttl time.Duration) string {
	t.Helper()
	now := time.Now()
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"iss":   tokenIssuer,
		"aud":   audience,
		"sub":   email,
		"email": email,
		"iat":   now.Add(-10 * time.Second).Unix(), // clock skew
		"exp":   now.Add(ttl).Unix(),
	})
	s, err := tok.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
