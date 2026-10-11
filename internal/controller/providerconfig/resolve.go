// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

// Package providerconfig resolves the ProviderConfig or ClusterProviderConfig
// of a managed resource into a connection configuration, a cache key and the
// effective policy.
package providerconfig

import (
	"context"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"

	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/crossplane/crossplane-runtime/v2/pkg/meta"
	"github.com/crossplane/crossplane-runtime/v2/pkg/resource"

	"github.com/infrabay/provider-clickhouse/apis/v1alpha1"
	ch "github.com/infrabay/provider-clickhouse/internal/clients/clickhouse"
	"github.com/infrabay/provider-clickhouse/internal/clients/clickhouse/auth"
	"github.com/infrabay/provider-clickhouse/internal/reason"
)

// Flags are the provider flags that govern ProviderConfigs.
type Flags struct {
	// EnableNamespacedProviderConfigs allows MRs to use namespaced
	// ProviderConfigs. When false they are refused with InvalidConfig.
	EnableNamespacedProviderConfigs bool

	// NamespacedEndpointPattern restricts the host:port that namespaced
	// ProviderConfigs may target. nil allows any.
	NamespacedEndpointPattern *regexp.Regexp

	// KubernetesAPIAudiences are the audiences of the provider's own
	// Kubernetes API token. Tokens carrying any of them are never sent.
	KubernetesAPIAudiences []string
}

// CompileEndpointPattern compiles the --namespaced-endpoint-pattern flag
// (implicitly anchored).
func CompileEndpointPattern(p string) (*regexp.Regexp, error) {
	if p == "" || p == ".*" {
		return nil, nil
	}
	return regexp.Compile(`^(?:` + p + `)$`)
}

// Endpoint is the published endpoint of a config.
type Endpoint struct {
	Host     string
	Port     int
	Protocol v1alpha1.Protocol
	TLS      bool
}

// Resolved is a resolved ProviderConfig.
type Resolved struct {
	// Kind is ProviderConfig or ClusterProviderConfig.
	Kind string
	// Name of the config.
	Name string

	// NamespaceDenied is set when a ClusterProviderConfig's
	// allowedNamespaces does not allow the MR's namespace and the MR is being
	// deleted. The MR is then orphaned: no connection is made, and nothing
	// else in Resolved is set.
	NamespaceDenied bool

	// CacheKey identifies the connection pool.
	CacheKey ch.Key
	// Config is the connection configuration.
	Config ch.Config
	// Endpoint as published in connection Secrets.
	Endpoint Endpoint

	// Policy is the compiled policy of a ClusterProviderConfig, or nil for a
	// namespaced ProviderConfig (which has none).
	Policy *Policy
	// AdoptionAllowed: always true for a namespaced ProviderConfig, and
	// policy.allowAdoption for a ClusterProviderConfig.
	AdoptionAllowed bool
	// Cluster from spec.cluster, or "".
	Cluster string
}

// A Resolver resolves the config of a managed resource.
type Resolver interface {
	Resolve(ctx context.Context, mg resource.ModernManaged) (*Resolved, error)
}

// APIResolver resolves configs through the Kubernetes API.
type APIResolver struct {
	// Kube reads configs and the password, CA and Google key Secrets.
	Kube client.Reader
	// APIReader is an uncached reader for token Secrets.
	APIReader client.Reader
	Flags     Flags
}

// Resolve implements Resolver.
func (r *APIResolver) Resolve(ctx context.Context, mg resource.ModernManaged) (*Resolved, error) {
	ref := mg.GetProviderConfigReference()
	if ref == nil || ref.Name == "" {
		return nil, reason.Errorf(reason.InvalidConfig, "providerConfigRef is not set")
	}
	switch ref.Kind {
	case v1alpha1.ClusterProviderConfigKind:
		return r.resolveCluster(ctx, mg, ref.Name)
	case v1alpha1.ProviderConfigKind:
		return r.resolveNamespaced(ctx, mg, ref.Name)
	default:
		return nil, reason.Errorf(reason.InvalidConfig, "unsupported providerConfigRef kind %q (want %s or %s)",
			ref.Kind, v1alpha1.ProviderConfigKind, v1alpha1.ClusterProviderConfigKind)
	}
}

