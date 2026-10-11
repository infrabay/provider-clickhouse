// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

// Package database contains the Database controller.
package database

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	"github.com/google/uuid"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/crossplane/crossplane-runtime/v2/pkg/controller"
	"github.com/crossplane/crossplane-runtime/v2/pkg/event"
	"github.com/crossplane/crossplane-runtime/v2/pkg/feature"
	"github.com/crossplane/crossplane-runtime/v2/pkg/meta"
	"github.com/crossplane/crossplane-runtime/v2/pkg/ratelimiter"
	"github.com/crossplane/crossplane-runtime/v2/pkg/reconciler/managed"
	"github.com/crossplane/crossplane-runtime/v2/pkg/resource"
	"github.com/crossplane/crossplane-runtime/v2/pkg/statemetrics"

	"github.com/infrabay/provider-clickhouse/apis/v1alpha1"
	ch "github.com/infrabay/provider-clickhouse/internal/clients/clickhouse"
	"github.com/infrabay/provider-clickhouse/internal/clients/clickhouse/sqlx"
	"github.com/infrabay/provider-clickhouse/internal/controller/access"
	"github.com/infrabay/provider-clickhouse/internal/controller/ownership"
	"github.com/infrabay/provider-clickhouse/internal/controller/providerconfig"
	"github.com/infrabay/provider-clickhouse/internal/reason"
)

const (
	// reconcileTimeout covers ON CLUSTER DDL, which the server bounds with
	// distributed_ddl_task_timeout.
	reconcileTimeout = 2 * time.Minute

	// ddlTaskTimeoutSeconds is distributed_ddl_task_timeout for ON CLUSTER
	// DDL, below the reconcile timeout.
	ddlTaskTimeoutSeconds = 45

	kind = "Database"
)

// Event reasons.
const (
	ReasonAdopted event.Reason = "Adopted"

	// ReasonOrphanedByPolicy: a deleting MR whose namespace or name is not
	// allowed by its ClusterProviderConfig. Nothing is read or dropped; the
	// database is left in place.
	ReasonOrphanedByPolicy event.Reason = "OrphanedByPolicy"

	// ReasonOrphaned: a deleting MR whose database is not its own (another
	// UUID, an unresolvable name, or another UUID on some host of the
	// cluster). Nothing is dropped; the database is left in place.
	ReasonOrphaned event.Reason = "Orphaned"
)

// Dependencies are shared by the controllers.
type Dependencies = access.Dependencies

// SetupGated registers the Database controller with safe-start: it is set up
// once the Database CRD is installed (and activated).
func SetupGated(mgr ctrl.Manager, o controller.Options, d Dependencies) error {
	o.Gate.Register(func() {
		if err := Setup(mgr, o, d); err != nil {
			panic(fmt.Errorf("cannot set up the Database controller: %w", err))
		}
	}, v1alpha1.DatabaseGroupVersionKind)
	return nil
}

