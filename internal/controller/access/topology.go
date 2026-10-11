// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package access

import (
	"context"

	ch "github.com/infrabay/provider-clickhouse/internal/clients/clickhouse"
)

// CheckWritable refuses access-entity writes (users, roles, grants and role
// grants) that would diverge between servers (design section 8). The first
// writable user directory must be replicated when the ProviderConfig names a
// cluster with more than one host, or when more than one server was seen
// behind the endpoint recently (a load-balanced Service in front of replicas
// with local user directories). Otherwise it returns
// AccessStorageNotReplicated. Access entities never use ON CLUSTER.
//
// It probes the server UUID each time, so call it once per write, right
// before the statements.
func (e *Env) CheckWritable(ctx context.Context) error {
	caps := e.Client.Capabilities()
	storage, err := caps.AccessStorage(ctx, e.Client)
	if err != nil {
		return err
	}
	var hosts uint64
	endpoint := ""
	if e.Resolved != nil {
		endpoint = e.Resolved.Endpoint.Addr()
		if c := e.Resolved.Cluster; c != "" {
			if hosts, err = caps.ClusterHosts(ctx, e.Client, c); err != nil {
				return err
			}
		}
	}
	servers, err := e.Client.ProbeServer(ctx)
	if err != nil {
		return err
	}
	return ch.CheckAccessStorage(endpoint, storage, hosts, servers)
}
