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