func (r *APIResolver) resolveCluster(ctx context.Context, mg resource.ModernManaged, name string) (*Resolved, error) {
	pc := &v1alpha1.ClusterProviderConfig{}
	if err := r.Kube.Get(ctx, types.NamespacedName{Name: name}, pc); err != nil {
		return nil, getError(v1alpha1.ClusterProviderConfigKind, name, err)
	}
	policy, err := CompilePolicy(pc.Spec.Policy)
	if err != nil {
		return nil, fmt.Errorf("ClusterProviderConfig %q: %w", name, err)
	}
	ns := mg.GetNamespace()
	if !policy.NamespaceAllowed(ns) {
		if meta.WasDeleted(mg) {
			return &Resolved{Kind: v1alpha1.ClusterProviderConfigKind, Name: name, NamespaceDenied: true}, nil
		}
		return nil, reason.Errorf(reason.PolicyViolation,
			"namespace %q is not in policy.allowedNamespaces of ClusterProviderConfig %q", ns, name)
	}

	b := &builder{kind: v1alpha1.ClusterProviderConfigKind, name: name}
	b.versions = append(b.versions, string(pc.GetUID()), strconv.FormatInt(pc.GetGeneration(), 10))
	if err := b.connection(pc.Spec.ConnectionSpec, pc.Spec.TLS != nil && pc.Spec.TLS.Enabled); err != nil {
		return nil, err
	}
	if t := pc.Spec.TLS; t != nil && t.Enabled {
		var ca []byte
		if t.CACertSecretRef != nil {
			if ca, err = b.secret(ctx, r.Kube, t.CACertSecretRef.Namespace, t.CACertSecretRef.Name, t.CACertSecretRef.Key); err != nil {
				return nil, err
			}
		}
		if err := b.tls(t.TLSCommon, ca); err != nil {
			return nil, err
		}
	}

	c := pc.Spec.Credentials
	checker := func(aud string) auth.Checker {
		return auth.Checker{Audience: aud, Forbidden: r.Flags.KubernetesAPIAudiences}
	}
	switch c.Source {
	case v1alpha1.CredentialsSourcePasswordSecret:
		if c.PasswordSecret == nil {
			return nil, missingBlock(c.Source)
		}
		s := c.PasswordSecret.PasswordSecretRef
		pw, err := b.secret(ctx, r.Kube, s.Namespace, s.Name, s.Key)
		if err != nil {
			return nil, err
		}
		b.cfg.Username, b.cfg.Password = c.PasswordSecret.Username, string(pw)
	case v1alpha1.CredentialsSourceTokenSecret:
		if c.TokenSecret == nil {
			return nil, missingBlock(c.Source)
		}
		s := c.TokenSecret.SecretRef
		b.cfg.TokenSource = auth.NewSecretSource(r.APIReader, types.NamespacedName{Namespace: s.Namespace, Name: s.Name}, s.Key, checker(c.TokenSecret.Audience))
	case v1alpha1.CredentialsSourceGoogleIDToken:
		if c.GoogleIDToken == nil {
			return nil, missingBlock(c.Source)
		}
		var key []byte
		if s := c.GoogleIDToken.CredentialsSecretRef; s != nil {
			if key, err = b.secret(ctx, r.Kube, s.Namespace, s.Name, s.Key); err != nil {
				return nil, err
			}
		}
		ts, err := auth.NewGoogleSource(c.GoogleIDToken.Audience, key)
		if err != nil {
			return nil, err
		}
		b.cfg.TokenSource = ts
	case v1alpha1.CredentialsSourceTokenFile:
		if c.TokenFile == nil {
			return nil, missingBlock(c.Source)
		}
		ts, err := auth.NewFileSource(c.TokenFile.Path, checker(c.TokenFile.Audience))
		if err != nil {
			return nil, err
		}
		b.cfg.TokenSource = ts
	default:
		return nil, reason.Errorf(reason.InvalidConfig, "unsupported credentials source %q", c.Source)
	}
	if err := b.finish(pc.Spec.ConnectionSpec, pc.Spec.TLS != nil && pc.Spec.TLS.InsecureSkipVerify); err != nil {
		return nil, err
	}
	res := b.resolved()
	res.Policy = policy
	res.AdoptionAllowed = policy.AdoptionAllowed()
	return res, nil
}

