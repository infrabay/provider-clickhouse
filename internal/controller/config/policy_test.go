// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"context"
	"strings"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/crossplane/crossplane-runtime/v2/pkg/event"

	"github.com/infrabay/provider-clickhouse/apis/v1alpha1"
)

type recorder struct {
	mu     sync.Mutex
	events []event.Event
}

func (r *recorder) Event(_ runtime.Object, e event.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *recorder) WithAnnotations(...string) event.Recorder { return r }

// next counts the requests handed on to the usage reconciler.
type next struct{ calls int }

func (n *next) Reconcile(context.Context, reconcile.Request) (reconcile.Result, error) {
	n.calls++
	return reconcile.Result{}, nil
}

// TestPolicyCheck: a ClusterProviderConfig whose policy does not compile
// gets a Warning event, which CEL cannot give (design 3.3); every request is
// handed on to the usage reconciler.
func TestPolicyCheck(t *testing.T) {
	s := runtime.NewScheme()
	if err := v1alpha1.SchemeBuilder.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	cpc := func(name string, p v1alpha1.ClusterPolicy) client.Object {
		return &v1alpha1.ClusterProviderConfig{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: v1alpha1.ClusterProviderConfigSpec{Policy: p}}
	}
	kube := clientfake.NewClientBuilder().WithScheme(s).WithObjects(
		cpc("bad-regexp", v1alpha1.ClusterPolicy{UserNamePatterns: []string{"app_("}}),
		cpc("bad-placement", v1alpha1.ClusterPolicy{RoleNamePatterns: []string{".*_{namespace}_x"}}),
		cpc("good", v1alpha1.ClusterPolicy{AllowedNamespaces: []string{"team-.*"}, UserNamePatterns: []string{"{namespace}_[a-z0-9_]+"}}),
	).Build()
	for name, want := range map[string]string{
		"bad-regexp":    "policy.userNamePatterns[0]",
		"bad-placement": "policy.roleNamePatterns[0]",
		"good":          "",
		"missing":       "",
	} {
		t.Run(name, func(t *testing.T) {
			rec, n := &recorder{}, &next{}
			p := &PolicyCheck{Kube: kube, Recorder: rec, Next: n}
			if _, err := p.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: name}}); err != nil {
				t.Fatal(err)
			}
			if n.calls != 1 {
				t.Errorf("the usage reconciler ran %d times, want once", n.calls)
			}
			switch {
			case want == "" && len(rec.events) > 0:
				t.Errorf("events = %+v, want none", rec.events)
			case want != "" && (len(rec.events) != 1 || rec.events[0].Type != event.TypeWarning || rec.events[0].Reason != ReasonInvalidConfig ||
				!strings.Contains(rec.events[0].Message, want) || !strings.HasPrefix(rec.events[0].Message, "InvalidConfig: ")):
				t.Errorf("events = %+v, want one InvalidConfig warning naming %s", rec.events, want)
			}
		})
	}
}
