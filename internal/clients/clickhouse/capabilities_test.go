// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package clickhouse_test

import (
	"context"
	"testing"

	ch "github.com/infrabay/provider-clickhouse/internal/clients/clickhouse"
	"github.com/infrabay/provider-clickhouse/internal/clients/clickhouse/fake"
	"github.com/infrabay/provider-clickhouse/internal/reason"
)

func TestJWTProbe(t *testing.T) {
	ctx := context.Background()
	cases := map[string]struct {
		res     fake.Result
		want    bool
		wantErr bool
	}{
		"Antalya":       {res: fake.Rows([]any{"CreateUserQuery"}), want: true},
		"Upstream26":    {res: fake.Error(fake.Exception(36, "CREATE USER is not supported for JWT"))},
		"Upstream25.3":  {res: fake.Error(fake.Exception(62, "Syntax error"))},
		"OtherFailure":  {res: fake.Error(fake.Exception(497, "denied")), wantErr: true},
		"TransportFail": {res: fake.Error(&ch.TransportError{Category: "dial failed"}), wantErr: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			db := fake.NewDB().On(ch.QueryJWTProbe, tc.res)
			got, err := ch.NewCapabilities().JWTUsers(ctx, db)
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("JWTUsers() = %t, %v", got, err)
			}
		})
	}
}

func TestCapabilitiesAreCached(t *testing.T) {
	ctx := context.Background()
	db := fake.NewDB().On(ch.QueryPrivilegeExists, fake.Row(uint64(1)))
	c := ch.NewCapabilities()
	for range 3 {
		ok, err := c.DatabaseCommentChange(ctx, db)
		if err != nil || !ok {
			t.Fatalf("DatabaseCommentChange() = %t, %v", ok, err)
		}
	}
	if len(db.Seen) != 1 {
		t.Errorf("probe ran %d times, want 1", len(db.Seen))
	}
	if db.Params[0]["privilege"] != "ALTER MODIFY DATABASE COMMENT" {
		t.Errorf("params = %v", db.Params[0])
	}

	// Failures are not cached.
	db2 := fake.NewDB().On(ch.QueryPrivilegeExists, fake.Error(fake.Exception(497, "denied")))
	c2 := ch.NewCapabilities()
	for range 2 {
		if _, err := c2.DatabaseCommentChange(ctx, db2); !reason.Is(err, reason.PermissionDenied) {
			t.Fatalf("err = %v", err)
		}
	}
	if len(db2.Seen) != 2 {
		t.Errorf("failed probe must be retried, ran %d times", len(db2.Seen))
	}
}

func TestClusterHosts(t *testing.T) {
	ctx := context.Background()
	db := fake.NewDB().OnFunc(ch.QueryClusterHosts, func(p ch.Params) fake.Result {
		if p["cluster"] == "my-cluster" {
			return fake.Row(uint64(2))
		}
		return fake.Row(uint64(0))
	})
	c := ch.NewCapabilities()
	if n, err := c.ClusterHosts(ctx, db, "my-cluster"); err != nil || n != 2 {
		t.Fatalf("ClusterHosts() = %d, %v", n, err)
	}
	if _, err := c.ClusterHosts(ctx, db, "nope"); !reason.Is(err, reason.InvalidConfig) {
		t.Fatalf("unknown cluster: %v", err)
	}
}

func TestAccessStorage(t *testing.T) {
	ctx := context.Background()
	db := fake.NewDB().On(ch.QueryUserDirectories, fake.Rows(
		[]any{"users_xml", "users_xml", uint64(0)},
		[]any{"replicated", "replicated", uint64(1)},
	))
	got, err := ch.NewCapabilities().AccessStorage(ctx, db)
	if err != nil || got != ch.StorageReplicated {
		t.Fatalf("AccessStorage() = %q, %v", got, err)
	}
	if _, err := ch.FirstWritableDirectory([]ch.UserDirectory{{Type: "users_xml"}}); !reason.Is(err, reason.InvalidConfig) {
		t.Fatalf("no writable directory: %v", err)
	}
}

func TestHasSystemColumn(t *testing.T) {
	ctx := context.Background()
	db := fake.NewDB().On(ch.QuerySystemColumns, fake.Rows([]any{"user_name"}, []any{"is_wildcard"}))
	c := ch.NewCapabilities()
	if ok, err := c.HasSystemColumn(ctx, db, "grants", "is_wildcard"); err != nil || !ok {
		t.Fatalf("is_wildcard: %t, %v", ok, err)
	}
	if ok, _ := c.HasSystemColumn(ctx, db, "grants", "access_object"); ok {
		t.Fatal("access_object should be missing")
	}
}

func TestQueryRow(t *testing.T) {
	ctx := context.Background()
	db := fake.NewDB().On("SELECT 1", fake.Rows()).On("SELECT 2", fake.Rows([]any{uint8(2)}, []any{uint8(3)}))
	var v uint8
	if err := ch.QueryRow(ctx, db, "SELECT 1", nil, &v); err != ch.ErrNoRows {
		t.Fatalf("no rows: %v", err)
	}
	if err := ch.QueryRow(ctx, db, "SELECT 2", nil, &v); err != nil || v != 2 {
		t.Fatalf("first row: %d, %v", v, err)
	}
}
