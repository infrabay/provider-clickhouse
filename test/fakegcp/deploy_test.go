// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package fakegcp

import (
	"os"
	"slices"
	"strings"
	"testing"

	pkgv1beta1 "github.com/crossplane/crossplane/apis/v2/pkg/v1beta1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	"github.com/infrabay/provider-clickhouse/apis/v1alpha1"
	"github.com/infrabay/provider-clickhouse/internal/controller/providerconfig"
)

// manifests decodes every document of a file under deploy/ strictly (unknown
// fields fail) into the type its kind maps to.
func manifests(t *testing.T, file string) []any {
	t.Helper()
	b, err := os.ReadFile("deploy/" + file) //nolint:gosec // Test fixture path.
	if err != nil {
		t.Fatal(err)
	}
	var out []any
	for doc := range strings.SplitSeq(string(b), "\n---\n") {
		var meta struct {
			Kind string `json:"kind"`
		}
		if err := yaml.Unmarshal([]byte(doc), &meta); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		var obj any
		switch meta.Kind {
		case "Namespace":
			obj = &corev1.Namespace{}
		case "ServiceAccount":
			obj = &corev1.ServiceAccount{}
		case "Service":
			obj = &corev1.Service{}
		case "ConfigMap":
			obj = &corev1.ConfigMap{}
		case "Deployment":
			obj = &appsv1.Deployment{}
		case "DeploymentRuntimeConfig":
			obj = &pkgv1beta1.DeploymentRuntimeConfig{}
		case "ClusterProviderConfig":
			obj = &v1alpha1.ClusterProviderConfig{}
		default:
			t.Fatalf("%s: unexpected kind %q", file, meta.Kind)
		}
		if err := yaml.UnmarshalStrict([]byte(doc), obj); err != nil {
			t.Fatalf("%s: %s: %v", file, meta.Kind, err)
		}
		out = append(out, obj)
	}
	return out
}

func only[T any](t *testing.T, objs []any, keep func(T) bool) T {
	t.Helper()
	var found []T
	for _, o := range objs {
		if v, ok := o.(T); ok && keep(v) {
			found = append(found, v)
		}
	}
	if len(found) != 1 {
		var zero T
		t.Fatalf("found %d objects of type %T, want 1", len(found), zero)
	}
	return found[0]
}

func all[T any](T) bool { return true }