// Setup adds the Database controller.
func Setup(mgr ctrl.Manager, o controller.Options, d Dependencies) error {
	name := managed.ControllerName(v1alpha1.DatabaseGroupKind)
	recorder := event.NewAPIRecorder(mgr.GetEventRecorderFor(name)) //nolint:staticcheck // crossplane-runtime still uses the legacy events API.

	conn := &Connector{
		Kube:     mgr.GetClient(),
		Usage:    resource.NewProviderConfigUsageTracker(mgr.GetClient(), &v1alpha1.ProviderConfigUsage{}),
		Resolver: &providerconfig.APIResolver{Kube: mgr.GetClient(), APIReader: mgr.GetAPIReader(), Flags: d.Flags},
		Cache:    d.Cache,
		Recorder: recorder,
	}

	opts := []managed.ReconcilerOption{
		managed.WithTypedExternalConnector[*v1alpha1.Database](conn),
		managed.WithInitializers(ownership.NewInitializer(mgr.GetClient(), forProviderName, ownership.WithPreassignedID())),
		// Every name comes from the spec and the UUID is chosen before the
		// CREATE, so a crash during Create is recoverable.
		managed.WithDeterministicExternalName(true),
		managed.WithTimeout(reconcileTimeout),
		managed.WithLogger(o.Logger.WithValues("controller", name)),
		managed.WithPollInterval(o.PollInterval),
		managed.WithRecorder(recorder),
		// Ownership depends on management policies: observe-only MRs never
		// claim, alter or drop anything. They are therefore always honoured,
		// whatever the feature flags say; without this option
		// crossplane-runtime would allow every action.
		managed.WithManagementPolicies(),
	}
	if o.Features.Enabled(feature.EnableAlphaChangeLogs) && o.ChangeLogOptions != nil {
		opts = append(opts, managed.WithChangeLogger(o.ChangeLogOptions.ChangeLogger))
	}
	if o.MetricOptions != nil {
		if o.MetricOptions.MRMetrics != nil {
			opts = append(opts, managed.WithMetricRecorder(o.MetricOptions.MRMetrics))
		}
		if o.MetricOptions.MRStateMetrics != nil {
			sm := statemetrics.NewMRStateRecorder(mgr.GetClient(), o.Logger, o.MetricOptions.MRStateMetrics, &v1alpha1.DatabaseList{}, o.MetricOptions.PollStateMetricInterval)
			if err := mgr.Add(sm); err != nil {
				return fmt.Errorf("cannot register MR state metrics recorder for DatabaseList: %w", err)
			}
		}
	}

	r := managed.NewReconciler(mgr, resource.ManagedKind(v1alpha1.DatabaseGroupVersionKind), opts...)

	return ctrl.NewControllerManagedBy(mgr).
		Named(name).
		WithOptions(o.ForControllerRuntime()).
		WithEventFilter(resource.DesiredStateChanged()).
		For(&v1alpha1.Database{}).
		Complete(ratelimiter.NewReconciler(name, r, o.GlobalRateLimiter))
}

func forProviderName(mg resource.Managed) *string {
	if db, ok := mg.(*v1alpha1.Database); ok {
		return db.Spec.ForProvider.Name
	}
	return nil
}

// A Tracker tracks ProviderConfig usage.
type Tracker interface {
	Track(ctx context.Context, mg resource.ModernManaged) error
}

// A ClientCache hands out ClickHouse clients.
type ClientCache interface {
	Acquire(ctx context.Context, key ch.Key, cfg ch.Config) (ch.Client, error)
}

// Connector produces external clients for Databases.
type Connector struct {
	Kube     client.Client
	Usage    Tracker
	Resolver providerconfig.Resolver
	Cache    ClientCache
	Recorder event.Recorder
}

// Connect resolves the config, checks the namespace policy, tracks usage
// (after the namespace check, so a denied namespace never creates a usage
// that would block deleting the config) and acquires a client.
func (c *Connector) Connect(ctx context.Context, cr *v1alpha1.Database) (managed.TypedExternalClient[*v1alpha1.Database], error) {
	res, err := c.Resolver.Resolve(ctx, cr)
	if err != nil {
		return nil, err
	}
	if res.NamespaceDenied {
		return &orphan{recorder: c.recorder(), why: res.DenyReason}, nil
	}
	if err := c.Usage.Track(ctx, cr); err != nil {
		return nil, fmt.Errorf("cannot track ProviderConfig usage: %w", err)
	}
	cl, err := c.Cache.Acquire(ctx, res.CacheKey, res.Config)
	if err != nil {
		return nil, err
	}
	return &External{Client: cl, Resolved: res, Kube: c.Kube, Recorder: c.recorder()}, nil
}

func (c *Connector) recorder() event.Recorder {
	if c.Recorder == nil {
		return event.NewNopRecorder()
	}
	return c.Recorder
}

