// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package sqlx

import (
	"errors"
	"slices"
	"strings"

	ch "github.com/infrabay/provider-clickhouse/internal/clients/clickhouse"
)

// User queries with server-side parameters. system.users,
// system.settings_profile_elements and system.role_grants are not filtered
// by privileges: without the grant that reading them needs they fail with 497
// instead of returning fewer rows.
const (
	userColumns = "toString(id), arrayMap(x -> toString(x), auth_type), host_ip, host_names, host_names_regexp, host_names_like, " +
		"default_roles_all, default_roles_list, default_roles_except, default_database"

	// QueryUser reads the details of a user on servers without
	// system.users.valid_until (25.3, 25.8). VALID UNTIL is then visible
	// only in SHOW CREATE USER.
	QueryUser = "SELECT " + userColumns + " FROM system.users WHERE name = {name:String}"

	// QueryUserWithValidUntil also reads the expiry of every authentication
	// method, in Unix seconds, where 0 means no expiry. The column is
	// Array(DateTime) on Antalya 26.3 and 26.6 and Array(DateTime64(0)) on
	// 26.8; toInt64 gives seconds for both.
	QueryUserWithValidUntil = "SELECT " + userColumns + ", arrayMap(x -> toInt64(x), valid_until) FROM system.users WHERE name = {name:String}"

	// QueryUserSettings reads the settings profile elements of a user, one
	// per row: an inherited profile or an individual setting (NULL is read
	// as '').
	QueryUserSettings = "SELECT ifNull(inherit_profile, ''), ifNull(setting_name, '') FROM system.settings_profile_elements WHERE user_name = {name:String} ORDER BY index"

	// QueryUserGrantedRoles reads the roles granted to a user.
	QueryUserGrantedRoles = "SELECT granted_role_name FROM system.role_grants WHERE user_name = {name:String}"
)

// ShowCreateUser returns SHOW CREATE USER `name`. Read its output raw
// through the driver and parse it with ParseShowCreateUser.
func ShowCreateUser(name string) string { return "SHOW CREATE USER " + ch.QuoteIdent(name) }

// Authentication method names as system.users.auth_type and SHOW CREATE USER
// report them. A user created with sha256_hash reports sha256_password.
const (
	AuthJWT            = "jwt"
	AuthSHA256Password = "sha256_password"
	AuthNoPassword     = "no_password"
)

// A UserAuth is an IDENTIFIED clause the provider writes: exactly one of a
// salted SHA-256 password hash and a jwt method with a pinned processor.
type UserAuth struct {
	Password *PasswordHash
	JWT      *JWTAuth
}

// A PasswordHash is sent as IDENTIFIED WITH sha256_hash BY '<Hash>' SALT
// '<Salt>', so the password itself never reaches the server.
type PasswordHash struct {
	// Hash is HashPassword(password, Salt): 64 hex digits.
	Hash string
	// Salt is the hex text of 16 random bytes.
	Salt string
}

// JWTAuth is sent as IDENTIFIED WITH jwt PROCESSOR '<Processor>' [CLAIMS
// '<Claims>'].
type JWTAuth struct {
	// Processor is the token_processors entry the user is pinned to.
	Processor string
	// Claims is a canonical JSON object (CanonicalClaims), or "" for none.
	Claims string
}

func (a UserAuth) clause() (string, error) {
	switch {
	case a.Password != nil && a.JWT != nil:
		return "", errors.New("set exactly one authentication method")
	case a.Password != nil:
		if !isHexDigest(a.Password.Hash) {
			return "", errors.New("the password hash must be 64 hex digits")
		}
		if a.Password.Salt == "" {
			return "", errors.New("the password salt is empty")
		}
		return "IDENTIFIED WITH sha256_hash BY " + ch.QuoteString(a.Password.Hash) + " SALT " + ch.QuoteString(a.Password.Salt), nil
	case a.JWT != nil:
		if a.JWT.Processor == "" {
			return "", errors.New("a jwt user needs a processor")
		}
		s := "IDENTIFIED WITH jwt PROCESSOR " + ch.QuoteString(a.JWT.Processor)
		if a.JWT.Claims != "" {
			s += " CLAIMS " + ch.QuoteString(a.JWT.Claims)
		}
		return s, nil
	}
	return "", errors.New("no authentication method")
}

func isHexDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

// AnyHostIP is how the server stores HOST ANY: host_ip = ['::/0']. A list
// with 0.0.0.0/0 or ::/0 collapses to it.
const AnyHostIP = "::/0"

// Hosts is a HOST clause: allowed client addresses (normalized, see
// NormalizeHostIP) and exact client host names. No entries, or the full
// range alone, mean ANY.
type Hosts struct {
	IPs   []string
	Names []string
}

// Any reports whether h allows every client address.
func (h Hosts) Any() bool {
	return len(h.Names) == 0 && (len(h.IPs) == 0 || (len(h.IPs) == 1 && h.IPs[0] == AnyHostIP))
}

// Equal reports whether h and o are the same sets. Both must be normalized.
func (h Hosts) Equal(o Hosts) bool {
	if h.Any() || o.Any() {
		return h.Any() == o.Any()
	}
	return slices.Equal(h.IPs, o.IPs) && slices.Equal(h.Names, o.Names)
}

func (h Hosts) clause() string {
	if h.Any() {
		return "HOST ANY"
	}
	var parts []string
	if len(h.IPs) > 0 {
		parts = append(parts, "IP "+quoteStrings(h.IPs))
	}
	if len(h.Names) > 0 {
		parts = append(parts, "NAME "+quoteStrings(h.Names))
	}
	return "HOST " + strings.Join(parts, ", ")
}

// DefaultRole is a DEFAULT ROLE clause, in the shape system.users stores it
// (default_roles_all, default_roles_list, default_roles_except).
type DefaultRole struct {
	// All selects every granted role (ALL), or every granted role except
	// Roles (ALL EXCEPT).
	All bool
	// Roles are the excepted roles when All is set, and the listed roles
	// otherwise; no listed roles is NONE.
	Roles []string
}

// Equal reports whether r and o select the same roles, comparing the role
// lists as sets.
func (r DefaultRole) Equal(o DefaultRole) bool {
	return r.All == o.All && slices.Equal(sortedSet(r.Roles), sortedSet(o.Roles))
}

// String renders r as status.atProvider.defaultRoles does: "ALL", "NONE",
// "r1, r2" or "ALL EXCEPT r1, r2", with sorted, unquoted names.
func (r DefaultRole) String() string {
	roles := strings.Join(sortedSet(r.Roles), ", ")
	switch {
	case r.All && roles == "":
		return "ALL"
	case r.All:
		return "ALL EXCEPT " + roles
	case roles == "":
		return "NONE"
	}
	return roles
}

func (r DefaultRole) clause() string {
	roles := sortedSet(r.Roles)
	switch {
	case r.All && len(roles) == 0:
		return "DEFAULT ROLE ALL"
	case r.All:
		return "DEFAULT ROLE ALL EXCEPT " + quoteIdents(roles)
	case len(roles) == 0:
		return "DEFAULT ROLE NONE"
	}
	return "DEFAULT ROLE " + quoteIdents(roles)
}

// defaultDatabaseClause renders DEFAULT DATABASE `db`, or DEFAULT DATABASE
// NONE for "". The server does not check that the database exists.
func defaultDatabaseClause(db string) string {
	if db == "" {
		return "DEFAULT DATABASE NONE"
	}
	return "DEFAULT DATABASE " + ch.QuoteIdent(db)
}

// settingsClause renders SETTINGS PROFILE '<profile>', which replaces every
// settings element of the user, or SETTINGS NONE for "".
func settingsClause(profile string) string {
	if profile == "" {
		return "SETTINGS NONE"
	}
	return "SETTINGS PROFILE " + ch.QuoteString(profile)
}

// CreateUser are the inputs to CreateUserStmt.
type CreateUser struct {
	Name        string
	Auth        UserAuth
	Hosts       Hosts
	DefaultRole DefaultRole
	// DefaultDatabase is omitted when nil; "" is DEFAULT DATABASE NONE.
	DefaultDatabase *string
	// SettingsProfile is omitted when nil; "" is SETTINGS NONE.
	SettingsProfile *string
}

