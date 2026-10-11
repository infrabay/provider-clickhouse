// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	chgo "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/crossplane/crossplane-runtime/v2/pkg/controller"
	"github.com/crossplane/crossplane-runtime/v2/pkg/feature"
	"github.com/crossplane/crossplane-runtime/v2/pkg/gate"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
	"github.com/crossplane/crossplane-runtime/v2/pkg/ratelimiter"
	"github.com/crossplane/crossplane-runtime/v2/pkg/reconciler/customresourcesgate"
	"github.com/crossplane/crossplane-runtime/v2/pkg/resource"

	"github.com/infrabay/provider-clickhouse/apis"
	"github.com/infrabay/provider-clickhouse/apis/v1alpha1"
	ch "github.com/infrabay/provider-clickhouse/internal/clients/clickhouse"
	xpcontroller "github.com/infrabay/provider-clickhouse/internal/controller"
	"github.com/infrabay/provider-clickhouse/internal/controller/access"
	"github.com/infrabay/provider-clickhouse/internal/controller/providerconfig"
	"github.com/infrabay/provider-clickhouse/internal/reason"
)

// startProvider starts the provider's controllers, wired as in cmd/provider,
// against a real kube-apiserver (envtest) with every packaged CRD installed,
// and returns a client of that API server. The management policies feature
// flag is deliberately not enabled: the controllers always honour management
// policies. It needs KUBEBUILDER_ASSETS (make test-integration sets it).
func startProvider(t *testing.T) client.Client {
	t.Helper()
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "package", "crds")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("cannot start envtest: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, apis.AddToScheme, apiextensionsv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	// Every test starts its own manager in this process; controller names
	// are only unique per manager.
	skip := true
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		Controller:             ctrlconfig.Controller{SkipNameValidation: &skip},
	})
	if err != nil {
		t.Fatal(err)
	}
	o := controller.Options{
		Logger:                  logging.NewNopLogger(),
		MaxConcurrentReconciles: 2,
		PollInterval:            2 * time.Second,
		GlobalRateLimiter:       ratelimiter.NewGlobal(10),
		Features:                &feature.Flags{},
		Gate:                    new(gate.Gate[schema.GroupVersionKind]),
	}
	cache := ch.NewCache(ch.OpenDriver)
	if err := mgr.Add(cache); err != nil {
		t.Fatal(err)
	}
	if err := customresourcesgate.Setup(mgr, o); err != nil {
		t.Fatal(err)
	}
	if err := xpcontroller.SetupGated(mgr, o, xpcontroller.Dependencies{Cache: cache, Flags: providerconfig.Flags{EnableNamespacedProviderConfigs: true}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mgr.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	kube, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	return kube
}

// TestProviderEndToEnd runs the provider's controllers, wired as in
// cmd/provider, against a real kube-apiserver (envtest) and an upstream
// ClickHouse container: a Database MR becomes Ready, follows spec changes and
// is dropped on deletion. An observe-only MR never changes or drops the
// database it observes, although the management policies feature flag is
// not enabled here: the controllers always honour management policies. It
// needs KUBEBUILDER_ASSETS (make test-integration sets it).
func TestProviderEndToEnd(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS is not set; run make test-integration")
	}
	image := images(t, envUpstreamImages, defaultUpstreamImages)[0]
	password := randHex(16)
	srv := upstreamServer(t, image, password)
	kube := startProvider(t)
	for _, obj := range []client.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "crossplane-system"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "crossplane-system", Name: "provider-clickhouse"}, Data: map[string][]byte{"password": []byte(password)}},
		func() client.Object {
			c := passwordCPC(srv, "e2e", v1alpha1.ClusterPolicy{AllowedNamespaces: []string{"team-.*"}, DatabaseNamePatterns: []string{"app_.+"}})
			c.UID = ""
			return c
		}(),
		&v1alpha1.Database{
			ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "events"},
			Spec: v1alpha1.DatabaseSpec{
				ManagedResourceSpec: xpv2.ManagedResourceSpec{ProviderConfigReference: &xpv2.ProviderConfigReference{Kind: "ClusterProviderConfig", Name: "e2e"}},
				ForProvider:         v1alpha1.DatabaseParameters{Name: new("app_events"), Comment: new("v1")},
			},
		},
	} {
		if err := kube.Create(context.Background(), obj); err != nil {
			t.Fatalf("create %T: %v", obj, err)
		}
	}

	// A policy pattern that CEL cannot check and Go cannot compile is
	// reported on the config itself (design 3.3).
	invalid := passwordCPC(srv, "e2e-invalid", v1alpha1.ClusterPolicy{DatabaseNamePatterns: []string{"app_("}})
	invalid.UID = ""
	if err := kube.Create(context.Background(), invalid); err != nil {
		t.Fatal(err)
	}
	eventually(t, 60*time.Second, "Warning event InvalidConfig on the ClusterProviderConfig", func() (bool, string) {
		events := &corev1.EventList{}
		if err := kube.List(context.Background(), events); err != nil {
			return false, err.Error()
		}
		for _, e := range events.Items {
			if e.InvolvedObject.Kind == v1alpha1.ClusterProviderConfigKind && e.InvolvedObject.Name == invalid.Name && e.Type == corev1.EventTypeWarning &&
				e.Reason == "InvalidConfig" && strings.Contains(e.Message, "policy.databaseNamePatterns[0]") {
				return true, ""
			}
		}
		return false, fmt.Sprintf("%d events, none for %s", len(events.Items), invalid.Name)
	})

	key := client.ObjectKey{Namespace: namespace, Name: "events"}
	eventually(t, 90*time.Second, "Database Ready and Synced", func() (bool, string) {
		d := &v1alpha1.Database{}
		if err := kube.Get(context.Background(), key, d); err != nil {
			return false, err.Error()
		}
		ready, synced := d.GetCondition(xpv2.TypeReady), d.GetCondition(xpv2.TypeSynced)
		return ready.Status == corev1.ConditionTrue && synced.Status == corev1.ConditionTrue,
			"ready=" + string(ready.Status) + " " + ready.Message + "; synced=" + string(synced.Status) + " " + synced.Message
	})

	d := &v1alpha1.Database{}
	_ = kube.Get(context.Background(), key, d)
	id, comment, ok := srv.database(t, "app_events")
	if !ok || id != d.GetAnnotations()[v1alpha1.AnnotationOwnedID] || comment != "v1" {
		t.Fatalf("server %q %q %t, owned-id %q", id, comment, ok, d.GetAnnotations()[v1alpha1.AnnotationOwnedID])
	}
	if d.Status.AtProvider.ID != id || d.Spec.ForProvider.Engine == nil {
		t.Errorf("status %+v, late-initialized engine %+v", d.Status.AtProvider, d.Spec.ForProvider.Engine)
	}
	usages := &v1alpha1.ProviderConfigUsageList{}
	if err := kube.List(context.Background(), usages, client.InNamespace(namespace)); err != nil || len(usages.Items) != 1 {
		t.Errorf("ProviderConfigUsages: %d, %v", len(usages.Items), err)
	}

	if srv.commentChangeSupported(t) {
		d.Spec.ForProvider.Comment = new("v2")
		if err := kube.Update(context.Background(), d); err != nil {
			t.Fatal(err)
		}
		eventually(t, 60*time.Second, "comment updated on the server", func() (bool, string) {
			_, c, _ := srv.database(t, "app_events")
			return c == "v2", "comment=" + c
		})
	}

	if err := kube.Delete(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	eventually(t, 60*time.Second, "Database dropped and MR gone", func() (bool, string) {
		_, _, exists := srv.database(t, "app_events")
		err := kube.Get(context.Background(), key, &v1alpha1.Database{})
		return !exists && kerrors.IsNotFound(err), "exists on server=" + boolString(exists)
	})

	// Observe-only: a database created by someone else is reported, never
	// altered, and left in place when the MR is deleted.
	srv.exec(t, "CREATE DATABASE app_legacy COMMENT 'theirs'")
	legacyID, _, _ := srv.database(t, "app_legacy")
	legacy := &v1alpha1.Database{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "legacy"},
		Spec: v1alpha1.DatabaseSpec{
			ManagedResourceSpec: xpv2.ManagedResourceSpec{
				ProviderConfigReference: &xpv2.ProviderConfigReference{Kind: "ClusterProviderConfig", Name: "e2e"},
				ManagementPolicies:      xpv2.ManagementPolicies{xpv2.ManagementActionObserve},
			},
			ForProvider: v1alpha1.DatabaseParameters{Name: new("app_legacy"), Comment: new("mine"), ForceDrop: new(true)},
		},
	}
	if err := kube.Create(context.Background(), legacy); err != nil {
		t.Fatal(err)
	}
	legacyKey := client.ObjectKeyFromObject(legacy)
	eventually(t, 60*time.Second, "observe-only Database Ready", func() (bool, string) {
		got := &v1alpha1.Database{}
		if err := kube.Get(context.Background(), legacyKey, got); err != nil {
			return false, err.Error()
		}
		ready := got.GetCondition(xpv2.TypeReady)
		return ready.Status == corev1.ConditionTrue && got.Status.AtProvider.ID == legacyID, "ready=" + string(ready.Status) + " " + ready.Message
	})
	// A few polls (2 s each) must not change the comment.
	time.Sleep(5 * time.Second)
	if _, comment, _ := srv.database(t, "app_legacy"); comment != "theirs" {
		t.Fatalf("an observe-only MR altered the database: comment %q", comment)
	}
	if err := kube.Get(context.Background(), legacyKey, legacy); err != nil {
		t.Fatal(err)
	}
	if err := kube.Delete(context.Background(), legacy); err != nil {
		t.Fatal(err)
	}
	eventually(t, 60*time.Second, "observe-only MR gone", func() (bool, string) {
		err := kube.Get(context.Background(), legacyKey, &v1alpha1.Database{})
		return kerrors.IsNotFound(err), "MR still exists"
	})
	if id, comment, ok := srv.database(t, "app_legacy"); !ok || id != legacyID || comment != "theirs" {
		t.Fatalf("an observe-only MR dropped or altered the database: %q %q %t", id, comment, ok)
	}
}