// External manages one Database through a ClickHouse client.
type External struct {
	Client   ch.Client
	Resolved *providerconfig.Resolved
	Kube     client.Client
	Recorder event.Recorder
}

func (e *External) sqlContext(ctx context.Context, cr *v1alpha1.Database) context.Context {
	ctx = ch.WithLogComment(ctx, kind+"/"+cr.GetNamespace()+"/"+cr.GetName())
	if e.Resolved.Cluster != "" {
		ctx = ch.WithDistributedDDLTimeout(ctx, ddlTaskTimeoutSeconds)
	}
	return ctx
}

func unavailable(cr *v1alpha1.Database, err error) (managed.ExternalObservation, error) {
	cr.SetConditions(xpv2.Unavailable().WithMessage(err.Error()))
	return managed.ExternalObservation{}, err
}

// Observe reads the database and decides ownership.
//
// While the MR is being deleted, Observe reports the database as existing
// only when it is the MR's own (owned, adopted or recovered), its name is
// allowed by the ClusterProviderConfig policy and, with a cluster, no host
// has a database of that name with another UUID. In every other case it
// emits a Warning event and reports it as absent: the MR goes away, no SQL
// write runs and the database is left in place. Deletion never blocks on a
// policy or a name conflict.
func (e *External) Observe(ctx context.Context, cr *v1alpha1.Database) (managed.ExternalObservation, error) {
	deleting := meta.WasDeleted(cr)
	name, err := ownership.EffectiveName(cr, cr.Spec.ForProvider.Name)
	if err != nil {
		if deleting {
			return e.leaveInPlace(cr, ReasonOrphaned, fmt.Errorf("%w; no database was dropped", err)), nil
		}
		return unavailable(cr, err)
	}
	// The name policy applies to every management policy, including
	// observe-only, and also while deleting. The ownership claim is only
	// annotations, which the tenant can edit, so the policy is the boundary
	// between tenants: a name outside it is never read, altered or dropped.
	if e.Resolved.Policy != nil {
		if err := e.Resolved.Policy.CheckDatabaseName(cr.GetNamespace(), name); err != nil {
			if deleting {
				return e.leaveInPlace(cr, ReasonOrphanedByPolicy, fmt.Errorf("%w; the database is left in place", err)), nil
			}
			return unavailable(cr, err)
		}
	}
	ctx = e.sqlContext(ctx, cr)
	if !deleting && e.Resolved.Cluster != "" {
		if _, err := e.Client.Capabilities().ClusterHosts(ctx, e.Client, e.Resolved.Cluster); err != nil {
			return unavailable(cr, err)
		}
	}

	obs, exists, err := e.read(ctx, name)
	if err != nil {
		return managed.ExternalObservation{}, err
	}
	if !exists {
		return managed.ExternalObservation{ResourceExists: false}, nil
	}

	d := ownership.Decide(cr, ownership.Input{
		Kind:            kind,
		Name:            name,
		ObservedID:      obs.ID,
		AdoptionAllowed: e.Resolved.AdoptionAllowed,
	})
	if d.Err != nil {
		cr.Status.AtProvider = obs
		return unavailable(cr, d.Err)
	}
	if !d.Exists {
		if deleting {
			return e.leaveInPlace(cr, ReasonOrphaned, fmt.Errorf("%s %q (id %s) was not created by this resource (%s); it is left in place", kind, name, obs.ID, d.Decision)), nil
		}
		return managed.ExternalObservation{ResourceExists: false}, nil
	}
	if deleting && e.Resolved.Cluster != "" {
		// DROP ... ON CLUSTER drops by name on every host; check the UUID
		// there before anything is claimed or dropped.
		foreign, err := e.foreignHosts(ctx, name, obs.ID)
		if err != nil {
			return managed.ExternalObservation{}, err
		}
		if len(foreign) > 0 {
			return e.leaveInPlace(cr, ReasonOrphaned, fmt.Errorf("%s %q has another UUID on %s of cluster %q; DROP DATABASE ... ON CLUSTER would drop it there, so nothing is dropped",
				kind, name, strings.Join(foreign, ", "), e.Resolved.Cluster)), nil
		}
	}
	if err := ownership.Patch(ctx, e.Kube, cr, d.Set, d.Remove); err != nil {
		return managed.ExternalObservation{}, err
	}
	if d.Event != "" {
		e.Recorder.Event(cr, event.Normal(event.Reason(d.Event), fmt.Sprintf("%s %s %q (id %s)", d.Event, kind, name, obs.ID)))
	}
	cr.Status.AtProvider = obs
	if deleting {
		return managed.ExternalObservation{ResourceExists: true}, nil
	}

	lateInit := e.lateInitialize(cr, obs)
	cr.SetConditions(xpv2.Available())
	upToDate, err := e.upToDate(ctx, cr, obs)
	if err != nil {
		return managed.ExternalObservation{}, err
	}
	return managed.ExternalObservation{
		ResourceExists:          true,
		ResourceUpToDate:        upToDate,
		ResourceLateInitialized: lateInit,
	}, nil
}