// CreateUserStmt returns
//
//	CREATE USER `name` IDENTIFIED WITH ... HOST ... DEFAULT ROLE ...
//	  [DEFAULT DATABASE `db` | DEFAULT DATABASE NONE]
//	  [SETTINGS PROFILE '<profile>' | SETTINGS NONE]
//
// This clause order is accepted on every supported server version. The
// statement always carries an explicit IDENTIFIED clause (without one the
// server creates a no_password user) and never uses OR REPLACE, which would
// replace an existing user with a new UUID.
func CreateUserStmt(p CreateUser) (string, error) {
	auth, err := p.Auth.clause()
	if err != nil {
		return "", err
	}
	parts := []string{"CREATE USER " + ch.QuoteIdent(p.Name), auth, p.Hosts.clause(), p.DefaultRole.clause()}
	if p.DefaultDatabase != nil {
		parts = append(parts, defaultDatabaseClause(*p.DefaultDatabase))
	}
	if p.SettingsProfile != nil {
		parts = append(parts, settingsClause(*p.SettingsProfile))
	}
	return strings.Join(parts, " "), nil
}

// AlterUser are the inputs to AlterUserStmt. Nil fields are not changed.
type AlterUser struct {
	Name            string
	Auth            *UserAuth
	Hosts           *Hosts
	DefaultDatabase *string
	SettingsProfile *string
}

// AlterUserStmt returns
//
//	ALTER USER `name` [IDENTIFIED WITH ...] [HOST ...]
//	  [DEFAULT DATABASE ...] [SETTINGS PROFILE '<p>' | SETTINGS NONE]
//
// IDENTIFIED WITH replaces every authentication method of the user,
// including ones added with ADD IDENTIFIED, and clears VALID UNTIL. HOST and
// SETTINGS replace the old values. DEFAULT ROLE has its own statement
// (AlterUserDefaultRoleStmt), so that an error in it does not block the
// other changes.
func AlterUserStmt(p AlterUser) (string, error) {
	parts := []string{"ALTER USER " + ch.QuoteIdent(p.Name)}
	if p.Auth != nil {
		auth, err := p.Auth.clause()
		if err != nil {
			return "", err
		}
		parts = append(parts, auth)
	}
	if p.Hosts != nil {
		parts = append(parts, p.Hosts.clause())
	}
	if p.DefaultDatabase != nil {
		parts = append(parts, defaultDatabaseClause(*p.DefaultDatabase))
	}
	if p.SettingsProfile != nil {
		parts = append(parts, settingsClause(*p.SettingsProfile))
	}
	if len(parts) == 1 {
		return "", errors.New("nothing to alter")
	}
	return strings.Join(parts, " "), nil
}

// AlterUserDefaultRoleStmt returns
// ALTER USER `name` DEFAULT ROLE {ALL | NONE | `r1`, `r2` | ALL EXCEPT `r1`}.
// Listed roles must be granted to the user (512 SET_NON_GRANTED_ROLE
// otherwise); excepted roles only need to exist (511 UNKNOWN_ROLE).
func AlterUserDefaultRoleStmt(name string, r DefaultRole) string {
	return "ALTER USER " + ch.QuoteIdent(name) + " " + r.clause()
}

// DropUserStmt returns DROP USER IF EXISTS `name`.
func DropUserStmt(name string) string { return "DROP USER IF EXISTS " + ch.QuoteIdent(name) }

func quoteStrings(ss []string) string {
	q := make([]string, len(ss))
	for i, s := range ss {
		q[i] = ch.QuoteString(s)
	}
	return strings.Join(q, ", ")
}

func quoteIdents(ss []string) string {
	q := make([]string, len(ss))
	for i, s := range ss {
		q[i] = ch.QuoteIdent(s)
	}
	return strings.Join(q, ", ")
}

// sortedSet returns the distinct elements of ss, sorted. It never modifies
// ss.
func sortedSet(ss []string) []string {
	out := slices.Clone(ss)
	slices.Sort(out)
	return slices.Compact(out)
}
