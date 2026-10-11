// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package sqlx

import ch "github.com/infrabay/provider-clickhouse/internal/clients/clickhouse"

// Role statements (design 5.5). The role's UUID and storage are read with
// QueryRoleEntity; the name collision check reads QueryUserEntity. The
// provider never uses OR REPLACE: CREATE ROLE OR REPLACE assigns a new UUID
// and removes the role from its grantees.

// CreateRoleStmt returns CREATE ROLE `name`. A role or user with the same
// name in another storage does not make it fail, so the name collision check
// runs before it; an existing role gives 493, a role from users.xml/users.d
// 495.
func CreateRoleStmt(name string) string {
	return "CREATE ROLE " + ch.QuoteIdent(name)
}

// DropRoleStmt returns DROP ROLE IF EXISTS `name`. Dropping a role removes it
// from every grantee and from every user's default role list.
func DropRoleStmt(name string) string {
	return "DROP ROLE IF EXISTS " + ch.QuoteIdent(name)
}
