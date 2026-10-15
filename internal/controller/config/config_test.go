// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/crossplane/crossplane-runtime/v2/pkg/controller"
	"github.com/crossplane/crossplane-runtime/v2/pkg/gate"

	"github.com/infrabay/provider-clickhouse/apis/v1alpha1"
)

// TestSetupGated: each config controller is set up only once the CRDs of its
// config kind and of ProviderConfigUsage are installed (Crossplane 2.0.0 and
// 2.0.1 install them only when they are activated).
func TestSetupGated(t *testing.T) {
	o := controller.Options{Gate: new(gate.Gate[schema.GroupVersionKind])}
	// Setup would need a manager; with no CRD installed it must not run.
	if err := SetupGated(nil, o); err != nil {
		t.Fatal(err)
	}

	o = controller.Options{Gate: new(gate.Gate[schema.GroupVersionKind])}
	pc, cpc := make(chan struct{}), make(chan struct{})
	setupGated(o,
		func() error { close(pc); return nil },
		func() error { close(cpc); return nil })
	ran := func(c chan struct{}) bool {
		select {
		case <-c:
			return true
		case <-time.After(100 * time.Millisecond):
			return false
		}
	}

	o.Gate.Set(v1alpha1.ProviderConfigGroupVersionKind, true)
	o.Gate.Set(v1alpha1.ClusterProviderConfigGroupVersionKind, true)
	if ran(pc) || ran(cpc) {
		t.Fatal("a config controller was set up before the ProviderConfigUsage CRD was installed")
	}
	o.Gate.Set(v1alpha1.ProviderConfigUsageGroupVersionKind, true)
	if !ran(pc) || !ran(cpc) {
		t.Fatal("the config controllers were not set up once their CRDs were installed")
	}

	o = controller.Options{Gate: new(gate.Gate[schema.GroupVersionKind])}
	pc, cpc = make(chan struct{}), make(chan struct{})
	setupGated(o,
		func() error { close(pc); return nil },
		func() error { close(cpc); return nil })
	o.Gate.Set(v1alpha1.ProviderConfigUsageGroupVersionKind, true)
	o.Gate.Set(v1alpha1.ProviderConfigGroupVersionKind, true)
	if !ran(pc) {
		t.Fatal("the ProviderConfig controller was not set up once its CRDs were installed")
	}
	if ran(cpc) {
		t.Fatal("the ClusterProviderConfig controller was set up before its CRD was installed")
	}
}
