// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package providerconfig

import (
	"fmt"
	"regexp"
	"regexp/syntax"
	"slices"
	"strings"
	"unicode"

	"github.com/infrabay/provider-clickhouse/apis/v1alpha1"
	"github.com/infrabay/provider-clickhouse/internal/reason"
)

// NamespacePlaceholder is replaced with the MR's namespace in policy patterns.
const NamespacePlaceholder = "{namespace}"

// A Pattern is a policy pattern: RE2, implicitly anchored, with at most one
// {namespace} placeholder.
type Pattern struct {
	raw    string
	prefix string // literal text before {namespace}
	suffix string // regexp after {namespace}
	hasNS  bool
	static *regexp.Regexp // compiled when there is no placeholder
}

// CompilePattern validates and compiles a policy pattern. The placeholder may
// appear at most once; the text before it must be a plain literal, and every
// string the regexp after it can match must start with a character that
// cannot occur in a namespace name (anything but a-z, 0-9 and '-'), in every
// alternative. This keeps the namespace part of a name unambiguous: with a
// wildcard on the left, or a separator that can occur in namespaces, one name
// could match for two namespaces. The placeholder may also end the pattern:
// patterns are anchored, so the name then ends with the namespace.
//
// Every regexp part is parsed on its own before it is wrapped, and is then
// wrapped in its own group, so an unbalanced ")" or a top-level "|" can never
// escape the anchors or the namespace.
func CompilePattern(raw string) (*Pattern, error) {
	p := &Pattern{raw: raw}
	n := strings.Count(raw, NamespacePlaceholder)
	switch {
	case n == 0:
		if _, err := syntax.Parse(raw, syntax.Perl); err != nil {
			return nil, fmt.Errorf("pattern %q: %w", raw, err)
		}
		re, err := regexp.Compile(`^(?:` + raw + `)$`)
		if err != nil {
			return nil, fmt.Errorf("pattern %q: %w", raw, err)
		}
		p.static = re
		return p, nil
	case n > 1:
		return nil, fmt.Errorf("pattern %q: %s may appear at most once", raw, NamespacePlaceholder)
	}
	i := strings.Index(raw, NamespacePlaceholder)
	p.hasNS = true
	p.prefix = raw[:i]
	p.suffix = raw[i+len(NamespacePlaceholder):]
	if regexp.QuoteMeta(p.prefix) != p.prefix {
		return nil, fmt.Errorf("pattern %q: the text before %s must be a plain literal without regexp operators", raw, NamespacePlaceholder)
	}
	if p.suffix != "" {
		re, err := syntax.Parse(p.suffix, syntax.Perl)
		if err != nil {
			return nil, fmt.Errorf("pattern %q: the regexp after %s: %w", raw, NamespacePlaceholder, err)
		}
		if !startsWithSeparator(re) {
			return nil, fmt.Errorf("pattern %q: every match of the regexp after %s must start with a literal character that cannot occur in a namespace name "+
				"(anything but a-z, 0-9 and '-'), in every alternative; for example _ @ \\. : /", raw, NamespacePlaceholder)
		}
	}
	// Compile once with a sample namespace to catch remaining errors early.
	if _, err := p.compile("ns"); err != nil {
		return nil, fmt.Errorf("pattern %q: %w", raw, err)
	}
	return p, nil
}

// startsWithSeparator reports whether every string re matches starts with a
// character that cannot occur in a namespace name. Only literals, character
// classes, concatenations, groups and alternations are understood; anything
// else (repetitions, wildcards, anchors, empty matches) is rejected.
func startsWithSeparator(re *syntax.Regexp) bool {
	switch re.Op {
	case syntax.OpLiteral:
		return len(re.Rune) > 0 && separatorRune(re.Rune[0], re.Flags&syntax.FoldCase != 0)
	case syntax.OpCharClass:
		// Rune holds inclusive ranges; case folding is already expanded.
		if len(re.Rune) == 0 {
			return false
		}
		for i := 0; i+1 < len(re.Rune); i += 2 {
			lo, hi := re.Rune[i], re.Rune[i+1]
			if overlaps(lo, hi, 'a', 'z') || overlaps(lo, hi, '0', '9') || overlaps(lo, hi, '-', '-') {
				return false
			}
		}
		return true
	case syntax.OpConcat, syntax.OpCapture:
		return len(re.Sub) > 0 && startsWithSeparator(re.Sub[0])
	case syntax.OpAlternate:
		for _, sub := range re.Sub {
			if !startsWithSeparator(sub) {
				return false
			}
		}
		return len(re.Sub) > 0
	}
	return false
}

