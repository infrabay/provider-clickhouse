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
	_ resource.ProviderConfig           = &ProviderConfig{}
	_ resource.ProviderConfig           = &ClusterProviderConfig{}
	_ resource.TypedProviderConfigUsage = &ProviderConfigUsage{}
	_ resource.ProviderConfigUsageList  = &ProviderConfigUsageList{}
)

// Protocol is the wire protocol used to reach ClickHouse. Only the native
// protocol is supported in v1alpha1. The enum keeps its type so that HTTP can
// be added later without a breaking change.
// +kubebuilder:validation:Enum=Native
type Protocol string

// Protocols.
const (
	ProtocolNative Protocol = "Native"
)

// Endpoint is where the provider connects. Usually a Kubernetes Service.
type Endpoint struct {
	// Host name or IP address.
	// +kubebuilder:validation:MinLength=1
	Host string `json:"host"`

	// Port. Defaults to 9000, or 9440 with TLS.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port *int32 `json:"port,omitempty"`
}

// TLSCommon holds TLS settings that do not reference Secrets.
type TLSCommon struct {
	// Enabled turns on TLS. Required unless allowInsecureTransport is true.
	// +kubebuilder:default=false
	Enabled bool `json:"enabled"`

	// ServerName overrides the name used for SNI and certificate checks.
	// +optional
	ServerName string `json:"serverName,omitempty"`

	// InsecureSkipVerify disables certificate verification. Not allowed with
	// token credentials. Discouraged with passwords: the password then travels
	// to an unauthenticated peer. Use caCertSecretRef instead.
	// +optional
	InsecureSkipVerify bool `json:"insecureSkipVerify,omitempty"`
}

// LocalTLS is the TLS configuration of a namespaced ProviderConfig. Secrets are
// read from the ProviderConfig's namespace.
type LocalTLS struct {
	TLSCommon `json:",inline"`

	// CACertSecretRef selects a CA bundle (PEM). Defaults to the system roots.
	// +optional
	CACertSecretRef *xpv2.LocalSecretKeySelector `json:"caCertSecretRef,omitempty"`
}

// ClusterTLS is the TLS configuration of a ClusterProviderConfig.
type ClusterTLS struct {
	TLSCommon `json:",inline"`

	// CACertSecretRef selects a CA bundle (PEM). Defaults to the system roots.
	// +optional
	CACertSecretRef *xpv2.SecretKeySelector `json:"caCertSecretRef,omitempty"`
}

// Pool limits the provider's connection pool to this server. Dial and read
// timeouts (5 s, 60 s) and idle connections (maxOpenConns/2) are internal
// defaults, not API.
type Pool struct {
	// MaxOpenConns bounds the connections the provider opens to this server.
	// +optional
	// +kubebuilder:default=4
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=64
	MaxOpenConns *int32 `json:"maxOpenConns,omitempty"`

	// ConnMaxLifetime bounds how long a pooled connection is reused. Native
	// connections are authenticated only at handshake, so this also bounds how
	// long a revoked or expired provider token keeps working.
	// +optional
	// +kubebuilder:default="30m"
	ConnMaxLifetime *metav1.Duration `json:"connMaxLifetime,omitempty"`
}

// ConnectionSpec is shared by both ProviderConfig kinds.
type ConnectionSpec struct {
	// Endpoint of the ClickHouse server.
	Endpoint Endpoint `json:"endpoint"`

	// Protocol to use. Only Native is supported in v1alpha1.
	// +optional
	// +kubebuilder:default=Native
	Protocol Protocol `json:"protocol,omitempty"`

	// DefaultDatabase is the database selected for the provider's session.
	// +optional
	// +kubebuilder:default=default
	DefaultDatabase string `json:"defaultDatabase,omitempty"`

	// Pool configures the provider's connection pool.
	// +optional
	Pool *Pool `json:"pool,omitempty"`

	// Cluster names a cluster from <remote_servers>. When set, Database DDL
	// uses ON CLUSTER <cluster>. Before every ON CLUSTER ALTER or DROP the
	// provider reads the database's UUID on every host of the cluster
	// (clusterAllReplicas, which needs the REMOTE privilege) and refuses the
	// write if any host has a database of that name with another UUID.
	// Deleting a Database then always needs forceDrop. The provider refuses
	// to write access entities unless they are stored in a replicated user
	// directory. Access entities never use ON CLUSTER.
	// +optional
	// +kubebuilder:validation:MinLength=1
	Cluster *string `json:"cluster,omitempty"`

	// AllowInsecureTransport permits a connection without TLS. The native
	// protocol then sends the provider's password or token in clear text. Only
	// for lab setups or traffic that is encrypted below ClickHouse.
	// +optional
	// +kubebuilder:default=false
	AllowInsecureTransport bool `json:"allowInsecureTransport,omitempty"`
}

