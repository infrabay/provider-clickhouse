// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/crossplane/crossplane-runtime/v2/pkg/event"
	"github.com/crossplane/crossplane-runtime/v2/pkg/meta"

	"github.com/infrabay/provider-clickhouse/apis/v1alpha1"
	cfgpolicy "github.com/infrabay/provider-clickhouse/internal/controller/providerconfig"
)

// ReasonInvalidConfig is the Warning event on a ClusterProviderConfig whose
// policy does not compile.
const ReasonInvalidConfig event.Reason = "InvalidConfig"

// PolicyCheck compiles the policy of a ClusterProviderConfig before the
// usage reconciler runs (design 3.3). CEL cannot check the patterns: a
// pattern that is not RE2, or whose {namespace} placement breaks the rules,
// is found in Go only, and every managed resource that uses the config then
// fails with InvalidConfig. PolicyCheck emits a Warning event on the config
// itself, so that its owner sees it there too. Writing the error into the
// config's status needs a health controller (OQ-7).
type PolicyCheck struct {
	// Kube reads the ClusterProviderConfig.
	Kube client.Reader
	// Recorder emits the event.
	Recorder event.Recorder
	// Next is the usage reconciler.
	Next reconcile.Reconciler
}

// Reconcile checks the policy and hands the request on to Next. A config
// that cannot be read is left to Next.
func (p *PolicyCheck) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	pc := &v1alpha1.ClusterProviderConfig{}
	if err := p.Kube.Get(ctx, req.NamespacedName, pc); err == nil && !meta.WasDeleted(pc) {
		if _, err := cfgpolicy.CompilePolicy(pc.Spec.Policy); err != nil {
			p.Recorder.Event(pc, event.Warning(ReasonInvalidConfig, fmt.Errorf("%w; every managed resource that uses this ClusterProviderConfig fails until the policy is fixed", err)))
		}
	}
	return p.Next.Reconcile(ctx, req)
}
