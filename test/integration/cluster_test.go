// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/network"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/infrabay/provider-clickhouse/apis/v1alpha1"
	ch "github.com/infrabay/provider-clickhouse/internal/clients/clickhouse"
	"github.com/infrabay/provider-clickhouse/internal/reason"
)

const testCluster = "c2"

// clusterServers starts two servers that form cluster c2 (one shard, two
// replicas, interserver secret) with an embedded Keeper on the first one, for
// ON CLUSTER DDL and Replicated databases. Both define the provider identity
// in users.d with the grants a cluster Database needs: the prefix-scoped
// Database grants, CLUSTER for ON CLUSTER, SELECT ON system.clusters for the
// cluster check, and REMOTE for the per-host ownership check.
func clusterServers(t *testing.T, image, password string) (*server, *server) {
	t.Helper()
	ctx := context.Background()
	nw, err := network.New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := nw.Remove(context.Background()); err != nil {
			t.Logf("cannot remove network: %v", err)
		}
	})
	hosts := []string{"xpch-node-a", "xpch-node-b"}
	secret := randHex(16)
	grants := append(databaseGrants(!strings.Contains(image, ":25.3")), "GRANT CLUSTER ON *.*", "GRANT SELECT ON system.clusters", "GRANT REMOTE ON *.*")
	usersD := fmt.Sprintf(`users:
  %s:
    password_sha256_hex: %q
    networks: {ip: ["::/0"]}
    profile: default
    quota: default
    grants:
      query:
%s`, providerUser, sha256Hex(password), grantsYAML(grants))
	keeper := fmt.Sprintf(`keeper_server:
  tcp_port: 9181
  server_id: 1
  log_storage_path: /var/lib/clickhouse/coordination/log
  snapshot_storage_path: /var/lib/clickhouse/coordination/snapshots
  raft_configuration:
    server:
      id: 1
      hostname: %s
      port: 9234
`, hosts[0])
	var servers []*server
	for i, host := range hosts {
		cluster := fmt.Sprintf(`zookeeper:
  node:
    host: %s
    port: 9181
macros:
  shard: "01"
  replica: %s
remote_servers:
  %s:
    secret: %q
    shard:
      internal_replication: true
      replica:
        - host: %s
          port: 9000
        - host: %s
          port: 9000
`, hosts[0], host, testCluster, secret, hosts[0], hosts[1])
		files := map[string]string{
			"/etc/clickhouse-server/config.d/cluster.yaml":            cluster,
			"/etc/clickhouse-server/users.d/provider-clickhouse.yaml": usersD,
		}
		if i == 0 {
			files["/etc/clickhouse-server/config.d/keeper.yaml"] = keeper
		}
		h := host
		servers = append(servers, startServer(t, image, files,
			network.WithNetwork([]string{h}, nw),
			testcontainers.WithConfigModifier(func(c *container.Config) { c.Hostname = h }),
		))
	}
	for _, s := range servers {
		deadline := time.Now().Add(60 * time.Second)
		for {
			var n uint64
			err := s.admin.QueryRow(ctx, "SELECT count() FROM system.zookeeper WHERE path = '/'").Scan(&n)
			if err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("Keeper not reachable: %v", err)
			}
			time.Sleep(500 * time.Millisecond)
		}
	}
	return servers[0], servers[1]
}

