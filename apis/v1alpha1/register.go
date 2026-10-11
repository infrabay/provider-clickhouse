// Copyright 2026 The provider-clickhouse Authors
// Copyright 2020 The Crossplane Authors
// SPDX-License-Identifier: Apache-2.0
//
// Portions of this file are adapted from the API registration files
// (register.go) of crossplane/provider-template
// (https://github.com/crossplane/provider-template) and
// crossplane-contrib/provider-sql
// (https://github.com/crossplane-contrib/provider-sql), Copyright 2020 The
// Crossplane Authors, licensed under the Apache License, Version 2.0.
// Modified by the provider-clickhouse Authors.

package v1alpha1

import (
	"reflect"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

// Package type metadata.
const (
	Group   = "clickhouse.m.infrabay.io"
	Version = "v1alpha1"
)

var (
	// SchemeGroupVersion is the group version used to register these objects.
	SchemeGroupVersion = schema.GroupVersion{Group: Group, Version: Version}

	// SchemeBuilder is used to add Go types to the GroupVersionKind scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: SchemeGroupVersion}
)

// ProviderConfig type metadata.
var (
	ProviderConfigKind             = reflect.TypeFor[ProviderConfig]().Name()
	ProviderConfigGroupKind        = schema.GroupKind{Group: Group, Kind: ProviderConfigKind}.String()
	ProviderConfigGroupVersionKind = SchemeGroupVersion.WithKind(ProviderConfigKind)
)

// ClusterProviderConfig type metadata.
var (
	ClusterProviderConfigKind             = reflect.TypeFor[ClusterProviderConfig]().Name()
	ClusterProviderConfigGroupKind        = schema.GroupKind{Group: Group, Kind: ClusterProviderConfigKind}.String()
	ClusterProviderConfigGroupVersionKind = SchemeGroupVersion.WithKind(ClusterProviderConfigKind)
)

// ProviderConfigUsage type metadata.
var (
	ProviderConfigUsageKind             = reflect.TypeFor[ProviderConfigUsage]().Name()
	ProviderConfigUsageGroupVersionKind = SchemeGroupVersion.WithKind(ProviderConfigUsageKind)

	ProviderConfigUsageListKind             = reflect.TypeFor[ProviderConfigUsageList]().Name()
	ProviderConfigUsageListGroupVersionKind = SchemeGroupVersion.WithKind(ProviderConfigUsageListKind)
)

// Database type metadata.
var (
	DatabaseKind             = reflect.TypeFor[Database]().Name()
	DatabaseGroupKind        = schema.GroupKind{Group: Group, Kind: DatabaseKind}.String()
	DatabaseGroupVersionKind = SchemeGroupVersion.WithKind(DatabaseKind)
)

// User type metadata.
var (
	UserKind             = reflect.TypeFor[User]().Name()
	UserGroupKind        = schema.GroupKind{Group: Group, Kind: UserKind}.String()
	UserGroupVersionKind = SchemeGroupVersion.WithKind(UserKind)
)

// Role type metadata.
var (
	RoleKind             = reflect.TypeFor[Role]().Name()
	RoleGroupKind        = schema.GroupKind{Group: Group, Kind: RoleKind}.String()
	RoleGroupVersionKind = SchemeGroupVersion.WithKind(RoleKind)
)

// Grant type metadata.
var (
	GrantKind             = reflect.TypeFor[Grant]().Name()
	GrantGroupKind        = schema.GroupKind{Group: Group, Kind: GrantKind}.String()
	GrantGroupVersionKind = SchemeGroupVersion.WithKind(GrantKind)
)

// RoleGrant type metadata.
var (
	RoleGrantKind             = reflect.TypeFor[RoleGrant]().Name()
	RoleGrantGroupKind        = schema.GroupKind{Group: Group, Kind: RoleGrantKind}.String()
	RoleGrantGroupVersionKind = SchemeGroupVersion.WithKind(RoleGrantKind)
)

func init() {
	SchemeBuilder.Register(&ProviderConfig{}, &ProviderConfigList{})
	SchemeBuilder.Register(&ClusterProviderConfig{}, &ClusterProviderConfigList{})
	SchemeBuilder.Register(&ProviderConfigUsage{}, &ProviderConfigUsageList{})
	SchemeBuilder.Register(&Database{}, &DatabaseList{})
	SchemeBuilder.Register(&User{}, &UserList{})
	SchemeBuilder.Register(&Role{}, &RoleList{})
	SchemeBuilder.Register(&Grant{}, &GrantList{})
	SchemeBuilder.Register(&RoleGrant{}, &RoleGrantList{})
}
