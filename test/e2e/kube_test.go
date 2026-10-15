// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/crossplane/crossplane-runtime/v2/pkg/resource"

	"github.com/infrabay/provider-clickhouse/internal/reason"
)

// eventually calls cond every second until it returns true, and fails the
// test with cond's last message after timeout.
func eventually(t *testing.T, timeout time.Duration, what string, cond func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last := ""
	for {
		ok, msg := cond()
		if ok {
			return
		}
		last = msg
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s: %s", timeout, what, last)
		}
		time.Sleep(time.Second)
	}
}

// labelled adds the harness label, which make e2e-cleanup uses to find what
// the tests created.
func labelled[T client.Object](obj T) T {
	l := obj.GetLabels()
	if l == nil {
		l = map[string]string{}
	}
	l[labelKey] = labelValue
	obj.SetLabels(l)
	return obj
}

// create creates obj with the harness label.
func create(t *testing.T, obj client.Object) {
	t.Helper()
	if err := kube.Create(context.Background(), labelled(obj)); err != nil {
		t.Fatalf("create %T %s: %v", obj, obj.GetName(), err)
	}
}

// get reads obj again by its key.
func get(t *testing.T, obj client.Object) {
	t.Helper()
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(obj), obj); err != nil {
		t.Fatalf("get %T %s: %v", obj, obj.GetName(), err)
	}
}

// conditions formats an MR's Ready and Synced conditions.
func conditions(mg resource.Managed) string {
	ready, synced := mg.GetCondition(xpv2.TypeReady), mg.GetCondition(xpv2.TypeSynced)
	return fmt.Sprintf("ready=%s %s %s; synced=%s %s %s", ready.Status, ready.Reason, ready.Message, synced.Status, synced.Reason, synced.Message)
}

// readyAndSynced is true when Ready and Synced are both True.
func readyAndSynced(mg resource.Managed) bool {
	return mg.GetCondition(xpv2.TypeReady).Status == corev1.ConditionTrue && mg.GetCondition(xpv2.TypeSynced).Status == corev1.ConditionTrue
}

// refusedWith returns a check that Synced is False with a message that
// starts with the reason, and Ready is not True.
func refusedWith(r reason.Reason) func(resource.Managed) bool {
	return func(mg resource.Managed) bool {
		synced := mg.GetCondition(xpv2.TypeSynced)
		return synced.Status == corev1.ConditionFalse && strings.Contains(synced.Message, string(r)) &&
			mg.GetCondition(xpv2.TypeReady).Status != corev1.ConditionTrue
	}
}

// waitMR waits until ok(mg) holds and leaves mg as last read.
func waitMR(t *testing.T, mg resource.Managed, timeout time.Duration, what string, ok func(resource.Managed) bool) {
	t.Helper()
	key := client.ObjectKeyFromObject(mg)
	eventually(t, timeout, fmt.Sprintf("%T %s/%s %s", mg, key.Namespace, key.Name, what), func() (bool, string) {
		if err := kube.Get(context.Background(), key, mg); err != nil {
			return false, err.Error()
		}
		return ok(mg), conditions(mg)
	})
}

// waitReady waits until every MR is Ready and Synced.
func waitReady(t *testing.T, mgs ...resource.Managed) {
	t.Helper()
	for _, mg := range mgs {
		waitMR(t, mg, 3*time.Minute, "Ready and Synced", readyAndSynced)
	}
}

// deleteAndWait deletes the objects and waits until they are gone.
func deleteAndWait(t *testing.T, timeout time.Duration, objs ...client.Object) {
	t.Helper()
	for _, obj := range objs {
		if err := kube.Delete(context.Background(), obj); err != nil && !kerrors.IsNotFound(err) {
			t.Fatalf("delete %T %s: %v", obj, obj.GetName(), err)
		}
	}
	for _, obj := range objs {
		key := client.ObjectKeyFromObject(obj)
		eventually(t, timeout, fmt.Sprintf("%T %s gone", obj, key.Name), func() (bool, string) {
			err := kube.Get(context.Background(), key, obj)
			if kerrors.IsNotFound(err) {
				return true, ""
			}
			if mg, ok := obj.(resource.Managed); ok && err == nil {
				return false, "still there: " + conditions(mg)
			}
			return false, fmt.Sprintf("still there (%v)", err)
		})
	}
}

