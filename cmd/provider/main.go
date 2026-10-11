// Copyright 2026 The provider-clickhouse Authors
// Copyright 2020 The Crossplane Authors
// SPDX-License-Identifier: Apache-2.0
//
// Portions of this file are adapted from cmd/provider/main.go of
// crossplane/provider-template (https://github.com/crossplane/provider-template)
// and crossplane-contrib/provider-sql
// (https://github.com/crossplane-contrib/provider-sql), Copyright 2020 The
// Crossplane Authors, licensed under the Apache License, Version 2.0.
// Modified by the provider-clickhouse Authors.

// Command provider runs provider-clickhouse, the Crossplane provider for
// self-hosted ClickHouse SQL access management.
package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/alecthomas/kingpin/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	changelogsv1alpha1 "github.com/crossplane/crossplane-runtime/v2/apis/changelogs/proto/v1alpha1"
	"github.com/crossplane/crossplane-runtime/v2/pkg/controller"
	"github.com/crossplane/crossplane-runtime/v2/pkg/feature"
	"github.com/crossplane/crossplane-runtime/v2/pkg/gate"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
	"github.com/crossplane/crossplane-runtime/v2/pkg/ratelimiter"
	"github.com/crossplane/crossplane-runtime/v2/pkg/reconciler/customresourcesgate"
	"github.com/crossplane/crossplane-runtime/v2/pkg/reconciler/managed"
	"github.com/crossplane/crossplane-runtime/v2/pkg/statemetrics"

	"github.com/infrabay/provider-clickhouse/apis"
	ch "github.com/infrabay/provider-clickhouse/internal/clients/clickhouse"
	"github.com/infrabay/provider-clickhouse/internal/clients/clickhouse/auth"
	xpcontroller "github.com/infrabay/provider-clickhouse/internal/controller"
	"github.com/infrabay/provider-clickhouse/internal/controller/providerconfig"
	"github.com/infrabay/provider-clickhouse/internal/version"
)

