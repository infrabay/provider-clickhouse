// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package access

import (
	"context"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// FinishOnShutdown wraps the reconciler of a managed resource controller so
// that a graceful shutdown does not interrupt a reconcile that is already
// running.
//
// controller-runtime cancels the context of running reconciles when the
// manager stops, for example after SIGTERM when the provider pod is deleted
// or a new package revision replaces it. A User or Role create interrupted
// between its CREATE and the annotation write that records the claim leaves
// an entity that the next provider process cannot tell from one somebody
// else created (design 4.2, D-29), and the MR stays NotOwned until it is
// adopted. The wrapped reconciler runs with a context that the shutdown does
// not cancel; it stays bounded by crossplane-runtime's reconcile timeout,
// and the manager waits for it up to its graceful shutdown timeout. A
// reconcile that would start after the shutdown began does not run.
func FinishOnShutdown(r reconcile.Reconciler) reconcile.Reconciler {
	return reconcile.Func(func(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
		if ctx.Err() != nil {
			// The manager is stopping: start nothing new. The request comes
			// back in case the context ended for another reason.
			return reconcile.Result{RequeueAfter: time.Second}, nil
		}
		return r.Reconcile(context.WithoutCancel(ctx), req)
	})
}
