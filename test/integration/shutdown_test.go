// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/infrabay/provider-clickhouse/apis/v1alpha1"
	ch "github.com/infrabay/provider-clickhouse/internal/clients/clickhouse"
	"github.com/infrabay/provider-clickhouse/internal/controller/access"
)

// TestGracefulShutdownDuringCreate stops the provider as SIGTERM does (the
// provider pod is deleted, or a new package revision replaces it) right
// after it sent the CREATE ROLE of a Role MR and before it recorded the
// claim. The reconcile that is running finishes: the claim is recorded, and
// the next provider process, whose create journal is empty, owns the role
// instead of refusing it as NotOwned. The end-to-end tests found this
// (design 10.4, provider restart).
func TestGracefulShutdownDuringCreate(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS is not set; run make test-integration")
	}
	image := images(t, envUpstreamImages, defaultUpstreamImages)[0]
	password := randHex(16)
	srv := accessUpstreamServer(t, image, password, nil)
	cfg := startAPIServer(t)
	kube := kubeClient(t, cfg)

	probe := &stopProbe{stopping: make(chan struct{})}
	held := &heldStatement{prefix: "CREATE ROLE ", sent: make(chan struct{}), release: probe.stopping}
	stop := runProvider(t, cfg, providerRun{
		open: func(c ch.Config) (ch.Conn, error) {
			conn, err := ch.OpenDriver(c)
			if err != nil {
				return nil, err
			}
			return &holdingConn{Conn: conn, held: held}, nil
		},
		runnables: []manager.Runnable{probe},
	})

	cpc := passwordCPC(srv, "shutdown", v1alpha1.ClusterPolicy{AllowedNamespaces: []string{"team-.*"}, RoleNamePatterns: []string{"app_.+"}})
	cpc.UID = ""
	role := &v1alpha1.Role{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "held"}}
	role.Spec.ProviderConfigReference = &xpv2.ProviderConfigReference{Kind: v1alpha1.ClusterProviderConfigKind, Name: cpc.Name}
	role.Spec.ForProvider.Name = new("app_held")
	for _, obj := range []client.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "crossplane-system"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "crossplane-system", Name: "provider-clickhouse"}, Data: map[string][]byte{"password": []byte(password)}},
		cpc, role,
	} {
		if err := kube.Create(context.Background(), obj); err != nil {
			t.Fatalf("create %T: %v", obj, err)
		}
	}

	select {
	case <-held.sent:
	case <-time.After(90 * time.Second):
		t.Fatal("the provider did not send CREATE ROLE")
	}
	if err := stop(); err != nil {
		t.Fatalf("the provider did not stop cleanly: %v", err)
	}
	id, _, ok := srv.entity(t, access.Role, "app_held")
	if !ok {
		t.Fatal("CREATE ROLE did not reach the server")
	}
	key := client.ObjectKeyFromObject(role)
	if err := kube.Get(context.Background(), key, role); err != nil {
		t.Fatal(err)
	}
	if got := role.GetAnnotations()[v1alpha1.AnnotationOwnedID]; got != id {
		t.Fatalf("after a graceful shutdown during the create, owned-id is %q, want the role's UUID %q (annotations %v)", got, id, role.GetAnnotations())
	}

	// The next provider process owns the role, which exists once.
	runProvider(t, cfg, providerRun{})
	eventually(t, 60*time.Second, "Role Ready and Synced after the restart", func() (bool, string) {
		r := &v1alpha1.Role{}
		if err := kube.Get(context.Background(), key, r); err != nil {
			return false, err.Error()
		}
		ready, synced := r.GetCondition(xpv2.TypeReady), r.GetCondition(xpv2.TypeSynced)
		return ready.Status == corev1.ConditionTrue && synced.Status == corev1.ConditionTrue && r.GetAnnotations()[v1alpha1.AnnotationOwnedID] == id,
			"ready=" + string(ready.Status) + " " + ready.Message + "; synced=" + string(synced.Status) + " " + synced.Message
	})
	if got, _, _ := srv.entity(t, access.Role, "app_held"); got != id {
		t.Errorf("the role's UUID changed from %q to %q", id, got)
	}
}

// heldStatement describes the statement a holdingConn holds: the first one
// that starts with prefix.
type heldStatement struct {
	prefix  string
	once    sync.Once
	sent    chan struct{}
	release <-chan struct{}
}

// holdingConn is a ClickHouse connection pool that runs the held statement
// on the server and then holds the reconcile that sent it until the
// controllers are stopping, as if SIGTERM arrived right after the statement
// was sent.
type holdingConn struct {
	ch.Conn
	held *heldStatement
}

func (c *holdingConn) Exec(ctx context.Context, stmt string) error {
	first := false
	if strings.HasPrefix(stmt, c.held.prefix) {
		c.held.once.Do(func() { first = true })
	}
	if !first {
		return c.Conn.Exec(ctx, stmt)
	}
	// The statement reaches the server, whatever happens to the reconcile.
	err := c.Conn.Exec(context.WithoutCancel(ctx), stmt)
	close(c.held.sent)
	select {
	case <-c.held.release:
	case <-time.After(time.Minute):
	}
	return err
}

// stopProbe is a runnable that needs leader election, like the controllers:
// the manager ends its context together with theirs, and it then closes
// stopping.
type stopProbe struct{ stopping chan struct{} }

func (p *stopProbe) Start(ctx context.Context) error {
	<-ctx.Done()
	close(p.stopping)
	return nil
}

func (p *stopProbe) NeedLeaderElection() bool { return true }
