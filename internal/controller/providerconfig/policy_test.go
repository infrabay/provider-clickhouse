// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package providerconfig

import (
	"testing"

	"github.com/infrabay/provider-clickhouse/apis/v1alpha1"
	"github.com/infrabay/provider-clickhouse/internal/reason"
)

func TestCompilePatternPlacement(t *testing.T) {
	valid := []string{
		"{namespace}_.*",
		"system:serviceaccount:{namespace}:.*",
		"{namespace}@example\\.com",
		"{namespace}\\.analytics",
		"{namespace}/x",
		"{namespace}",
		"prefix-{namespace}_x",
		"{namespace}_[a-z0-9_]+",
		"analytics_reader",
		".*",
		"{namespace}_a|_b",       // every alternative starts with '_'
		"{namespace}(_x|@y\\.z)", // a group of alternatives
		"{namespace}[_@]x",       // a class without namespace characters
		"{namespace}(?i)_x",      // flags stay inside the suffix group
		"{namespace}X",           // upper case never occurs in namespaces
		"app_.*|reporting",       // a static alternation stays anchored
	}
	for _, p := range valid {
		if _, err := CompilePattern(p); err != nil {
			t.Errorf("CompilePattern(%q) = %v, want valid", p, err)
		}
	}
	invalid := []string{
		"{namespace}-.*",      // '-' occurs in namespaces
		".*_{namespace}_.*",   // wildcard before the placeholder
		"[a-z]+{namespace}_x", // regexp before the placeholder
		"{namespace}x",        // letter after
		"{namespace}1",        // digit after
		"{namespace}.*",       // regexp operator after
		"{namespace}\\d",      // class after
		"{namespace}_{namespace}_",
		"a(b",           // syntax error
		"{namespace}_(", // syntax error after the placeholder
		// A top-level alternative or an unbalanced group must not escape the
		// namespace anchor.
		"{namespace}_[a-z]+|.*",
		"{namespace}_x)|(.*",
		"a)|(.*",                // unbalanced group in a static pattern
		"{namespace}[-_]x",      // the class contains '-'
		"{namespace}(?i)K",      // folds to k
		"{namespace}(?i)\u212a", // the Kelvin sign folds to k too
		"{namespace}(_x)?",      // can match the empty string
		"{namespace}$",
	}
	for _, p := range invalid {
		if _, err := CompilePattern(p); err == nil {
			t.Errorf("CompilePattern(%q) = nil, want an error", p)
		}
	}
}

func TestPatternMatch(t *testing.T) {
	p, err := CompilePattern("{namespace}_[a-z0-9_]+")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		ns, name string
		want     bool
	}{
		{"team-a", "team-a_db", true},
		{"team-a", "team-b_db", false},
		{"team-a", "xteam-a_db", false},  // anchored at the start
		{"team-a", "team-a_db-x", false}, // anchored at the end
		{"team.a", "teamxa_db", false},   // the namespace is regexp-quoted
		{"team.a", "team.a_db", true},
	}
	for _, tc := range cases {
		if got := p.Match(tc.ns, tc.name); got != tc.want {
			t.Errorf("Match(%q, %q) = %t, want %t", tc.ns, tc.name, got, tc.want)
		}
	}
	s, _ := CompilePattern("app_.*")
	if !s.Match("any", "app_x") || s.Match("any", "xapp_x") {
		t.Error("static patterns are anchored")
	}
}

func TestPatternCannotEscapeNamespace(t *testing.T) {
	// Patterns that compile must never let one namespace use another's
	// names.
	for _, raw := range []string{"{namespace}_a|_b", "{namespace}(_x|@y)", "{namespace}_.*", "{namespace}"} {
		p, err := CompilePattern(raw)
		if err != nil {
			t.Fatalf("CompilePattern(%q) = %v", raw, err)
		}
		for _, name := range []string{"team-b_a", "team-b_b", "team-b@y", "team-b", "team-ab_a", "team-a-b_a"} {
			if p.Match("team-a", name) {
				t.Errorf("%q: namespace team-a matches %q", raw, name)
			}
		}
	}
	s, err := CompilePattern("app_.*|reporting")
	if err != nil {
		t.Fatal(err)
	}
	if !s.Match("x", "reporting") || s.Match("x", "xreporting") || s.Match("x", "reporting_x") {
		t.Error("a static alternation must stay anchored")
	}
}