// TestDeployManifests checks that the Kubernetes manifests under deploy/
// decode into their types and agree with each other: the fake's Service in
// GCE_METADATA_HOST and in jwks_uri, the audience of the processor and of the
// ClusterProviderConfig, the provider's service account in the fake's --email
// flag and in users.d, and the processor in users.d and in the policy.
func TestDeployManifests(t *testing.T) {
	fake := manifests(t, "fakegcp.yaml")
	clickhouse := manifests(t, "clickhouse-google-jwt.yaml")
	provider := manifests(t, "provider-runtime.yaml")

	// The fake: one replica (state is in memory), the control API on its own
	// Service, the metadata server and the JWKS on port 80.
	dep := only(t, fake, all[*appsv1.Deployment])
	if dep.Spec.Replicas == nil || *dep.Spec.Replicas != 1 {
		t.Errorf("fakegcp Deployment: replicas %v, want 1", dep.Spec.Replicas)
	}
	ctr := dep.Spec.Template.Spec.Containers[0]
	flags := map[string]string{}
	for _, a := range ctr.Args {
		k, v, _ := strings.Cut(strings.TrimPrefix(a, "--"), "=")
		flags[k] = v
	}
	if flags["listen"] != ":8080" || flags["control-listen"] != ":8081" {
		t.Errorf("fakegcp args %v", ctr.Args)
	}
	ports := map[string]int32{}
	for _, p := range ctr.Ports {
		ports[p.Name] = p.ContainerPort
	}
	if ports["metadata"] != 8080 || ports["control"] != 8081 {
		t.Errorf("container ports %v", ports)
	}
	svc := only(t, fake, func(s *corev1.Service) bool { return s.Name == "fakegcp" })
	if p := svc.Spec.Ports; len(p) != 1 || p[0].Port != 80 || p[0].TargetPort.String() != "metadata" {
		t.Errorf("Service fakegcp ports %+v, want 80 -> metadata only", p)
	}
	ctl := only(t, fake, func(s *corev1.Service) bool { return s.Name == "fakegcp-control" })
	if p := ctl.Spec.Ports; len(p) != 1 || p[0].TargetPort.String() != "control" {
		t.Errorf("Service fakegcp-control ports %+v, want -> control only", p)
	}
	host := svc.Name + "." + svc.Namespace + ".svc.cluster.local"

	// ClickHouse: the processor against the fake, and the provider's
	// service account pinned to it.
	cm := only(t, clickhouse, all[*corev1.ConfigMap])
	var tp struct {
		TokenProcessors map[string]map[string]any `json:"token_processors"`
	}
	if err := yaml.Unmarshal([]byte(cm.Data["token_processors.yaml"]), &tp); err != nil {
		t.Fatal(err)
	}
	proc, ok := tp.TokenProcessors[GoogleProcessorName]
	if !ok || len(tp.TokenProcessors) != 1 {
		t.Fatalf("token processors %v, want only %s", tp.TokenProcessors, GoogleProcessorName)
	}
	if got, want := proc["jwks_uri"], "http://"+host+JWKSPath; got != want {
		t.Errorf("jwks_uri %v, want %s", got, want)
	}
	var users struct {
		Users map[string]struct {
			JWT struct {
				Processor string `json:"processor"`
			} `json:"jwt"`
		} `json:"users"`
	}
	if err := yaml.Unmarshal([]byte(cm.Data["provider-clickhouse.yaml"]), &users); err != nil {
		t.Fatal(err)
	}
	u, ok := users.Users[flags["email"]]
	if !ok || len(users.Users) != 1 {
		t.Fatalf("users.d users %v, want only the fake's --email %q", users.Users, flags["email"])
	}
	if u.JWT.Processor != GoogleProcessorName {
		t.Errorf("users.d user is pinned to processor %q, want %q", u.JWT.Processor, GoogleProcessorName)
	}

	// The provider: GCE_METADATA_HOST names the fake's Service (port 80, so
	// no port), and the ClusterProviderConfig asks for the processor's
	// audience and allows the processor.
	drc := only(t, provider, all[*pkgv1beta1.DeploymentRuntimeConfig])
	var env []corev1.EnvVar
	for _, c := range drc.Spec.DeploymentTemplate.Spec.Template.Spec.Containers {
		if c.Name == "package-runtime" {
			env = c.Env
		}
	}
	if i := slices.IndexFunc(env, func(e corev1.EnvVar) bool { return e.Name == "GCE_METADATA_HOST" }); i < 0 || env[i].Value != host {
		t.Errorf("package-runtime env %v, want GCE_METADATA_HOST=%s", env, host)
	}
	cpc := only(t, provider, all[*v1alpha1.ClusterProviderConfig])
	creds := cpc.Spec.Credentials
	if creds.Source != v1alpha1.CredentialsSourceGoogleIDToken || creds.GoogleIDToken == nil || creds.GoogleIDToken.CredentialsSecretRef != nil {
		t.Fatalf("credentials %+v, want GoogleIDToken without a key", creds)
	}
	if got := creds.GoogleIDToken.Audience; got != proc["expected_audience"] {
		t.Errorf("audience %q, processor expects %v", got, proc["expected_audience"])
	}
	policy, err := providerconfig.CompilePolicy(cpc.Spec.Policy)
	if err != nil {
		t.Fatal(err)
	}
	if err := policy.CheckJWTProcessor(GoogleProcessorName); err != nil {
		t.Error(err)
	}
	if err := policy.CheckAuthentication(v1alpha1.AuthenticationMethodJWT); err != nil {
		t.Error(err)
	}
	if err := policy.CheckUserName("team-a", "app-api@example-project.iam.gserviceaccount.com"); err != nil {
		t.Error(err)
	}
}
