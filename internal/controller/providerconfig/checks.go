// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package providerconfig

import (
	"regexp"

	"github.com/infrabay/provider-clickhouse/apis/v1alpha1"
	"github.com/infrabay/provider-clickhouse/internal/reason"
)

// The checks below bundle the per-kind policy checks of design 5.8. Name
// checks run for every management policy, including observe-only, and also
// while the MR is being deleted (where a failure orphans the MR instead of
// failing it). Write checks run only when the management policies allow
// Create or Update, and never while deleting. All of them pass for a nil
// policy (a namespaced ProviderConfig), except the JWT processor syntax,
// which applies to every config kind.

// A GranteeKind is the kind of a Grant or RoleGrant grantee.
type GranteeKind string

// Grantee kinds, as rendered in status.atProvider.grantee ("user:<name>").
const (
	GranteeUser GranteeKind = "user"
	GranteeRole GranteeKind = "role"
)

// GlobalDatabase is the Grant database value for ON *.*.
const GlobalDatabase = "*"

// CheckGrantee is the name check of a Grant or RoleGrant grantee: a user
// must match userNamePatterns, a role roleNamePatterns.
func (p *Policy) CheckGrantee(namespace string, kind GranteeKind, name string) error {
	switch kind {
	case GranteeUser:
		return p.CheckUserName(namespace, name)
	case GranteeRole:
		return p.CheckRoleName(namespace, name)
	}
	return reason.Errorf(reason.Internal, "unknown grantee kind %q", kind)
}

// CheckGrantTarget is the name check of a Grant's target database: it must
// match databaseNamePatterns. The global target "*" is limited by the write
// check (allowGlobalGrants) instead, so that deleting a global Grant is never
// blocked by a policy change.
func (p *Policy) CheckGrantTarget(namespace, database string) error {
	if database == GlobalDatabase {
		return nil
	}
	return p.CheckDatabaseName(namespace, database)
}

// CheckGrantWrite is the write check of a Grant: the global target "*" needs
// allowGlobalGrants and withGrantOption needs allowGrantOption.
func (p *Policy) CheckGrantWrite(database string, withGrantOption bool) error {
	if database == GlobalDatabase {
		if err := p.CheckGlobalGrant(); err != nil {
			return err
		}
	}
	if withGrantOption {
		return p.CheckGrantOption()
	}
	return nil
}

// CheckRoleGrantNames is the name check of a RoleGrant: the grantee matches
// the user or role patterns, and memberOf (the role being granted) matches
// grantableRoles. The latter also covers roles from users.xml/users.d, which
// may be granted only if the policy lists them.
func (p *Policy) CheckRoleGrantNames(namespace string, kind GranteeKind, grantee, memberOf string) error {
	if err := p.CheckGrantee(namespace, kind, grantee); err != nil {
		return err
	}
	return p.CheckGrantableRole(namespace, memberOf)
}

// CheckRoleGrantWrite is the write check of a RoleGrant: adminOption needs
// allowAdminOption.
func (p *Policy) CheckRoleGrantWrite(adminOption bool) error {
	if adminOption {
		return p.CheckAdminOption()
	}
	return nil
}

// UserWrite holds the fields of a User that its write checks look at.
type UserWrite struct {
	// Authentication is the desired method.
	Authentication v1alpha1.AuthenticationMethod
	// JWTProcessor is the pinned processor of a jwt user.
	JWTProcessor string
	// DefaultRoles are the roles of a Listed or AllExcept defaultRoles.
	DefaultRoles []string
}

// CheckUserWrite is the write check of a User. On every config kind a jwt
// processor name must be one a server can configure (the server accepts any
// name at DDL time, and the user then cannot log in). On a
// ClusterProviderConfig the method must be in allowedAuthentication, the
// processor in allowedJWTProcessors and every default role in
// grantableRoles.
func (p *Policy) CheckUserWrite(namespace string, w UserWrite) error {
	if w.Authentication == v1alpha1.AuthenticationMethodJWT {
		if err := ValidateJWTProcessorName(w.JWTProcessor); err != nil {
			return err
		}
	}
	if err := p.CheckAuthentication(w.Authentication); err != nil {
		return err
	}
	if w.Authentication == v1alpha1.AuthenticationMethodJWT {
		if err := p.CheckJWTProcessor(w.JWTProcessor); err != nil {
			return err
		}
	}
	for _, r := range w.DefaultRoles {
		if err := p.CheckGrantableRole(namespace, r); err != nil {
			return err
		}
	}
	return nil
}

// jwtProcessorName is the syntax of a token processor name. Processors are
// keys under token_processors in the server configuration: a name with a '.'
// (the configuration path separator), a space, a quote or another special
// character can never be configured, so a user pinned to it could never log
// in.
var jwtProcessorName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]{0,127}$`)

// ValidateJWTProcessorName returns InvalidSpec unless name can be the name of
// a configured token processor: a letter or '_' followed by at most 127
// letters, digits, '_' or '-'. The server does not check processor names at
// DDL time, and no system table lists the configured processors, so this and
// policy.allowedJWTProcessors are the only checks.
func ValidateJWTProcessorName(name string) error {
	if !jwtProcessorName.MatchString(name) {
		return reason.Errorf(reason.InvalidSpec,
			"token processor name %q is not valid: a processor is a key under token_processors in the server configuration (a letter or '_' followed by letters, digits, '_' or '-')", name)
	}
	return nil
}