// replicas lists the replicas registered under a Replicated database's
// Keeper path.
func (s *server) replicas(t *testing.T, path string) []string {
	t.Helper()
	rows, err := s.admin.Query(context.Background(), "SELECT name FROM system.zookeeper WHERE path = '"+strings.ReplaceAll(path, "'", "")+"/replicas' ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close() //nolint:errcheck // Test cleanup.
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	return out
}

func TestClusterDatabase(t *testing.T) {
	image := images(t, envUpstreamImages, defaultUpstreamImages)[0]
	password := randHex(16)
	a, b := clusterServers(t, image, password)
	commentChange := a.commentChangeSupported(t)

	cpc := passwordCPC(a, "cluster", v1alpha1.ClusterPolicy{AllowedNamespaces: []string{"team-.*"}, DatabaseNamePatterns: []string{"app_.+"}})
	cpc.Spec.Cluster = new(testCluster)
	kube := newKube(t, cpc,
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "crossplane-system", Name: "provider-clickhouse"}, Data: map[string][]byte{"password": []byte(password)}},
	)
	h := newHarness(t, kube, nil)

	t.Run("CreateOnEveryHost", func(t *testing.T) {
		h.createMR("events", v1alpha1.DatabaseParameters{Name: new("app_events"), Comment: new("v1")}, "ClusterProviderConfig", "cluster")
		if o := h.reconcile("events"); o.err != nil || !o.created {
			t.Fatalf("create: %s", o)
		}
		owned := h.get("events").GetAnnotations()[v1alpha1.AnnotationOwnedID]
		for _, s := range []*server{a, b} {
			if id, comment, ok := s.database(t, "app_events"); !ok || id != owned || comment != "v1" {
				t.Fatalf("server: %q %q %t, want %s", id, comment, ok, owned)
			}
		}
		if o := h.reconcile("events"); o.err != nil || !o.obs.ResourceExists {
			t.Fatalf("observe: %s", o)
		}
	})

	t.Run("UpdateOnEveryHost", func(t *testing.T) {
		if !commentChange {
			t.Skip("the server cannot change database comments")
		}
		h.mutate("events", func(d *v1alpha1.Database) { d.Spec.ForProvider.Comment = new("v2") })
		if o := h.reconcile("events"); o.err != nil || !o.updated {
			t.Fatalf("update: %s", o)
		}
		for _, s := range []*server{a, b} {
			if _, comment, _ := s.database(t, "app_events"); comment != "v2" {
				t.Fatalf("comment = %q", comment)
			}
		}
	})

	t.Run("ForeignDatabaseOnOneHostIsNeverChanged", func(t *testing.T) {
		b.exec(t, "CREATE DATABASE app_shared COMMENT 'b only'")
		foreign, _, _ := b.database(t, "app_shared")
		h.createMR("shared", v1alpha1.DatabaseParameters{Name: new("app_shared"), Comment: new("mine")}, "ClusterProviderConfig", "cluster")
		// CREATE ... ON CLUSTER fails on b (82) and succeeds on a.
		if o := h.reconcile("shared"); !reason.Is(o.err, reason.AlreadyExists) || o.created {
			t.Fatalf("create: %s, want AlreadyExists from the other host", o)
		}
		owned := h.get("shared").GetAnnotations()[v1alpha1.AnnotationOwnedID]
		if id, _, ok := a.database(t, "app_shared"); !ok || id != owned {
			t.Fatalf("host a: %q %t, want %s", id, ok, owned)
		}
		if commentChange {
			h.mutate("shared", func(d *v1alpha1.Database) { d.Spec.ForProvider.Comment = new("mine too") })
			if o := h.reconcile("shared"); !reason.Is(o.err, reason.NotOwned) || !strings.Contains(o.err.Error(), foreign) || o.updated {
				t.Fatalf("update: %s, want NotOwned naming %s", o, foreign)
			}
			if _, comment, _ := a.database(t, "app_shared"); comment != "mine" {
				t.Errorf("host a was altered: %q", comment)
			}
		}
		h.mutate("shared", func(d *v1alpha1.Database) { d.Spec.ForProvider.ForceDrop = new(true) })
		h.delete("shared")
		if o := h.reconcile("shared"); o.err != nil || !o.gone || o.deleted {
			t.Fatalf("delete: %s, want the MR released without a DROP", o)
		}
		if id, comment, ok := b.database(t, "app_shared"); !ok || id != foreign || comment != "b only" {
			t.Fatalf("host b's database was touched: %q %q %t", id, comment, ok)
		}
		if _, _, ok := a.database(t, "app_shared"); !ok {
			t.Fatal("host a's database was dropped")
		}
	})

	var replicatedID string
	t.Run("ReplicatedDefaultPath", func(t *testing.T) {
		h.createMR("repl", v1alpha1.DatabaseParameters{Name: new("app_repl"), Engine: &v1alpha1.DatabaseEngine{Name: v1alpha1.DatabaseEngineReplicated}}, "ClusterProviderConfig", "cluster")
		if o := h.reconcile("repl"); o.err != nil || !o.created {
			t.Fatalf("create: %s", o)
		}
		replicatedID = h.get("repl").GetAnnotations()[v1alpha1.AnnotationOwnedID]
		if got := a.replicas(t, "/clickhouse/databases/"+replicatedID); len(got) != 2 {
			t.Fatalf("replicas = %v, want one per host", got)
		}
		if o := h.reconcile("repl"); o.err != nil || !o.obs.ResourceExists || !o.obs.ResourceUpToDate {
			t.Fatalf("observe: %s", o)
		}
	})

	t.Run("KeeperPathOfAnotherDatabaseIsRefused", func(t *testing.T) {
		if replicatedID == "" {
			t.Skip("needs ReplicatedDefaultPath")
		}
		path := "/clickhouse/databases/" + replicatedID
		h.createMR("evil", v1alpha1.DatabaseParameters{Name: new("app_evil"), Engine: &v1alpha1.DatabaseEngine{
			Name:       v1alpha1.DatabaseEngineReplicated,
			Replicated: &v1alpha1.ReplicatedEngine{ZooKeeperPath: path, ShardName: "01", ReplicaName: "evil"},
		}}, "ClusterProviderConfig", "cluster")
		if o := h.reconcile("evil"); !reason.Is(o.err, reason.PolicyViolation) || o.created {
			t.Fatalf("create: %s, want PolicyViolation", o)
		}
		if _, _, ok := a.database(t, "app_evil"); ok {
			t.Fatal("the database was created")
		}
		if got := a.replicas(t, path); len(got) != 2 {
			t.Fatalf("replicas of the other database = %v", got)
		}
	})

	t.Run("DeleteNeedsForceDrop", func(t *testing.T) {
		h.delete("events")
		if o := h.reconcile("events"); !reason.Is(o.err, reason.DatabaseNotEmpty) || o.deleted {
			t.Fatalf("delete without forceDrop: %s, want DatabaseNotEmpty", o)
		}
		h.mutate("events", func(d *v1alpha1.Database) { d.Spec.ForProvider.ForceDrop = new(true) })
		if o := h.reconcile("events"); o.err != nil || !o.deleted {
			t.Fatalf("delete with forceDrop: %s", o)
		}
		for _, s := range []*server{a, b} {
			if _, _, ok := s.database(t, "app_events"); ok {
				t.Fatal("database still exists")
			}
		}
		if o := h.reconcile("events"); o.err != nil || !o.gone {
			t.Fatalf("finalize: %s", o)
		}
	})

	t.Run("QuotingInClusterQuery", func(t *testing.T) {
		name := "app_we!rd`q'\\z"
		h.createMR("weird", v1alpha1.DatabaseParameters{Name: new(name), ForceDrop: new(true)}, "ClusterProviderConfig", "cluster")
		if o := h.reconcile("weird"); o.err != nil || !o.created {
			t.Fatalf("create: %s", o)
		}
		h.delete("weird")
		if o := h.reconcile("weird"); o.err != nil || !o.deleted {
			t.Fatalf("delete: %s", o)
		}
		for _, s := range []*server{a, b} {
			if _, _, ok := s.database(t, name); ok {
				t.Fatalf("%s still exists", ch.QuoteIdent(name))
			}
		}
	})
}
