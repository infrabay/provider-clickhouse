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
	_ resource.ModernManaged = &RoleGrant{}
	_ resource.ManagedList   = &RoleGrantList{}
)

// RoleGrantParameters are the configurable fields of a RoleGrant. memberOf is
// the role being granted; user or role is the grantee.
// +kubebuilder:validation:XValidation:rule="has(self.memberOf) || has(self.memberOfRef) || has(self.memberOfSelector)",message="memberOf is required"
// +kubebuilder:validation:XValidation:rule="[has(self.user) || has(self.userRef) || has(self.userSelector), has(self.role) || has(self.roleRef) || has(self.roleSelector)].exists_one(x, x)",message="set exactly one grantee: user(Ref|Selector) or role(Ref|Selector)"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.memberOf) || (has(self.memberOf) && self.memberOf == oldSelf.memberOf)",message="memberOf is immutable"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.user) || (has(self.user) && self.user == oldSelf.user)",message="user is immutable"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.role) || (has(self.role) && self.role == oldSelf.role)",message="role is immutable"
// +kubebuilder:validation:XValidation:rule="!has(self.memberOfRef) || !has(self.memberOfRef.__namespace__) || self.memberOfRef.__namespace__.size() == 0",message="cross-namespace references are not allowed"
// +kubebuilder:validation:XValidation:rule="!has(self.memberOfSelector) || !has(self.memberOfSelector.__namespace__) || self.memberOfSelector.__namespace__.size() == 0",message="cross-namespace selectors are not allowed"
// +kubebuilder:validation:XValidation:rule="!has(self.userRef) || !has(self.userRef.__namespace__) || self.userRef.__namespace__.size() == 0",message="cross-namespace references are not allowed"
// +kubebuilder:validation:XValidation:rule="!has(self.userSelector) || !has(self.userSelector.__namespace__) || self.userSelector.__namespace__.size() == 0",message="cross-namespace selectors are not allowed"
// +kubebuilder:validation:XValidation:rule="!has(self.roleRef) || !has(self.roleRef.__namespace__) || self.roleRef.__namespace__.size() == 0",message="cross-namespace references are not allowed"
// +kubebuilder:validation:XValidation:rule="!has(self.roleSelector) || !has(self.roleSelector.__namespace__) || self.roleSelector.__namespace__.size() == 0",message="cross-namespace selectors are not allowed"
type RoleGrantParameters struct {
	// MemberOf is the role being granted; the grantee becomes a member of it.
	// It may be a Role MR or a role defined in users.xml/users.d (plain name).
	// It must match policy.grantableRoles on a ClusterProviderConfig.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	// +kubebuilder:validation:Pattern=`^[^\x00-\x1F\x7F]+$`
	// +crossplane:generate:reference:type=Role
	MemberOf *string `json:"memberOf,omitempty"`

	// MemberOfRef references a Role MR in the same namespace.
	// +optional
	MemberOfRef *xpv2.NamespacedReference `json:"memberOfRef,omitempty"`

	// MemberOfSelector selects a Role MR in the same namespace.
	// +optional
	MemberOfSelector *xpv2.NamespacedSelector `json:"memberOfSelector,omitempty"`

	// User grantee: an SQL user, not one from users.xml.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	// +kubebuilder:validation:Pattern=`^[^\x00-\x1F\x7F]+$`
	// +crossplane:generate:reference:type=User
	User *string `json:"user,omitempty"`

	// UserRef references a User MR in the same namespace.
	// +optional
	UserRef *xpv2.NamespacedReference `json:"userRef,omitempty"`

	// UserSelector selects a User MR in the same namespace.
	// +optional
	UserSelector *xpv2.NamespacedSelector `json:"userSelector,omitempty"`

	// Role grantee (role inheritance). No user with the same name may exist.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	// +kubebuilder:validation:Pattern=`^[^\x00-\x1F\x7F]+$`
	// +crossplane:generate:reference:type=Role
	Role *string `json:"role,omitempty"`

	// RoleRef references a Role MR in the same namespace.
	// +optional
	RoleRef *xpv2.NamespacedReference `json:"roleRef,omitempty"`

	// RoleSelector selects a Role MR in the same namespace.
	// +optional
	RoleSelector *xpv2.NamespacedSelector `json:"roleSelector,omitempty"`

	// AdminOption grants the role WITH ADMIN OPTION. Needs
	// policy.allowAdminOption on a ClusterProviderConfig.
	// +optional
	// +kubebuilder:default=false
	AdminOption *bool `json:"adminOption,omitempty"`
}

// RoleGrantObservation are the observable fields of a RoleGrant.
type RoleGrantObservation struct {
	// Grantee is "user:<name>" or "role:<name>".
	// +optional
	Grantee string `json:"grantee,omitempty"`

	// AdminOption as stored by the server.
	// +optional
	AdminOption bool `json:"adminOption,omitempty"`

	// IsDefault is granted_role_is_default, reported for information. Use
	// User.defaultRoles to change it.
	// +optional
	IsDefault bool `json:"isDefault,omitempty"`
}

// A RoleGrantSpec defines the desired state of a RoleGrant.
type RoleGrantSpec struct {
	xpv2.ManagedResourceSpec `json:",inline"`
	ForProvider              RoleGrantParameters `json:"forProvider"`
}

// A RoleGrantStatus represents the observed state of a RoleGrant.
type RoleGrantStatus struct {
	xpv2.ManagedResourceStatus `json:",inline"`
	AtProvider                 RoleGrantObservation `json:"atProvider,omitempty"`
}

// +kubebuilder:object:root=true

// A RoleGrant grants a role to a user or a role.
// +kubebuilder:printcolumn:name="READY",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="SYNCED",type="string",JSONPath=".status.conditions[?(@.type=='Synced')].status"
// +kubebuilder:printcolumn:name="MEMBER-OF",type="string",JSONPath=".spec.forProvider.memberOf"
// +kubebuilder:printcolumn:name="GRANTEE",type="string",JSONPath=".status.atProvider.grantee"
// +kubebuilder:printcolumn:name="AGE",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,categories={crossplane,managed,clickhouse},shortName=chrolegrant
type RoleGrant struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RoleGrantSpec   `json:"spec"`
	Status RoleGrantStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// RoleGrantList contains a list of RoleGrant.
type RoleGrantList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RoleGrant `json:"items"`
}
