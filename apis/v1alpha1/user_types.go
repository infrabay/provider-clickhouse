// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/crossplane/crossplane-runtime/v2/pkg/resource"
)

// Interface checks.
var (
	_ resource.ModernManaged = &User{}
	_ resource.ManagedList   = &UserList{}
)

// Default role policies.
const (
	DefaultRolesAll       = "All"
	DefaultRolesNone      = "None"
	DefaultRolesListed    = "Listed"
	DefaultRolesAllExcept = "AllExcept"
)

// UserParameters are the configurable fields of a User.
// +kubebuilder:validation:XValidation:rule="has(self.name) == has(oldSelf.name)",message="name cannot be added or removed after creation"
type UserParameters struct {
	// Name in ClickHouse, for example "svc@my-project.iam.gserviceaccount.com".
	// Defaults to the effective external name. Immutable. The provider never
	// splits it on '@' or '.'.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	// +kubebuilder:validation:Pattern=`^[^\x00-\x1F\x7F]+$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="name is immutable"
	Name *string `json:"name,omitempty"`

	// Authentication method of the user.
	Authentication UserAuthentication `json:"authentication"`

	// HostIP lists allowed client addresses (IP or CIDR; mask notation such as
	// 10.0.0.0/255.0.0.0 is accepted). With hostNames, this forms the HOST
	// clause. If both are empty the user may connect from ANY host. Hosts are
	// always enforced, never late-initialized. 0.0.0.0/0 or ::/0 together with
	// hostNames is invalid: the server would drop the names.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=256
	// +kubebuilder:validation:items:MaxLength=64
	HostIP []string `json:"hostIP,omitempty"`

	// HostNames lists exact client host names (HOST NAME). Compared
	// case-sensitively, as the server stores them.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=256
	// +kubebuilder:validation:items:MaxLength=255
	HostNames []string `json:"hostNames,omitempty"`

	// SettingsProfile is an existing settings profile to inherit. The profile
	// must already exist (users.xml profiles or SQL).
	//  - nil: not managed (late-initialized);
	//  - "": no profile (SETTINGS NONE);
	//  - "x": exactly profile x.
	// Setting a profile replaces all per-user settings elements.
	// +optional
	SettingsProfile *string `json:"settingsProfile,omitempty"`

	// DefaultDatabase is selected when a client does not name one.
	//  - nil: not managed (late-initialized);
	//  - "": DEFAULT DATABASE NONE;
	//  - "db": DEFAULT DATABASE `db`.
	// The server does not check that the database exists; a missing default
	// database makes logins fail, so pair it with a Database MR.
	// +optional
	DefaultDatabase *string `json:"defaultDatabase,omitempty"`

	// DefaultRoles selects which granted roles are active at login. nil means
	// not managed (late-initialized). Membership itself is managed with
	// RoleGrant.
	// +optional
	DefaultRoles *DefaultRoles `json:"defaultRoles,omitempty"`
}

// UserAuthentication selects exactly one authentication method.
// +kubebuilder:validation:XValidation:rule="has(self.password) != has(self.jwt)",message="set exactly one of password or jwt"
type UserAuthentication struct {
	// Password authentication. The password is stored as a salted SHA-256
	// hash and published only to the connection Secret.
	// +optional
	Password *PasswordAuthentication `json:"password,omitempty"`

	// JWT authentication. JWT users are supported only on servers that accept
	// IDENTIFIED WITH jwt (Altinity Antalya).
	// +optional
	JWT *JWTAuthentication `json:"jwt,omitempty"`
}

// PasswordAuthentication takes the password from a Secret or generates one.
// +kubebuilder:validation:XValidation:rule="has(self.passwordSecretRef) != has(self.generate)",message="set exactly one of passwordSecretRef or generate"
type PasswordAuthentication struct {
	// PasswordSecretRef selects a password from a Secret in the MR's
	// namespace. Changing the Secret's value changes the password.
	// +optional
	PasswordSecretRef *xpv2.LocalSecretKeySelector `json:"passwordSecretRef,omitempty"`

	// Generate a random password and publish it in the connection Secret.
	// +optional
	Generate *GeneratePassword `json:"generate,omitempty"`
}

// GeneratePassword configures a generated password.
type GeneratePassword struct {
	// Length in characters, drawn from [A-Za-z0-9] with crypto/rand.
	// +optional
	// +kubebuilder:default=32
	// +kubebuilder:validation:Minimum=16
	// +kubebuilder:validation:Maximum=128
	Length *int32 `json:"length,omitempty"`

	// RotationToken: set or change it to any new value to rotate the password.
	// The token that was last applied is stored in the connection Secret.
	// +optional
	// +kubebuilder:validation:MaxLength=63
	RotationToken *string `json:"rotationToken,omitempty"`
}

