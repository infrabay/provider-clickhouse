// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

// Package controller wires the provider's controllers.
package controller

import (
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/crossplane/crossplane-runtime/v2/pkg/controller"

	"github.com/infrabay/provider-clickhouse/apis/v1alpha1"
	"github.com/infrabay/provider-clickhouse/internal/controller/config"
	"github.com/infrabay/provider-clickhouse/internal/controller/database"
	"github.com/infrabay/provider-clickhouse/internal/controller/grant"
	"github.com/infrabay/provider-clickhouse/internal/controller/role"
	"github.com/infrabay/provider-clickhouse/internal/controller/rolegrant"
	"github.com/infrabay/provider-clickhouse/internal/controller/user"
)

// Dependencies are shared by the managed resource controllers.
type Dependencies = database.Dependencies

// ManagedKinds are the managed resource kinds that have a controller. Only
// their CRDs are packaged (package/crds). A test keeps the two in sync, so a
// kind can only be packaged together with its controller.
var ManagedKinds = []string{
	v1alpha1.DatabaseKind,
	v1alpha1.UserKind,
	v1alpha1.RoleKind,
	v1alpha1.GrantKind,
	v1alpha1.RoleGrantKind,
}

// SetupGated adds every controller to the manager. Every controller is
// registered with safe-start: it is set up once the CRDs it watches are
// installed, and for managed resource kinds activated (capabilities:
// [safe-start] in crossplane.yaml). The ProviderConfig kinds are gated too,
// because Crossplane 2.0.0 and 2.0.1 activate them like managed resource
// kinds.
func SetupGated(mgr ctrl.Manager, o controller.Options, d Dependencies) error {
	for _, setup := range []func(ctrl.Manager, controller.Options) error{
		config.SetupGated,
		func(mgr ctrl.Manager, o controller.Options) error { return database.SetupGated(mgr, o, d) },
		func(mgr ctrl.Manager, o controller.Options) error { return user.SetupGated(mgr, o, d) },
		func(mgr ctrl.Manager, o controller.Options) error { return role.SetupGated(mgr, o, d) },
		func(mgr ctrl.Manager, o controller.Options) error { return grant.SetupGated(mgr, o, d) },
		func(mgr ctrl.Manager, o controller.Options) error { return rolegrant.SetupGated(mgr, o, d) },
	} {
		if err := setup(mgr, o); err != nil {
			return err
		}
	}
	return nil
}
