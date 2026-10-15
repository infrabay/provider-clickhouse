// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package access

import (
	"context"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// TestFinishOnShutdown: a reconcile that is running when the manager stops
// keeps a live context and completes; one that would start afterwards does
// not run.
func TestFinishOnShutdown(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var innerErr error
	calls := 0
	r := FinishOnShutdown(reconcile.Func(func(ctx context.Context, _ reconcile.Request) (reconcile.Result, error) {
		calls++
		close(started)
		<-release
		innerErr = ctx.Err()
		return reconcile.Result{RequeueAfter: time.Minute}, nil
	}))

	ctx, cancel := context.WithCancel(context.Background())
	type result struct {
		res reconcile.Result
		err error
	}
	done := make(chan result, 1)
	go func() {
		res, err := r.Reconcile(ctx, reconcile.Request{})
		done <- result{res, err}
	}()
	<-started
	cancel() // The manager stops while the reconcile runs.
	close(release)
	got := <-done
	if innerErr != nil {
		t.Errorf("the running reconcile saw its context end: %v", innerErr)
	}
	if got.err != nil || got.res.RequeueAfter != time.Minute {
		t.Errorf("the running reconcile's result was not passed on: %+v, %v", got.res, got.err)
	}

	res, err := r.Reconcile(ctx, reconcile.Request{})
	if calls != 1 {
		t.Errorf("a reconcile started after the shutdown began (%d calls)", calls)
	}
	if err != nil || res.RequeueAfter == 0 {
		t.Errorf("a skipped reconcile must be requeued without an error: %+v, %v", res, err)
	}
}
