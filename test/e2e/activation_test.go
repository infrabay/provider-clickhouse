// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	xpextv1alpha1 "github.com/crossplane/crossplane/apis/v2/apiextensions/v1alpha1"
)

// The provider's managed resource kinds (plural names of their CRDs and
// ManagedResourceDefinitions) and its other kinds.
var (
	mrKinds     = []string{"databases", "users", "roles", "grants", "rolegrants"}
	configKinds = []string{"providerconfigs", "clusterproviderconfigs", "providerconfigusages"}
)

// crdEstablished reports whether a CRD exists and is established.
func crdEstablished(name string) (exists, established bool) {
	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := kube.Get(context.Background(), client.ObjectKey{Name: name}, crd); err != nil {
		return false, false
	}
	for _, c := range crd.Status.Conditions {
		if c.Type == apiextensionsv1.Established {
			return true, c.Status == apiextensionsv1.ConditionTrue
		}
	}
	return true, false
}

// mrdState returns the state of a ManagedResourceDefinition and whether its
// CRD is established according to its status.
func mrdState(t *testing.T, name string) (state xpextv1alpha1.ManagedResourceDefinitionState, established bool) {
	t.Helper()
	mrd := &xpextv1alpha1.ManagedResourceDefinition{}
	if err := kube.Get(context.Background(), client.ObjectKey{Name: name}, mrd); err != nil {
		if kerrors.IsNotFound(err) {
			return "", false
		}
		t.Fatal(err)
	}
	for _, c := range mrd.Status.Conditions {
		if string(c.Type) == "Established" {
			established = c.Status == "True"
		}
	}
	return mrd.Spec.State, established
}

// configKindsActivated reports whether Crossplane turns the ProviderConfig
// kinds of a safe-start provider into ManagedResourceDefinitions that must
// be activated like the managed resource kinds: 2.0.0 and 2.0.1 do, later
// releases install them as CRDs. Without a version, the cluster tells.
func configKindsActivated(t *testing.T) bool {
	t.Helper()
	switch v := strings.TrimPrefix(crossplaneVersion, "v"); {
	case v == "":
		state, _ := mrdState(t, configKinds[0]+"."+group)
		return state != ""
	case v == "2.0.0", v == "2.0.1", strings.HasPrefix(v, "2.0.0-"), strings.HasPrefix(v, "2.0.1-"):
		return true
	}
	return false
}

// testActivation checks safe-start (design 3.1, examples/mrap.yaml): the
// package installs a ManagedResourceDefinition per managed resource kind,
// which stays inactive, and no CRD, until a ManagedResourceActivationPolicy
// activates it. The ProviderConfig kinds are installed right away, except on
// Crossplane 2.0.0 and 2.0.1, which activate them like the managed resource
// kinds; the provider's config controllers wait for their CRDs there.
func testActivation(t *testing.T) {
	gated := slices.Clone(mrKinds)
	if configKindsActivated(t) {
		t.Logf("Crossplane %s installs the ProviderConfig kinds as ManagedResourceDefinitions", crossplaneVersion)
		gated = append(gated, configKinds...)
	} else {
		for _, k := range configKinds {
			if _, ok := crdEstablished(k + "." + group); !ok {
				t.Fatalf("CRD %s.%s is not established after the package was installed", k, group)
			}
			if state, _ := mrdState(t, k+"."+group); state != "" {
				t.Fatalf("Crossplane %s installed %s.%s as a ManagedResourceDefinition", crossplaneVersion, k, group)
			}
		}
	}
	for _, k := range gated {
		name := k + "." + group
		eventually(t, 2*time.Minute, "ManagedResourceDefinition "+name, func() (bool, string) {
			state, _ := mrdState(t, name)
			return state != "", "not found"
		})
		if state, _ := mrdState(t, name); state != xpextv1alpha1.ManagedResourceDefinitionInactive {
			t.Fatalf("ManagedResourceDefinition %s is %q before any activation policy, want Inactive", name, state)
		}
		if exists, _ := crdEstablished(name); exists {
			t.Fatalf("CRD %s exists before any activation policy", name)
		}
	}

	// One kind first: only its CRD appears.
	create(t, &xpextv1alpha1.ManagedResourceActivationPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "xpch-e2e-databases"},
		Spec:       xpextv1alpha1.ManagedResourceActivationPolicySpec{Activations: []xpextv1alpha1.ActivationPolicy{"databases." + group}},
	})
	waitActive(t, "databases")
	time.Sleep(5 * time.Second)
	for _, k := range gated[1:] {
		name := k + "." + group
		if state, _ := mrdState(t, name); state != xpextv1alpha1.ManagedResourceDefinitionInactive {
			t.Errorf("ManagedResourceDefinition %s is %q although only databases were activated", name, state)
		}
		if exists, _ := crdEstablished(name); exists {
			t.Errorf("CRD %s exists although only databases were activated", name)
		}
	}

	// Then every kind, with the policy of examples/mrap.yaml.
	b, err := os.ReadFile(filepath.Join("..", "..", "examples", "mrap.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	mrap := &xpextv1alpha1.ManagedResourceActivationPolicy{}
	if err := yaml.UnmarshalStrict(b, mrap); err != nil {
		t.Fatalf("examples/mrap.yaml: %v", err)
	}
	create(t, mrap)
	for _, k := range gated {
		waitActive(t, k)
	}

	// The provider's runtime (Crossplane 2.1 and later start it only once a
	// kind is active) pulls the package from the registry's node-local
	// address and becomes ready. On 2.0 it has run since the package was
	// installed, and must not have restarted while its kinds were inactive.
	pod := readyPod(t, nsCrossplane, "pkg.crossplane.io/provider="+providerName)
	t.Logf("provider pod %s is ready", pod)
	if n := providerRestarts(t); n != 0 {
		t.Errorf("the provider container restarted %d times", n)
	}
}

// providerRestarts returns the restart count of the provider's containers.
func providerRestarts(t *testing.T) int32 {
	t.Helper()
	pods, err := clientset.CoreV1().Pods(nsCrossplane).List(context.Background(), metav1.ListOptions{LabelSelector: "pkg.crossplane.io/provider=" + providerName})
	if err != nil {
		t.Fatal(err)
	}
	var n int32
	for _, p := range pods.Items {
		for _, c := range p.Status.ContainerStatuses {
			n += c.RestartCount
		}
	}
	return n
}

// waitActive waits until the ManagedResourceDefinition of a kind is active
// and established and its CRD is established.
func waitActive(t *testing.T, kind string) {
	t.Helper()
	name := kind + "." + group
	eventually(t, 2*time.Minute, name+" active", func() (bool, string) {
		state, mrdEstablished := mrdState(t, name)
		exists, established := crdEstablished(name)
		return state == xpextv1alpha1.ManagedResourceDefinitionActive && mrdEstablished && exists && established,
			"mrd state " + string(state) + ", mrd established " + boolString(mrdEstablished) + ", crd established " + boolString(established)
	})
}

func boolString(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
