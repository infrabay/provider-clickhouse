// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

// Package e2e tests the real provider package in a local Kubernetes cluster:
// Crossplane installs it from a registry inside the cluster, and every
// managed resource kind is reconciled against upstream ClickHouse (password
// identity) and Altinity Antalya (JWT identity from a projected service
// account token), with assertions through kubectl-style reads and through SQL
// as the servers' administrators.
//
// hack/e2e/run.sh (make e2e) builds and installs everything these tests
// need; see DEVELOPMENT.md. The tests run only against the cluster named by
// E2E_KUBE_CONTEXT, which must be a local cluster; the kubeconfig's current
// context is never used.
package e2e

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	xpextv1alpha1 "github.com/crossplane/crossplane/apis/v2/apiextensions/v1alpha1"
	xppkgv1 "github.com/crossplane/crossplane/apis/v2/pkg/v1"

	"github.com/infrabay/provider-clickhouse/apis"
)

// Environment of the tests, set by hack/e2e/run.sh.
const (
	envPoll       = "E2E_POLL_INTERVAL"
	envCrossplane = "E2E_CROSSPLANE_VERSION"
)

// Names shared with hack/e2e.
const (
	nsCrossplane = "crossplane-system"
	nsClickHouse = "xpch-e2e-clickhouse"
	nsOutsider   = "xpch-e2e-outsider"
	providerName = "provider-clickhouse"
	labelKey     = "app.kubernetes.io/part-of"
	labelValue   = "xpch-e2e"
	group        = "clickhouse.m.infrabay.io"
)

var (
	restConfig   *rest.Config
	kube         client.Client
	clientset    kubernetes.Interface
	pollInterval = 10 * time.Second
	// crossplaneVersion is the installed Crossplane release.
	crossplaneVersion string
)

func TestMain(m *testing.M) {
	if err := setup(); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: %v\n", err)
		os.Exit(2)
	}
	os.Exit(m.Run())
}

func setup() error {
	cfg, err := loadConfig(os.Getenv(envContext), os.Getenv(envKubeconfig))
	if err != nil {
		return err
	}
	if v := os.Getenv(envPoll); v != "" {
		if pollInterval, err = time.ParseDuration(v); err != nil {
			return fmt.Errorf("%s: %w", envPoll, err)
		}
	}
	crossplaneVersion = os.Getenv(envCrossplane)

	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		clientgoscheme.AddToScheme, apiextensionsv1.AddToScheme, apis.AddToScheme,
		xpextv1alpha1.AddToScheme, xppkgv1.AddToScheme,
	} {
		if err := add(s); err != nil {
			return err
		}
	}
	cfg.QPS, cfg.Burst = 50, 100
	if kube, err = client.New(cfg, client.Options{Scheme: s}); err != nil {
		return err
	}
	if clientset, err = kubernetes.NewForConfig(cfg); err != nil {
		return err
	}
	nodes, err := clientset.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list the nodes: %w", err)
	}
	for _, n := range nodes.Items {
		if cloudNode(n) {
			return fmt.Errorf("node %s of context %q runs at a cloud provider; refusing", n.Name, os.Getenv(envContext))
		}
	}
	restConfig = cfg
	return nil
}
