// Copyright 2026 The provider-clickhouse Authors
// Copyright 2020 The Crossplane Authors
// SPDX-License-Identifier: Apache-2.0
//
// Portions of this file are adapted from
// pkg/controller/namespaced/mysql/config/config.go of
// crossplane-contrib/provider-sql
// (https://github.com/crossplane-contrib/provider-sql), Copyright 2020 The
// Crossplane Authors, licensed under the Apache License, Version 2.0.
// Modified by the provider-clickhouse Authors.

// Package config contains the ProviderConfig and ClusterProviderConfig
// controllers. They account for usage: a config cannot be deleted while
// ProviderConfigUsages reference it. The ClusterProviderConfig controller
// also reports a policy that does not compile with a Warning event.
package config

import (
	"fmt"

	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/crossplane/crossplane-runtime/v2/pkg/controller"
	"github.com/crossplane/crossplane-runtime/v2/pkg/event"
	"github.com/crossplane/crossplane-runtime/v2/pkg/ratelimiter"
	"github.com/crossplane/crossplane-runtime/v2/pkg/reconciler/providerconfig"
	"github.com/crossplane/crossplane-runtime/v2/pkg/resource"

	"github.com/infrabay/provider-clickhouse/apis/v1alpha1"
)

// Setup adds the ProviderConfig and ClusterProviderConfig controllers.
func Setup(mgr ctrl.Manager, o controller.Options) error {
	if err := setupProviderConfig(mgr, o); err != nil {
		return err
	}
	return setupClusterProviderConfig(mgr, o)
}

// SetupGated registers the ProviderConfig and ClusterProviderConfig
// controllers with safe-start, like the managed resource controllers: each
// is set up once the CRDs of its config kind and of ProviderConfigUsage are
// installed. Crossplane 2.0.2 and later install these CRDs with the package.
// Crossplane 2.0.0 and 2.0.1 turn them into ManagedResourceDefinitions that
// stay inactive until an activation policy activates them; a controller set
// up before that cannot start its watches, and the manager stops after the
// cache sync timeout.
func SetupGated(mgr ctrl.Manager, o controller.Options) error {
	setupGated(o,
		func() error { return setupProviderConfig(mgr, o) },
		func() error { return setupClusterProviderConfig(mgr, o) })
	return nil
}

// setupGated registers the setup functions of the two config controllers
// with the gate of o.
func setupGated(o controller.Options, providerConfig, clusterProviderConfig func() error) {
	for _, c := range []struct {
		setup   func() error
		depends []schema.GroupVersionKind
	}{
		{providerConfig, []schema.GroupVersionKind{v1alpha1.ProviderConfigGroupVersionKind, v1alpha1.ProviderConfigUsageGroupVersionKind}},
		{clusterProviderConfig, []schema.GroupVersionKind{v1alpha1.ClusterProviderConfigGroupVersionKind, v1alpha1.ProviderConfigUsageGroupVersionKind}},
	} {
		kind := c.depends[0].Kind
		o.Gate.Register(func() {
			// A failing setup panics, as the managed resource controllers'
			// do: the manager cannot run without it.
			if err := c.setup(); err != nil {
				panic(fmt.Errorf("cannot set up the %s controller: %w", kind, err))
			}
		}, c.depends...)
	}
}

func setupProviderConfig(mgr ctrl.Manager, o controller.Options) error {
	name := providerconfig.ControllerName(v1alpha1.ProviderConfigGroupKind)
	of := resource.ProviderConfigKinds{
		Config:    v1alpha1.ProviderConfigGroupVersionKind,
		Usage:     v1alpha1.ProviderConfigUsageGroupVersionKind,
		UsageList: v1alpha1.ProviderConfigUsageListGroupVersionKind,
	}
	r := providerconfig.NewReconciler(mgr, of,
		providerconfig.WithLogger(o.Logger.WithValues("controller", name)),
		providerconfig.WithRecorder(event.NewAPIRecorder(mgr.GetEventRecorderFor(name)))) //nolint:staticcheck // crossplane-runtime still uses the legacy events API.

	return ctrl.NewControllerManagedBy(mgr).
		Named(name).
		WithOptions(o.ForControllerRuntime()).
		For(&v1alpha1.ProviderConfig{}).
		Watches(&v1alpha1.ProviderConfigUsage{}, &resource.EnqueueRequestForProviderConfig{Kind: v1alpha1.ProviderConfigKind}).
		Complete(ratelimiter.NewReconciler(name, r, o.GlobalRateLimiter))
}

func setupClusterProviderConfig(mgr ctrl.Manager, o controller.Options) error {
	name := providerconfig.ControllerName(v1alpha1.ClusterProviderConfigGroupKind)
	// Every MR of this provider is namespaced, so every usage is a namespaced
	// ProviderConfigUsage, also for a ClusterProviderConfig. The usage tracker
	// labels each usage with the config kind, so the two controllers do not
	// count each other's usages.
	of := resource.ProviderConfigKinds{
		Config:    v1alpha1.ClusterProviderConfigGroupVersionKind,
		Usage:     v1alpha1.ProviderConfigUsageGroupVersionKind,
		UsageList: v1alpha1.ProviderConfigUsageListGroupVersionKind,
	}
	recorder := event.NewAPIRecorder(mgr.GetEventRecorderFor(name)) //nolint:staticcheck // crossplane-runtime still uses the legacy events API.
	r := providerconfig.NewReconciler(mgr, of,
		providerconfig.WithLogger(o.Logger.WithValues("controller", name)),
		providerconfig.WithRecorder(recorder))
	// The policy is compiled first, so that an invalid pattern is reported
	// on the config itself (design 3.3).
	check := &PolicyCheck{Kube: mgr.GetClient(), Recorder: recorder, Next: r}

	return ctrl.NewControllerManagedBy(mgr).
		Named(name).
		WithOptions(o.ForControllerRuntime()).
		For(&v1alpha1.ClusterProviderConfig{}).
		Watches(&v1alpha1.ProviderConfigUsage{}, &resource.EnqueueRequestForProviderConfig{Kind: v1alpha1.ClusterProviderConfigKind}).
		Complete(ratelimiter.NewReconciler(name, check, o.GlobalRateLimiter))
}
