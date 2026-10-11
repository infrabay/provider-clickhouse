// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

//go:build generate

// Code generation for the API types. Run "make generate".

// Remove existing CRDs so that deleted kinds do not linger.
//go:generate rm -rf ../package/crds ./unreleased-crds

// Deepcopy methods and CRD manifests.
//go:generate go tool controller-gen object:headerFile=../hack/boilerplate.go.txt paths=./... crd:crdVersions=v1 output:artifacts:config=../package/crds

// Only kinds with a controller are packaged. The CRDs of kinds whose
// controllers are not implemented yet (User, Role, Grant, RoleGrant) move to
// apis/unreleased-crds: the envtest suite still checks their schemas and CEL
// rules, but the provider package never installs them, so no managed
// resource of those kinds can be created and silently ignored. Move a CRD
// back by deleting it from this list when its controller is registered in
// internal/controller/register.go.
//go:generate mkdir -p ./unreleased-crds
//go:generate mv ../package/crds/clickhouse.m.infrabay.io_users.yaml ../package/crds/clickhouse.m.infrabay.io_roles.yaml ../package/crds/clickhouse.m.infrabay.io_grants.yaml ../package/crds/clickhouse.m.infrabay.io_rolegrants.yaml ./unreleased-crds/

// crossplane-runtime method sets (managed, provider config, usage) and reference resolvers.
//go:generate go tool angryjet generate-methodsets --header-file=../hack/boilerplate.go.txt ./...

package apis