// CredentialsSource selects how the provider authenticates to ClickHouse.
// +kubebuilder:validation:Enum=PasswordSecret;TokenSecret;GoogleIDToken;TokenFile
type CredentialsSource string

// Credentials sources.
const (
	CredentialsSourcePasswordSecret CredentialsSource = "PasswordSecret"
	CredentialsSourceTokenSecret    CredentialsSource = "TokenSecret"
	CredentialsSourceGoogleIDToken  CredentialsSource = "GoogleIDToken"
	CredentialsSourceTokenFile      CredentialsSource = "TokenFile"
)

// LocalCredentials are the credentials of a namespaced ProviderConfig. Ambient
// identities (TokenFile, GoogleIDToken without a key) are not available here.
// +kubebuilder:validation:XValidation:rule="(self.source == 'PasswordSecret') == has(self.passwordSecret) && (self.source == 'TokenSecret') == has(self.tokenSecret) && (self.source == 'GoogleIDToken') == has(self.googleIDToken)",message="set exactly the block that matches source"
// +kubebuilder:validation:XValidation:rule="self.source != 'TokenFile'",message="TokenFile is only allowed on ClusterProviderConfig"
type LocalCredentials struct {
	// Source of the credentials.
	Source CredentialsSource `json:"source"`

	// PasswordSecret authenticates with a user name and a password.
	// +optional
	PasswordSecret *LocalPasswordCredentials `json:"passwordSecret,omitempty"`

	// TokenSecret authenticates with a JWT stored in a Secret.
	// +optional
	TokenSecret *LocalTokenSecretCredentials `json:"tokenSecret,omitempty"`

	// GoogleIDToken authenticates with a Google ID token. On a namespaced
	// ProviderConfig a credentials Secret is mandatory: ambient Google identity
	// is not available to tenants.
	// +optional
	GoogleIDToken *LocalGoogleIDTokenCredentials `json:"googleIDToken,omitempty"`
}

// LocalPasswordCredentials is a user name and a password Secret in the
// ProviderConfig's namespace.
type LocalPasswordCredentials struct {
	// Username of the provider identity.
	// +kubebuilder:validation:MinLength=1
	Username string `json:"username"`

	// PasswordSecretRef selects the password.
	PasswordSecretRef xpv2.LocalSecretKeySelector `json:"passwordSecretRef"`
}

// LocalTokenSecretCredentials is a JWT stored in a Secret in the
// ProviderConfig's namespace.
type LocalTokenSecretCredentials struct {
	// SecretRef selects the Secret key that holds a JWT. It is re-read
	// (uncached) on every new connection, so external rotation needs no
	// restart.
	SecretRef xpv2.LocalSecretKeySelector `json:"secretRef"`

	// Audience the token must carry in its aud claim. Checked before the token
	// is sent.
	// +kubebuilder:validation:MinLength=1
	Audience string `json:"audience"`
}

// LocalGoogleIDTokenCredentials requests Google ID tokens with a service
// account key from a Secret in the ProviderConfig's namespace.
type LocalGoogleIDTokenCredentials struct {
	// Audience requested in the ID token. It must equal expected_audience of
	// the server's token processor. Use a value unique to this ClickHouse
	// deployment, for example its URL.
	// +kubebuilder:validation:MinLength=1
	Audience string `json:"audience"`

	// CredentialsSecretRef selects a service account key JSON. It is validated
	// before use.
	CredentialsSecretRef xpv2.LocalSecretKeySelector `json:"credentialsSecretRef"`
}

