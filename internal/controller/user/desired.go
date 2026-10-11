// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package user

import (
	"slices"

	"github.com/infrabay/provider-clickhouse/apis/v1alpha1"
	"github.com/infrabay/provider-clickhouse/internal/clients/clickhouse/sqlx"
	"github.com/infrabay/provider-clickhouse/internal/controller/providerconfig"
	"github.com/infrabay/provider-clickhouse/internal/reason"
)

// desired is the desired state of a user, validated and normalized from its
// spec. Nil pointers are not managed.
type desired struct {
	// jwt is set for a jwt user, password for a password user.
	jwt      *sqlx.JWTAuth
	password *v1alpha1.PasswordAuthentication
	// hosts are normalized; no entries mean ANY.
	hosts           sqlx.Hosts
	defaultRoles    *v1alpha1.DefaultRoles
	defaultDatabase *string
	settingsProfile *string
}

// desiredOf validates and normalizes the parameters of a user. CEL already
// enforces most rules; they are checked again because the provider must
// never send a statement built from an invalid spec. Errors are InvalidSpec.
func desiredOf(p v1alpha1.UserParameters) (desired, error) {
	d := desired{defaultDatabase: p.DefaultDatabase, settingsProfile: p.SettingsProfile}
	a := p.Authentication
	switch {
	case (a.JWT != nil) == (a.Password != nil):
		return desired{}, reason.Errorf(reason.InvalidSpec, "set exactly one of authentication.password or authentication.jwt")
	case a.JWT != nil:
		if a.JWT.Processor == "" {
			return desired{}, reason.Errorf(reason.InvalidSpec, "authentication.jwt.processor is required")
		}
		d.jwt = &sqlx.JWTAuth{Processor: a.JWT.Processor}
		if a.JWT.Claims != nil && len(a.JWT.Claims.Raw) > 0 {
			c, err := sqlx.CanonicalClaims(a.JWT.Claims.Raw)
			if err != nil {
				return desired{}, reason.Errorf(reason.InvalidSpec, "authentication.jwt.claims: %v", err)
			}
			d.jwt.Claims = c
		}
	default:
		pw := a.Password
		if (pw.PasswordSecretRef == nil) == (pw.Generate == nil) {
			return desired{}, reason.Errorf(reason.InvalidSpec, "set exactly one of authentication.password.passwordSecretRef or authentication.password.generate")
		}
		if g := pw.Generate; g != nil && g.Length != nil && (*g.Length < sqlx.MinPasswordLength || *g.Length > sqlx.MaxPasswordLength) {
			return desired{}, reason.Errorf(reason.InvalidSpec, "authentication.password.generate.length must be between %d and %d", sqlx.MinPasswordLength, sqlx.MaxPasswordLength)
		}
		d.password = pw
	}
	hosts, err := sqlx.NormalizeHosts(p.HostIP, p.HostNames)
	if err != nil {
		return desired{}, reason.Wrap(reason.InvalidSpec, err)
	}
	d.hosts = hosts
	if r := p.DefaultRoles; r != nil {
		listed := r.Policy == v1alpha1.DefaultRolesListed || r.Policy == v1alpha1.DefaultRolesAllExcept
		switch {
		case !slices.Contains([]string{v1alpha1.DefaultRolesAll, v1alpha1.DefaultRolesNone, v1alpha1.DefaultRolesListed, v1alpha1.DefaultRolesAllExcept}, r.Policy):
			return desired{}, reason.Errorf(reason.InvalidSpec, "defaultRoles.policy %q is not one of All, None, Listed, AllExcept", r.Policy)
		case listed != (len(r.Roles) > 0):
			return desired{}, reason.Errorf(reason.InvalidSpec, "defaultRoles.roles is required for Listed and AllExcept and forbidden otherwise")
		}
		d.defaultRoles = r
	}
	return d, nil
}