// leaveInPlace emits a Warning event and reports the database as absent
// while the MR is being deleted: the MR goes away without any SQL write.
func (e *External) leaveInPlace(cr *v1alpha1.Database, r event.Reason, err error) managed.ExternalObservation {
	e.Recorder.Event(cr, event.Warning(r, err))
	return managed.ExternalObservation{ResourceExists: false}
}

// read reads the database's details on the connected server. It reports
// exists=false only when EXISTS DATABASE, which cannot be silently filtered
// (without a privilege on the database it fails with 497 instead of
// returning 0), said 0.
func (e *External) read(ctx context.Context, name string) (v1alpha1.DatabaseObservation, bool, error) {
	var obs v1alpha1.DatabaseObservation
	var exists uint8
	if err := ch.QueryRow(ctx, e.Client, sqlx.ExistsDatabase(name), nil, &exists); err != nil {
		return obs, false, ch.WithDefaultReason(err)
	}
	if exists == 0 {
		return obs, false, nil
	}
	err := ch.QueryRow(ctx, e.Client, sqlx.QueryDatabase, ch.Params{"name": name}, &obs.ID, &obs.Engine, &obs.EngineFull, &obs.Comment)
	switch {
	case errors.Is(err, ch.ErrNoRows):
		// system.databases is filtered by privileges: zero rows after
		// EXISTS said 1 is never "not found".
		return obs, false, notVisible(name)
	case err != nil:
		return obs, false, ch.WithDefaultReason(err)
	}
	return obs, true, nil
}

func notVisible(name string) error {
	return reason.Errorf(reason.PermissionDenied,
		"database %q exists but is not visible in system.databases; grant the provider identity a privilege on it", name)
}

// currentID re-reads the database's UUID on the connected server. exists is
// false only when EXISTS DATABASE said 0.
func (e *External) currentID(ctx context.Context, name string) (string, bool, error) {
	var id string
	err := ch.QueryRow(ctx, e.Client, sqlx.QueryDatabaseUUID, ch.Params{"name": name}, &id)
	if err == nil {
		return id, true, nil
	}
	if !errors.Is(err, ch.ErrNoRows) {
		return "", false, ch.WithDefaultReason(err)
	}
	var exists uint8
	if err := ch.QueryRow(ctx, e.Client, sqlx.ExistsDatabase(name), nil, &exists); err != nil {
		return "", false, ch.WithDefaultReason(err)
	}
	if exists == 0 {
		return "", false, nil
	}
	return "", false, notVisible(name)
}

