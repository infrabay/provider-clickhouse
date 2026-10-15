// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package access

import (
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/crossplane/crossplane-runtime/v2/pkg/controller"
	"github.com/crossplane/crossplane-runtime/v2/pkg/event"
	"github.com/crossplane/crossplane-runtime/v2/pkg/feature"
	"github.com/crossplane/crossplane-runtime/v2/pkg/ratelimiter"
	"github.com/crossplane/crossplane-runtime/v2/pkg/reconciler/managed"
	"github.com/crossplane/crossplane-runtime/v2/pkg/resource"
	"github.com/crossplane/crossplane-runtime/v2/pkg/statemetrics"

	ch "github.com/infrabay/provider-clickhouse/internal/clients/clickhouse"
	"github.com/infrabay/provider-clickhouse/internal/controller/ownership"
	"github.com/infrabay/provider-clickhouse/internal/controller/providerconfig"
)

// Dependencies are shared by every managed resource controller.
type Dependencies struct {
	// Cache holds one connection pool per ProviderConfig.
	Cache *ch.Cache
	// Flags are the provider flags that govern ProviderConfigs.
	Flags providerconfig.Flags
	// Journal is the create journal of User and Role (D-29); nil means
	// ownership.DefaultJournal, the journal of this process. Tests that run
	// several managers in one process give each its own.
	Journal *ownership.Journal
}

// A Controller describes the controller of one access entity kind.
type Controller[T resource.ModernManaged] struct {
	// GroupKind is the kind's group kind string, e.g. v1alpha1.UserGroupKind.
	GroupKind string
	// GVK is the kind's group version kind.
	GVK schema.GroupVersionKind
	// Object is an empty object of the kind, List an empty list of it (for
	// the state metrics).
	Object T
	List   resource.ManagedList
	// NewConnector builds the kind's connector with the controller's event
	// recorder.
	NewConnector func(recorder event.Recorder) managed.TypedExternalConnector[T]
	// Initializers run before Connect: ownership.NewInitializer (external
	// name and claim) for User and Role, none for Grant and RoleGrant.
	// crossplane-runtime's default NameAsExternalName is never used.
	Initializers []managed.Initializer
	// References resolves references (providerconfig.NewReferenceResolver);
	// nil for kinds without references.
	References managed.ReferenceResolver
	// Timeout overrides crossplane-runtime's reconcile timeout when set.
	Timeout time.Duration
}

// Options returns the reconciler options every access entity kind shares
// (design 5.9), with the recorder the connector also uses.
//
//   - WithManagementPolicies is always passed, whatever the feature flags
//     say: ownership depends on management policies, and without the option
//     crossplane-runtime would allow every action, so an observe-only MR
//     would be updated and deleted (D-26);
//   - WithDeterministicExternalName: every name comes from the spec, and a
//     crash during Create is recoverable (D-8);
//   - WithInitializers is always passed, even empty, so that the default
//     NameAsExternalName initializer never runs.
func Options[T resource.ModernManaged](mgr ctrl.Manager, o controller.Options, c Controller[T], recorder event.Recorder) ([]managed.ReconcilerOption, error) {
	name := managed.ControllerName(c.GroupKind)
	opts := []managed.ReconcilerOption{
		managed.WithTypedExternalConnector[T](c.NewConnector(recorder)),
		managed.WithInitializers(c.Initializers...),
		managed.WithDeterministicExternalName(true),
		managed.WithManagementPolicies(),
		managed.WithLogger(o.Logger.WithValues("controller", name)),
		managed.WithPollInterval(o.PollInterval),
		managed.WithRecorder(recorder),
	}
	if c.References != nil {
		opts = append(opts, managed.WithReferenceResolver(c.References))
	}
	if c.Timeout > 0 {
		opts = append(opts, managed.WithTimeout(c.Timeout))
	}
	if o.Features != nil && o.Features.Enabled(feature.EnableAlphaChangeLogs) && o.ChangeLogOptions != nil {
		opts = append(opts, managed.WithChangeLogger(o.ChangeLogOptions.ChangeLogger))
	}
	if o.MetricOptions != nil {
		if o.MetricOptions.MRMetrics != nil {
			opts = append(opts, managed.WithMetricRecorder(o.MetricOptions.MRMetrics))
		}
		if o.MetricOptions.MRStateMetrics != nil && c.List != nil {
			sm := statemetrics.NewMRStateRecorder(mgr.GetClient(), o.Logger, o.MetricOptions.MRStateMetrics, c.List, o.MetricOptions.PollStateMetricInterval)
			if err := mgr.Add(sm); err != nil {
				return nil, fmt.Errorf("cannot register MR state metrics recorder for %s: %w", c.GroupKind, err)
			}
		}
	}
	return opts, nil
}

// Setup adds the controller of an access entity kind to the manager.
func Setup[T resource.ModernManaged](mgr ctrl.Manager, o controller.Options, c Controller[T]) error {
	name := managed.ControllerName(c.GroupKind)
	recorder := event.NewAPIRecorder(mgr.GetEventRecorderFor(name)) //nolint:staticcheck // crossplane-runtime still uses the legacy events API.
	opts, err := Options(mgr, o, c, recorder)
	if err != nil {
		return err
	}
	r := managed.NewReconciler(mgr, resource.ManagedKind(c.GVK), opts...)
	return ctrl.NewControllerManagedBy(mgr).
		Named(name).
		WithOptions(o.ForControllerRuntime()).
		WithEventFilter(resource.DesiredStateChanged()).
		For(c.Object).
		Complete(FinishOnShutdown(ratelimiter.NewReconciler(name, r, o.GlobalRateLimiter)))
}

// SetupGated registers setup with safe-start: it runs once the CRD of gvk is
// installed and activated. A failing setup panics, as the Database
// controller's does: the manager cannot run without it.
func SetupGated(o controller.Options, gvk schema.GroupVersionKind, setup func() error) {
	o.Gate.Register(func() {
		if err := setup(); err != nil {
			panic(fmt.Errorf("cannot set up the %s controller: %w", gvk.Kind, err))
		}
	}, gvk)
}
