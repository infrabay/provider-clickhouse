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
)

// Dependencies are shared by the managed resource controllers.
type Dependencies = database.Dependencies

// ManagedKinds are the managed resource kinds that have a controller. Only
// their CRDs are packaged (package/crds); the CRDs of the other kinds are
// generated into apis/unreleased-crds (see apis/generate.go). A test keeps
// the two in sync.
var ManagedKinds = []string{v1alpha1.DatabaseKind}

// SetupGated adds every controller to the manager. Managed resource
// controllers are registered with safe-start: each is set up once its CRD is
// installed and activated (capabilities: [safe-start] in crossplane.yaml).
func SetupGated(mgr ctrl.Manager, o controller.Options, d Dependencies) error {
	for _, setup := range []func(ctrl.Manager, controller.Options) error{
		config.Setup,
		func(mgr ctrl.Manager, o controller.Options) error { return database.SetupGated(mgr, o, d) },

		// TODO(v1alpha1): register the remaining kinds once their controllers
		// are implemented (design sections 5.4 to 5.7), add them to
		// ManagedKinds and stop moving their CRDs to apis/unreleased-crds in
		// apis/generate.go. Until then their CRDs are not packaged, so MRs of
		// these kinds cannot be created.
		//   user.SetupGated      (design 5.4: jwt/password users, SHOW CREATE USER parser, connection Secrets)
		//   role.SetupGated      (design 5.5: ownership as for User, name collision checks)
		//   grant.SetupGated     (design 5.6: privilege catalogue, revoke-aware coverage)
		//   rolegrant.SetupGated (design 5.7: membership and admin option)
	} {
		if err := setup(mgr, o); err != nil {
			return err
		}
	}
	return nil
}
