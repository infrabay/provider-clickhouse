// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/crossplane/crossplane-runtime/v2/pkg/resource"
)

// Interface checks.
var (
	_ resource.ModernManaged = &Role{}
	_ resource.ManagedList   = &RoleList{}
)

// RoleParameters are the configurable fields of a Role.
// +kubebuilder:validation:XValidation:rule="has(self.name) == has(oldSelf.name)",message="name cannot be added or removed after creation"
type RoleParameters struct {
	// Name in ClickHouse. Defaults to the effective external name. Immutable.
	// It must not collide with a role from users.xml/users.d (the server
	// returns 495 ACCESS_STORAGE_READONLY), and no user with the same name may
	// exist (NameCollision).
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	// +kubebuilder:validation:Pattern=`^[^\x00-\x1F\x7F]+$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="name is immutable"
	Name *string `json:"name,omitempty"`

	// Role settings (SETTINGS ...) are planned for a later version.
}

// RoleObservation are the observable fields of a Role.
type RoleObservation struct {
	// ID is the UUID from system.roles.id. Informational.
	// +optional
	ID string `json:"id,omitempty"`

	// Storage is local_directory, replicated or users_xml.
	// +optional
	Storage string `json:"storage,omitempty"`
}

// A RoleSpec defines the desired state of a Role.
type RoleSpec struct {
	xpv2.ManagedResourceSpec `json:",inline"`
	ForProvider              RoleParameters `json:"forProvider"`
}

// A RoleStatus represents the observed state of a Role.
type RoleStatus struct {
	xpv2.ManagedResourceStatus `json:",inline"`
	AtProvider                 RoleObservation `json:"atProvider,omitempty"`
}

// +kubebuilder:object:root=true

// A Role is a ClickHouse SQL role.
// +kubebuilder:printcolumn:name="READY",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="SYNCED",type="string",JSONPath=".status.conditions[?(@.type=='Synced')].status"
// +kubebuilder:printcolumn:name="EXTERNAL-NAME",type="string",JSONPath=".metadata.annotations.crossplane\\.io/external-name"
// +kubebuilder:printcolumn:name="AGE",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,categories={crossplane,managed,clickhouse},shortName=chrole
type Role struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RoleSpec   `json:"spec"`
	Status RoleStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// RoleList contains a list of Role.
type RoleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Role `json:"items"`
}