// Names of the access end-to-end scenario. The provider identity of the
// access fixtures may only create users and roles whose names start with
// app_ (design 9.4).
const (
	e2eDatabase     = "app_shop"
	e2eRole         = "app_reader"
	e2ePasswordUser = "app_svc"             // upstream: generated password
	e2eJWTUser      = "app_svc@example.com" // Antalya: JWT, pinned to processorTest
	e2eForeignUser  = "app_foreign"
	e2eForeignRole  = "app_foreign_role"
)

// TestProviderEndToEndAccess runs every managed resource kind through the
// provider's real controllers (envtest), against each image of
// XPCH_UPSTREAM_IMAGES and XPCH_ANTALYA_IMAGES, with the minimal,
// prefix-scoped identity of design 9.4 and a closed ClusterProviderConfig
// policy: a Database, a Role, a User (a generated password upstream, JWT with
// a pinned processor and claims on Antalya), a Grant on the database for the
// role and a RoleGrant of the role to the user, the last two through
// references. The user then logs in with what the connection Secret holds
// (or a token) and can do exactly what was granted. A user, a role and their
// grants that an administrator created before stay byte-identical
// throughout, although MRs try to take over the user, grant to it and give
// it the provider's role. Deleting the MRs revokes and drops only what the
// provider created.
func TestProviderEndToEndAccess(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS is not set; run make test-integration")
	}
	ran := false
	for _, fam := range []struct{ name, env, def string }{
		{familyUpstream, envUpstreamImages, defaultUpstreamImages},
		{familyAntalya, envAntalyaImages, defaultAntalyaImages},
	} {
		for _, image := range imageList(fam.env, fam.def) {
			ran = true
			t.Run(image, func(t *testing.T) { testEndToEndAccess(t, fam.name, image) })
		}
	}
	if !ran {
		t.Skip("no images selected")
	}
}

