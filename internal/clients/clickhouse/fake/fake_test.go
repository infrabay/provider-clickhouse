// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package fake

import (
	"context"
	"errors"
	"testing"

	ch "github.com/infrabay/provider-clickhouse/internal/clients/clickhouse"
	"github.com/infrabay/provider-clickhouse/internal/clients/clickhouse/sqlx"
)

// The profiles drive the real capability probes, so a profile that does not
// match what the probes expect fails here.
func TestProfiles(t *testing.T) {
	ctx := context.Background()
	for _, p := range Profiles {
		t.Run(p.Version, func(t *testing.T) {
			c := NewClientFor(NewDB(), p)
			caps := c.Capabilities()
			got := Profile{Version: c.Info().Version, JWTProbeCode: p.JWTProbeCode, Storage: p.Storage}
			var err error
			if got.JWTUsers, err = caps.JWTUsers(ctx, c); err != nil {
				t.Fatal(err)
			}
			if got.ValidUntil, err = caps.UsersValidUntil(ctx, c); err != nil {
				t.Fatal(err)
			}
			if got.AccessObject, err = caps.GrantsAccessObject(ctx, c); err != nil {
				t.Fatal(err)
			}
			if got.IsWildcard, err = caps.GrantsIsWildcard(ctx, c); err != nil {
				t.Fatal(err)
			}
			if got.DBComment, err = caps.DatabaseCommentChange(ctx, c); err != nil {
				t.Fatal(err)
			}
			if got != p {
				t.Errorf("probed %+v, want %+v", got, p)
			}
			if s, err := caps.AccessStorage(ctx, c); err != nil || s != ch.StorageLocalDirectory {
				t.Errorf("AccessStorage() = %q, %v", s, err)
			}
		})
	}
}

func TestEntities(t *testing.T) {
	ctx := context.Background()
	db := NewDB().WithUser("alice", "u-1", ch.StorageLocalDirectory).WithUser("prov", "u-2", ch.StorageUsersXML).WithRole("reader", "r-1", ch.StorageReplicated)
	var id, storage string
	if err := ch.QueryRow(ctx, db, sqlx.QueryUserEntity, ch.Params{"name": "prov"}, &id, &storage); err != nil || id != "u-2" || storage != ch.StorageUsersXML {
		t.Fatalf("user = %q %q, %v", id, storage, err)
	}
	if err := ch.QueryRow(ctx, db, sqlx.QueryUserEntity, ch.Params{"name": "reader"}, &id, &storage); !errors.Is(err, ch.ErrNoRows) {
		t.Fatalf("a role is not a user: %v", err)
	}
	if err := ch.QueryRow(ctx, db, sqlx.QueryRoleEntity, ch.Params{"name": "reader"}, &id, &storage); err != nil || id != "r-1" {
		t.Fatalf("role = %q, %v", id, err)
	}
	db.RemoveUser("alice").RemoveRole("reader")
	if err := ch.QueryRow(ctx, db, sqlx.QueryUserEntity, ch.Params{"name": "alice"}, &id, &storage); !errors.Is(err, ch.ErrNoRows) {
		t.Fatalf("removed user: %v", err)
	}
	if err := ch.QueryRow(ctx, db, sqlx.QueryRoleEntity, ch.Params{"name": "reader"}, &id, &storage); !errors.Is(err, ch.ErrNoRows) {
		t.Fatalf("removed role: %v", err)
	}
}

func TestExecScripting(t *testing.T) {
	ctx := context.Background()
	boom := Exception(493, "already exists")
	db := NewDB().OnExec("CREATE ROLE `r`", boom)
	if err := db.Exec(ctx, "CREATE ROLE `r`"); !errors.Is(err, boom) {
		t.Errorf("scripted statement: %v", err)
	}
	if err := db.Exec(ctx, "DROP ROLE IF EXISTS `r`"); err != nil {
		t.Errorf("other statements succeed: %v", err)
	}
	if len(db.Execs) != 2 {
		t.Errorf("Execs = %v", db.Execs)
	}
	db.ExecFn = func(string) error { return nil }
	if err := db.Exec(ctx, "CREATE ROLE `r`"); err != nil {
		t.Errorf("ExecFn takes precedence: %v", err)
	}

	c := NewClient(db)
	c.Servers = []string{"s1", "s2"}
	if s, err := c.ProbeServer(ctx); err != nil || len(s) != 2 {
		t.Errorf("ProbeServer() = %v, %v", s, err)
	}
	c.ProbeErr = boom
	if _, err := c.ProbeServer(ctx); !errors.Is(err, boom) {
		t.Errorf("ProbeErr: %v", err)
	}
}
