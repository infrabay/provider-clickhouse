// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration

import (
	"context"
	"fmt"
	"strings"
	"testing"

	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/crossplane/crossplane-runtime/v2/pkg/meta"

	"github.com/infrabay/provider-clickhouse/apis/v1alpha1"
	ch "github.com/infrabay/provider-clickhouse/internal/clients/clickhouse"
	"github.com/infrabay/provider-clickhouse/internal/reason"
)

const providerUser = "provider-clickhouse"

// upstreamServer starts upstream ClickHouse with a password provider identity
// defined in users.d, holding only the prefix-scoped Database grants.
func upstreamServer(t *testing.T, image, password string) *server {
	t.Helper()
	// 25.3 lacks ALTER MODIFY DATABASE COMMENT; granting it would be a
	// syntax error, so the identity is defined without it there.
	withComment := !strings.Contains(image, ":25.3")
	usersD := fmt.Sprintf(`users:
  %s:
    password_sha256_hex: %q
    networks: {ip: ["::/0"]}
    profile: default
    quota: default
    grants:
      query:
%s`, providerUser, sha256Hex(password), grantsYAML(databaseGrants(withComment)))
	return startServer(t, image, map[string]string{"/etc/clickhouse-server/users.d/provider-clickhouse.yaml": usersD})
}

func passwordCPC(s *server, name string, policy v1alpha1.ClusterPolicy) *v1alpha1.ClusterProviderConfig {
	port := int32(s.nativePort) //nolint:gosec // A mapped port fits.
	return &v1alpha1.ClusterProviderConfig{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID("cpc-" + name)},
		Spec: v1alpha1.ClusterProviderConfigSpec{
			ConnectionSpec: v1alpha1.ConnectionSpec{
				Endpoint:               v1alpha1.Endpoint{Host: s.host, Port: &port},
				AllowInsecureTransport: true,
			},
			Credentials: v1alpha1.ClusterCredentials{
				Source: v1alpha1.CredentialsSourcePasswordSecret,
				PasswordSecret: &v1alpha1.ClusterPasswordCredentials{
					Username:          providerUser,
					PasswordSecretRef: xpv2.SecretKeySelector{SecretReference: xpv2.SecretReference{Namespace: "crossplane-system", Name: "provider-clickhouse"}, Key: "password"},
				},
			},
			Policy: policy,
		},
	}
}

func TestUpstreamDatabaseLifecycle(t *testing.T) {
	for _, image := range images(t, envUpstreamImages, defaultUpstreamImages) {
		t.Run(image, func(t *testing.T) { testUpstreamDatabaseLifecycle(t, image) })
	}
}