// e2eAccess is one run of the access end-to-end scenario.
type e2eAccess struct {
	t      *testing.T
	family string
	srv    *server
	kube   client.Client
	pc     *xpv2.ProviderConfigReference
	// user is the ClickHouse name of the User MR "app".
	user string
	// jwtKeys are the token processors' keys (Antalya only).
	jwtKeys map[string][]byte
}

func testEndToEndAccess(t *testing.T, family, image string) {
	e := &e2eAccess{t: t, family: family, pc: &xpv2.ProviderConfigReference{Kind: v1alpha1.ClusterProviderConfigKind, Name: "e2e"}}
	policy := v1alpha1.ClusterPolicy{
		AllowedNamespaces:    []string{"team-.*"},
		DatabaseNamePatterns: []string{"app_.+"},
		UserNamePatterns:     []string{"app_.+"},
		RoleNamePatterns:     []string{"app_.+"},
		GrantableRoles:       []string{"app_.+"},
		AllowedJWTProcessors: []string{processorTest},
	}
	objs := []client.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "crossplane-system"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}},
	}
	var cpc *v1alpha1.ClusterProviderConfig
	switch family {
	case familyUpstream:
		password := randHex(16)
		e.srv = accessUpstreamServer(t, image, password, nil)
		e.user = e2ePasswordUser
		policy.AllowedAuthentication = []v1alpha1.AuthenticationMethod{v1alpha1.AuthenticationMethodPassword}
		cpc = passwordCPC(e.srv, "e2e", policy)
		objs = append(objs, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: "crossplane-system", Name: "provider-clickhouse"},
			Data:       map[string][]byte{"password": []byte(password)},
		})
	case familyAntalya:
		e.srv, e.jwtKeys = accessAntalyaServer(t, image, nil)
		e.user = e2eJWTUser
		tokenPath := filepath.Join(t.TempDir(), "token")
		writeToken(t, tokenPath, mintToken(t, e.jwtKeys[processorTest], jwtProviderUser, tokenAudience, 2*time.Hour))
		policy.AllowedAuthentication = []v1alpha1.AuthenticationMethod{v1alpha1.AuthenticationMethodJWT}
		cpc = tokenFileCPC(e.srv, tokenPath)
		cpc.Name = "e2e"
		cpc.Spec.Policy = policy
	default:
		t.Fatalf("unknown image family %q", family)
	}
	cpc.UID = ""
	objs = append(objs, cpc)

	// Entities an administrator created before the provider was installed.
	e.srv.exec(t, "CREATE USER "+e2eForeignUser+" IDENTIFIED WITH sha256_hash BY "+ch.QuoteString(testHash)+" SALT "+ch.QuoteString(testSalt)+" SETTINGS max_threads = 3")
	e.srv.exec(t, "CREATE ROLE "+e2eForeignRole)
	e.srv.exec(t, "GRANT SELECT ON "+e2eDatabase+".* TO "+e2eForeignRole)
	e.srv.exec(t, "GRANT INSERT ON "+e2eDatabase+".orders TO "+e2eForeignUser)
	e.srv.exec(t, "GRANT "+e2eForeignRole+" TO "+e2eForeignUser)
	foreign := e.foreignState()

	e.kube = startProvider(t)
	for _, obj := range objs {
		if err := e.kube.Create(context.Background(), obj); err != nil {
			t.Fatalf("create %T: %v", obj, err)
		}
	}

	db := &v1alpha1.Database{ObjectMeta: e.meta("shop")}
	db.Labels = map[string]string{"e2e": "shop"}
	db.Spec.ProviderConfigReference = e.pc
	db.Spec.ForProvider.Name = new(e2eDatabase)

	reader := &v1alpha1.Role{ObjectMeta: e.meta("reader")}
	reader.Labels = map[string]string{"e2e": "reader"}
	reader.Spec.ProviderConfigReference = e.pc
	reader.Spec.ForProvider.Name = new(e2eRole)

	app := e.userMR("app", e.user)
	app.Labels = map[string]string{"e2e": "app"}
	app.Spec.ForProvider.DefaultDatabase = new(e2eDatabase)

	// The Grant selects its database and role, the RoleGrant references its
	// role and selects its user. crossplane-runtime writes the reference a
	// selector resolves to with the MR's own namespace.
	grant := &v1alpha1.Grant{ObjectMeta: e.meta("reader-shop")}
	grant.Spec.ProviderConfigReference = e.pc
	grant.Spec.ForProvider = v1alpha1.GrantParameters{
		Privileges:       []string{"SELECT", "INSERT"},
		DatabaseSelector: &xpv2.NamespacedSelector{MatchLabels: map[string]string{"e2e": "shop"}},
		RoleSelector:     &xpv2.NamespacedSelector{MatchLabels: map[string]string{"e2e": "reader"}},
	}

	membership := &v1alpha1.RoleGrant{ObjectMeta: e.meta("app-reader")}
	membership.Spec.ProviderConfigReference = e.pc
	membership.Spec.ForProvider = v1alpha1.RoleGrantParameters{
		MemberOfRef:  &xpv2.NamespacedReference{Name: "reader"},
		UserSelector: &xpv2.NamespacedSelector{MatchLabels: map[string]string{"e2e": "app"}},
	}

	// MRs that point at the foreign user: a User that would take it over,
	// a Grant and a RoleGrant that would change its privileges.
	intruder := e.userMR("intruder", e2eForeignUser)
	intruderGrant := &v1alpha1.Grant{ObjectMeta: e.meta("intruder-grant")}
	intruderGrant.Spec.ProviderConfigReference = e.pc
	intruderGrant.Spec.ForProvider = v1alpha1.GrantParameters{Privileges: []string{"SELECT", "INSERT"}, Database: new(e2eDatabase), User: new(e2eForeignUser)}
	intruderRoleGrant := &v1alpha1.RoleGrant{ObjectMeta: e.meta("intruder-rolegrant")}
	intruderRoleGrant.Spec.ProviderConfigReference = e.pc
	intruderRoleGrant.Spec.ForProvider = v1alpha1.RoleGrantParameters{MemberOfRef: &xpv2.NamespacedReference{Name: "reader"}, User: new(e2eForeignUser)}

	owned := []resource.Managed{db, reader, app, grant, membership}
	intruders := []resource.Managed{intruder, intruderGrant, intruderRoleGrant}
	for _, mg := range append(slices.Clone(owned), intruders...) {
		if err := e.kube.Create(context.Background(), mg); err != nil {
			t.Fatalf("create %T %s: %v", mg, mg.GetName(), err)
		}
	}
	for _, mg := range owned {
		e.waitFor(mg, "Ready and Synced", func(ready, synced xpv2.Condition) bool {
			return ready.Status == corev1.ConditionTrue && synced.Status == corev1.ConditionTrue
		})
	}
	for _, mg := range intruders {
		e.waitFor(mg, "NotOwned", func(ready, synced xpv2.Condition) bool {
			return synced.Status == corev1.ConditionFalse && strings.Contains(synced.Message, string(reason.NotOwned)) && ready.Status != corev1.ConditionTrue
		})
	}
	// The selectors resolved to references in the MRs' own namespace.
	for _, c := range []struct {
		field, name, value, want string
		ref                      *xpv2.NamespacedReference
	}{
		{"databaseRef", "shop", deref(grant.Spec.ForProvider.Database), e2eDatabase, grant.Spec.ForProvider.DatabaseRef},
		{"roleRef", "reader", deref(grant.Spec.ForProvider.Role), e2eRole, grant.Spec.ForProvider.RoleRef},
		{"userRef", "app", deref(membership.Spec.ForProvider.User), e.user, membership.Spec.ForProvider.UserRef},
	} {
		if c.ref == nil || c.ref.Name != c.name || (c.ref.Namespace != "" && c.ref.Namespace != namespace) || c.value != c.want {
			t.Errorf("%s resolved to %+v (%q), want %s in namespace %s (%q)", c.field, c.ref, c.value, c.name, namespace, c.want)
		}
	}

	// The server holds exactly what the MRs describe, under the UUIDs the
	// MRs claim.
	for _, c := range []struct {
		mg   resource.Managed
		kind access.EntityKind
		name string
	}{{reader, access.Role, e2eRole}, {app, access.User, e.user}} {
		id, storage, ok := e.srv.entity(t, c.kind, c.name)
		if !ok || storage != ch.StorageLocalDirectory || id != c.mg.GetAnnotations()[v1alpha1.AnnotationOwnedID] {
			t.Fatalf("%s %q: id %q storage %q exists %t; owned-id %q", c.kind, c.name, id, storage, ok, c.mg.GetAnnotations()[v1alpha1.AnnotationOwnedID])
		}
	}
	if id, _, ok := e.srv.database(t, e2eDatabase); !ok || id != db.GetAnnotations()[v1alpha1.AnnotationOwnedID] {
		t.Fatalf("database %s: id %q exists %t; owned-id %q", e2eDatabase, id, ok, db.GetAnnotations()[v1alpha1.AnnotationOwnedID])
	}
	granted := []string{"INSERT " + e2eDatabase + ".*", "SELECT " + e2eDatabase + ".*"}
	if got := e.srv.grantsOf(t, true, e2eRole); !slices.Equal(got, granted) {
		t.Fatalf("grants of %s: %v, want %v", e2eRole, got, granted)
	}
	if admin, _, ok := e.srv.rgMembership(t, access.User, e.user, e2eRole); !ok || admin {
		t.Fatalf("membership of %s in %s: exists %t, admin option %t", e.user, e2eRole, ok, admin)
	}
	if grant.Status.AtProvider.Grantee != "role:"+e2eRole || grant.Status.AtProvider.Target != e2eDatabase+".*" || membership.Status.AtProvider.Grantee != "user:"+e.user {
		t.Errorf("status: Grant %+v, RoleGrant %+v", grant.Status.AtProvider, membership.Status.AtProvider)
	}
	e.sameForeign(foreign, "creating the MRs")

	// A privilege that someone else adds at the Grant's target is revoked
	// on the next poll (the identity can revoke only what it holds WITH
	// GRANT OPTION, which includes SHOW TABLES on app_*.*).
	e.srv.exec(t, "GRANT SHOW TABLES ON "+e2eDatabase+".* TO "+e2eRole)
	eventually(t, 60*time.Second, "foreign privilege at the Grant's target revoked", func() (bool, string) {
		got := e.srv.grantsOf(t, true, e2eRole)
		g := &v1alpha1.Grant{}
		_ = e.kube.Get(context.Background(), client.ObjectKeyFromObject(grant), g)
		return slices.Equal(got, granted), strings.Join(got, ", ") + "; synced: " + g.GetCondition(xpv2.TypeSynced).Message
	})

	// The user logs in and can do exactly what was granted.
	e.srv.exec(t, "CREATE TABLE "+e2eDatabase+".orders (id UInt64, item String) ENGINE = MergeTree ORDER BY id")
	e.srv.exec(t, "CREATE DATABASE e2e_other")
	e.srv.exec(t, "CREATE TABLE e2e_other.secrets (x UInt8) ENGINE = Memory")
	conn := e.login()
	var who, current string
	if err := conn.QueryRow(context.Background(), "SELECT currentUser(), currentDatabase()").Scan(&who, &current); err != nil || who != e.user || current != e2eDatabase {
		t.Fatalf("logged in as %q in %q: %v", who, current, err)
	}
	if err := conn.Exec(context.Background(), "INSERT INTO "+e2eDatabase+".orders VALUES (1, 'book')"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	var n uint64
	if err := conn.QueryRow(context.Background(), "SELECT count() FROM "+e2eDatabase+".orders").Scan(&n); err != nil || n != 1 {
		t.Fatalf("SELECT: %d rows, %v", n, err)
	}
	for _, stmt := range []string{
		"SELECT count() FROM e2e_other.secrets",
		"CREATE TABLE " + e2eDatabase + ".extra (x UInt8) ENGINE = Memory",
		"ALTER TABLE " + e2eDatabase + ".orders DELETE WHERE 1",
		"TRUNCATE TABLE " + e2eDatabase + ".orders",
		"DROP TABLE " + e2eDatabase + ".orders",
		"GRANT SELECT ON " + e2eDatabase + ".* TO " + e2eForeignUser,
		"CREATE USER app_evil IDENTIFIED WITH sha256_hash BY " + ch.QuoteString(testHash) + " SALT " + ch.QuoteString(testSalt),
	} {
		expectDenied(t, conn, stmt)
	}
	e.sameForeign(foreign, "the user's session")

	// Deleting the Grant and the RoleGrant revokes exactly what they
	// granted; the user and the role stay.
	e.deleteAll(grant, membership)
	if got := e.srv.grantsOf(t, true, e2eRole); len(got) != 0 {
		t.Errorf("grants of %s after deleting the Grant: %v", e2eRole, got)
	}
	if _, _, ok := e.srv.rgMembership(t, access.User, e.user, e2eRole); ok {
		t.Errorf("%s is still a member of %s after deleting the RoleGrant", e.user, e2eRole)
	}
	if _, _, ok := e.srv.entity(t, access.User, e.user); !ok {
		t.Errorf("deleting the RoleGrant dropped user %s", e.user)
	}
	expectDenied(t, e.login(), "SELECT count() FROM "+e2eDatabase+".orders")
	e.sameForeign(foreign, "deleting the Grant and the RoleGrant")

	// Deleting the User, the Role and the (empty) Database drops them. The
	// identity holds no DROP TABLE, so the table goes first.
	e.srv.exec(t, "DROP TABLE "+e2eDatabase+".orders SYNC")
	e.deleteAll(app, reader, db)
	if _, _, ok := e.srv.entity(t, access.User, e.user); ok {
		t.Errorf("user %s still exists", e.user)
	}
	if _, _, ok := e.srv.entity(t, access.Role, e2eRole); ok {
		t.Errorf("role %s still exists", e2eRole)
	}
	if _, _, ok := e.srv.database(t, e2eDatabase); ok {
		t.Errorf("database %s still exists", e2eDatabase)
	}
	e.sameForeign(foreign, "deleting the User, the Role and the Database")

	// The MRs that pointed at the foreign user go away without touching it,
	// and it still logs in with its own password.
	e.deleteAll(intruders...)
	e.sameForeign(foreign, "deleting the MRs that pointed at the foreign user")
	fc, err := chgo.Open(&chgo.Options{
		Addr: []string{net.JoinHostPort(e.srv.host, strconv.Itoa(e.srv.nativePort))},
		Auth: chgo.Auth{Username: e2eForeignUser, Password: testPassword},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer fc.Close() //nolint:errcheck // Test connection.
	if err := fc.QueryRow(context.Background(), "SELECT currentUser()").Scan(&who); err != nil || who != e2eForeignUser {
		t.Errorf("the foreign user cannot log in with its own password: %q, %v", who, err)
	}
}

func (e *e2eAccess) meta(name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Namespace: namespace, Name: name}
}

// userMR returns a User MR for the ClickHouse user name: with a generated
// password on upstream, and on Antalya a JWT user pinned to processorTest
// that must carry the claim groups: [shop].
func (e *e2eAccess) userMR(mrName, name string) *v1alpha1.User {
	u := &v1alpha1.User{ObjectMeta: e.meta(mrName)}
	u.Spec.ProviderConfigReference = e.pc
	u.Spec.ForProvider.Name = new(name)
	u.Spec.WriteConnectionSecretToReference = &xpv2.LocalSecretReference{Name: mrName + "-clickhouse"}
	if e.family == familyAntalya {
		u.Spec.ForProvider.Authentication.JWT = &v1alpha1.JWTAuthentication{Processor: processorTest, Claims: &apiextensionsv1.JSON{Raw: []byte(`{"groups":["shop"]}`)}}
	} else {
		u.Spec.ForProvider.Authentication.Password = &v1alpha1.PasswordAuthentication{Generate: &v1alpha1.GeneratePassword{}}
	}
	return u
}

// waitFor waits until mg's Ready and Synced conditions pass ok, and leaves
// the MR as last read in mg.
func (e *e2eAccess) waitFor(mg resource.Managed, what string, ok func(ready, synced xpv2.Condition) bool) {
	e.t.Helper()
	key := client.ObjectKeyFromObject(mg)
	eventually(e.t, 120*time.Second, fmt.Sprintf("%T %s %s", mg, key.Name, what), func() (bool, string) {
		if err := e.kube.Get(context.Background(), key, mg); err != nil {
			return false, err.Error()
		}
		ready, synced := mg.GetCondition(xpv2.TypeReady), mg.GetCondition(xpv2.TypeSynced)
		return ok(ready, synced), fmt.Sprintf("ready=%s %s; synced=%s %s", ready.Status, ready.Message, synced.Status, synced.Message)
	})
}

// deleteAll deletes the MRs and waits until they are gone.
func (e *e2eAccess) deleteAll(mgs ...resource.Managed) {
	e.t.Helper()
	for _, mg := range mgs {
		if err := e.kube.Delete(context.Background(), mg); err != nil {
			e.t.Fatalf("delete %T %s: %v", mg, mg.GetName(), err)
		}
	}
	for _, mg := range mgs {
		key := client.ObjectKeyFromObject(mg)
		eventually(e.t, 120*time.Second, fmt.Sprintf("%T %s gone", mg, key.Name), func() (bool, string) {
			err := e.kube.Get(context.Background(), key, mg)
			if kerrors.IsNotFound(err) {
				return true, ""
			}
			synced := mg.GetCondition(xpv2.TypeSynced)
			return false, fmt.Sprintf("still there (%v); synced=%s %s", err, synced.Status, synced.Message)
		})
	}
}

// login connects as the User MR "app": with the password from its
// connection Secret on upstream, with a token of processorTest that carries
// the required claim on Antalya. It also checks the connection Secret's
// keys, and on Antalya that a token without the claim is refused.
func (e *e2eAccess) login() driver.Conn {
	e.t.Helper()
	s := &corev1.Secret{}
	if err := e.kube.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: "app-clickhouse"}, s); err != nil {
		e.t.Fatalf("connection Secret: %v", err)
	}
	want := map[string]string{
		v1alpha1.ConnectionKeyUsername: e.user,
		v1alpha1.ConnectionKeyEndpoint: e.srv.host,
		v1alpha1.ConnectionKeyPort:     strconv.Itoa(e.srv.nativePort),
		v1alpha1.ConnectionKeyProtocol: string(v1alpha1.ProtocolNative),
		v1alpha1.ConnectionKeyTLS:      "false",
	}
	opts := &chgo.Options{Addr: []string{net.JoinHostPort(e.srv.host, strconv.Itoa(e.srv.nativePort))}}
	if e.family == familyAntalya {
		want[v1alpha1.ConnectionKeyJWTProcessor] = processorTest
		if _, ok := s.Data[v1alpha1.ConnectionKeyPassword]; ok {
			e.t.Error("the connection Secret of a JWT user has a password key")
		}
		e.expectLoginRefused(mintTokenClaims(e.t, e.jwtKeys[processorTest], jwt.MapClaims{"email": e.user}))
		token := mintTokenClaims(e.t, e.jwtKeys[processorTest], jwt.MapClaims{"email": e.user, "groups": []string{"other", "shop"}})
		opts.GetJWT = func(context.Context) (string, error) { return token, nil }
	} else {
		password := string(s.Data[v1alpha1.ConnectionKeyPassword])
		if len(password) != 32 {
			e.t.Fatalf("the connection Secret holds a password of %d characters, want 32", len(password))
		}
		opts.Auth = chgo.Auth{Username: string(s.Data[v1alpha1.ConnectionKeyUsername]), Password: password}
	}
	for k, v := range want {
		if got := string(s.Data[k]); got != v {
			e.t.Errorf("connection Secret key %s = %q, want %q", k, got, v)
		}
	}
	conn, err := chgo.Open(opts)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// expectLoginRefused checks that the token does not log in (516).