// ClusterCredentials are the credentials of a ClusterProviderConfig.
// +kubebuilder:validation:XValidation:rule="(self.source == 'PasswordSecret') == has(self.passwordSecret) && (self.source == 'TokenSecret') == has(self.tokenSecret) && (self.source == 'GoogleIDToken') == has(self.googleIDToken) && (self.source == 'TokenFile') == has(self.tokenFile)",message="set exactly the block that matches source"
type ClusterCredentials struct {
	// Source of the credentials.
	Source CredentialsSource `json:"source"`

	// PasswordSecret authenticates with a user name and a password.
	// +optional
	PasswordSecret *ClusterPasswordCredentials `json:"passwordSecret,omitempty"`

	// TokenSecret authenticates with a JWT stored in a Secret.
	// +optional
	TokenSecret *ClusterTokenSecretCredentials `json:"tokenSecret,omitempty"`

	// GoogleIDToken authenticates with a Google ID token.
	// +optional
	GoogleIDToken *ClusterGoogleIDTokenCredentials `json:"googleIDToken,omitempty"`

	// TokenFile authenticates with a JWT read from a file in the provider
	// container, for example a projected service account token.
	// +optional
	TokenFile *TokenFileCredentials `json:"tokenFile,omitempty"`
}

// ClusterPasswordCredentials is a user name and a password Secret.
type ClusterPasswordCredentials struct {
	// Username of the provider identity.
	// +kubebuilder:validation:MinLength=1
	Username string `json:"username"`

	// PasswordSecretRef selects the password.
	PasswordSecretRef xpv2.SecretKeySelector `json:"passwordSecretRef"`
}

// ClusterTokenSecretCredentials is a JWT stored in a Secret.
type ClusterTokenSecretCredentials struct {
	// SecretRef selects the Secret key that holds a JWT. It is re-read
	// (uncached) on every new connection.
	SecretRef xpv2.SecretKeySelector `json:"secretRef"`

	// Audience the token must carry in its aud claim. Checked before the token
	// is sent.
	// +kubebuilder:validation:MinLength=1
	Audience string `json:"audience"`
}

// ClusterGoogleIDTokenCredentials requests Google ID tokens.
type ClusterGoogleIDTokenCredentials struct {
	// Audience requested in the ID token. It must equal expected_audience of
	// the server's token processor. Use a value unique to this ClickHouse
	// deployment, for example its URL.
	// +kubebuilder:validation:MinLength=1
	Audience string `json:"audience"`

	// CredentialsSecretRef optionally selects a service account key JSON. When
	// omitted, Application Default Credentials are used: on GKE with Workload
	// Identity, the metadata server.
	// +optional
	CredentialsSecretRef *xpv2.SecretKeySelector `json:"credentialsSecretRef,omitempty"`
}

// TokenFileCredentials reads a JWT from a file.
type TokenFileCredentials struct {
	// Path is an absolute path inside the provider container, for example a
	// projected service account token mounted through a
	// DeploymentRuntimeConfig. The file is re-read on every new connection.
	// Paths under /var/run/secrets/kubernetes.io/serviceaccount/ are refused:
	// that is the provider's own Kubernetes API credential.
	// +kubebuilder:validation:Pattern=`^/`
	Path string `json:"path"`

	// Audience the token must carry in its aud claim, for example the audience
	// of the projected volume ("clickhouse"). Checked before the token is
	// sent.
	// +kubebuilder:validation:MinLength=1
	Audience string `json:"audience"`
}

// AuthenticationMethod of a managed User.
// +kubebuilder:validation:Enum=Password;JWT
type AuthenticationMethod string

// Authentication methods.
const (
	AuthenticationMethodPassword AuthenticationMethod = "Password"
	AuthenticationMethodJWT      AuthenticationMethod = "JWT"
)