// objects converts MRs to client.Objects.
func objects(mgs ...resource.Managed) []client.Object {
	out := make([]client.Object, 0, len(mgs))
	for _, mg := range mgs {
		out = append(out, mg)
	}
	return out
}

// secretData reads one key of a Secret. The value is never logged.
func secretData(t *testing.T, ns, name, key string) []byte {
	t.Helper()
	s := &corev1.Secret{}
	if err := kube.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, s); err != nil {
		t.Fatalf("Secret %s/%s: %v", ns, name, err)
	}
	v, ok := s.Data[key]
	if !ok {
		t.Fatalf("Secret %s/%s has no key %q", ns, name, key)
	}
	return v
}

// readyPod returns the name of a running, ready pod matching the selector.
func readyPod(t *testing.T, ns, selector string) string {
	t.Helper()
	var name string
	eventually(t, 3*time.Minute, "a ready pod "+selector+" in "+ns, func() (bool, string) {
		pods, err := clientset.CoreV1().Pods(ns).List(context.Background(), metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			return false, err.Error()
		}
		for _, p := range pods.Items {
			if p.DeletionTimestamp == nil && podReady(&p) {
				name = p.Name
				return true, ""
			}
		}
		return false, fmt.Sprintf("%d pods, none ready", len(pods.Items))
	})
	return name
}

func podReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// portForward forwards a local port to a port of a pod until the test ends
// and returns the local address. The pod network is not reachable from the
// host on every local cluster (kind), so the tests reach the servers this
// way.
func portForward(t *testing.T, ns, pod string, port int) string {
	t.Helper()
	transport, upgrader, err := spdy.RoundTripperFor(restConfig)
	if err != nil {
		t.Fatal(err)
	}
	u := clientset.CoreV1().RESTClient().Post().Resource("pods").Namespace(ns).Name(pod).SubResource("portforward").URL()
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, u)
	stop, ready := make(chan struct{}), make(chan struct{})
	fw, err := portforward.NewOnAddresses(dialer, []string{"127.0.0.1"}, []string{fmt.Sprintf("0:%d", port)}, stop, ready, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- fw.ForwardPorts() }()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("port-forward to %s/%s:%d: %v", ns, pod, port, err)
	case <-time.After(30 * time.Second):
		t.Fatalf("port-forward to %s/%s:%d did not come up", ns, pod, port)
	}
	t.Cleanup(func() { close(stop) })
	ports, err := fw.GetPorts()
	if err != nil || len(ports) != 1 {
		t.Fatalf("port-forward ports: %v %v", ports, err)
	}
	return fmt.Sprintf("127.0.0.1:%d", ports[0].Local)
}

// serviceAccountToken requests a token of a service account for the
// audience (TokenRequest API, like a projected volume). It is never logged.
func serviceAccountToken(t *testing.T, ns, sa, audience string) string {
	t.Helper()
	ttl := int64(3600)
	tr, err := clientset.CoreV1().ServiceAccounts(ns).CreateToken(context.Background(), sa, &authenticationv1.TokenRequest{
		Spec: authenticationv1.TokenRequestSpec{Audiences: []string{audience}, ExpirationSeconds: &ttl},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("token of %s/%s: %v", ns, sa, err)
	}
	return tr.Status.Token
}

// ensureServiceAccount creates a service account unless it exists.
func ensureServiceAccount(t *testing.T, ns, name string) {
	t.Helper()
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
	if err := kube.Create(context.Background(), labelled(sa)); err != nil && !kerrors.IsAlreadyExists(err) {
		t.Fatal(err)
	}
}