func (r *APIResolver) resolveNamespaced(ctx context.Context, mg resource.ModernManaged, name string) (*Resolved, error) {
	if !r.Flags.EnableNamespacedProviderConfigs {
		return nil, reason.Errorf(reason.InvalidConfig, "namespaced ProviderConfigs are disabled by the provider flag --enable-namespaced-provider-configs=false; use a ClusterProviderConfig")
	}
	ns := mg.GetNamespace()
	pc := &v1alpha1.ProviderConfig{}
	if err := r.Kube.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, pc); err != nil {
		return nil, getError(v1alpha1.ProviderConfigKind, ns+"/"+name, err)
	}

	b := &builder{kind: v1alpha1.ProviderConfigKind, namespace: ns, name: name}
	b.versions = append(b.versions, string(pc.GetUID()), strconv.FormatInt(pc.GetGeneration(), 10))
	if err := b.connection(pc.Spec.ConnectionSpec, pc.Spec.TLS != nil && pc.Spec.TLS.Enabled); err != nil {
		return nil, err
	}
	if p := r.Flags.NamespacedEndpointPattern; p != nil && !p.MatchString(b.cfg.Addr()) {
		return nil, reason.Errorf(reason.InvalidConfig, "endpoint %s of ProviderConfig %s/%s is not allowed by the provider flag --namespaced-endpoint-pattern", b.cfg.Addr(), ns, name)
	}
	var err error
	if t := pc.Spec.TLS; t != nil && t.Enabled {
		var ca []byte
		if t.CACertSecretRef != nil {
			// Secrets of a namespaced ProviderConfig are always read from its
			// own namespace.
			if ca, err = b.secret(ctx, r.Kube, ns, t.CACertSecretRef.Name, t.CACertSecretRef.Key); err != nil {
				return nil, err
			}
		}
		if err := b.tls(t.TLSCommon, ca); err != nil {
			return nil, err
		}
	}

	c := pc.Spec.Credentials
	checker := func(aud string) auth.Checker {
		return auth.Checker{Audience: aud, Forbidden: r.Flags.KubernetesAPIAudiences}
	}
	switch c.Source {
	case v1alpha1.CredentialsSourcePasswordSecret:
		if c.PasswordSecret == nil {
			return nil, missingBlock(c.Source)
		}
		s := c.PasswordSecret.PasswordSecretRef
		pw, err := b.secret(ctx, r.Kube, ns, s.Name, s.Key)
		if err != nil {
			return nil, err
		}
		b.cfg.Username, b.cfg.Password = c.PasswordSecret.Username, string(pw)
	case v1alpha1.CredentialsSourceTokenSecret:
		if c.TokenSecret == nil {
			return nil, missingBlock(c.Source)
		}
		s := c.TokenSecret.SecretRef
		b.cfg.TokenSource = auth.NewSecretSource(r.APIReader, types.NamespacedName{Namespace: ns, Name: s.Name}, s.Key, checker(c.TokenSecret.Audience))
	case v1alpha1.CredentialsSourceGoogleIDToken:
		// A credentials Secret is mandatory: ambient Google identity is not
		// available to tenants.
		if c.GoogleIDToken == nil || c.GoogleIDToken.CredentialsSecretRef.Name == "" {
			return nil, reason.Errorf(reason.InvalidConfig, "GoogleIDToken on a namespaced ProviderConfig requires credentialsSecretRef")
		}
		s := c.GoogleIDToken.CredentialsSecretRef
		key, err := b.secret(ctx, r.Kube, ns, s.Name, s.Key)
		if err != nil {
			return nil, err
		}
		ts, err := auth.NewGoogleSource(c.GoogleIDToken.Audience, key)
		if err != nil {
			return nil, err
		}
		b.cfg.TokenSource = ts
	case v1alpha1.CredentialsSourceTokenFile:
		return nil, reason.Errorf(reason.InvalidConfig, "TokenFile is only allowed on ClusterProviderConfig")
	default:
		return nil, reason.Errorf(reason.InvalidConfig, "unsupported credentials source %q", c.Source)
	}
	if err := b.finish(pc.Spec.ConnectionSpec, pc.Spec.TLS != nil && pc.Spec.TLS.InsecureSkipVerify); err != nil {
		return nil, err
	}
	res := b.resolved()
	res.AdoptionAllowed = true
	return res, nil
}

type builder struct {
	kind, namespace, name string
	cfg                   ch.Config
	tlsOn                 bool
	cluster               string
	versions              []string
}

