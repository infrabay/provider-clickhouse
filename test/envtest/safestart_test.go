// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

//go:build envtest

package envtest

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
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

	"github.com/infrabay/provider-clickhouse/apis"
	xpcontroller "github.com/infrabay/provider-clickhouse/internal/controller"
)

// TestConfigControllersWaitForTheirCRDs starts the provider's controllers,
// wired as in cmd/provider, in a cluster without the provider's CRDs.
// Crossplane 2.0.0 and 2.0.1 install the ProviderConfig,
// ClusterProviderConfig and ProviderConfigUsage CRDs of a safe-start
// provider only once an activation policy activates them, like those of the
// managed resource kinds. The config controllers must wait for their CRDs:
// a controller started before its CRD exists stops the manager after the
// cache sync timeout. Once the CRDs are installed, the controllers reconcile
// (they add the in-use finalizer to a config).
func TestConfigControllersWaitForTheirCRDs(t *testing.T) {
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("cannot start envtest (is KUBEBUILDER_ASSETS set?): %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, apiextensionsv1.AddToScheme, apis.AddToScheme} {
		if err := add(s); err != nil {
			t.Fatal(err)
		}
	}
	skip := true
	// The same timeout applies once the CRDs are installed, when the
	// controllers' watches wait for the API server to serve the new kinds; a
	// few seconds of discovery lag on a loaded CI runner exceeded 3s.
	const syncTimeout = 15 * time.Second
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 s,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		Controller:             ctrlconfig.Controller{SkipNameValidation: &skip, CacheSyncTimeout: syncTimeout},
	})
	if err != nil {
		t.Fatal(err)
	}
	o := controller.Options{
		Logger:                  logging.NewNopLogger(),
		MaxConcurrentReconciles: 1,
		PollInterval:            time.Minute,
		GlobalRateLimiter:       ratelimiter.NewGlobal(10),
		Features:                &feature.Flags{},
		Gate:                    new(gate.Gate[schema.GroupVersionKind]),
	}
	if err := customresourcesgate.Setup(mgr, o); err != nil {
		t.Fatal(err)
	}
	// No managed resource CRD is installed, so the managed resource
	// controllers (and their dependencies) are never set up.
	if err := xpcontroller.SetupGated(mgr, o, xpcontroller.Dependencies{}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	var startErr error
	go func() {
		startErr = mgr.Start(ctx)
		close(stopped)
	}()
	t.Cleanup(func() {
		cancel()
		<-stopped
	})
	running := func(what string) {
		t.Helper()
		select {
		case <-stopped:
			t.Fatalf("the manager stopped %s: %v", what, startErr)
		default:
		}
	}

	// Longer than the cache sync timeout without the CRDs.
	time.Sleep(syncTimeout + 5*time.Second)
	running("while the config CRDs were missing")

	crds := filepath.Join("..", "..", "package", "crds")
	if _, err := envtest.InstallCRDs(cfg, envtest.CRDInstallOptions{
		Paths: []string{
			filepath.Join(crds, "clickhouse.m.infrabay.io_providerconfigs.yaml"),
			filepath.Join(crds, "clickhouse.m.infrabay.io_clusterproviderconfigs.yaml"),
			filepath.Join(crds, "clickhouse.m.infrabay.io_providerconfigusages.yaml"),
		},
		ErrorIfPathMissing: true,
	}); err != nil {
		t.Fatal(err)
	}

	c, err := client.New(cfg, client.Options{Scheme: s})
	if err != nil {
		t.Fatal(err)
	}
	pc := obj(t, "apiVersion: clickhouse.m.infrabay.io/v1alpha1\nkind: ProviderConfig\nmetadata: {name: pc, namespace: gated}\n"+
		"spec:\n  endpoint: {host: h}\n  tls: {enabled: true}\n  credentials:\n    source: PasswordSecret\n    passwordSecret: {username: p, passwordSecretRef: {name: s, key: password}}\n")
	cpc := obj(t, cpcHeader+"spec:\n  endpoint: {host: h}\n  tls: {enabled: true}\n  credentials:\n    source: TokenFile\n    tokenFile: {path: /var/run/secrets/clickhouse/token, audience: clickhouse}\n  policy: {}\n")
	for _, o := range []client.Object{obj(t, "apiVersion: v1\nkind: Namespace\nmetadata: {name: gated}"), pc, cpc} {
		if err := c.Create(ctx, o); err != nil {
			t.Fatalf("create %s %s: %v", o.GetObjectKind().GroupVersionKind().Kind, o.GetName(), err)
		}
	}
	for _, o := range []client.Object{pc, cpc} {
		deadline := time.Now().Add(30 * time.Second)
		for {
			if err := c.Get(ctx, client.ObjectKeyFromObject(o), o); err != nil {
				t.Fatal(err)
			}
			if slices.Contains(o.GetFinalizers(), "in-use.crossplane.io") {
				break
			}
			running("after the config CRDs were installed")
			if time.Now().After(deadline) {
				t.Fatalf("%s %s was not reconciled after its CRD was installed (finalizers %v)", o.GetObjectKind().GroupVersionKind().Kind, o.GetName(), o.GetFinalizers())
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	running("after reconciling the configs")
}
