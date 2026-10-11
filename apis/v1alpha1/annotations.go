// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

// Ownership annotations. A claim is valid only when AnnotationOwnerUID equals
// the managed resource's current metadata.uid, so copied or restored manifests
// never take over an object by accident.
const (
	// AnnotationOwnerUID holds the metadata.uid of the MR that holds the claim.
	AnnotationOwnerUID = Group + "/owner-uid"

	// AnnotationOwnedID holds the UUID of the ClickHouse object the MR owns.
	AnnotationOwnedID = Group + "/owned-id"

	// AnnotationCreateOutcome is "rejected" or "unknown" after a failed
	// Create of a User or Role.
	AnnotationCreateOutcome = Group + "/create-outcome"

	// AnnotationAdopt is set by a human to the UUID of a pre-existing object
	// the MR should take over. The provider removes it after adoption.
	AnnotationAdopt = Group + "/adopt"
)

// Values of AnnotationCreateOutcome.
const (
	CreateOutcomeRejected = "rejected"
	CreateOutcomeUnknown  = "unknown"
)