// verifyOwned re-checks ownership immediately before a write: the database
// could have been dropped and re-created by someone else since Observe, and
// the statements address it by name. This narrows that window to one round
// trip. With a cluster it also checks every host, because ON CLUSTER writes
// go to every host by name. It returns false when the database no longer
// exists on the connected server.
func (e *External) verifyOwned(ctx context.Context, cr *v1alpha1.Database, name string) (bool, error) {
	claim := ownership.ClaimOf(cr)
	if !ownership.ClaimValid(cr) || claim.OwnedID == "" {
		return false, reason.Errorf(reason.NotOwned, "%s %q is not claimed by this resource; nothing was changed", kind, name)
	}
	id, exists, err := e.currentID(ctx, name)
	if err != nil || !exists {
		return false, err
	}
	if id != claim.OwnedID {
		return false, reason.Errorf(reason.Replaced, "%s %q was replaced since it was observed (id %s, now %s); nothing was changed", kind, name, claim.OwnedID, id)
	}
	if c := e.Resolved.Cluster; c != "" {
		foreign, err := e.foreignHosts(ctx, name, claim.OwnedID)
		if err != nil {
			return false, err
		}
		if len(foreign) > 0 {
			return false, reason.Errorf(reason.NotOwned, "%s %q has another UUID on %s of cluster %q; an ON CLUSTER statement would change it there, so nothing was changed",
				kind, name, strings.Join(foreign, ", "), c)
		}
	}
	return true, nil
}

// foreignHosts returns "<host> (id <uuid>)" for every host of the cluster
// where a database with this name has a UUID other than id. Hosts without
// the database return no row: ALTER fails there with 81 and DROP ... IF
// EXISTS does nothing. An unreachable host fails the query, so the check
// never passes by omission.
func (e *External) foreignHosts(ctx context.Context, name, id string) ([]string, error) {
	c := e.Resolved.Cluster
	var foreign []string
	err := e.Client.Query(ctx, sqlx.ClusterDatabaseIDs(c), ch.Params{"name": name}, func(r ch.Row) error {
		var host, got string
		if err := r.Scan(&host, &got); err != nil {
			return err
		}
		if got != id {
			foreign = append(foreign, fmt.Sprintf("%s (id %s)", host, got))
		}
		return nil
	})
	if ch.IsCode(err, ch.CodeAccessDenied) {
		return nil, reason.Wrap(reason.PermissionDenied, fmt.Errorf(
			"cannot verify the UUID of database %q on every host of cluster %q (grant REMOTE ON *.* to the provider identity, or remove Update and Delete from the management policies): %w",
			name, c, err))
	}
	if err != nil {
		return nil, ch.WithDefaultReason(err)
	}
	slices.Sort(foreign)
	return foreign, nil
}

// lateInitialize fills nil fields from the server when the management
// policies include LateInitialize.
func (e *External) lateInitialize(cr *v1alpha1.Database, obs v1alpha1.DatabaseObservation) bool {
	if !ownership.PoliciesOf(cr).LateInitialize() {
		return false
	}
	p := &cr.Spec.ForProvider
	changed := false
	if p.Engine == nil {
		switch obs.Engine {
		case v1alpha1.DatabaseEngineAtomic:
			p.Engine = &v1alpha1.DatabaseEngine{Name: v1alpha1.DatabaseEngineAtomic}
			changed = true
		case v1alpha1.DatabaseEngineReplicated:
			if _, args, err := sqlx.ParseEngineArgs(obs.EngineFull); err == nil && len(args) == 3 {
				p.Engine = &v1alpha1.DatabaseEngine{Name: v1alpha1.DatabaseEngineReplicated, Replicated: &v1alpha1.ReplicatedEngine{
					ZooKeeperPath: args[0], ShardName: args[1], ReplicaName: args[2],
				}}
				changed = true
			}
		}
	}
	if p.Comment == nil {
		c := obs.Comment
		p.Comment = &c
		changed = true
	}
	return changed
}

