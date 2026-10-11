// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package providerconfig

import (
	"context"
	"regexp"
	"testing"
	"time"

	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/infrabay/provider-clickhouse/apis/v1alpha1"
	ch "github.com/infrabay/provider-clickhouse/internal/clients/clickhouse"
	"github.com/infrabay/provider-clickhouse/internal/clients/clickhouse/auth"
	"github.com/infrabay/provider-clickhouse/internal/reason"
)

func scheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.SchemeBuilder.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func secret(ns, name, key, value string) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Data: map[string][]byte{key: []byte(value)}}
}

func mr(ns, kind, pc string) *v1alpha1.Database {
	return &v1alpha1.Database{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "db", UID: "mr-uid"},
		Spec: v1alpha1.DatabaseSpec{ManagedResourceSpec: xpv2.ManagedResourceSpec{
			ProviderConfigReference: &xpv2.ProviderConfigReference{Kind: kind, Name: pc},
		}},
	}
}

func passwordCPC(policy v1alpha1.ClusterPolicy) *v1alpha1.ClusterProviderConfig {
	return &v1alpha1.ClusterProviderConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "main", UID: "cpc-uid", Generation: 1},
		Spec: v1alpha1.ClusterProviderConfigSpec{
			ConnectionSpec: v1alpha1.ConnectionSpec{Endpoint: v1alpha1.Endpoint{Host: "clickhouse.example.com"}, AllowInsecureTransport: true},
			Credentials: v1alpha1.ClusterCredentials{
				Source: v1alpha1.CredentialsSourcePasswordSecret,
				PasswordSecret: &v1alpha1.ClusterPasswordCredentials{
					Username:          "provider",
					PasswordSecretRef: xpv2.SecretKeySelector{SecretReference: xpv2.SecretReference{Namespace: "crossplane-system", Name: "pw"}, Key: "password"},
				},
			},
			Policy: policy,
		},
	}
}

