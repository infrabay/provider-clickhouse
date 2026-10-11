// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/crossplane/crossplane-runtime/v2/pkg/controller"
	"github.com/crossplane/crossplane-runtime/v2/pkg/feature"
	"github.com/crossplane/crossplane-runtime/v2/pkg/gate"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
	"github.com/crossplane/crossplane-runtime/v2/pkg/ratelimiter"
	"github.com/crossplane/crossplane-runtime/v2/pkg/reconciler/customresourcesgate"

	"github.com/infrabay/provider-clickhouse/apis"
	"github.com/infrabay/provider-clickhouse/apis/v1alpha1"
	ch "github.com/infrabay/provider-clickhouse/internal/clients/clickhouse"
	xpcontroller "github.com/infrabay/provider-clickhouse/internal/controller"
	"github.com/infrabay/provider-clickhouse/internal/controller/providerconfig"
)

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
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{Scheme: scheme, Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0"})
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
	// The management policies feature is deliberately not enabled.
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