// JWTAuthentication pins a token processor and optional claims.
type JWTAuthentication struct {
	// Processor is the name of a token_processors entry. Required: without it
	// the server accepts a token from ANY configured processor whose username
	// claim equals the user name, which allows cross-issuer impersonation.
	// +kubebuilder:validation:MinLength=1
	Processor string `json:"processor"`

	// Claims is a JSON object the token must contain. The server matches it as
	// a subset; arrays match by inclusion, e.g. {groups: [ch-analysts]}
	// accepts a token whose groups contain ch-analysts. The provider
	// canonicalizes it (sorted keys, no insignificant whitespace) before
	// sending and comparing.
	// +optional
	// +kubebuilder:validation:Type=object
	// +kubebuilder:pruning:PreserveUnknownFields
	Claims *apiextensionsv1.JSON `json:"claims,omitempty"`
}

// DefaultRoles selects the roles that are active at login.
// +kubebuilder:validation:XValidation:rule="(self.policy in ['Listed', 'AllExcept']) == (has(self.roles) && size(self.roles) > 0)",message="roles is required for Listed and AllExcept and forbidden otherwise"
type DefaultRoles struct {
	// Policy: All, None, Listed or AllExcept.
	// +kubebuilder:validation:Enum=All;None;Listed;AllExcept
	// +kubebuilder:default=All
	Policy string `json:"policy"`

	// Roles for Listed and AllExcept.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=256
	// +kubebuilder:validation:items:MaxLength=255
	Roles []string `json:"roles,omitempty"`
}

// UserObservation are the observable fields of a User.
type UserObservation struct {
	// ID is the UUID from system.users.id. Informational; ownership is
	// recorded in annotations.
	// +optional
	ID string `json:"id,omitempty"`

	// Storage is local_directory, replicated or users_xml.
	// +optional
	Storage string `json:"storage,omitempty"`

	// AuthTypes, e.g. ["jwt"] or ["sha256_password"].
	// +optional
	AuthTypes []string `json:"authTypes,omitempty"`

	// JWTProcessor parsed from SHOW CREATE USER.
	// +optional
	JWTProcessor string `json:"jwtProcessor,omitempty"`

	// HostIP as stored by the server.
	// +optional
	HostIP []string `json:"hostIP,omitempty"`

	// HostNames as stored by the server.
	// +optional
	HostNames []string `json:"hostNames,omitempty"`

	// SettingsProfile inherited by the user.
	// +optional
	SettingsProfile string `json:"settingsProfile,omitempty"`

	// DefaultDatabase of the user.
	// +optional
	DefaultDatabase string `json:"defaultDatabase,omitempty"`

	// DefaultRoles rendered as "ALL", "NONE", "r1, r2" or "ALL EXCEPT r1".
	// +optional
	DefaultRoles string `json:"defaultRoles,omitempty"`
}

// A UserSpec defines the desired state of a User.
// +kubebuilder:validation:XValidation:rule="!has(self.forProvider.authentication.password) || has(self.writeConnectionSecretToRef)",message="password users require writeConnectionSecretToRef"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.forProvider.authentication.password) || !has(self.forProvider.authentication.password) || !has(oldSelf.writeConnectionSecretToRef) || (has(self.writeConnectionSecretToRef) && self.writeConnectionSecretToRef.name == oldSelf.writeConnectionSecretToRef.name)",message="writeConnectionSecretToRef.name is immutable for password users; rotate with generate.rotationToken"
type UserSpec struct {
	xpv2.ManagedResourceSpec `json:",inline"`
	ForProvider              UserParameters `json:"forProvider"`
}

// A UserStatus represents the observed state of a User.
type UserStatus struct {
	xpv2.ManagedResourceStatus `json:",inline"`
	AtProvider                 UserObservation `json:"atProvider,omitempty"`
}

// +kubebuilder:object:root=true

// A User is a ClickHouse SQL user with password or JWT authentication.
// +kubebuilder:printcolumn:name="READY",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="SYNCED",type="string",JSONPath=".status.conditions[?(@.type=='Synced')].status"
// +kubebuilder:printcolumn:name="EXTERNAL-NAME",type="string",JSONPath=".metadata.annotations.crossplane\\.io/external-name"
// +kubebuilder:printcolumn:name="AUTH",type="string",JSONPath=".status.atProvider.authTypes"
// +kubebuilder:printcolumn:name="STORAGE",type="string",JSONPath=".status.atProvider.storage",priority=1
// +kubebuilder:printcolumn:name="AGE",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,categories={crossplane,managed,clickhouse},shortName=chuser
type User struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   UserSpec   `json:"spec"`
	Status UserStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// UserList contains a list of User.
type UserList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []User `json:"items"`
}