func (e *e2eAccess) expectLoginRefused(token string) {
	e.t.Helper()
	conn, err := chgo.Open(&chgo.Options{
		Addr:   []string{net.JoinHostPort(e.srv.host, strconv.Itoa(e.srv.nativePort))},
		GetJWT: func(context.Context) (string, error) { return token, nil },
	})
	if err != nil {
		e.t.Fatal(err)
	}
	defer conn.Close() //nolint:errcheck // Test connection.
	if code := exceptionCode(conn.Ping(context.Background())); code != 516 {
		e.t.Errorf("a token without the required claim: code %d, want 516", code)
	}
}

// foreignState is what an administrator sees of the foreign user and role:
// their UUIDs, definitions, grants and memberships, exactly as the server
// prints them.
func (e *e2eAccess) foreignState() []string {
	e.t.Helper()
	var out []string
	for _, q := range []string{
		"SELECT toString(id) FROM system.users WHERE name = " + ch.QuoteString(e2eForeignUser),
		"SELECT toString(id) FROM system.roles WHERE name = " + ch.QuoteString(e2eForeignRole),
		"SHOW CREATE USER " + ch.QuoteIdent(e2eForeignUser),
		"SHOW CREATE ROLE " + ch.QuoteIdent(e2eForeignRole),
		"SHOW GRANTS FOR " + ch.QuoteIdent(e2eForeignUser),
		"SHOW GRANTS FOR " + ch.QuoteIdent(e2eForeignRole),
		"SELECT concat(ifNull(user_name, ''), '|', ifNull(role_name, ''), '|', granted_role_name, '|', toString(with_admin_option), '|', toString(granted_role_is_default)) " +
			"FROM system.role_grants WHERE user_name = " + ch.QuoteString(e2eForeignUser) + " OR role_name = " + ch.QuoteString(e2eForeignRole) + " ORDER BY 1",
	} {
		out = append(out, q+" => "+strings.Join(e.srv.rawStrings(e.t, q), "\n"))
	}
	return out
}

// sameForeign fails the test if the foreign user or role changed.
func (e *e2eAccess) sameForeign(before []string, what string) {
	e.t.Helper()
	if diff := cmp.Diff(before, e.foreignState()); diff != "" {
		e.t.Errorf("%s changed the foreign user or role (-before +after):\n%s", what, diff)
	}
}

// expectDenied checks that stmt fails with 497 ACCESS_DENIED.
func expectDenied(t *testing.T, conn driver.Conn, stmt string) {
	t.Helper()
	if code := exceptionCode(conn.Exec(context.Background(), stmt)); code != 497 {
		t.Errorf("%s: code %d, want 497 (ACCESS_DENIED)", stmt, code)
	}
}

// exceptionCode returns the code of a ClickHouse exception, or 0.
func exceptionCode(err error) int32 {
	var ex *chgo.Exception
	if errors.As(err, &ex) {
		return ex.Code
	}
	return 0
}

func eventually(t *testing.T, timeout time.Duration, what string, cond func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last := ""
	for time.Now().Before(deadline) {
		ok, msg := cond()
		if ok {
			return
		}
		last = msg
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s: %s", what, last)
}

func boolString(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// deref returns *s, or "" for nil.
func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