// namespaceRune reports whether r can occur in a namespace name (RFC 1123
// label: a-z, 0-9 and '-').
func namespaceRune(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-'
}

// separatorRune reports whether the literal r (case-folded when fold is set,
// including Unicode folds such as the Kelvin sign) never matches a character
// that can occur in a namespace name.
func separatorRune(r rune, fold bool) bool {
	if namespaceRune(r) {
		return false
	}
	if fold {
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			if namespaceRune(f) {
				return false
			}
		}
	}
	return true
}

func overlaps(lo, hi, a, b rune) bool { return lo <= b && a <= hi }

func (p *Pattern) compile(namespace string) (*regexp.Regexp, error) {
	if !p.hasNS {
		return p.static, nil
	}
	return regexp.Compile(`^(?:` + regexp.QuoteMeta(p.prefix) + regexp.QuoteMeta(namespace) + `(?:` + p.suffix + `))$`)
}

// Match reports whether name matches the pattern for an MR in namespace.
func (p *Pattern) Match(namespace, name string) bool {
	re, err := p.compile(namespace)
	if err != nil {
		return false
	}
	return re.MatchString(name)
}

// String returns the pattern as written.
func (p *Pattern) String() string { return p.raw }

func compileAll(field string, raws []string, placeholder bool) ([]*Pattern, error) {
	out := make([]*Pattern, 0, len(raws))
	for i, r := range raws {
		if !placeholder && strings.Contains(r, NamespacePlaceholder) {
			// The namespace would be matched against itself, which allows
			// every namespace.
			return nil, fmt.Errorf("policy.%s[%d]: %s is not allowed here", field, i, NamespacePlaceholder)
		}
		p, err := CompilePattern(r)
		if err != nil {
			return nil, fmt.Errorf("policy.%s[%d]: %w", field, i, err)
		}
		out = append(out, p)
	}
	return out, nil
}

func matchAny(ps []*Pattern, namespace, name string) bool {
	for _, p := range ps {
		if p.Match(namespace, name) {
			return true
		}
	}
	return false
}

// A Policy is a compiled ClusterPolicy. It is closed by default.
//
// A nil *Policy is the policy of a namespaced ProviderConfig, which has none
// (D-3): every check passes and adoption is allowed. Only the checks that
// apply to every config kind (storage, self-modification, name collisions,
// capabilities, JWT processor syntax) still apply there.
type Policy struct {
	unrestricted bool

	namespaces     []*Pattern
	users          []*Pattern
	roles          []*Pattern
	databases      []*Pattern
	zooKeeperPaths []*Pattern
	grantableRoles []*Pattern
	authentication []v1alpha1.AuthenticationMethod
	processors     []string

	allowGlobalGrants bool
	allowGrantOption  bool
	allowAdminOption  bool
	allowAdoption     bool
}

// CompilePolicy compiles a ClusterPolicy. Any invalid pattern makes the whole
// config invalid (InvalidConfig).
func CompilePolicy(cp v1alpha1.ClusterPolicy) (*Policy, error) {
	p := &Policy{
		unrestricted:      cp.Unrestricted,
		authentication:    slices.Clone(cp.AllowedAuthentication),
		processors:        slices.Clone(cp.AllowedJWTProcessors),
		allowGlobalGrants: cp.AllowGlobalGrants,
		allowGrantOption:  cp.AllowGrantOption,
		allowAdminOption:  cp.AllowAdminOption,
		allowAdoption:     cp.AllowAdoption,
	}
	var err error
	for _, f := range []struct {
		name        string
		in          []string
		out         *[]*Pattern
		placeholder bool
	}{
		{"allowedNamespaces", cp.AllowedNamespaces, &p.namespaces, false},
		{"userNamePatterns", cp.UserNamePatterns, &p.users, true},
		{"roleNamePatterns", cp.RoleNamePatterns, &p.roles, true},
		{"databaseNamePatterns", cp.DatabaseNamePatterns, &p.databases, true},
		{"zooKeeperPathPatterns", cp.ZooKeeperPathPatterns, &p.zooKeeperPaths, true},
		{"grantableRoles", cp.GrantableRoles, &p.grantableRoles, true},
	} {
		if *f.out, err = compileAll(f.name, f.in, f.placeholder); err != nil {
			return nil, reason.Wrap(reason.InvalidConfig, err)
		}
	}
	return p, nil
}

// Unrestricted reports whether every policy check passes: unrestricted is
// set, or there is no policy (a namespaced ProviderConfig).
func (p *Policy) Unrestricted() bool { return p == nil || p.unrestricted }

// NamespaceAllowed reports whether MRs in namespace may use the config.
func (p *Policy) NamespaceAllowed(namespace string) bool {
	return p == nil || p.unrestricted || matchAny(p.namespaces, namespace, namespace)
}

