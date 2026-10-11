// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package sqlx

import ch "github.com/infrabay/provider-clickhouse/internal/clients/clickhouse"

// Role grant reads with server-side parameters (design 5.7). system.role_grants
// is not filtered by grantee: with SELECT on it every user's and role's rows
// are visible, and without it the query fails with 497, so zero rows mean
// that the grantee does not hold the role. The columns are the same on every
// supported version: user_name and role_name are Nullable(String) (one of
// them is set), with_admin_option and granted_role_is_default are UInt8.
const (
	// QueryUserRoleGrant reads the membership of the user {grantee} in the
	// role {memberOf}.
	QueryUserRoleGrant = "SELECT with_admin_option, granted_role_is_default FROM system.role_grants " +
		"WHERE granted_role_name = {memberOf:String} AND user_name = {grantee:String}"

	// QueryRoleRoleGrant reads the membership of the role {grantee} in the
	// role {memberOf}.
	QueryRoleRoleGrant = "SELECT with_admin_option, granted_role_is_default FROM system.role_grants " +
		"WHERE granted_role_name = {memberOf:String} AND role_name = {grantee:String}"
)

// QueryRoleGrant returns QueryRoleRoleGrant for a role grantee and
// QueryUserRoleGrant for a user grantee.
func QueryRoleGrant(roleGrantee bool) string {
	if roleGrantee {
		return QueryRoleRoleGrant
	}
	return QueryUserRoleGrant
}

// GrantRoleStmt returns GRANT `memberOf` TO `grantee` [WITH ADMIN OPTION].
// The quoted name is always a role, even when it reads like a privilege
// (`SELECT`). The grantee is resolved user-first: a user with the grantee's
// name takes the grant, so a role grantee must not share its name with a
// user. Without WITH ADMIN OPTION an admin option the grantee already holds
// is kept; only RevokeAdminOptionStmt removes it.
func GrantRoleStmt(memberOf, grantee string, adminOption bool) string {
	s := "GRANT " + ch.QuoteIdent(memberOf) + " TO " + ch.QuoteIdent(grantee)
	if adminOption {
		s += " WITH ADMIN OPTION"
	}
	return s
}

// RevokeAdminOptionStmt returns REVOKE ADMIN OPTION FOR `memberOf` FROM
// `grantee`. It keeps the membership; it is a no-op when the grantee does not
// hold the admin option or the role.
func RevokeAdminOptionStmt(memberOf, grantee string) string {
	return "REVOKE ADMIN OPTION FOR " + ch.QuoteIdent(memberOf) + " FROM " + ch.QuoteIdent(grantee)
}

// RevokeRoleStmt returns REVOKE `memberOf` FROM `grantee`. It removes the
// membership, with its admin option, and nothing else; it is a no-op when the
// grantee does not hold the role. An unknown role or grantee gives 511.
func RevokeRoleStmt(memberOf, grantee string) string {
	return "REVOKE " + ch.QuoteIdent(memberOf) + " FROM " + ch.QuoteIdent(grantee)
}
