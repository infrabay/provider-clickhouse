// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

// Package access holds what the User, Role, Grant and RoleGrant controllers
// share: reading access entities, the ownership flow of design 4.2 for
// users and roles (including the hardened Recovered rule), the grantee
// binding of grants and role grants, the topology guard of section 8, the
// connector of section 5.9 and the controller wiring.
//
// The provider identity can create, alter and drop users and roles and hand
// out roles and privileges. Everything here exists so that it never alters,
// takes over, grants to, revokes from or drops an access entity that it did
// not create or adopt by UUID, never writes an entity from users.xml or
// users.d, never modifies its own identity, and never writes for an MR whose
// names the ClusterProviderConfig policy does not allow, including while the
// MR is being deleted.
package access

import (
	"context"
	"fmt"

	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/crossplane/crossplane-runtime/v2/pkg/event"
	"github.com/crossplane/crossplane-runtime/v2/pkg/reconciler/managed"
	"github.com/crossplane/crossplane-runtime/v2/pkg/resource"

	"github.com/infrabay/provider-clickhouse/apis/v1alpha1"
	ch "github.com/infrabay/provider-clickhouse/internal/clients/clickhouse"
	"github.com/infrabay/provider-clickhouse/internal/controller/ownership"
	"github.com/infrabay/provider-clickhouse/internal/controller/providerconfig"
)

// Event reasons shared by the access entity kinds (design 5.2).
const (
	// ReasonAdopted: the MR took over an existing entity by UUID.
	ReasonAdopted event.Reason = "Adopted"
	// ReasonCreateRecovered: a create whose response was lost was recognised.
	ReasonCreateRecovered event.Reason = "CreateRecovered"
	// ReasonOrphaned: a deleting MR whose object is not its own (another
	// UUID, an unresolvable name, the provider's own identity, users.xml, an
	// ambiguous or unowned grantee, or no Delete management policy). Nothing
	// is written; the object is left in place.
	ReasonOrphaned event.Reason = "Orphaned"
	// ReasonOrphanedByPolicy: a deleting MR whose namespace or names the
	// ClusterProviderConfig policy (or the provider flags) no longer allow.
	// Nothing is read or written; the object is left in place.
	ReasonOrphanedByPolicy event.Reason = "OrphanedByPolicy"
)

// An Env is what the external client of an access entity kind holds for one
// reconcile. The Connector builds it. Embed *Env in the kind's external
// client: Disconnect releases the ClickHouse client.
type Env struct {
	// Client is the ClickHouse client of the MR's ProviderConfig.
	Client ch.Client
	// Resolved is the resolved ProviderConfig: its policy (nil for a
	// namespaced ProviderConfig), adoption, cluster and endpoint.
	Resolved *providerconfig.Resolved
	// Kube persists ownership annotations written in Observe (adoption and
	// recovery).
	Kube client.Client
	// Reader lists User and Role MRs to find the owner of a grantee. The
	// manager's cached client is fine.
	Reader client.Reader
	// APIReader is the uncached API reader. The User controller reads the
	// connection Secret of a password user with it (design 5.4): right after
	// Create published the Secret, a stale cache would report it missing.
	APIReader client.Reader
	// Recorder emits events.
	Recorder event.Recorder
	// Journal records the creates of this process for the Recovered rule;
	// nil means ownership.DefaultJournal.
	Journal *ownership.Journal
}

func (e *Env) journal() *ownership.Journal {
	if e.Journal == nil {
		return ownership.DefaultJournal
	}
	return e.Journal
}

func (e *Env) recorder() event.Recorder {
	if e.Recorder == nil {
		return event.NewNopRecorder()
	}
	return e.Recorder
}

// closed is the policy of an Env without a resolved config: a programming
// error, which must fail closed rather than pass every check.
var closed = func() *providerconfig.Policy {
	p, err := providerconfig.CompilePolicy(v1alpha1.ClusterPolicy{})
	if err != nil {
		panic(err)
	}
	return p
}()

// Policy returns the ClusterProviderConfig policy, or nil for a namespaced
// ProviderConfig, where every policy check passes. Without a resolved config
// it returns a closed policy.
func (e *Env) Policy() *providerconfig.Policy {
	if e.Resolved == nil {
		return closed
	}
	return e.Resolved.Policy
}

// CurrentUser returns the provider identity's user name on the server.
func (e *Env) CurrentUser() string { return e.Client.Info().CurrentUser }

// Disconnect releases the ClickHouse client.
func (e *Env) Disconnect(_ context.Context) error { return e.Client.Close() }

// LeaveInPlace emits a Warning event for a deleting MR whose object is left
// in place, and returns the observation that releases the MR: the object is
// reported as absent and no SQL is written.
func (e *Env) LeaveInPlace(mg resource.Managed, r event.Reason, err error) managed.ExternalObservation {
	e.recorder().Event(mg, event.Warning(r, err))
	return managed.ExternalObservation{ResourceExists: false}
}

func (e *Env) createKey(kind EntityKind, mg resource.Managed, name string) ownership.CreateKey {
	scope := e.Client.Info().ServerUUID
	if e.Resolved != nil {
		scope = e.Resolved.CacheKey.Identity + "|" + scope
	}
	return ownership.CreateKey{Kind: string(kind), UID: string(mg.GetUID()), Name: name, Scope: scope}
}

// SQLContext returns a context whose statements carry log_comment =
// "<kind>/<namespace>/<name>", so that every statement in system.query_log is
// attributable to the MR (design 5.1).
func SQLContext(ctx context.Context, kind string, mg resource.Managed) context.Context {
	return ch.WithLogComment(ctx, kind+"/"+mg.GetNamespace()+"/"+mg.GetName())
}

// Unavailable sets Ready=False with err as the message and returns err.
// Design 5.10 lists the reasons that make an MR unavailable.
func Unavailable(mg resource.Managed, err error) error {
	mg.SetConditions(xpv2.Unavailable().WithMessage(err.Error()))
	return err
}

// IgnoreGone returns nil for 192 UNKNOWN_USER and 511 UNKNOWN_ROLE, which
// Delete paths count as already gone (design 5.2), and err otherwise.
func IgnoreGone(err error) error {
	if ch.IsCode(err, ch.CodeUnknownUser, ch.CodeUnknownRole) {
		return nil
	}
	return err
}

func orphanf(format string, args ...any) error { return fmt.Errorf(format, args...) }