// CheckDatabaseName returns PolicyViolation unless name matches
// databaseNamePatterns.
func (p *Policy) CheckDatabaseName(namespace, name string) error {
	if p == nil || p.unrestricted || matchAny(p.databases, namespace, name) {
		return nil
	}
	return reason.Errorf(reason.PolicyViolation, "database name %q does not match policy.databaseNamePatterns of the ClusterProviderConfig", name)
}

// CheckZooKeeperPath returns PolicyViolation unless a Replicated database's
// Keeper path is the default path (unique per database UUID) or matches
// zooKeeperPathPatterns. A custom path could register a replica under
// another tenant's Replicated database, or nest under its Keeper node.
func (p *Policy) CheckZooKeeperPath(namespace, path string) error {
	if p == nil || p.unrestricted || path == v1alpha1.DefaultZooKeeperPath || matchAny(p.zooKeeperPaths, namespace, path) {
		return nil
	}
	return reason.Errorf(reason.PolicyViolation,
		"Replicated zooKeeperPath %q is not allowed by the ClusterProviderConfig: use the default %q, or a path that matches policy.zooKeeperPathPatterns",
		path, v1alpha1.DefaultZooKeeperPath)
}

// CheckUserName returns PolicyViolation unless name matches userNamePatterns.
func (p *Policy) CheckUserName(namespace, name string) error {
	if p == nil || p.unrestricted || matchAny(p.users, namespace, name) {
		return nil
	}
	return reason.Errorf(reason.PolicyViolation, "user name %q does not match policy.userNamePatterns of the ClusterProviderConfig", name)
}

// CheckRoleName returns PolicyViolation unless name matches roleNamePatterns.
func (p *Policy) CheckRoleName(namespace, name string) error {
	if p == nil || p.unrestricted || matchAny(p.roles, namespace, name) {
		return nil
	}
	return reason.Errorf(reason.PolicyViolation, "role name %q does not match policy.roleNamePatterns of the ClusterProviderConfig", name)
}

// CheckGrantableRole returns PolicyViolation unless role matches
// grantableRoles.
func (p *Policy) CheckGrantableRole(namespace, role string) error {
	if p == nil || p.unrestricted || matchAny(p.grantableRoles, namespace, role) {
		return nil
	}
	return reason.Errorf(reason.PolicyViolation, "role %q does not match policy.grantableRoles of the ClusterProviderConfig", role)
}

// CheckAuthentication returns PolicyViolation unless m is allowed.
func (p *Policy) CheckAuthentication(m v1alpha1.AuthenticationMethod) error {
	if p == nil || p.unrestricted || slices.Contains(p.authentication, m) {
		return nil
	}
	return reason.Errorf(reason.PolicyViolation, "authentication method %s is not in policy.allowedAuthentication of the ClusterProviderConfig", m)
}

// CheckJWTProcessor returns PolicyViolation unless processor is allowed.
func (p *Policy) CheckJWTProcessor(processor string) error {
	if p == nil || p.unrestricted || slices.Contains(p.processors, processor) {
		return nil
	}
	return reason.Errorf(reason.PolicyViolation, "token processor %q is not in policy.allowedJWTProcessors of the ClusterProviderConfig", processor)
}

// CheckGlobalGrant returns PolicyViolation unless global grants are allowed.
func (p *Policy) CheckGlobalGrant() error {
	if p == nil || p.unrestricted || p.allowGlobalGrants {
		return nil
	}
	return reason.Errorf(reason.PolicyViolation, "global grants (database \"*\") need policy.allowGlobalGrants on the ClusterProviderConfig")
}

// CheckGrantOption returns PolicyViolation unless grant option is allowed.
func (p *Policy) CheckGrantOption() error {
	if p == nil || p.unrestricted || p.allowGrantOption {
		return nil
	}
	return reason.Errorf(reason.PolicyViolation, "withGrantOption needs policy.allowGrantOption on the ClusterProviderConfig")
}

// CheckAdminOption returns PolicyViolation unless admin option is allowed.
func (p *Policy) CheckAdminOption() error {
	if p == nil || p.unrestricted || p.allowAdminOption {
		return nil
	}
	return reason.Errorf(reason.PolicyViolation, "adminOption needs policy.allowAdminOption on the ClusterProviderConfig")
}

// AdoptionAllowed reports whether MRs may adopt pre-existing objects.
// Unrestricted does not imply it. Adoption through a namespaced
// ProviderConfig (nil policy) is always allowed.
func (p *Policy) AdoptionAllowed() bool { return p == nil || p.allowAdoption }
