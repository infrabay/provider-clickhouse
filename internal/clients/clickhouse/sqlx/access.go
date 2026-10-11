// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package sqlx

// Access entity reads with server-side parameters, shared by every access
// entity kind. system.users and system.roles are not filtered by privileges:
// without SHOW USERS or SHOW ROLES they fail with 497 instead of returning
// fewer rows, so zero rows mean that no entity has the name.
const (
	// QueryUserEntity reads a user's UUID and access storage (users_xml,
	// local_directory, replicated or memory).
	QueryUserEntity = "SELECT toString(id), storage FROM system.users WHERE name = {name:String}"

	// QueryRoleEntity reads a role's UUID and access storage.
	QueryRoleEntity = "SELECT toString(id), storage FROM system.roles WHERE name = {name:String}"
)
