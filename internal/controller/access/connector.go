// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package access

import (
	"context"
	"errors"
	"fmt"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/crossplane/crossplane-runtime/v2/pkg/event"
	"github.com/crossplane/crossplane-runtime/v2/pkg/meta"
	"github.com/crossplane/crossplane-runtime/v2/pkg/reconciler/managed"
	"github.com/crossplane/crossplane-runtime/v2/pkg/resource"

	"github.com/infrabay/provider-clickhouse/apis/v1alpha1"
	ch "github.com/infrabay/provider-clickhouse/internal/clients/clickhouse"
	"github.com/infrabay/provider-clickhouse/internal/controller/ownership"
	"github.com/infrabay/provider-clickhouse/internal/controller/providerconfig"
	"github.com/infrabay/provider-clickhouse/internal/reason"
)

// A Tracker tracks ProviderConfig usage.
type Tracker interface {
	Track(ctx context.Context, mg resource.ModernManaged) error
}

// A ClientCache hands out ClickHouse clients.
type ClientCache interface {
	Acquire(ctx context.Context, key ch.Key, cfg ch.Config) (ch.Client, error)
}

// A Connector connects the external clients of one access entity kind
// (design 5.9):
//
//  1. resolve the ProviderConfig or ClusterProviderConfig, which checks
//     allowedNamespaces and the provider flags. A config the MR may no longer
//     use while it is being deleted orphans it (D-20);
//  2. track usage, only after that check, so that a denied namespace never
//     creates a usage that blocks deleting the config;
//  3. check that referenced MRs are in the MR's namespace and use the same
//     ProviderConfig (not while deleting: references are not resolved then);
//  4. acquire a client and build the kind's external client around an Env.
type Connector[T resource.ModernManaged] struct {
	// Kind names the kind in messages, e.g. "User".
	Kind string

	Kube client.Client
	// Reader lists MRs (grantee owners, referenced MRs). nil means Kube.
	Reader client.Reader
	// APIReader is the uncached API reader, for reads that a stale cache
	// must not answer (the connection Secret of a password User).
	APIReader client.Reader
	Usage     Tracker
	Resolver  providerconfig.Resolver
	Cache     ClientCache
	Recorder  event.Recorder
	// Journal is the create journal; nil means ownership.DefaultJournal.
	Journal *ownership.Journal

	// References returns the MR's references, or nil for kinds without
	// references.
	References func(T) []providerconfig.Reference
	// NewClient builds the kind's external client.
	NewClient func(env *Env) managed.TypedExternalClient[T]
}

func (c *Connector[T]) reader() client.Reader {
	if c.Reader == nil {
		return c.Kube
	}
	return c.Reader
}

func (c *Connector[T]) recorder() event.Recorder {
	if c.Recorder == nil {
		return event.NewNopRecorder()
	}
	return c.Recorder
}

// Connect implements managed.TypedExternalConnector.
func (c *Connector[T]) Connect(ctx context.Context, mg T) (managed.TypedExternalClient[T], error) {
	res, err := c.Resolver.Resolve(ctx, mg)
	if err != nil {
		return nil, err
	}
	if res.NamespaceDenied {
		return &Orphan[T]{Kind: c.Kind, Why: res.DenyReason, Recorder: c.recorder()}, nil
	}
	if err := c.Usage.Track(ctx, mg); err != nil {
		return nil, fmt.Errorf("cannot track ProviderConfig usage: %w", err)
	}
	if c.References != nil && !meta.WasDeleted(mg) {
		if err := providerconfig.CheckReferencedConfigs(ctx, c.reader(), mg, c.References(mg)); err != nil {
			return nil, Unavailable(mg, err)
		}
	}
	cl, err := c.Cache.Acquire(ctx, res.CacheKey, res.Config)
	if err != nil {
		return nil, err
	}
	return c.NewClient(&Env{
		Client:    cl,
		Resolved:  res,
		Kube:      c.Kube,
		Reader:    c.reader(),
		APIReader: c.APIReader,
		Recorder:  c.recorder(),
		Journal:   c.Journal,
	}), nil
}

// NewConnector returns the production connector of an access entity kind:
// the manager's client (also for listing MRs), the uncached API reader, the
// ProviderConfigUsage tracker, the API resolver with the provider flags and
// the shared client cache. refs may be nil.
func NewConnector[T resource.ModernManaged](mgr ctrl.Manager, d Dependencies, kind string, recorder event.Recorder,
	newClient func(*Env) managed.TypedExternalClient[T], refs func(T) []providerconfig.Reference) *Connector[T] {
	return &Connector[T]{
		Kind:       kind,
		Kube:       mgr.GetClient(),
		Reader:     mgr.GetClient(),
		APIReader:  mgr.GetAPIReader(),
		Usage:      resource.NewProviderConfigUsageTracker(mgr.GetClient(), &v1alpha1.ProviderConfigUsage{}),
		Resolver:   &providerconfig.APIResolver{Kube: mgr.GetClient(), APIReader: mgr.GetAPIReader(), Flags: d.Flags},
		Cache:      d.Cache,
		Recorder:   recorder,
		References: refs,
		NewClient:  newClient,
	}
}

// An Orphan is the external client of an MR that may no longer use its
// config while it is being deleted (Resolved.NamespaceDenied): its namespace
// is not allowed by the ClusterProviderConfig policy, or the provider flags
// forbid its namespaced ProviderConfig. Observe emits OrphanedByPolicy and
// reports the object as absent; nothing is read or written and the MR goes
// away, so namespace deletion does not hang.
type Orphan[T resource.Managed] struct {
	Kind     string
	Why      string
	Recorder event.Recorder
}

// Observe implements managed.TypedExternalClient.
func (o *Orphan[T]) Observe(_ context.Context, mg T) (managed.ExternalObservation, error) {
	why := o.Why
	if why == "" {
		why = "the namespace is no longer allowed by the ClusterProviderConfig policy"
	}
	o.Recorder.Event(mg, event.Warning(ReasonOrphanedByPolicy, errors.New(why+"; the "+o.Kind+" is left in place")))
	return managed.ExternalObservation{ResourceExists: false}, nil
}

// Create implements managed.TypedExternalClient.
func (o *Orphan[T]) Create(context.Context, T) (managed.ExternalCreation, error) {
	return managed.ExternalCreation{}, reason.Errorf(reason.PolicyViolation, "the config may not be used by this resource")
}

// Update implements managed.TypedExternalClient.
func (o *Orphan[T]) Update(context.Context, T) (managed.ExternalUpdate, error) {
	return managed.ExternalUpdate{}, reason.Errorf(reason.PolicyViolation, "the config may not be used by this resource")
}

// Delete implements managed.TypedExternalClient.
func (o *Orphan[T]) Delete(context.Context, T) (managed.ExternalDelete, error) {
	return managed.ExternalDelete{}, nil
}

// Disconnect implements managed.TypedExternalClient.
func (o *Orphan[T]) Disconnect(context.Context) error { return nil }
