// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

//go:build generate

// Code generation for the API types. Run "make generate".

// Remove existing CRDs so that deleted kinds do not linger.
//go:generate rm -rf ../package/crds

// Deepcopy methods and CRD manifests. Every managed resource kind has a
// controller (internal/controller.ManagedKinds), so every CRD is packaged. A
// kind added without a controller must not be packaged: a unit test in
// internal/controller fails when package/crds and ManagedKinds disagree.
//go:generate go tool controller-gen object:headerFile=../hack/boilerplate.go.txt paths=./... crd:crdVersions=v1 output:artifacts:config=../package/crds

// crossplane-runtime method sets (managed, provider config, usage) and reference resolvers.
//go:generate go tool angryjet generate-methodsets --header-file=../hack/boilerplate.go.txt ./...

package apis