// ClusterPolicy limits what managed resources may do through a
// ClusterProviderConfig. It is closed by default: an empty or absent list
// allows nothing, and every boolean defaults to false. Write [".*"] (or set
// unrestricted) to allow everything.
//
// Pattern semantics (allowedNamespaces and every *Patterns / grantableRoles
// entry):
//   - RE2, implicitly anchored (^(?:p)$);
//   - "{namespace}" is replaced with the MR's namespace, regexp-quoted;
//   - "{namespace}" may appear at most once. The text before it must be a
//     plain literal (no regexp operators), and every match of the regexp
//     after it must start with a literal character that cannot occur in a
//     namespace name (anything but a-z, 0-9 and '-'), in every alternative,
//     for example _ @ \. : /
//   - "{namespace}" is not allowed in allowedNamespaces.
//
// A pattern that breaks these rules makes the ClusterProviderConfig invalid.
//
// +kubebuilder:validation:XValidation:rule="!(has(self.unrestricted) && self.unrestricted) || [has(self.allowedNamespaces), has(self.userNamePatterns), has(self.roleNamePatterns), has(self.databaseNamePatterns), has(self.zooKeeperPathPatterns), has(self.grantableRoles), has(self.allowedAuthentication), has(self.allowedJWTProcessors)].all(x, !x)",message="unrestricted cannot be combined with restriction lists"
type ClusterPolicy struct {
	// Unrestricted allows every namespace, name, role, authentication method
	// and processor, global grants, grant option and admin option. Meant for
	// single-tenant installs. Adoption still needs allowAdoption.
	// +optional
	Unrestricted bool `json:"unrestricted,omitempty"`

	// AllowedNamespaces lists the namespaces whose MRs may use this
	// ClusterProviderConfig. "{namespace}" is not allowed here: it would
	// match every namespace.
	// +optional
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MaxLength=253
	// +kubebuilder:validation:XValidation:rule="self.all(x, !x.contains('{namespace}'))",message="{namespace} is not allowed in allowedNamespaces"
	AllowedNamespaces []string `json:"allowedNamespaces,omitempty"`

	// UserNamePatterns lists the names that User may create, manage or
	// observe. Also applies to user grantees of Grant and RoleGrant.
	// +optional
	UserNamePatterns []string `json:"userNamePatterns,omitempty"`

	// RoleNamePatterns lists the names that Role may create, manage or
	// observe. Also applies to role grantees of Grant and RoleGrant.
	// +optional
	RoleNamePatterns []string `json:"roleNamePatterns,omitempty"`

	// DatabaseNamePatterns lists the names that Database may create, manage or
	// observe, and the databases a Grant may target.
	// +optional
	DatabaseNamePatterns []string `json:"databaseNamePatterns,omitempty"`

	// ZooKeeperPathPatterns lists the Keeper paths that a Replicated Database
	// may use besides the default "/clickhouse/databases/{uuid}", which is
	// unique per database UUID and always allowed. Any other path is refused:
	// a tenant could otherwise register a replica under another tenant's
	// Replicated database, which stalls that database's DDL and can copy its
	// tables and data. Anchor custom paths to the namespace, for example
	// "/clickhouse/databases/{namespace}/.+". ClickHouse macros such as {uuid}
	// are compared literally.
	// +optional
	// +kubebuilder:validation:MaxItems=64
	ZooKeeperPathPatterns []string `json:"zooKeeperPathPatterns,omitempty"`

	// GrantableRoles lists the roles that RoleGrant may grant (memberOf) and
	// that User.defaultRoles may list.
	// +optional
	GrantableRoles []string `json:"grantableRoles,omitempty"`

	// AllowedAuthentication lists the authentication methods User may use.
	// +optional
	// +listType=set
	AllowedAuthentication []AuthenticationMethod `json:"allowedAuthentication,omitempty"`

	// AllowedJWTProcessors lists the exact token processor names that jwt
	// Users may pin. The server accepts unknown processors at DDL time, and
	// those users then cannot log in.
	// +optional
	// +listType=set
	AllowedJWTProcessors []string `json:"allowedJWTProcessors,omitempty"`

	// AllowGlobalGrants lets Grant use database "*" (ON *.*).
	// +optional
	AllowGlobalGrants bool `json:"allowGlobalGrants,omitempty"`

	// AllowGrantOption lets Grant set withGrantOption. A grantee holding a
	// grant option can grant and revoke outside the provider.
	// +optional
	AllowGrantOption bool `json:"allowGrantOption,omitempty"`

	// AllowAdminOption lets RoleGrant set adminOption, with the same caveat.
	// +optional
	AllowAdminOption bool `json:"allowAdminOption,omitempty"`

	// AllowAdoption lets MRs take over pre-existing objects with the
	// clickhouse.m.infrabay.io/adopt annotation.
	// +optional
	AllowAdoption bool `json:"allowAdoption,omitempty"`
}

// ProviderConfigSpec configures a namespaced ProviderConfig. It is owned by
// the namespace tenant together with its credentials and carries no policy.
// +kubebuilder:validation:XValidation:rule="(has(self.tls) && self.tls.enabled) || (has(self.allowInsecureTransport) && self.allowInsecureTransport)",message="tls.enabled must be true unless allowInsecureTransport is true: the native protocol sends the password or token in clear text"
// +kubebuilder:validation:XValidation:rule="self.credentials.source == 'PasswordSecret' || !has(self.tls) || !has(self.tls.insecureSkipVerify) || !self.tls.insecureSkipVerify",message="insecureSkipVerify is not allowed with token credentials"
type ProviderConfigSpec struct {
	ConnectionSpec `json:",inline"`

	// TLS configures transport encryption.
	// +optional
	TLS *LocalTLS `json:"tls,omitempty"`

	// Credentials of the provider identity.
	Credentials LocalCredentials `json:"credentials"`
}