func testUpstreamDatabaseLifecycle(t *testing.T, image string) {
	password := randHex(16)
	srv := upstreamServer(t, image, password)
	commentChange := srv.commentChangeSupported(t)

	policy := v1alpha1.ClusterPolicy{AllowedNamespaces: []string{"team-.*"}, DatabaseNamePatterns: []string{"app_.+"}}
	kube := newKube(t,
		passwordCPC(srv, "upstream", policy),
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "crossplane-system", Name: "provider-clickhouse"}, Data: map[string][]byte{"password": []byte(password)}},
	)
	h := newHarness(t, kube, nil)

	t.Run("CreateWithPreassignedUUID", func(t *testing.T) {
		h.createMR("analytics", v1alpha1.DatabaseParameters{Name: new("app_analytics"), Comment: new("v1")}, "ClusterProviderConfig", "upstream")
		o := h.reconcile("analytics")
		if o.err != nil || !o.created {
			t.Fatalf("first reconcile: %s", o)
		}
		id, comment, ok := srv.database(t, "app_analytics")
		owned := h.get("analytics").GetAnnotations()[v1alpha1.AnnotationOwnedID]
		if !ok || id != owned || comment != "v1" {
			t.Fatalf("server has %q (%q, exists %t), owned-id annotation %q", id, comment, ok, owned)
		}
	})

	t.Run("ObserveOwnedAndLateInitialize", func(t *testing.T) {
		o := h.reconcile("analytics")
		if o.err != nil || !o.obs.ResourceExists || !o.obs.ResourceUpToDate || !o.obs.ResourceLateInitialized {
			t.Fatalf("second reconcile: %s", o)
		}
		d := h.get("analytics")
		if e := d.Spec.ForProvider.Engine; e == nil || e.Name != "Atomic" {
			t.Errorf("engine not late-initialized: %+v", e)
		}
		if o := h.reconcile("analytics"); o.err != nil || !o.obs.ResourceUpToDate || o.obs.ResourceLateInitialized || o.updated {
			t.Fatalf("steady state: %s", o)
		}
	})

	t.Run("DriftIsRepaired", func(t *testing.T) {
		if !commentChange {
			// 25.3: a desired comment change is reported as Unsupported.
			h.mutate("analytics", func(d *v1alpha1.Database) { d.Spec.ForProvider.Comment = new("v2") })
			if o := h.reconcile("analytics"); !reason.Is(o.err, reason.Unsupported) {
				t.Fatalf("comment change on %s: %s, want Unsupported", image, o)
			}
			h.mutate("analytics", func(d *v1alpha1.Database) { d.Spec.ForProvider.Comment = new("v1") })
			return
		}
		srv.exec(t, "ALTER DATABASE app_analytics MODIFY COMMENT 'changed out of band'")
		o := h.reconcile("analytics")
		if o.err != nil || o.obs.ResourceUpToDate || !o.updated {
			t.Fatalf("drift reconcile: %s", o)
		}
		if _, comment, _ := srv.database(t, "app_analytics"); comment != "v1" {
			t.Fatalf("comment after repair = %q", comment)
		}
		h.mutate("analytics", func(d *v1alpha1.Database) { d.Spec.ForProvider.Comment = new("v2") })
		if o := h.reconcile("analytics"); o.err != nil || !o.updated {
			t.Fatalf("spec change: %s", o)
		}
		if _, comment, _ := srv.database(t, "app_analytics"); comment != "v2" {
			t.Fatalf("comment after spec change = %q", comment)
		}
	})

	t.Run("DroppedOutOfBandIsRecreatedWithSameUUID", func(t *testing.T) {
		before, _, _ := srv.database(t, "app_analytics")
		srv.exec(t, "DROP DATABASE app_analytics")
		o := h.reconcile("analytics")
		if o.err != nil || !o.created {
			t.Fatalf("re-create: %s", o)
		}
		after, _, ok := srv.database(t, "app_analytics")
		if !ok || after != before {
			t.Fatalf("re-created with uuid %q, want %q", after, before)
		}
	})

	t.Run("ReplacedOutOfBand", func(t *testing.T) {
		// The replacement carries the desired comment, so that after adoption
		// nothing needs to change (25.3 cannot change comments).
		desired := *h.get("analytics").Spec.ForProvider.Comment
		srv.exec(t, "DROP DATABASE app_analytics")
		srv.exec(t, "CREATE DATABASE app_analytics COMMENT "+ch.QuoteString(desired))
		newID, _, _ := srv.database(t, "app_analytics")
		o := h.reconcile("analytics")
		if !reason.Is(o.err, reason.Replaced) || !strings.Contains(o.err.Error(), newID) {
			t.Fatalf("replaced: %s", o)
		}
		// Adoption needs policy.allowAdoption.
		h.mutate("analytics", func(d *v1alpha1.Database) {
			meta.AddAnnotations(d, map[string]string{v1alpha1.AnnotationAdopt: newID})
		})
		if o := h.reconcile("analytics"); !reason.Is(o.err, reason.AdoptionRejected) {
			t.Fatalf("adopt without policy: %s", o)
		}
		cpc := &v1alpha1.ClusterProviderConfig{}
		_ = kube.Get(context.Background(), client.ObjectKey{Name: "upstream"}, cpc)
		cpc.Spec.Policy.AllowAdoption = true
		cpc.Generation++
		if err := kube.Update(context.Background(), cpc); err != nil {
			t.Fatal(err)
		}
		o = h.reconcile("analytics")
		if o.err != nil || !o.obs.ResourceExists || !o.obs.ResourceUpToDate {
			t.Fatalf("adopt: %s", o)
		}
		a := h.get("analytics").GetAnnotations()
		if a[v1alpha1.AnnotationOwnedID] != newID || a[v1alpha1.AnnotationAdopt] != "" {
			t.Fatalf("annotations after adoption: %v", a)
		}
		cpc = &v1alpha1.ClusterProviderConfig{}
		_ = kube.Get(context.Background(), client.ObjectKey{Name: "upstream"}, cpc)
		cpc.Spec.Policy.AllowAdoption = false
		cpc.Generation++
		_ = kube.Update(context.Background(), cpc)
	})

	t.Run("PreExistingDatabaseIsNotTakenOver", func(t *testing.T) {
		srv.exec(t, "CREATE DATABASE app_legacy COMMENT 'owned by someone else'")
		id, _, _ := srv.database(t, "app_legacy")
		h.createMR("legacy", v1alpha1.DatabaseParameters{Name: new("app_legacy"), Comment: new("mine now")}, "ClusterProviderConfig", "upstream")
		o := h.reconcile("legacy")
		if !reason.Is(o.err, reason.NotOwned) || !strings.Contains(o.err.Error(), id) || o.created || o.updated {
			t.Fatalf("collision: %s", o)
		}
		h.mutate("legacy", func(d *v1alpha1.Database) {
			meta.AddAnnotations(d, map[string]string{v1alpha1.AnnotationAdopt: "00000000-0000-0000-0000-000000000001"})
		})
		if o := h.reconcile("legacy"); !reason.Is(o.err, reason.AdoptionRejected) {
			t.Fatalf("wrong adopt id: %s", o)
		}
		// Deleting the MR leaves the database alone.
		h.delete("legacy")
		if o := h.reconcile("legacy"); o.err != nil || !o.gone || o.deleted {
			t.Fatalf("delete non-owned: %s", o)
		}
		after, comment, ok := srv.database(t, "app_legacy")
		if !ok || after != id || comment != "owned by someone else" {
			t.Fatalf("pre-existing database was touched: %q %q %t", after, comment, ok)
		}
	})

	t.Run("ForgedClaimOutsidePolicyIsNeverDropped", func(t *testing.T) {
		// The identity's grants cover app_*, but this config only lets the
		// tenant use app_a_*. A tenant points an MR at another tenant's
		// database, forges the ownership claim with its UUID, sets forceDrop
		// and deletes the MR.
		kubeT := newKube(t,
			passwordCPC(srv, "tenant-a", v1alpha1.ClusterPolicy{AllowedNamespaces: []string{"team-.*"}, DatabaseNamePatterns: []string{"app_a_.+"}}),
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "crossplane-system", Name: "provider-clickhouse"}, Data: map[string][]byte{"password": []byte(password)}},
		)
		hT := newHarness(t, kubeT, nil)
		srv.exec(t, "CREATE DATABASE app_b_prod COMMENT 'team b'")
		victim, _, _ := srv.database(t, "app_b_prod")
		hT.createMR("attack", v1alpha1.DatabaseParameters{ForceDrop: new(true)}, "ClusterProviderConfig", "tenant-a")
		hT.mutate("attack", func(d *v1alpha1.Database) {
			meta.SetExternalName(d, "app_b_prod")
			meta.AddAnnotations(d, map[string]string{v1alpha1.AnnotationOwnerUID: string(d.GetUID()), v1alpha1.AnnotationOwnedID: victim})
		})
		if o := hT.reconcile("attack"); !reason.Is(o.err, reason.PolicyViolation) || o.updated {
			t.Fatalf("observe outside policy: %s, want PolicyViolation", o)
		}
		hT.delete("attack")
		if o := hT.reconcile("attack"); o.err != nil || !o.gone || o.deleted {
			t.Fatalf("delete outside policy: %s, want the MR released without a DROP", o)
		}
		if id, comment, ok := srv.database(t, "app_b_prod"); !ok || id != victim || comment != "team b" {
			t.Fatalf("the other tenant's database was touched: %q %q %t", id, comment, ok)
		}
	})

	t.Run("PolicyViolation", func(t *testing.T) {
		h.createMR("outside", v1alpha1.DatabaseParameters{Name: new("other_db")}, "ClusterProviderConfig", "upstream")
		if o := h.reconcile("outside"); !reason.Is(o.err, reason.PolicyViolation) || o.created {
			t.Fatalf("policy: %s", o)
		}
		if _, _, ok := srv.database(t, "other_db"); ok {
			t.Fatal("a database outside the policy was created")
		}
	})

	t.Run("ServerSideGrantsAreTheCeiling", func(t *testing.T) {
		kube2 := newKube(t,
			passwordCPC(srv, "wide", v1alpha1.ClusterPolicy{Unrestricted: true}),
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "crossplane-system", Name: "provider-clickhouse"}, Data: map[string][]byte{"password": []byte(password)}},
		)
		h2 := newHarness(t, kube2, nil)
		h2.createMR("zzz", v1alpha1.DatabaseParameters{Name: new("zzz_db")}, "ClusterProviderConfig", "wide")
		o := h2.reconcile("zzz")
		if !reason.Is(o.err, reason.PermissionDenied) {
			t.Fatalf("create outside the identity's grants: %s, want PermissionDenied", o)
		}
	})

	t.Run("AdversarialName", func(t *testing.T) {
		name := "app_we!rd.name@x`q'\\z ü"
		h.createMR("weird", v1alpha1.DatabaseParameters{Name: new(name), Comment: new("it's \\ fine")}, "ClusterProviderConfig", "upstream")
		if o := h.reconcile("weird"); o.err != nil || !o.created {
			t.Fatalf("create: %s", o)
		}
		id, comment, ok := srv.database(t, name)
		if !ok || comment != "it's \\ fine" || id != h.get("weird").GetAnnotations()[v1alpha1.AnnotationOwnedID] {
			t.Fatalf("server: %q %q %t", id, comment, ok)
		}
		if o := h.reconcile("weird"); o.err != nil || !o.obs.ResourceUpToDate {
			t.Fatalf("observe: %s", o)
		}
		h.delete("weird")
		if o := h.reconcile("weird"); o.err != nil || !o.deleted {
			t.Fatalf("delete: %s", o)
		}
		if _, _, ok := srv.database(t, name); ok {
			t.Fatal("database still exists")
		}
	})

	t.Run("DeleteRefusesNonEmptyDatabase", func(t *testing.T) {
		srv.exec(t, "CREATE TABLE app_analytics.events (id UInt64) ENGINE = MergeTree ORDER BY id")
		h.delete("analytics")
		o := h.reconcile("analytics")
		if !reason.Is(o.err, reason.DatabaseNotEmpty) || o.deleted {
			t.Fatalf("delete non-empty: %s", o)
		}
		if _, _, ok := srv.database(t, "app_analytics"); !ok {
			t.Fatal("a non-empty database was dropped")
		}
		srv.exec(t, "DROP TABLE app_analytics.events SYNC")
		if o := h.reconcile("analytics"); o.err != nil || !o.deleted {
			t.Fatalf("delete empty: %s", o)
		}
		if _, _, ok := srv.database(t, "app_analytics"); ok {
			t.Fatal("database still exists")
		}
		if o := h.reconcile("analytics"); o.err != nil || !o.gone {
			t.Fatalf("finalize: %s", o)
		}
	})

	t.Run("ErrorsNeverEchoCredentials", func(t *testing.T) {
		kube3 := newKube(t,
			passwordCPC(srv, "badpw", v1alpha1.ClusterPolicy{Unrestricted: true}),
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "crossplane-system", Name: "provider-clickhouse"}, Data: map[string][]byte{"password": []byte("wrong-" + password)}},
		)
		h3 := newHarness(t, kube3, nil)
		h3.createMR("x", v1alpha1.DatabaseParameters{Name: new("app_x")}, "ClusterProviderConfig", "badpw")
		o := h3.reconcile("x")
		if !reason.Is(o.err, reason.AuthenticationFailed) {
			t.Fatalf("wrong password: %s, want AuthenticationFailed", o)
		}
		if strings.Contains(o.err.Error(), password) {
			t.Fatal("the error contains the password")
		}
		if code, ok := ch.Code(o.err); !ok || code != ch.CodeAuthenticationFailed {
			t.Errorf("code = %d, %t", code, ok)
		}
	})
}