func TestResolveClusterPassword(t *testing.T) {
	ctx := context.Background()
	kube := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(
		passwordCPC(v1alpha1.ClusterPolicy{AllowedNamespaces: []string{"team-.*"}, DatabaseNamePatterns: []string{"{namespace}_.*"}}),
		secret("crossplane-system", "pw", "password", "s3cret"),
	).Build()
	r := &APIResolver{Kube: kube, APIReader: kube}

	res, err := r.Resolve(ctx, mr("team-a", "ClusterProviderConfig", "main"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Config.Username != "provider" || res.Config.Password != "s3cret" || res.Config.TokenAuth() {
		t.Errorf("credentials = %q/%q", res.Config.Username, res.Config.Password)
	}
	if res.Config.Port != ch.DefaultNativePort || res.Config.TLS != nil || res.Config.DefaultDatabase != "default" {
		t.Errorf("connection = %+v", res.Config)
	}
	if res.Policy == nil || res.AdoptionAllowed || res.Endpoint.Addr() != "clickhouse.example.com:9000" {
		t.Errorf("policy/adoption/endpoint = %v/%t/%s", res.Policy, res.AdoptionAllowed, res.Endpoint.Addr())
	}
	if res.CacheKey.Identity != "ClusterProviderConfig//main" {
		t.Errorf("identity = %q", res.CacheKey.Identity)
	}

	// The key changes with the config generation and the Secret's
	// resourceVersion.
	cpc := &v1alpha1.ClusterProviderConfig{}
	_ = kube.Get(ctx, client.ObjectKey{Name: "main"}, cpc)
	cpc.Generation = 2
	_ = kube.Update(ctx, cpc)
	res2, _ := r.Resolve(ctx, mr("team-a", "ClusterProviderConfig", "main"))
	if res2.CacheKey.Version == res.CacheKey.Version {
		t.Error("the cache key must change with the generation")
	}
	s := &corev1.Secret{}
	_ = kube.Get(ctx, client.ObjectKey{Namespace: "crossplane-system", Name: "pw"}, s)
	s.Data["password"] = []byte("rotated")
	_ = kube.Update(ctx, s)
	res3, _ := r.Resolve(ctx, mr("team-a", "ClusterProviderConfig", "main"))
	if res3.CacheKey.Version == res2.CacheKey.Version || res3.Config.Password != "rotated" {
		t.Error("the cache key must change with the Secret's resourceVersion")
	}
}

func TestResolveClusterNamespacePolicy(t *testing.T) {
	ctx := context.Background()
	kube := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(
		passwordCPC(v1alpha1.ClusterPolicy{AllowedNamespaces: []string{"team-.*"}}),
		secret("crossplane-system", "pw", "password", "s3cret"),
	).Build()
	r := &APIResolver{Kube: kube, APIReader: kube}

	if _, err := r.Resolve(ctx, mr("default", "ClusterProviderConfig", "main")); !reason.Is(err, reason.PolicyViolation) {
		t.Fatalf("denied namespace: %v, want PolicyViolation", err)
	}
	deleting := mr("default", "ClusterProviderConfig", "main")
	now := metav1.NewTime(time.Now())
	deleting.SetDeletionTimestamp(&now)
	res, err := r.Resolve(ctx, deleting)
	if err != nil || !res.NamespaceDenied {
		t.Fatalf("denied namespace while deleting: %+v, %v; want NamespaceDenied", res, err)
	}
}

func TestResolveClusterErrors(t *testing.T) {
	ctx := context.Background()
	tlsOff := passwordCPC(v1alpha1.ClusterPolicy{Unrestricted: true})
	tlsOff.Spec.AllowInsecureTransport = false

	badPattern := passwordCPC(v1alpha1.ClusterPolicy{UserNamePatterns: []string{"{namespace}-x"}})
	badPattern.Name = "bad"

	tokenSkipVerify := &v1alpha1.ClusterProviderConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "skip"},
		Spec: v1alpha1.ClusterProviderConfigSpec{
			ConnectionSpec: v1alpha1.ConnectionSpec{Endpoint: v1alpha1.Endpoint{Host: "h"}},
			TLS:            &v1alpha1.ClusterTLS{TLSCommon: v1alpha1.TLSCommon{Enabled: true, InsecureSkipVerify: true}},
			Credentials: v1alpha1.ClusterCredentials{Source: v1alpha1.CredentialsSourceTokenFile,
				TokenFile: &v1alpha1.TokenFileCredentials{Path: "/var/run/secrets/clickhouse/token", Audience: "clickhouse"}},
			Policy: v1alpha1.ClusterPolicy{Unrestricted: true},
		},
	}
	saPath := tokenSkipVerify.DeepCopy()
	saPath.Name = "sa"
	saPath.Spec.TLS.InsecureSkipVerify = false
	saPath.Spec.Credentials.TokenFile.Path = auth.KubernetesTokenPath

	noSecret := passwordCPC(v1alpha1.ClusterPolicy{Unrestricted: true})
	noSecret.Name = "nosecret"
	noSecret.Spec.Credentials.PasswordSecret.PasswordSecretRef.Name = "missing"

	kube := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(tlsOff, badPattern, tokenSkipVerify, saPath, noSecret,
		secret("crossplane-system", "pw", "password", "s3cret")).Build()
	r := &APIResolver{Kube: kube, APIReader: kube}

	for name, pc := range map[string]string{
		"TLSRequired":       "main",
		"InvalidPattern":    "bad",
		"SkipVerifyToken":   "skip",
		"ServiceAccountTok": "sa",
		"MissingSecret":     "nosecret",
		"MissingConfig":     "nope",
	} {
		if _, err := r.Resolve(ctx, mr("team-a", "ClusterProviderConfig", pc)); !reason.Is(err, reason.InvalidConfig) {
			t.Errorf("%s: %v, want InvalidConfig", name, err)
		}
	}
	if _, err := r.Resolve(ctx, mr("team-a", "SomethingElse", "main")); !reason.Is(err, reason.InvalidConfig) {
		t.Errorf("invalid kind: %v", err)
	}
}

func TestResolveClusterTokenFileTLS(t *testing.T) {
	ctx := context.Background()
	pc := &v1alpha1.ClusterProviderConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "jwt"},
		Spec: v1alpha1.ClusterProviderConfigSpec{
			ConnectionSpec: v1alpha1.ConnectionSpec{Endpoint: v1alpha1.Endpoint{Host: "h"}},
			TLS:            &v1alpha1.ClusterTLS{TLSCommon: v1alpha1.TLSCommon{Enabled: true, ServerName: "clickhouse.example.com"}},
			Credentials: v1alpha1.ClusterCredentials{Source: v1alpha1.CredentialsSourceTokenFile,
				TokenFile: &v1alpha1.TokenFileCredentials{Path: "/var/run/secrets/clickhouse/token", Audience: "clickhouse"}},
			Policy: v1alpha1.ClusterPolicy{Unrestricted: true, AllowAdoption: true},
		},
	}
	kube := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(pc).Build()
	res, err := (&APIResolver{Kube: kube, APIReader: kube}).Resolve(ctx, mr("ns", "ClusterProviderConfig", "jwt"))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Config.TokenAuth() || res.Config.TLS == nil || res.Config.TLS.ServerName != "clickhouse.example.com" || res.Config.Port != ch.DefaultNativeTLSPort {
		t.Errorf("config = %+v", res.Config)
	}
	if !res.AdoptionAllowed || !res.Endpoint.TLS {
		t.Errorf("adoption/tls = %t/%t", res.AdoptionAllowed, res.Endpoint.TLS)
	}
}