func (b *builder) connection(cs v1alpha1.ConnectionSpec, tlsOn bool) error {
	b.tlsOn = tlsOn
	host := strings.TrimSpace(cs.Endpoint.Host)
	if host == "" {
		return reason.Errorf(reason.InvalidConfig, "spec.endpoint.host is empty")
	}
	port := ch.DefaultNativePort
	if tlsOn {
		port = ch.DefaultNativeTLSPort
	}
	if cs.Endpoint.Port != nil {
		port = int(*cs.Endpoint.Port)
	}
	protocol := cs.Protocol
	if protocol == "" {
		protocol = v1alpha1.ProtocolNative
	}
	if protocol != v1alpha1.ProtocolNative {
		return reason.Errorf(reason.InvalidConfig, "unsupported protocol %q", protocol)
	}
	db := cs.DefaultDatabase
	if db == "" {
		db = "default"
	}
	b.cfg = ch.Config{
		Host:            host,
		Port:            port,
		Protocol:        ch.ProtocolNative,
		DefaultDatabase: db,
		MaxOpenConns:    ch.DefaultMaxOpenConns,
		ConnMaxLifetime: ch.DefaultConnMaxLifetime,
	}
	if p := cs.Pool; p != nil {
		if p.MaxOpenConns != nil {
			b.cfg.MaxOpenConns = int(*p.MaxOpenConns)
		}
		if p.ConnMaxLifetime != nil && p.ConnMaxLifetime.Duration > 0 {
			b.cfg.ConnMaxLifetime = p.ConnMaxLifetime.Duration
		}
	}
	if cs.Cluster != nil {
		b.cluster = *cs.Cluster
	}
	return nil
}

func (b *builder) tls(t v1alpha1.TLSCommon, ca []byte) error {
	cfg, err := ch.NewTLSConfig(ch.TLSOptions{ServerName: t.ServerName, InsecureSkipVerify: t.InsecureSkipVerify, CAPEM: ca})
	if err != nil {
		return err
	}
	b.cfg.TLS = cfg
	return nil
}

// secret reads a Secret key and records its resourceVersion in the cache key.
func (b *builder) secret(ctx context.Context, kube client.Reader, namespace, name, key string) ([]byte, error) {
	if namespace == "" || name == "" || key == "" {
		return nil, reason.Errorf(reason.InvalidConfig, "incomplete Secret reference (namespace %q, name %q, key %q)", namespace, name, key)
	}
	s := &corev1.Secret{}
	if err := kube.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, s); err != nil {
		if kerrors.IsNotFound(err) {
			return nil, reason.Errorf(reason.InvalidConfig, "Secret %s/%s not found", namespace, name)
		}
		return nil, fmt.Errorf("cannot get Secret %s/%s: %w", namespace, name, err)
	}
	v, ok := s.Data[key]
	if !ok || len(v) == 0 {
		return nil, reason.Errorf(reason.InvalidConfig, "Secret %s/%s has no key %q or it is empty", namespace, name, key)
	}
	b.versions = append(b.versions, namespace+"/"+name+"@"+s.GetResourceVersion())
	return v, nil
}

// finish enforces the transport rules in Go as well as in CEL: TLS is
// required unless allowInsecureTransport is set, and insecureSkipVerify is
// refused with tokens.
func (b *builder) finish(cs v1alpha1.ConnectionSpec, insecureSkipVerify bool) error {
	if !b.tlsOn && !cs.AllowInsecureTransport {
		return reason.Errorf(reason.InvalidConfig, "tls.enabled must be true unless allowInsecureTransport is true: the native protocol sends the password or token in clear text")
	}
	if insecureSkipVerify && b.cfg.TokenAuth() {
		return reason.Errorf(reason.InvalidConfig, "insecureSkipVerify is not allowed with token credentials")
	}
	return b.cfg.Validate()
}

func (b *builder) resolved() *Resolved {
	identity := b.kind + "/" + b.namespace + "/" + b.name
	return &Resolved{
		Kind:     b.kind,
		Name:     b.name,
		CacheKey: ch.Key{Identity: identity, Version: strings.Join(b.versions, ";")},
		Config:   b.cfg,
		Endpoint: Endpoint{Host: b.cfg.Host, Port: b.cfg.Port, Protocol: v1alpha1.ProtocolNative, TLS: b.tlsOn},
		Cluster:  b.cluster,
	}
}

func missingBlock(s v1alpha1.CredentialsSource) error {
	return reason.Errorf(reason.InvalidConfig, "credentials.source is %s but the matching block is not set", s)
}

func getError(kind, name string, err error) error {
	if kerrors.IsNotFound(err) {
		return reason.Errorf(reason.InvalidConfig, "%s %q not found", kind, name)
	}
	return fmt.Errorf("cannot get %s %q: %w", kind, name, err)
}

// SameConfig reports whether two MRs reference the same config (kind and
// name). Referenced MRs must use the same config as the referencing MR.
func SameConfig(a, b *xpv2.ProviderConfigReference) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Kind == b.Kind && a.Name == b.Name
}

// Addr formats an endpoint as host:port.
func (e Endpoint) Addr() string { return net.JoinHostPort(e.Host, strconv.Itoa(e.Port)) }

// Interface check.
var _ Resolver = &APIResolver{}
