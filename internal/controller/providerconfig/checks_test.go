// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package providerconfig

import (
	"strings"
	"testing"

	"github.com/infrabay/provider-clickhouse/apis/v1alpha1"
	"github.com/infrabay/provider-clickhouse/internal/reason"
)

// sharedPolicy is the shared ClusterProviderConfig example of design 3.3.
func sharedPolicy(t *testing.T, mutate ...func(*v1alpha1.ClusterPolicy)) *Policy {
	t.Helper()
	cp := v1alpha1.ClusterPolicy{
		AllowedNamespaces:     []string{"team-.*"},
		UserNamePatterns:      []string{"{namespace}_[a-z0-9_]+", "system:serviceaccount:{namespace}:[a-z0-9-]+"},
		RoleNamePatterns:      []string{"{namespace}_[a-z0-9_]+"},
		DatabaseNamePatterns:  []string{"{namespace}_[a-z0-9_]+"},
		GrantableRoles:        []string{"{namespace}_[a-z0-9_]+", "analytics_reader"},
		AllowedAuthentication: []v1alpha1.AuthenticationMethod{v1alpha1.AuthenticationMethodJWT},
		AllowedJWTProcessors:  []string{"kubernetes"},
	}
	for _, m := range mutate {
		m(&cp)
	}
	p, err := CompilePolicy(cp)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func wantReason(t *testing.T, what string, err error, r reason.Reason) {
	t.Helper()
	if r == "" {
		if err != nil {
			t.Errorf("%s: %v, want nil", what, err)
		}
		return
	}
	if !reason.Is(err, r) {
		t.Errorf("%s: %v, want %s", what, err, r)
	}
}

// A namespaced ProviderConfig has no policy (D-3): every policy check
// passes, adoption is allowed, and only the processor syntax still applies.
func TestNilPolicy(t *testing.T) {
	var p *Policy
	if !p.NamespaceAllowed("any") || !p.Unrestricted() || !p.AdoptionAllowed() {
		t.Error("a nil policy allows every namespace and adoption")
	}
	for name, err := range map[string]error{
		"database":   p.CheckDatabaseName("ns", "x"),
		"zkPath":     p.CheckZooKeeperPath("ns", "/any"),
		"user":       p.CheckUserName("ns", "x"),
		"role":       p.CheckRoleName("ns", "x"),
		"grantable":  p.CheckGrantableRole("ns", "x"),
		"auth":       p.CheckAuthentication(v1alpha1.AuthenticationMethodPassword),
		"processor":  p.CheckJWTProcessor("p"),
		"global":     p.CheckGlobalGrant(),
		"grantOpt":   p.CheckGrantOption(),
		"adminOpt":   p.CheckAdminOption(),
		"grantee":    p.CheckGrantee("ns", GranteeRole, "x"),
		"target":     p.CheckGrantTarget("ns", "x"),
		"grantWrite": p.CheckGrantWrite(GlobalDatabase, true),
		"rgNames":    p.CheckRoleGrantNames("ns", GranteeUser, "u", "admin_role"),
		"rgWrite":    p.CheckRoleGrantWrite(true),
		"userWrite":  p.CheckUserWrite("ns", UserWrite{Authentication: v1alpha1.AuthenticationMethodJWT, JWTProcessor: "any_proc", DefaultRoles: []string{"r"}}),
	} {
		wantReason(t, name, err, "")
	}
	wantReason(t, "processor syntax", p.CheckUserWrite("ns", UserWrite{Authentication: v1alpha1.AuthenticationMethodJWT, JWTProcessor: "a.b"}), reason.InvalidSpec)
}

func TestCheckGrantee(t *testing.T) {
	p := sharedPolicy(t)
	wantReason(t, "own user", p.CheckGrantee("team-a", GranteeUser, "team-a_api"), "")
	wantReason(t, "own sa user", p.CheckGrantee("team-a", GranteeUser, "system:serviceaccount:team-a:api"), "")
	wantReason(t, "other tenant's user", p.CheckGrantee("team-a", GranteeUser, "team-b_api"), reason.PolicyViolation)
	wantReason(t, "own role", p.CheckGrantee("team-a", GranteeRole, "team-a_reader"), "")
	wantReason(t, "user name as role", p.CheckGrantee("team-a", GranteeRole, "system:serviceaccount:team-a:api"), reason.PolicyViolation)
	wantReason(t, "unknown kind", p.CheckGrantee("team-a", "group", "team-a_x"), reason.Internal)
}

func TestCheckGrant(t *testing.T) {
	p := sharedPolicy(t)
	wantReason(t, "own database", p.CheckGrantTarget("team-a", "team-a_db"), "")
	wantReason(t, "other database", p.CheckGrantTarget("team-a", "team-b_db"), reason.PolicyViolation)
	// The name check never blocks a global grant (and so never blocks its
	// deletion); the write check does.
	wantReason(t, "global name", p.CheckGrantTarget("team-a", GlobalDatabase), "")
	wantReason(t, "global write", p.CheckGrantWrite(GlobalDatabase, false), reason.PolicyViolation)
	wantReason(t, "grant option", p.CheckGrantWrite("team-a_db", true), reason.PolicyViolation)
	wantReason(t, "plain write", p.CheckGrantWrite("team-a_db", false), "")
	// A database literally named "team-a_*" is a name like any other.
	wantReason(t, "star inside a name", p.CheckGrantTarget("team-a", "team-a_*"), reason.PolicyViolation)

	open := sharedPolicy(t, func(cp *v1alpha1.ClusterPolicy) { cp.AllowGlobalGrants, cp.AllowGrantOption = true, true })
	wantReason(t, "global allowed", open.CheckGrantWrite(GlobalDatabase, true), "")

	unrestricted, err := CompilePolicy(v1alpha1.ClusterPolicy{Unrestricted: true})
	if err != nil {
		t.Fatal(err)
	}
	wantReason(t, "unrestricted", unrestricted.CheckGrantWrite(GlobalDatabase, true), "")
	wantReason(t, "unrestricted target", unrestricted.CheckGrantTarget("x", "anything"), "")
}

func TestCheckRoleGrant(t *testing.T) {
	p := sharedPolicy(t)
	wantReason(t, "own role to own user", p.CheckRoleGrantNames("team-a", GranteeUser, "team-a_api", "team-a_reader"), "")
	wantReason(t, "shared users.d role", p.CheckRoleGrantNames("team-a", GranteeUser, "team-a_api", "analytics_reader"), "")
	wantReason(t, "unlisted users.d role", p.CheckRoleGrantNames("team-a", GranteeUser, "team-a_api", "super_admin"), reason.PolicyViolation)
	wantReason(t, "other tenant's role", p.CheckRoleGrantNames("team-a", GranteeRole, "team-a_x", "team-b_reader"), reason.PolicyViolation)
	wantReason(t, "other tenant's grantee", p.CheckRoleGrantNames("team-a", GranteeUser, "team-b_api", "team-a_reader"), reason.PolicyViolation)
	wantReason(t, "role inheritance", p.CheckRoleGrantNames("team-a", GranteeRole, "team-a_writer", "team-a_reader"), "")
	wantReason(t, "admin option", p.CheckRoleGrantWrite(true), reason.PolicyViolation)
	wantReason(t, "no admin option", p.CheckRoleGrantWrite(false), "")
	withAdmin := sharedPolicy(t, func(cp *v1alpha1.ClusterPolicy) { cp.AllowAdminOption = true })
	wantReason(t, "admin option allowed", withAdmin.CheckRoleGrantWrite(true), "")
}

func TestCheckUserWrite(t *testing.T) {
	p := sharedPolicy(t)
	jwt := func(proc string, roles ...string) UserWrite {
		return UserWrite{Authentication: v1alpha1.AuthenticationMethodJWT, JWTProcessor: proc, DefaultRoles: roles}
	}
	wantReason(t, "allowed jwt", p.CheckUserWrite("team-a", jwt("kubernetes", "team-a_reader", "analytics_reader")), "")
	wantReason(t, "password not allowed", p.CheckUserWrite("team-a", UserWrite{Authentication: v1alpha1.AuthenticationMethodPassword}), reason.PolicyViolation)
	wantReason(t, "processor not allowed", p.CheckUserWrite("team-a", jwt("google")), reason.PolicyViolation)
	wantReason(t, "invalid processor", p.CheckUserWrite("team-a", jwt("kubernetes.x")), reason.InvalidSpec)
	wantReason(t, "default role not grantable", p.CheckUserWrite("team-a", jwt("kubernetes", "team-b_reader")), reason.PolicyViolation)

	unrestricted, err := CompilePolicy(v1alpha1.ClusterPolicy{Unrestricted: true})
	if err != nil {
		t.Fatal(err)
	}
	wantReason(t, "unrestricted", unrestricted.CheckUserWrite("x", jwt("any_proc", "any")), "")
	wantReason(t, "unrestricted still validates the processor", unrestricted.CheckUserWrite("x", jwt("p'1")), reason.InvalidSpec)
	// The processor is not looked at for password users.
	wantReason(t, "password ignores processor", unrestricted.CheckUserWrite("x", UserWrite{Authentication: v1alpha1.AuthenticationMethodPassword, JWTProcessor: "a b"}), "")
}

func TestValidateJWTProcessorName(t *testing.T) {
	for _, ok := range []string{"google", "kubernetes", "kubernetes_static", "k8s-prod", "_internal", "P1", strings.Repeat("a", 128)} {
		if err := ValidateJWTProcessorName(ok); err != nil {
			t.Errorf("ValidateJWTProcessorName(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "a.b", "a b", "p'1", "1abc", "-x", "a/b", "ü", "a\nb", strings.Repeat("a", 129)} {
		if err := ValidateJWTProcessorName(bad); !reason.Is(err, reason.InvalidSpec) {
			t.Errorf("ValidateJWTProcessorName(%q) = %v, want InvalidSpec", bad, err)
		}
	}
}