func namespacedPC(ns string, creds v1alpha1.LocalCredentials) *v1alpha1.ProviderConfig {
	return &v1alpha1.ProviderConfig{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "own", UID: "pc-uid"},
		Spec: v1alpha1.ProviderConfigSpec{
			ConnectionSpec: v1alpha1.ConnectionSpec{Endpoint: v1alpha1.Endpoint{Host: "ch.team-a.svc"}, AllowInsecureTransport: true},
			Credentials:    creds,
		},
	}
}

func TestResolveNamespaced(t *testing.T) {
	ctx := context.Background()
	pw := v1alpha1.LocalCredentials{Source: v1alpha1.CredentialsSourcePasswordSecret, PasswordSecret: &v1alpha1.LocalPasswordCredentials{
		Username: "tenant", PasswordSecretRef: xpv2.LocalSecretKeySelector{LocalSecretReference: xpv2.LocalSecretReference{Name: "pw"}, Key: "password"},
	}}
	kube := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(
		namespacedPC("team-a", pw),
		// Only the Secret in the config's own namespace may be used.
		secret("team-a", "pw", "password", "tenant-secret"),
		secret("crossplane-system", "pw", "password", "admin-secret"),
	).Build()

	r := &APIResolver{Kube: kube, APIReader: kube, Flags: Flags{EnableNamespacedProviderConfigs: true}}
	res, err := r.Resolve(ctx, mr("team-a", "ProviderConfig", "own"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Config.Password != "tenant-secret" || res.Policy != nil || !res.AdoptionAllowed {
		t.Errorf("namespaced config: password %q policy %v adoption %t", res.Config.Password, res.Policy, res.AdoptionAllowed)
	}
	if res.CacheKey.Identity != "ProviderConfig/team-a/own" {
		t.Errorf("identity = %q", res.CacheKey.Identity)
	}
	// An MR in another namespace cannot use it.
	if _, err := r.Resolve(ctx, mr("team-b", "ProviderConfig", "own")); !reason.Is(err, reason.InvalidConfig) {
		t.Errorf("other namespace: %v", err)
	}

	disabled := &APIResolver{Kube: kube, APIReader: kube, Flags: Flags{EnableNamespacedProviderConfigs: false}}
	if _, err := disabled.Resolve(ctx, mr("team-a", "ProviderConfig", "own")); !reason.Is(err, reason.InvalidConfig) {
		t.Errorf("disabled by flag: %v", err)
	}
	restricted := &APIResolver{Kube: kube, APIReader: kube, Flags: Flags{
		EnableNamespacedProviderConfigs: true,
		NamespacedEndpointPattern:       regexp.MustCompile(`^(?:clickhouse-[a-z]+\.clickhouse\.svc:9440)$`),
	}}
	if _, err := restricted.Resolve(ctx, mr("team-a", "ProviderConfig", "own")); !reason.Is(err, reason.InvalidConfig) {
		t.Errorf("endpoint pattern: %v", err)
	}
}

func TestResolveNamespacedRefusesAmbientIdentity(t *testing.T) {
	ctx := context.Background()
	tf := namespacedPC("team-a", v1alpha1.LocalCredentials{Source: v1alpha1.CredentialsSourceTokenFile})
	kube := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(tf).Build()
	r := &APIResolver{Kube: kube, APIReader: kube, Flags: Flags{EnableNamespacedProviderConfigs: true}}
	if _, err := r.Resolve(ctx, mr("team-a", "ProviderConfig", "own")); !reason.Is(err, reason.InvalidConfig) {
		t.Errorf("TokenFile on a namespaced ProviderConfig: %v", err)
	}

	g := namespacedPC("team-a", v1alpha1.LocalCredentials{Source: v1alpha1.CredentialsSourceGoogleIDToken,
		GoogleIDToken: &v1alpha1.LocalGoogleIDTokenCredentials{Audience: "https://clickhouse.example.com"}})
	g.Name = "own"
	kube2 := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(g).Build()
	r2 := &APIResolver{Kube: kube2, APIReader: kube2, Flags: Flags{EnableNamespacedProviderConfigs: true}}
	if _, err := r2.Resolve(ctx, mr("team-a", "ProviderConfig", "own")); !reason.Is(err, reason.InvalidConfig) {
		t.Errorf("GoogleIDToken without a key on a namespaced ProviderConfig: %v", err)
	}
}

func TestCompileEndpointPattern(t *testing.T) {
	if p, err := CompileEndpointPattern(".*"); p != nil || err != nil {
		t.Errorf("default pattern = %v, %v", p, err)
	}
	p, err := CompileEndpointPattern(`ch-[a-z]+:9440`)
	if err != nil || !p.MatchString("ch-a:9440") || p.MatchString("xch-a:9440") {
		t.Errorf("pattern must be anchored: %v", err)
	}
	if _, err := CompileEndpointPattern("("); err == nil {
		t.Error("invalid pattern must fail")
	}
}
