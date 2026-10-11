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
	cases := map[string]struct {
		res     fake.Result
		want    bool
		wantErr reason.Reason
	}{
		"Present":        {res: fake.Rows(), want: true},
		"Missing":        {res: fake.Error(fake.Exception(47, "Unknown expression identifier `valid_until`"))},
		"MissingLegacy":  {res: fake.Error(fake.Exception(16, "There is no column valid_until in table"))},
		"NoGrant":        {res: fake.Error(fake.Exception(497, "necessary to have the grant SHOW USERS")), wantErr: reason.PermissionDenied},
		"TransportError": {res: fake.Error(&ch.TransportError{Category: "timeout"}), wantErr: "any"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			q := ch.QuerySystemColumnProbe(ch.TableUsers, ch.ColumnValidUntil)
			if q != "SELECT `valid_until` FROM system.`users` LIMIT 0" {
				t.Fatalf("probe = %q", q)
			}
			db := fake.NewDB().On(q, tc.res)
			got, err := ch.NewCapabilities().UsersValidUntil(ctx, db)
			switch {
			case tc.wantErr == "any":
				if err == nil {
					t.Fatal("a transport error must be returned, never 'missing'")
				}
			case tc.wantErr != "":
				if !reason.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %s", err, tc.wantErr)
				}
			case err != nil || got != tc.want:
				t.Fatalf("UsersValidUntil() = %t, %v; want %t", got, err, tc.want)
			}
		})
	}
}

func TestGrantsColumns(t *testing.T) {
	ctx := context.Background()
	missing := fake.Error(fake.Exception(47, "Unknown expression identifier"))
	for name, tc := range map[string]struct {
		accessObject, isWildcard fake.Result
		wantAO, wantWC           bool
	}{
		"25.3":    {accessObject: missing, isWildcard: missing},
		"25.8":    {accessObject: fake.Rows(), isWildcard: missing, wantAO: true},
		"26.8":    {accessObject: fake.Rows(), isWildcard: fake.Rows(), wantAO: true, wantWC: true},
		"Antalya": {accessObject: fake.Rows(), isWildcard: missing, wantAO: true},
	} {
		t.Run(name, func(t *testing.T) {
			db := fake.NewDB().
				On(ch.QuerySystemColumnProbe(ch.TableGrants, ch.ColumnAccessObject), tc.accessObject).
				On(ch.QuerySystemColumnProbe(ch.TableGrants, ch.ColumnIsWildcard), tc.isWildcard)
			c := ch.NewCapabilities()
			for range 2 {
				ao, err := c.GrantsAccessObject(ctx, db)
				if err != nil || ao != tc.wantAO {
					t.Fatalf("GrantsAccessObject() = %t, %v", ao, err)
				}
				wc, err := c.GrantsIsWildcard(ctx, db)
				if err != nil || wc != tc.wantWC {
					t.Fatalf("GrantsIsWildcard() = %t, %v", wc, err)
				}
			}
			if len(db.Seen) != 2 {
				t.Errorf("probes ran %d times, want 2 (cached per client)", len(db.Seen))
			}
		})
	}
}

func TestPrivileges(t *testing.T) {
	ctx := context.Background()
	db := fake.NewDB().On(ch.QueryPrivileges, fake.Rows(
		[]any{"SELECT", []string{}, "COLUMN", "ALL"},
		[]any{"SHOW USERS", []string{"SHOW CREATE USER"}, "GLOBAL", "SHOW ACCESS"},
		[]any{"NONE", []string{"USAGE", "NO PRIVILEGES"}, "", ""},
	))
	c := ch.NewCapabilities()
	got, err := c.Privileges(ctx, db)
	if err != nil || len(got) != 3 {
		t.Fatalf("Privileges() = %v, %v", got, err)
	}
	if got[1].Name != "SHOW USERS" || got[1].Aliases[0] != "SHOW CREATE USER" || got[1].Level != "GLOBAL" || got[1].Parent != "SHOW ACCESS" {
		t.Errorf("row = %+v", got[1])
	}
	if got[2].Level != "" || got[2].Parent != "" {
		t.Errorf("NULL level and parent must be empty: %+v", got[2])
	}
	if _, err := c.Privileges(ctx, db); err != nil || len(db.Seen) != 1 {
		t.Errorf("the catalogue must be cached: %d queries, %v", len(db.Seen), err)
	}
	empty := fake.NewDB().On(ch.QueryPrivileges, fake.Rows())
	if _, err := ch.NewCapabilities().Privileges(ctx, empty); !reason.Is(err, reason.Internal) {
		t.Errorf("an empty catalogue is an error: %v", err)
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