func TestPolicyClosedByDefault(t *testing.T) {
	p, err := CompilePolicy(v1alpha1.ClusterPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if p.NamespaceAllowed("team-a") {
		t.Error("no namespace is allowed by default")
	}
	checks := map[string]error{
		"database":  p.CheckDatabaseName("team-a", "team-a_db"),
		"user":      p.CheckUserName("team-a", "u"),
		"role":      p.CheckRoleName("team-a", "r"),
		"grantable": p.CheckGrantableRole("team-a", "r"),
		"auth":      p.CheckAuthentication(v1alpha1.AuthenticationMethodJWT),
		"processor": p.CheckJWTProcessor("kubernetes"),
		"global":    p.CheckGlobalGrant(),
		"zkPath":    p.CheckZooKeeperPath("team-a", "/clickhouse/databases/team-a/x"),
		"grantOpt":  p.CheckGrantOption(),
		"adminOpt":  p.CheckAdminOption(),
	}
	for name, err := range checks {
		if !reason.Is(err, reason.PolicyViolation) {
			t.Errorf("%s: %v, want PolicyViolation", name, err)
		}
	}
	if p.AdoptionAllowed() {
		t.Error("adoption is not allowed by default")
	}
}

func TestPolicyUnrestricted(t *testing.T) {
	p, err := CompilePolicy(v1alpha1.ClusterPolicy{Unrestricted: true})
	if err != nil {
		t.Fatal(err)
	}
	if !p.NamespaceAllowed("x") || p.CheckDatabaseName("x", "anything") != nil || p.CheckGlobalGrant() != nil ||
		p.CheckGrantOption() != nil || p.CheckAdminOption() != nil || p.CheckJWTProcessor("p") != nil {
		t.Error("unrestricted allows everything")
	}
	if p.AdoptionAllowed() {
		t.Error("unrestricted does not imply adoption")
	}
}

func TestPolicyShared(t *testing.T) {
	p, err := CompilePolicy(v1alpha1.ClusterPolicy{
		AllowedNamespaces:     []string{"team-.*"},
		DatabaseNamePatterns:  []string{"{namespace}_[a-z0-9_]+"},
		UserNamePatterns:      []string{"system:serviceaccount:{namespace}:[a-z0-9-]+"},
		AllowedAuthentication: []v1alpha1.AuthenticationMethod{v1alpha1.AuthenticationMethodJWT},
		AllowedJWTProcessors:  []string{"kubernetes"},
		AllowAdoption:         true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !p.NamespaceAllowed("team-a") || p.NamespaceAllowed("default") || p.NamespaceAllowed("xteam-a") {
		t.Error("allowedNamespaces")
	}
	if p.CheckDatabaseName("team-a", "team-a_analytics") != nil || p.CheckDatabaseName("team-a", "team-b_analytics") == nil {
		t.Error("databaseNamePatterns")
	}
	if p.CheckUserName("team-a", "system:serviceaccount:team-a:api") != nil || p.CheckUserName("team-a", "system:serviceaccount:team-b:api") == nil {
		t.Error("userNamePatterns")
	}
	if p.CheckAuthentication(v1alpha1.AuthenticationMethodJWT) != nil || p.CheckAuthentication(v1alpha1.AuthenticationMethodPassword) == nil {
		t.Error("allowedAuthentication")
	}
	if !p.AdoptionAllowed() {
		t.Error("allowAdoption")
	}
}

func TestCompilePolicyInvalid(t *testing.T) {
	for name, cp := range map[string]v1alpha1.ClusterPolicy{
		"BadPlacement": {RoleNamePatterns: []string{"{namespace}-x"}},
		// The namespace would be matched against itself: every namespace.
		"NamespaceInAllowedNamespaces": {AllowedNamespaces: []string{"{namespace}"}},
		"EscapingAlternative":          {DatabaseNamePatterns: []string{"{namespace}_[a-z]+|.*"}},
		"UnbalancedGroup":              {DatabaseNamePatterns: []string{"{namespace}_x)|(.*"}},
		"BadZooKeeperPathPattern":      {ZooKeeperPathPatterns: []string{"/x/{namespace}-y"}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := CompilePolicy(cp); !reason.Is(err, reason.InvalidConfig) {
				t.Fatalf("CompilePolicy() = %v, want InvalidConfig", err)
			}
		})
	}
}

func TestZooKeeperPathPolicy(t *testing.T) {
	closed, err := CompilePolicy(v1alpha1.ClusterPolicy{AllowedNamespaces: []string{"team-.*"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := closed.CheckZooKeeperPath("team-a", v1alpha1.DefaultZooKeeperPath); err != nil {
		t.Errorf("the default path is always allowed: %v", err)
	}
	for _, path := range []string{
		"/clickhouse/databases/team-b_prod",            // another tenant's path
		"/clickhouse/databases/victim/replicas/{uuid}", // nested under another database
		"/clickhouse/databases/{uuid}/x",
		"/clickhouse/databases/{database}",
	} {
		if err := closed.CheckZooKeeperPath("team-a", path); !reason.Is(err, reason.PolicyViolation) {
			t.Errorf("CheckZooKeeperPath(%q) = %v, want PolicyViolation", path, err)
		}
	}

	anchored, err := CompilePolicy(v1alpha1.ClusterPolicy{ZooKeeperPathPatterns: []string{"/clickhouse/databases/{namespace}/\\{uuid\\}"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := anchored.CheckZooKeeperPath("team-a", "/clickhouse/databases/team-a/{uuid}"); err != nil {
		t.Errorf("own namespace: %v", err)
	}
	if err := anchored.CheckZooKeeperPath("team-a", "/clickhouse/databases/team-b/{uuid}"); err == nil {
		t.Error("another namespace's path must be refused")
	}

	open, err := CompilePolicy(v1alpha1.ClusterPolicy{Unrestricted: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := open.CheckZooKeeperPath("team-a", "/anything"); err != nil {
		t.Errorf("unrestricted: %v", err)
	}
}