// method returns the authentication method, as the policy names it.
func (d desired) method() v1alpha1.AuthenticationMethod {
	if d.jwt != nil {
		return v1alpha1.AuthenticationMethodJWT
	}
	return v1alpha1.AuthenticationMethodPassword
}

// userWrite returns what the policy's write checks look at.
func (d desired) userWrite() providerconfig.UserWrite {
	w := providerconfig.UserWrite{Authentication: d.method()}
	if d.jwt != nil {
		w.JWTProcessor = d.jwt.Processor
	}
	if d.defaultRoles != nil {
		w.DefaultRoles = d.defaultRoles.Roles
	}
	return w
}

// listed reports whether the default roles are Listed: their comparison
// needs the roles that are granted to the user.
func (d desired) listed() bool {
	return d.defaultRoles != nil && d.defaultRoles.Policy == v1alpha1.DefaultRolesListed
}

// createDefaultRole returns the DEFAULT ROLE clause of CREATE USER:
//
//   - All (or not managed): ALL;
//   - None: NONE;
//   - Listed: NONE. The listed roles must be granted first (ALTER USER ...
//     DEFAULT ROLE with a role that is not granted fails with 512), so the
//     granted subset is applied by Update once RoleGrants grant them;
//   - AllExcept: ALL EXCEPT the roles, which only need to exist.
//
// A Listed or AllExcept user is therefore never left on the implicit
// DEFAULT ROLE ALL, where a role granted in the meantime, even an excluded
// one, would become active.
func (d desired) createDefaultRole() sqlx.DefaultRole {
	r := d.defaultRoles
	if r == nil {
		return sqlx.DefaultRole{All: true}
	}
	switch r.Policy {
	case v1alpha1.DefaultRolesNone, v1alpha1.DefaultRolesListed:
		return sqlx.DefaultRole{}
	case v1alpha1.DefaultRolesAllExcept:
		return sqlx.DefaultRole{All: true, Roles: slices.Clone(r.Roles)}
	}
	return sqlx.DefaultRole{All: true}
}

// wantDefaultRole returns the DEFAULT ROLE a user with these default roles
// should have, given the roles granted to it. For Listed it is the listed
// roles that are granted; the others are returned as notGranted and left
// out until they are granted, so that Update never loops on 512.
func wantDefaultRole(r v1alpha1.DefaultRoles, granted []string) (want sqlx.DefaultRole, notGranted []string) {
	switch r.Policy {
	case v1alpha1.DefaultRolesNone:
		return sqlx.DefaultRole{}, nil
	case v1alpha1.DefaultRolesAllExcept:
		return sqlx.DefaultRole{All: true, Roles: slices.Clone(r.Roles)}, nil
	case v1alpha1.DefaultRolesListed:
		for _, role := range r.Roles {
			if slices.Contains(granted, role) {
				want.Roles = append(want.Roles, role)
			} else {
				notGranted = append(notGranted, role)
			}
		}
		slices.Sort(want.Roles)
		slices.Sort(notGranted)
		return want, notGranted
	}
	return sqlx.DefaultRole{All: true}, nil
}

// defaultRolesOf returns the defaultRoles spec that matches the default
// roles a user has, for late initialization.
func defaultRolesOf(r sqlx.DefaultRole) *v1alpha1.DefaultRoles {
	roles := slices.Clone(r.Roles)
	slices.Sort(roles)
	roles = slices.Compact(roles)
	switch {
	case r.All && len(roles) == 0:
		return &v1alpha1.DefaultRoles{Policy: v1alpha1.DefaultRolesAll}
	case r.All:
		return &v1alpha1.DefaultRoles{Policy: v1alpha1.DefaultRolesAllExcept, Roles: roles}
	case len(roles) == 0:
		return &v1alpha1.DefaultRoles{Policy: v1alpha1.DefaultRolesNone}
	}
	return &v1alpha1.DefaultRoles{Policy: v1alpha1.DefaultRolesListed, Roles: roles}
}