func desiredEngine(e *v1alpha1.DatabaseEngine) *sqlx.Engine {
	if e == nil {
		return nil
	}
	out := &sqlx.Engine{Name: e.Name}
	if e.Name == v1alpha1.DatabaseEngineReplicated {
		out.ZooKeeperPath, out.ShardName, out.ReplicaName = v1alpha1.DefaultZooKeeperPath, v1alpha1.DefaultShardName, v1alpha1.DefaultReplicaName
		if r := e.Replicated; r != nil {
			if r.ZooKeeperPath != "" {
				out.ZooKeeperPath = r.ZooKeeperPath
			}
			if r.ShardName != "" {
				out.ShardName = r.ShardName
			}
			if r.ReplicaName != "" {
				out.ReplicaName = r.ReplicaName
			}
		}
	}
	return out
}

// upToDate compares the desired state with the server. A changed engine
// cannot be fixed with SQL and is reported as InvalidSpec.
func (e *External) upToDate(ctx context.Context, cr *v1alpha1.Database, obs v1alpha1.DatabaseObservation) (bool, error) {
	p := cr.Spec.ForProvider
	if want := desiredEngine(p.Engine); want != nil {
		if want.Name != obs.Engine {
			return false, reason.Errorf(reason.InvalidSpec, "engine is immutable (observed %s, desired %s); re-create the database", obs.Engine, want.Name)
		}
		if want.Name == v1alpha1.DatabaseEngineReplicated {
			_, args, err := sqlx.ParseEngineArgs(obs.EngineFull)
			if err != nil || len(args) != 3 {
				return false, reason.Errorf(reason.Internal, "cannot parse engine_full %q", obs.EngineFull)
			}
			if args[0] != want.ZooKeeperPath || args[1] != want.ShardName || args[2] != want.ReplicaName {
				return false, reason.Errorf(reason.InvalidSpec, "Replicated engine arguments are immutable (observed %q, desired %q, %q, %q); re-create the database",
					obs.EngineFull, want.ZooKeeperPath, want.ShardName, want.ReplicaName)
			}
		}
	}
	if p.Comment != nil && *p.Comment != obs.Comment {
		if !ownership.PoliciesOf(cr).Writes() {
			return false, nil
		}
		ok, err := e.Client.Capabilities().DatabaseCommentChange(ctx, e.Client)
		if err != nil {
			return false, err
		}
		if !ok {
			err := reason.Errorf(reason.Unsupported, "changing a database comment needs ClickHouse 25.8 or later (server %s)", e.Client.Info().Version)
			cr.SetConditions(xpv2.Unavailable().WithMessage(err.Error()))
			return false, err
		}
		return false, nil
	}
	return true, nil
}

// Create runs CREATE DATABASE with the UUID the initializer chose.
func (e *External) Create(ctx context.Context, cr *v1alpha1.Database) (managed.ExternalCreation, error) {
	name, err := ownership.EffectiveName(cr, cr.Spec.ForProvider.Name)
	if err != nil {
		return managed.ExternalCreation{}, err
	}
	claim := ownership.ClaimOf(cr)
	if !ownership.ClaimValid(cr) || claim.OwnedID == "" {
		return managed.ExternalCreation{}, reason.Errorf(reason.Internal, "the ownership claim is missing; the initializer did not run")
	}
	if u, err := uuid.Parse(claim.OwnedID); err != nil || u.String() != claim.OwnedID || claim.OwnedID == ownership.ZeroUUID {
		return managed.ExternalCreation{}, reason.Errorf(reason.InvalidSpec, "annotation %s is not a non-zero UUID in canonical form", v1alpha1.AnnotationOwnedID)
	}
	if err := sqlx.CheckDatabaseNameLength(name); err != nil {
		return managed.ExternalCreation{}, reason.Wrap(reason.InvalidSpec, err)
	}
	engine := desiredEngine(cr.Spec.ForProvider.Engine)
	if engine != nil && engine.Name == sqlx.EngineReplicated && e.Resolved.Policy != nil {
		// The Keeper path decides which Replicated database a new replica
		// joins; it is checked before the CREATE registers anything there.
		if err := e.Resolved.Policy.CheckZooKeeperPath(cr.GetNamespace(), engine.ZooKeeperPath); err != nil {
			cr.SetConditions(xpv2.Unavailable().WithMessage(err.Error()))
			return managed.ExternalCreation{}, err
		}
	}
	stmt, err := sqlx.CreateDatabaseStmt(sqlx.CreateDatabase{
		Name:    name,
		UUID:    claim.OwnedID,
		Cluster: e.Resolved.Cluster,
		Engine:  engine,
		Comment: cr.Spec.ForProvider.Comment,
	})
	if err != nil {
		return managed.ExternalCreation{}, reason.Wrap(reason.InvalidSpec, err)
	}
	if err := e.Client.Exec(e.sqlContext(ctx, cr), stmt); err != nil {
		return managed.ExternalCreation{}, ch.WithDefaultReason(err)
	}
	return managed.ExternalCreation{}, nil
}

