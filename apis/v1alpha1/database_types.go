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
	_ resource.ModernManaged = &Database{}
	_ resource.ManagedList   = &DatabaseList{}
)

// Database engines.
const (
	DatabaseEngineAtomic     = "Atomic"
	DatabaseEngineReplicated = "Replicated"
)

// Default arguments of the Replicated engine. DefaultZooKeeperPath is unique
// per database UUID; a ClusterProviderConfig policy allows other paths only
// through zooKeeperPathPatterns.
const (
	DefaultZooKeeperPath = "/clickhouse/databases/{uuid}"
	DefaultShardName     = "{shard}"
	DefaultReplicaName   = "{replica}"
)

// DatabaseParameters are the configurable fields of a Database.
// +kubebuilder:validation:XValidation:rule="has(self.name) == has(oldSelf.name)",message="name cannot be added or removed after creation"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.engine) || has(self.engine)",message="engine cannot be removed"
type DatabaseParameters struct {
	// Name in ClickHouse. Defaults to the effective external name. Immutable.
	// The server stores the name in file names, escaped (non-alphanumeric
	// bytes become %XX); the escaped form plus ".sql.tmp" must fit in 255
	// bytes. The provider checks this before creating the database.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	// +kubebuilder:validation:Pattern=`^[^\x00-\x1F\x7F]+$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="name is immutable"
	// +kubebuilder:validation:XValidation:rule="self != '*'",message="'*' is not a valid database name"
	Name *string `json:"name,omitempty"`

	// Engine. Immutable. When omitted, the ENGINE clause is omitted, the server
	// default (Atomic) is used, and the engine is late-initialized.
	// +optional
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="engine is immutable"
	Engine *DatabaseEngine `json:"engine,omitempty"`

	// Comment. An empty string clears the comment. Changing it needs
	// ALTER DATABASE ... MODIFY COMMENT, which upstream 25.3 lacks: there a
	// change is reported as Unsupported.
	// +optional
	Comment *string `json:"comment,omitempty"`

	// ForceDrop lets Delete drop a database that still has tables, or whose
	// emptiness cannot be verified. With a cluster (spec.cluster on the
	// config), emptiness can never be verified on every host, so Delete
	// always needs forceDrop there. Default false: Delete refuses and the MR
	// stays in deletion until fixed.
	// +optional
	ForceDrop *bool `json:"forceDrop,omitempty"`
}

// DatabaseEngine is a closed set of engines with structured arguments. It is
// never a raw SQL fragment.
// +kubebuilder:validation:XValidation:rule="self.name == 'Replicated' || !has(self.replicated)",message="replicated is only valid with name: Replicated"
type DatabaseEngine struct {
	// Name of the engine.
	// +kubebuilder:validation:Enum=Atomic;Replicated
	Name string `json:"name"`

	// Replicated holds the arguments of the Replicated engine.
	// +optional
	Replicated *ReplicatedEngine `json:"replicated,omitempty"`
}

// ReplicatedEngine holds the arguments of the Replicated database engine.
// Every value is sent as a quoted string literal. Macros such as {uuid},
// {shard} and {replica} are expanded by the server when the database is
// attached. The {uuid} macro works because the provider always sends an
// explicit UUID. system.databases.engine_full keeps the arguments exactly as
// written, so they are compared verbatim.
type ReplicatedEngine struct {
	// ZooKeeperPath is the Keeper path of the database. On a
	// ClusterProviderConfig that is not unrestricted, only the default and
	// paths that match policy.zooKeeperPathPatterns are allowed.
	// +optional
	// +kubebuilder:default="/clickhouse/databases/{uuid}"
	ZooKeeperPath string `json:"zooKeeperPath,omitempty"`

	// ShardName is the shard name.
	// +optional
	// +kubebuilder:default="{shard}"
	ShardName string `json:"shardName,omitempty"`

	// ReplicaName is the replica name.
	// +optional
	// +kubebuilder:default="{replica}"
	ReplicaName string `json:"replicaName,omitempty"`
}

// DatabaseObservation are the observable fields of a Database.
type DatabaseObservation struct {
	// ID is the UUID from system.databases.uuid. Informational; ownership is
	// recorded in annotations.
	// +optional
	ID string `json:"id,omitempty"`

	// Engine from system.databases.engine.
	// +optional
	Engine string `json:"engine,omitempty"`

	// EngineFull from system.databases.engine_full.
	// +optional
	EngineFull string `json:"engineFull,omitempty"`

	// Comment from system.databases.comment.
	// +optional
	Comment string `json:"comment,omitempty"`
}

// A DatabaseSpec defines the desired state of a Database.
type DatabaseSpec struct {
	xpv2.ManagedResourceSpec `json:",inline"`
	ForProvider              DatabaseParameters `json:"forProvider"`
}

// A DatabaseStatus represents the observed state of a Database.
type DatabaseStatus struct {
	xpv2.ManagedResourceStatus `json:",inline"`
	AtProvider                 DatabaseObservation `json:"atProvider,omitempty"`
}

// +kubebuilder:object:root=true

// A Database is a ClickHouse database.
// +kubebuilder:printcolumn:name="READY",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="SYNCED",type="string",JSONPath=".status.conditions[?(@.type=='Synced')].status"
// +kubebuilder:printcolumn:name="EXTERNAL-NAME",type="string",JSONPath=".metadata.annotations.crossplane\\.io/external-name"
// +kubebuilder:printcolumn:name="ENGINE",type="string",JSONPath=".status.atProvider.engine"
// +kubebuilder:printcolumn:name="AGE",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,categories={crossplane,managed,clickhouse},shortName=chdb
type Database struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DatabaseSpec   `json:"spec"`
	Status DatabaseStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// DatabaseList contains a list of Database.
type DatabaseList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Database `json:"items"`
}
