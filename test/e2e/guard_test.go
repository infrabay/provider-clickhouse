// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

// This file has no build tag: the guard that keeps the end-to-end tests on
// local clusters is unit-tested by go test ./... as well.

package e2e

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"path"
	"strings"
	"testing"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	envContext    = "E2E_KUBE_CONTEXT"
	envKubeconfig = "E2E_KUBECONFIG"
)

// allowedContexts are the local cluster contexts the tests may use (the same
// list as hack/e2e/lib.sh).
var allowedContexts = []string{"orbstack", "docker-desktop", "minikube", "rancher-desktop", "kind-*", "k3d-*"}

// loadConfig returns the REST config of the named context, which must be
// set, match allowedContexts and point at a local API server. The current
// context of the kubeconfig is never used.
func loadConfig(name, kubeconfig string) (*rest.Config, error) {
	if name == "" {
		return nil, errors.New(envContext + " is not set: run make e2e E2E_KUBE_CONTEXT=<local cluster context> (the current context is never used)")
	}
	if !contextAllowed(name) {
		return nil, fmt.Errorf("context %q is not a local cluster context (allowed: %s)", name, strings.Join(allowedContexts, ", "))
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		rules.ExplicitPath = kubeconfig
	}
	raw, err := rules.Load()
	if err != nil {
		return nil, err
	}
	kctx, ok := raw.Contexts[name]
	if !ok {
		return nil, fmt.Errorf("context %q does not exist in the kubeconfig", name)
	}
	cluster, ok := raw.Clusters[kctx.Cluster]
	if !ok || !serverLocal(cluster.Server) {
		return nil, fmt.Errorf("context %q does not point at a local API server; refusing", name)
	}
	return clientcmd.NewNonInteractiveClientConfig(*raw, name, &clientcmd.ConfigOverrides{}, rules).ClientConfig()
}

func contextAllowed(name string) bool {
	for _, p := range allowedContexts {
		if ok, _ := path.Match(p, name); ok {
			return true
		}
	}
	return false
}

// serverLocal reports whether an API server URL is on this machine or on a
// private network.
func serverLocal(server string) bool {
	u, err := url.Parse(server)
	if err != nil {
		return false
	}
	host := u.Hostname()
	switch {
	case host == "localhost", host == "host.docker.internal", host == "kubernetes.docker.internal", strings.HasSuffix(host, ".orb.local"):
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified())
}

func TestContextGuard(t *testing.T) {
	for _, c := range []struct {
		name string
		want bool
	}{
		{"orbstack", true}, {"kind-xpch-e2e", true}, {"k3d-dev", true}, {"docker-desktop", true},
		{"gke_example-project_us-central1_cluster", false}, {"arn:aws:eks:us-east-1:111122223333:cluster/example", false}, {"", false}, {"prod", false}, {"kind", false},
	} {
		if got := contextAllowed(c.name); got != c.want {
			t.Errorf("contextAllowed(%q) = %t, want %t", c.name, got, c.want)
		}
	}
	for _, c := range []struct {
		server string
		want   bool
	}{
		{"https://127.0.0.1:26443", true}, {"https://[::1]:6443", true}, {"https://k8s.orb.local", true},
		{"https://192.168.49.2:8443", true}, {"https://34.1.2.3", false}, {"https://cluster.example.com", false},
	} {
		if got := serverLocal(c.server); got != c.want {
			t.Errorf("serverLocal(%q) = %t, want %t", c.server, got, c.want)
		}
	}
	if _, err := loadConfig("", ""); err == nil {
		t.Error("loadConfig without a context name succeeded")
	}
}
