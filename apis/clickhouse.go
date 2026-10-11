// Copyright 2026 The provider-clickhouse Authors
// Copyright 2020 The Crossplane Authors
// SPDX-License-Identifier: Apache-2.0
//
// Portions of this file are adapted from apis/template.go of
// crossplane/provider-template (https://github.com/crossplane/provider-template),
// Copyright 2020 The Crossplane Authors, licensed under the Apache License,
// Version 2.0. Modified by the provider-clickhouse Authors.

// Package apis contains the Kubernetes API types of provider-clickhouse.
package apis

import (
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/infrabay/provider-clickhouse/apis/v1alpha1"
)

// AddToSchemes may be used to add all resources defined in the project to a
// Scheme.
var AddToSchemes = runtime.SchemeBuilder{
	v1alpha1.SchemeBuilder.AddToScheme,
}

// AddToScheme adds all resources to the Scheme.
func AddToScheme(s *runtime.Scheme) error {
	return AddToSchemes.AddToScheme(s)
}