func main() {
	var (
		app            = kingpin.New(filepath.Base(os.Args[0]), "Crossplane provider for self-hosted ClickHouse SQL access management.").DefaultEnvars()
		debug          = app.Flag("debug", "Run with debug logging.").Short('d').Bool()
		leaderElection = app.Flag("leader-election", "Use leader election for the controller manager.").Short('l').Default("false").Envar("LEADER_ELECTION").Bool()

		syncInterval            = app.Flag("sync", "How often all resources will be double-checked for drift from the desired state.").Short('s').Default("1h").Duration()
		pollInterval            = app.Flag("poll", "How often individual resources will be checked for drift from the desired state.").Default("1m").Duration()
		pollStateMetricInterval = app.Flag("poll-state-metric", "State metric recording interval.").Default("5s").Duration()
		maxReconcileRate        = app.Flag("max-reconcile-rate", "The global maximum rate per second at which resources may be checked for drift from the desired state.").Default("10").Int()

		metricsAddr = app.Flag("metrics-bind-address", "Address the Prometheus metrics endpoint binds to. Set to 0 to disable.").Default(":8080").String()
		healthAddr  = app.Flag("health-probe-bind-address", "Address the health probe endpoint (/healthz, /readyz) binds to. Set to 0 to disable.").Default(":8081").String()

		// Management policies cannot be disabled: ownership depends on them.
		// The flag is kept, hidden, so that existing runtime configs that set
		// it to true keep working.
		enableManagementPolicies = app.Flag("enable-management-policies", "Management policies are always enabled; false is refused.").Hidden().Default("true").Envar("ENABLE_MANAGEMENT_POLICIES").Bool()
		enableChangeLogs         = app.Flag("enable-changelogs", "Enable support for capturing change logs during reconciliation.").Default("false").Envar("ENABLE_CHANGE_LOGS").Bool()
		changelogsSocketPath     = app.Flag("changelogs-socket-path", "Path for the change logs socket (if enabled).").Default("/var/run/changelogs/changelogs.sock").Envar("CHANGELOGS_SOCKET_PATH").String()
		enableSecretCache        = app.Flag("enable-secret-cache", "Serve Secrets from the informer cache instead of direct API calls. Token Secrets are always read uncached.").Default("true").Envar("ENABLE_SECRET_CACHE").Bool()

		enableNamespacedPCs = app.Flag("enable-namespaced-provider-configs", "Allow managed resources to use namespaced ProviderConfigs. When false, only ClusterProviderConfigs can be used.").Default("true").Envar("ENABLE_NAMESPACED_PROVIDER_CONFIGS").Bool()
		namespacedEndpoint  = app.Flag("namespaced-endpoint-pattern", "RE2 pattern (implicitly anchored) on host:port that namespaced ProviderConfigs may target.").Default(".*").Envar("NAMESPACED_ENDPOINT_PATTERN").String()
	)
	kingpin.MustParse(app.Parse(os.Args[1:]))

	if !*enableManagementPolicies {
		kingpin.Fatalf("--enable-management-policies=false is not supported: ownership checks depend on management policies (observe-only resources must never alter or drop anything)")
	}

	zl := zap.New(zap.UseDevMode(*debug))
	log := logging.NewLogrLogger(zl.WithName("provider-clickhouse"))
	if *debug {
		// controller-runtime is very verbose even at info level, so it only
		// gets a real logger in debug mode.
		ctrl.SetLogger(zl)
	} else {
		ctrl.SetLogger(zap.New(zap.WriteTo(io.Discard)))
	}
	log.Info("Starting", "version", version.Version)

	endpointPattern, err := providerconfig.CompileEndpointPattern(*namespacedEndpoint)
	kingpin.FatalIfError(err, "Cannot compile --namespaced-endpoint-pattern")

	cfg, err := ctrl.GetConfig()
	kingpin.FatalIfError(err, "Cannot get API server rest config")

	var clientOpts client.Options
	if !*enableSecretCache {
		clientOpts = client.Options{Cache: &client.CacheOptions{DisableFor: []client.Object{&corev1.Secret{}}}}
	}

	scheme := runtime.NewScheme()
	kingpin.FatalIfError(clientgoscheme.AddToScheme(scheme), "Cannot add client-go APIs to scheme")
	kingpin.FatalIfError(apis.AddToScheme(scheme), "Cannot add ClickHouse APIs to scheme")
	kingpin.FatalIfError(apiextensionsv1.AddToScheme(scheme), "Cannot add CustomResourceDefinition to scheme")

	mgr, err := ctrl.NewManager(ratelimiter.LimitRESTConfig(cfg, *maxReconcileRate), ctrl.Options{
		Scheme: scheme,
		Cache: cache.Options{
			SyncPeriod: syncInterval,
			// Strip large schemas from CRDs before they enter the informer
			// cache.
			ByObject: map[client.Object]cache.ByObject{
				&apiextensionsv1.CustomResourceDefinition{}: {Transform: customresourcesgate.TransformStripCRDSchema},
			},
		},
		Client:                 clientOpts,
		Metrics:                metricsserver.Options{BindAddress: *metricsAddr},
		HealthProbeBindAddress: *healthAddr,

		// Leases only, with longer leases than the controller-runtime
		// defaults, as in the Crossplane provider template.
		LeaderElection:             *leaderElection,
		LeaderElectionID:           "crossplane-leader-election-provider-clickhouse",
		LeaderElectionResourceLock: resourcelock.LeasesResourceLock,
		LeaseDuration:              new(60 * time.Second),
		RenewDeadline:              new(50 * time.Second),
	})
	kingpin.FatalIfError(err, "Cannot create controller manager")
	kingpin.FatalIfError(mgr.AddHealthzCheck("healthz", healthz.Ping), "Cannot add health check")
	kingpin.FatalIfError(mgr.AddReadyzCheck("readyz", healthz.Ping), "Cannot add ready check")

	metricRecorder := managed.NewMRMetricRecorder()
	stateMetrics := statemetrics.NewMRStateMetrics()
	metrics.Registry.MustRegister(metricRecorder)
	metrics.Registry.MustRegister(stateMetrics)

	o := controller.Options{
		Logger:                  log,
		MaxConcurrentReconciles: *maxReconcileRate,
		PollInterval:            *pollInterval,
		GlobalRateLimiter:       ratelimiter.NewGlobal(*maxReconcileRate),
		Features:                &feature.Flags{},
		Gate:                    new(gate.Gate[schema.GroupVersionKind]),
		MetricOptions: &controller.MetricOptions{
			PollStateMetricInterval: *pollStateMetricInterval,
			MRMetrics:               metricRecorder,
			MRStateMetrics:          stateMetrics,
		},
	}
	// Always on; every managed resource controller also passes
	// managed.WithManagementPolicies() unconditionally.
	o.Features.Enable(feature.EnableBetaManagementPolicies)
	if *enableChangeLogs {
		o.Features.Enable(feature.EnableAlphaChangeLogs)
		log.Info("Alpha feature enabled", "flag", feature.EnableAlphaChangeLogs)
		conn, err := grpc.NewClient("unix://"+*changelogsSocketPath, grpc.WithTransportCredentials(insecure.NewCredentials()))
		kingpin.FatalIfError(err, "Cannot create change logs client connection at %s", *changelogsSocketPath)
		o.ChangeLogOptions = &controller.ChangeLogOptions{
			ChangeLogger: managed.NewGRPCChangeLogger(
				changelogsv1alpha1.NewChangeLogServiceClient(conn),
				managed.WithProviderVersion(fmt.Sprintf("provider-clickhouse:%s", version.Version))),
		}
	}

	// One connection pool per ProviderConfig, shared by all controllers. The
	// cache sweeps idle pools and closes every pool on shutdown.
	clients := ch.NewCache(ch.OpenDriver)
	kingpin.FatalIfError(mgr.Add(clients), "Cannot add the ClickHouse client cache")

	deps := xpcontroller.Dependencies{
		Cache: clients,
		Flags: providerconfig.Flags{
			EnableNamespacedProviderConfigs: *enableNamespacedPCs,
			NamespacedEndpointPattern:       endpointPattern,
			// Tokens minted for the provider's own Kubernetes API audience are
			// never sent to ClickHouse.
			KubernetesAPIAudiences: auth.KubernetesAPIAudiences(auth.KubernetesTokenPath),
		},
	}

	kingpin.FatalIfError(customresourcesgate.Setup(mgr, o), "Cannot set up the CRD gate controller")
	kingpin.FatalIfError(xpcontroller.SetupGated(mgr, o, deps), "Cannot set up ClickHouse controllers")
	kingpin.FatalIfError(mgr.Start(ctrl.SetupSignalHandler()), "Cannot start controller manager")
}