// Update changes the comment, the only mutable aspect. The statement is
// idempotent, so it is sent whenever a comment is desired: status may have
// been reset by a late-initialization update earlier in the reconcile.
// Ownership is re-checked first, on every host with a cluster.
func (e *External) Update(ctx context.Context, cr *v1alpha1.Database) (managed.ExternalUpdate, error) {
	p := cr.Spec.ForProvider
	if p.Comment == nil {
		return managed.ExternalUpdate{}, nil
	}
	name, err := ownership.EffectiveName(cr, p.Name)
	if err != nil {
		return managed.ExternalUpdate{}, err
	}
	ctx = e.sqlContext(ctx, cr)
	ok, err := e.verifyOwned(ctx, cr, name)
	if err != nil || !ok {
		// Not ok: the database disappeared since Observe; the next Observe
		// decides.
		return managed.ExternalUpdate{}, err
	}
	err = e.Client.Exec(ctx, sqlx.AlterDatabaseCommentStmt(name, e.Resolved.Cluster, *p.Comment))
	if ch.IsCode(err, ch.CodeUnknownDatabase) {
		// The database disappeared between the check and the ALTER; the next
		// Observe decides.
		return managed.ExternalUpdate{}, nil
	}
	return managed.ExternalUpdate{}, ch.WithDefaultReason(err)
}

// Delete drops the database. It is only reached for the MR's own database:
// Observe reports every other case as absent while the MR is being deleted.
// Unless forceDrop is set it refuses to drop a database that has tables, or
// whose emptiness cannot be verified; with a cluster emptiness can never be
// verified on every host. Ownership is re-checked immediately before the
// DROP, on every host with a cluster.
func (e *External) Delete(ctx context.Context, cr *v1alpha1.Database) (managed.ExternalDelete, error) {
	cr.SetConditions(xpv2.Deleting())
	name, err := ownership.EffectiveName(cr, cr.Spec.ForProvider.Name)
	if err != nil {
		return managed.ExternalDelete{}, err
	}
	if e.Resolved.Policy != nil {
		if err := e.Resolved.Policy.CheckDatabaseName(cr.GetNamespace(), name); err != nil {
			return managed.ExternalDelete{}, err
		}
	}
	ctx = e.sqlContext(ctx, cr)
	force := cr.Spec.ForProvider.ForceDrop != nil && *cr.Spec.ForProvider.ForceDrop
	if !force {
		if err := e.checkEmpty(ctx, name); err != nil {
			return managed.ExternalDelete{}, err
		}
	}
	ok, err := e.verifyOwned(ctx, cr, name)
	if err != nil {
		return managed.ExternalDelete{}, err
	}
	if !ok {
		// Already gone on the connected server.
		return managed.ExternalDelete{}, nil
	}
	if err := e.Client.Exec(ctx, sqlx.DropDatabaseStmt(name, e.Resolved.Cluster)); err != nil {
		return managed.ExternalDelete{}, ch.WithDefaultReason(err)
	}
	return managed.ExternalDelete{}, nil
}

