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
	_ resource.ModernManaged = &Grant{}
	_ resource.ManagedList   = &GrantList{}
)

// GrantParameters are the configurable fields of a Grant. A Grant owns one
// exact target (*.*, db.*, db.table or db.table(columns)) for one grantee.
// +kubebuilder:validation:XValidation:rule="has(self.database) || has(self.databaseRef) || has(self.databaseSelector)",message="database is required (\"*\" for all databases)"
// +kubebuilder:validation:XValidation:rule="[has(self.user) || has(self.userRef) || has(self.userSelector), has(self.role) || has(self.roleRef) || has(self.roleSelector)].exists_one(x, x)",message="set exactly one grantee: user(Ref|Selector) or role(Ref|Selector)"
// +kubebuilder:validation:XValidation:rule="!has(self.columns) || (has(self.table) && self.table != '*')",message="columns require a specific table"
// +kubebuilder:validation:XValidation:rule="!has(self.table) || self.table == '*' || !has(self.database) || self.database != '*'",message="a specific table requires a specific database"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.database) || (has(self.database) && self.database == oldSelf.database)",message="database is immutable"
// +kubebuilder:validation:XValidation:rule="has(self.table) == has(oldSelf.table) && (!has(self.table) || self.table == oldSelf.table)",message="table is immutable"
// +kubebuilder:validation:XValidation:rule="has(self.columns) == has(oldSelf.columns) && (!has(self.columns) || self.columns == oldSelf.columns)",message="columns are immutable"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.user) || (has(self.user) && self.user == oldSelf.user)",message="user is immutable"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.role) || (has(self.role) && self.role == oldSelf.role)",message="role is immutable"
// +kubebuilder:validation:XValidation:rule="!has(self.databaseRef) || !has(self.databaseRef.__namespace__) || self.databaseRef.__namespace__.size() == 0",message="cross-namespace references are not allowed"
// +kubebuilder:validation:XValidation:rule="!has(self.databaseSelector) || !has(self.databaseSelector.__namespace__) || self.databaseSelector.__namespace__.size() == 0",message="cross-namespace selectors are not allowed"
// +kubebuilder:validation:XValidation:rule="!has(self.userRef) || !has(self.userRef.__namespace__) || self.userRef.__namespace__.size() == 0",message="cross-namespace references are not allowed"
// +kubebuilder:validation:XValidation:rule="!has(self.userSelector) || !has(self.userSelector.__namespace__) || self.userSelector.__namespace__.size() == 0",message="cross-namespace selectors are not allowed"
// +kubebuilder:validation:XValidation:rule="!has(self.roleRef) || !has(self.roleRef.__namespace__) || self.roleRef.__namespace__.size() == 0",message="cross-namespace references are not allowed"
// +kubebuilder:validation:XValidation:rule="!has(self.roleSelector) || !has(self.roleSelector.__namespace__) || self.roleSelector.__namespace__.size() == 0",message="cross-namespace selectors are not allowed"
type GrantParameters struct {
	// Privileges, case-insensitive. Names and aliases from system.privileges
	// are accepted, e.g. SELECT, INSERT, "ALTER UPDATE", dictGet, ALL. Source
	// privileges (S3, URL, REMOTE, FILE, ..., READ, WRITE, SOURCES) and
	// privileges of the levels NAMED_COLLECTION, USER_NAME, TABLE_ENGINE,
	// DEFINER and SOURCE are rejected in v1alpha1.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=128
	// +listType=set
	// +kubebuilder:validation:items:Pattern=`^[A-Za-z][A-Za-z0-9_ ]*$`
	// +kubebuilder:validation:items:MaxLength=64
	Privileges []string `json:"privileges"`

	// Database name, or "*" for a global grant (ON *.*, which needs
	// policy.allowGlobalGrants on a ClusterProviderConfig). Wildcard targets
	// such as "team_*" are not supported in v1alpha1. A '*' inside a name is
	// quoted and therefore literal.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	// +kubebuilder:validation:Pattern=`^[^\x00-\x1F\x7F]+$`
	// +crossplane:generate:reference:type=Database
	Database *string `json:"database,omitempty"`

	// DatabaseRef references a Database MR in the same namespace.
	// +optional
	DatabaseRef *xpv2.NamespacedReference `json:"databaseRef,omitempty"`

	// DatabaseSelector selects a Database MR in the same namespace.
	// +optional
	DatabaseSelector *xpv2.NamespacedSelector `json:"databaseSelector,omitempty"`

	// Table name, or "*" (the default) for all tables.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	// +kubebuilder:validation:Pattern=`^[^\x00-\x1F\x7F]+$`
	Table *string `json:"table,omitempty"`

	// Columns. Only valid with a specific table and column-level privileges.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=256
	// +kubebuilder:validation:items:MaxLength=255
	// +kubebuilder:validation:items:Pattern=`^[^\x00-\x1F\x7F]+$`
	Columns []string `json:"columns,omitempty"`

	// User grantee.
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

	// Role grantee. No user with the same name may exist: the server resolves
	// grantee names user-first (AmbiguousGrantee).
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

	// WithGrantOption grants the privileges WITH GRANT OPTION. Needs
	// policy.allowGrantOption on a ClusterProviderConfig.
	// +optional
	// +kubebuilder:default=false
	WithGrantOption *bool `json:"withGrantOption,omitempty"`
}

// GrantObservation are the observable fields of a Grant.
type GrantObservation struct {
	// Grantee is "user:<name>" or "role:<name>".
	// +optional
	Grantee string `json:"grantee,omitempty"`

	// Target rendered, e.g. "*.*", "app.*", "app.events(a, b)".
	// +optional
	Target string `json:"target,omitempty"`

	// Privileges stored at exactly this target, in the server's
	// group-compressed form, sorted. Informational only: the compressed form
	// differs between server versions and is never compared.
	// +optional
	Privileges []string `json:"privileges,omitempty"`
}

// A GrantSpec defines the desired state of a Grant.
type GrantSpec struct {
	xpv2.ManagedResourceSpec `json:",inline"`
	ForProvider              GrantParameters `json:"forProvider"`
}

// A GrantStatus represents the observed state of a Grant.
type GrantStatus struct {
	xpv2.ManagedResourceStatus `json:",inline"`
	AtProvider                 GrantObservation `json:"atProvider,omitempty"`
}

// +kubebuilder:object:root=true

// A Grant grants privileges on one target to one user or role.
// +kubebuilder:printcolumn:name="READY",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="SYNCED",type="string",JSONPath=".status.conditions[?(@.type=='Synced')].status"
// +kubebuilder:printcolumn:name="GRANTEE",type="string",JSONPath=".status.atProvider.grantee"
// +kubebuilder:printcolumn:name="TARGET",type="string",JSONPath=".status.atProvider.target"
// +kubebuilder:printcolumn:name="AGE",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,categories={crossplane,managed,clickhouse},shortName=chgrant
type Grant struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GrantSpec   `json:"spec"`
	Status GrantStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// GrantList contains a list of Grant.
type GrantList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Grant `json:"items"`
}