// ClusterProviderConfigSpec configures a ClusterProviderConfig.
// +kubebuilder:validation:XValidation:rule="(has(self.tls) && self.tls.enabled) || (has(self.allowInsecureTransport) && self.allowInsecureTransport)",message="tls.enabled must be true unless allowInsecureTransport is true: the native protocol sends the password or token in clear text"
// +kubebuilder:validation:XValidation:rule="self.credentials.source == 'PasswordSecret' || !has(self.tls) || !has(self.tls.insecureSkipVerify) || !self.tls.insecureSkipVerify",message="insecureSkipVerify is not allowed with token credentials"
type ClusterProviderConfigSpec struct {
	ConnectionSpec `json:",inline"`

	// TLS configures transport encryption.
	// +optional
	TLS *ClusterTLS `json:"tls,omitempty"`

	// Credentials of the provider identity.
	Credentials ClusterCredentials `json:"credentials"`

	// Policy limits what managed resources may do through this config.
	// Required. Write {unrestricted: true} for a single-tenant install.
	Policy ClusterPolicy `json:"policy"`
}

// A ProviderConfigStatus is the status of a ProviderConfig.
type ProviderConfigStatus struct {
	xpv2.ProviderConfigStatus `json:",inline"`
}

// +kubebuilder:object:root=true
// +kubebuilder:storageversion

// A ProviderConfig configures how the provider connects to a ClickHouse server
// for managed resources in its own namespace.
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,categories={crossplane,provider,clickhouse},shortName=chpc
// +kubebuilder:printcolumn:name="ENDPOINT",type="string",JSONPath=".spec.endpoint.host"
// +kubebuilder:printcolumn:name="SOURCE",type="string",JSONPath=".spec.credentials.source"
// +kubebuilder:printcolumn:name="AGE",type="date",JSONPath=".metadata.creationTimestamp"
type ProviderConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ProviderConfigSpec   `json:"spec"`
	Status ProviderConfigStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ProviderConfigList contains a list of ProviderConfig.
type ProviderConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ProviderConfig `json:"items"`
}

// +kubebuilder:object:root=true
// +kubebuilder:storageversion

// A ClusterProviderConfig configures how the provider connects to a ClickHouse
// server, and limits what managed resources may do through it.
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,categories={crossplane,provider,clickhouse},shortName=chcpc
// +kubebuilder:printcolumn:name="ENDPOINT",type="string",JSONPath=".spec.endpoint.host"
// +kubebuilder:printcolumn:name="SOURCE",type="string",JSONPath=".spec.credentials.source"
// +kubebuilder:printcolumn:name="AGE",type="date",JSONPath=".metadata.creationTimestamp"
type ClusterProviderConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ClusterProviderConfigSpec `json:"spec"`
	Status ProviderConfigStatus      `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ClusterProviderConfigList contains a list of ClusterProviderConfig.
type ClusterProviderConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClusterProviderConfig `json:"items"`
}

// +kubebuilder:object:root=true
// +kubebuilder:storageversion

// A ProviderConfigUsage records that a managed resource uses a ProviderConfig
// or a ClusterProviderConfig. There is one namespaced usage type for both
// config kinds; it lives in the namespace of the managed resource and records
// which kind of config it refers to.
// +kubebuilder:resource:scope=Namespaced,categories={crossplane,provider,clickhouse}
// +kubebuilder:printcolumn:name="CONFIG-KIND",type="string",JSONPath=".providerConfigRef.kind"
// +kubebuilder:printcolumn:name="CONFIG-NAME",type="string",JSONPath=".providerConfigRef.name"
// +kubebuilder:printcolumn:name="RESOURCE-KIND",type="string",JSONPath=".resourceRef.kind"
// +kubebuilder:printcolumn:name="RESOURCE-NAME",type="string",JSONPath=".resourceRef.name"
// +kubebuilder:printcolumn:name="AGE",type="date",JSONPath=".metadata.creationTimestamp"
type ProviderConfigUsage struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	xpv2.TypedProviderConfigUsage `json:",inline"`
}

// +kubebuilder:object:root=true

// ProviderConfigUsageList contains a list of ProviderConfigUsage.
type ProviderConfigUsageList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ProviderConfigUsage `json:"items"`
}