// checkEmpty verifies that the database has no tables. With a cluster it
// always fails: remote hosts may hide tables from the provider identity
// (system.tables is filtered by privileges, depending on the cluster's
// credentials), and DROP DATABASE ... ON CLUSTER drops non-empty databases on
// remote hosts even without DROP TABLE.
func (e *External) checkEmpty(ctx context.Context, name string) error {
	if c := e.Resolved.Cluster; c != "" {
		return reason.Errorf(reason.DatabaseNotEmpty,
			"cannot verify that %q is empty on every host of cluster %q, and DROP DATABASE ... ON CLUSTER drops non-empty databases there; set forceDrop, or remove Delete from the management policies", name, c)
	}
	// system.tables silently hides tables the identity cannot SHOW, so the
	// count is only trusted when SHOW TABLES is granted.
	var granted uint8
	if err := ch.QueryRow(ctx, e.Client, sqlx.CheckGrantShowTables(name), nil, &granted); err != nil {
		return ch.WithDefaultReason(err)
	}
	if granted != 1 {
		return reason.Errorf(reason.DatabaseNotEmpty, "cannot verify that %q is empty (grant SHOW TABLES ON %s.*)", name, ch.QuoteIdent(name))
	}
	var n uint64
	if err := ch.QueryRow(ctx, e.Client, sqlx.QueryDatabaseTableCount, ch.Params{"name": name}, &n); err != nil {
		return ch.WithDefaultReason(err)
	}
	if n > 0 {
		return reason.Errorf(reason.DatabaseNotEmpty, "database %q still has %d tables; drop them, set forceDrop, or remove Delete from the management policies", name, n)
	}
	return nil
}

// Disconnect releases the client.
func (e *External) Disconnect(_ context.Context) error {
	return e.Client.Close()
}

// orphan is the external client for a Database that may no longer use its
// config while the MR is being deleted: its namespace is not allowed by the
// ClusterProviderConfig policy, or the provider flags forbid its namespaced
// ProviderConfig. The database is left in place and the MR goes away, so
// namespace deletion does not hang.
type orphan struct {
	recorder event.Recorder
	why      string
}

func (o *orphan) Observe(_ context.Context, cr *v1alpha1.Database) (managed.ExternalObservation, error) {
	why := o.why
	if why == "" {
		why = "the namespace is no longer allowed by the ClusterProviderConfig policy"
	}
	o.recorder.Event(cr, event.Warning(ReasonOrphanedByPolicy, errors.New(why+"; the database is left in place")))
	return managed.ExternalObservation{ResourceExists: false}, nil
}

func (o *orphan) Create(context.Context, *v1alpha1.Database) (managed.ExternalCreation, error) {
	return managed.ExternalCreation{}, reason.Errorf(reason.PolicyViolation, "the namespace is not allowed by the ClusterProviderConfig policy")
}

func (o *orphan) Update(context.Context, *v1alpha1.Database) (managed.ExternalUpdate, error) {
	return managed.ExternalUpdate{}, reason.Errorf(reason.PolicyViolation, "the namespace is not allowed by the ClusterProviderConfig policy")
}

func (o *orphan) Delete(context.Context, *v1alpha1.Database) (managed.ExternalDelete, error) {
	return managed.ExternalDelete{}, nil
}

func (o *orphan) Disconnect(context.Context) error { return nil }

// Interface checks.
var (
	_ managed.TypedExternalConnector[*v1alpha1.Database] = &Connector{}
	_ managed.TypedExternalClient[*v1alpha1.Database]    = &External{}
	_ managed.TypedExternalClient[*v1alpha1.Database]    = &orphan{}
)
