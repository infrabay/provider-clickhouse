// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package user

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	"github.com/crossplane/crossplane-runtime/v2/pkg/reconciler/managed"

	"github.com/infrabay/provider-clickhouse/apis/v1alpha1"
	"github.com/infrabay/provider-clickhouse/internal/clients/clickhouse/sqlx"
	"github.com/infrabay/provider-clickhouse/internal/controller/access"
	"github.com/infrabay/provider-clickhouse/internal/controller/providerconfig"
	"github.com/infrabay/provider-clickhouse/internal/reason"
)

func TestDesiredOf(t *testing.T) {
	d, err := desiredOf(newUser(jwtUser("kubernetes", `{"z": [1, 2], "a": {"y": "x"}}`), hosts([]string{"10.1.2.3/8"}, []string{"b", "a"}),
		roles(v1alpha1.DefaultRolesAllExcept, "r1"), managedFields("db", "p")).Spec.ForProvider)
	if err != nil {
		t.Fatal(err)
	}
	if d.jwt == nil || d.jwt.Processor != "kubernetes" || d.jwt.Claims != `{"a":{"y":"x"},"z":[1,2]}` || d.password != nil {
		t.Errorf("jwt: %+v", d.jwt)
	}
	if diff := cmp.Diff(sqlx.Hosts{IPs: []string{"10.0.0.0/8"}, Names: []string{"a", "b"}}, d.hosts); diff != "" {
		t.Errorf("hosts: %s", diff)
	}
	if d.method() != v1alpha1.AuthenticationMethodJWT || *d.defaultDatabase != "db" || *d.settingsProfile != "p" {
		t.Errorf("desired: %+v", d)
	}
	if diff := cmp.Diff(providerconfig.UserWrite{Authentication: v1alpha1.AuthenticationMethodJWT, JWTProcessor: "kubernetes", DefaultRoles: []string{"r1"}}, d.userWrite()); diff != "" {
		t.Errorf("userWrite: %s", diff)
	}
	p, err := desiredOf(newUser().Spec.ForProvider)
	if err != nil || p.password == nil || p.jwt != nil || p.method() != v1alpha1.AuthenticationMethodPassword || !p.hosts.Any() {
		t.Errorf("password user: %+v, %v", p, err)
	}
	if w := p.userWrite(); w.Authentication != v1alpha1.AuthenticationMethodPassword || w.JWTProcessor != "" || w.DefaultRoles != nil {
		t.Errorf("userWrite: %+v", w)
	}
	if e, err := desiredOf(newUser(jwtUser("p", `{}`)).Spec.ForProvider); err != nil || e.jwt.Claims != "" {
		t.Errorf("empty claims send no CLAIMS clause: %+v, %v", e.jwt, err)
	}

	invalid := map[string]func(*v1alpha1.User){
		"NoMethod": func(u *v1alpha1.User) { u.Spec.ForProvider.Authentication = v1alpha1.UserAuthentication{} },
		"BothMethods": func(u *v1alpha1.User) {
			u.Spec.ForProvider.Authentication.JWT = &v1alpha1.JWTAuthentication{Processor: "p"}
		},
		"NoProcessor": jwtUser("", ""),
		"ClaimsArray": jwtUser("p", `[1]`),
		"ClaimsJunk": func(u *v1alpha1.User) {
			jwtUser("p", "")(u)
			u.Spec.ForProvider.Authentication.JWT.Claims = &apiextensionsv1.JSON{Raw: []byte(`{"a":1}x`)}
		},
		"SecretAndGenerate": func(u *v1alpha1.User) {
			secretRef("s", "k")(u)
			u.Spec.ForProvider.Authentication.Password.Generate = &v1alpha1.GeneratePassword{}
		},
		"NeitherSecretNorGenerate": func(u *v1alpha1.User) {
			u.Spec.ForProvider.Authentication.Password = &v1alpha1.PasswordAuthentication{}
		},
		"ShortLength":           generate(8, ""),
		"LongLength":            generate(129, ""),
		"BadHost":               hosts([]string{"10.0.0.0/33"}, nil),
		"FullRangeWithNames":    hosts([]string{"::/0"}, []string{"a"}),
		"UnknownRolePolicy":     roles("Some"),
		"ListedWithoutRoles":    roles(v1alpha1.DefaultRolesListed),
		"AllExceptWithoutRoles": roles(v1alpha1.DefaultRolesAllExcept),
		"AllWithRoles":          roles(v1alpha1.DefaultRolesAll, "r"),
		"NoneWithRoles":         roles(v1alpha1.DefaultRolesNone, "r"),
	}
	for name, mod := range invalid {
		t.Run(name, func(t *testing.T) {
			if _, err := desiredOf(newUser(mod).Spec.ForProvider); !reason.Is(err, reason.InvalidSpec) {
				t.Errorf("desiredOf() = %v, want InvalidSpec", err)
			}
		})
	}
}

func TestDefaultRoles(t *testing.T) {
	create := map[string]struct {
		in   *v1alpha1.DefaultRoles
		want sqlx.DefaultRole
	}{
		"NotManaged": {nil, sqlx.DefaultRole{All: true}},
		"All":        {&v1alpha1.DefaultRoles{Policy: v1alpha1.DefaultRolesAll}, sqlx.DefaultRole{All: true}},
		"None":       {&v1alpha1.DefaultRoles{Policy: v1alpha1.DefaultRolesNone}, sqlx.DefaultRole{}},
		"Listed":     {&v1alpha1.DefaultRoles{Policy: v1alpha1.DefaultRolesListed, Roles: []string{"r1"}}, sqlx.DefaultRole{}},
		"AllExcept":  {&v1alpha1.DefaultRoles{Policy: v1alpha1.DefaultRolesAllExcept, Roles: []string{"r1"}}, sqlx.DefaultRole{All: true, Roles: []string{"r1"}}},
	}
	for name, tc := range create {
		if diff := cmp.Diff(tc.want, desired{defaultRoles: tc.in}.createDefaultRole()); diff != "" {
			t.Errorf("createDefaultRole(%s): %s", name, diff)
		}
	}

	want, notGranted := wantDefaultRole(v1alpha1.DefaultRoles{Policy: v1alpha1.DefaultRolesListed, Roles: []string{"r3", "r1", "r2"}}, []string{"r2", "x", "r1"})
	if diff := cmp.Diff(sqlx.DefaultRole{Roles: []string{"r1", "r2"}}, want); diff != "" || !cmp.Equal([]string{"r3"}, notGranted) {
		t.Errorf("Listed: %s, not granted %v", diff, notGranted)
	}
	if want, _ := wantDefaultRole(v1alpha1.DefaultRoles{Policy: v1alpha1.DefaultRolesListed, Roles: []string{"r1"}}, nil); want.All || len(want.Roles) != 0 {
		t.Errorf("Listed with nothing granted is NONE: %+v", want)
	}
	for policy, w := range map[string]sqlx.DefaultRole{
		v1alpha1.DefaultRolesAll:       {All: true},
		v1alpha1.DefaultRolesNone:      {},
		v1alpha1.DefaultRolesAllExcept: {All: true, Roles: []string{"r1"}},
	} {
		roles := []string{"r1"}
		if policy == v1alpha1.DefaultRolesAll || policy == v1alpha1.DefaultRolesNone {
			roles = nil
		}
		got, ng := wantDefaultRole(v1alpha1.DefaultRoles{Policy: policy, Roles: roles}, nil)
		if !got.Equal(w) || ng != nil {
			t.Errorf("wantDefaultRole(%s) = %+v, %v", policy, got, ng)
		}
	}

	for name, tc := range map[string]struct {
		in   sqlx.DefaultRole
		want *v1alpha1.DefaultRoles
	}{
		"All":       {sqlx.DefaultRole{All: true}, &v1alpha1.DefaultRoles{Policy: v1alpha1.DefaultRolesAll}},
		"None":      {sqlx.DefaultRole{}, &v1alpha1.DefaultRoles{Policy: v1alpha1.DefaultRolesNone}},
		"Listed":    {sqlx.DefaultRole{Roles: []string{"r2", "r1", "r2"}}, &v1alpha1.DefaultRoles{Policy: v1alpha1.DefaultRolesListed, Roles: []string{"r1", "r2"}}},
		"AllExcept": {sqlx.DefaultRole{All: true, Roles: []string{"r1"}}, &v1alpha1.DefaultRoles{Policy: v1alpha1.DefaultRolesAllExcept, Roles: []string{"r1"}}},
	} {
		if diff := cmp.Diff(tc.want, defaultRolesOf(tc.in)); diff != "" {
			t.Errorf("defaultRolesOf(%s): %s", name, diff)
		}
	}
}

func TestDetails(t *testing.T) {
	e := &External{Env: &access.Env{Resolved: &providerconfig.Resolved{Endpoint: endpoint}}}
	secret := map[string][]byte{
		v1alpha1.ConnectionKeyPassword:      []byte("old"),
		v1alpha1.ConnectionKeyRotationToken: []byte("t1"),
		"custom":                            []byte("kept"),
	}
	cases := map[string]struct {
		in   connection
		want managed.ConnectionDetails
	}{
		"Password": {
			in:   connection{name: userName, password: new("pw"), token: new("t2"), secret: secret},
			want: details(v1alpha1.ConnectionKeyPassword, "pw", v1alpha1.ConnectionKeyRotationToken, "t2"),
		},
		// Keys of the other method are removed, others are left alone.
		"JWTRemovesPasswordKeys": {
			in:   connection{name: userName, processor: "kubernetes", secret: secret},
			want: details(v1alpha1.ConnectionKeyJWTProcessor, "kubernetes", v1alpha1.ConnectionKeyPassword, "<nil>", v1alpha1.ConnectionKeyRotationToken, "<nil>"),
		},
		"JWTKeepsPasswordKeysUntilApplied": {
			in:   connection{name: userName, processor: "kubernetes", secret: secret, keepStale: true},
			want: details(v1alpha1.ConnectionKeyJWTProcessor, "kubernetes", v1alpha1.ConnectionKeyPassword, "old", v1alpha1.ConnectionKeyRotationToken, "t1"),
		},
		// Keys the Secret does not hold are never set to nil.
		"NoSecret": {
			in:   connection{name: userName, processor: "kubernetes"},
			want: details(v1alpha1.ConnectionKeyJWTProcessor, "kubernetes"),
		},
	}
	for name, tc := range cases {
		if diff := cmp.Diff(tc.want, e.details(tc.in)); diff != "" {
			t.Errorf("%s: -want +got:\n%s", name, diff)
		}
	}
	if got := (&External{Env: &access.Env{}}).details(connection{name: "u"}); len(got) != 1 {
		t.Errorf("without a resolved config only the user name is known: %v", got)
	}
}

func TestComparisons(t *testing.T) {
	for _, tc := range []struct {
		elems []settingsElement
		want  string
		ok    bool
	}{
		{nil, "", true},
		{[]settingsElement{{profile: "p"}}, "p", true},
		{[]settingsElement{{setting: "max_threads"}}, "", false},
		{[]settingsElement{{profile: "p"}, {profile: "q"}}, "", false},
		{[]settingsElement{{profile: "p"}, {setting: "max_threads"}}, "", false},
	} {
		if got, ok := singleProfile(tc.elems); got != tc.want || ok != tc.ok {
			t.Errorf("singleProfile(%v) = %q, %t", tc.elems, got, ok)
		}
	}
	if o := (&observed{settings: []settingsElement{{profile: "p"}, {setting: "s"}, {profile: "q"}}}); o.profiles() != "p, q" {
		t.Errorf("profiles() = %q", o.profiles())
	}

	if !claimsEqual("", "") || !claimsEqual("", "{}") || !claimsEqual(`{"a":1}`, `{ "a" : 1 }`) || claimsEqual(`{"a":1}`, "") || claimsEqual("", `{"a":1}`) || claimsEqual(`{"a":1}`, "not json") {
		t.Error("claimsEqual")
	}

	for _, tc := range []struct {
		d    desired
		auth []string
		vu   bool
		want bool
	}{
		{desired{jwt: &sqlx.JWTAuth{}}, []string{"sha256_password"}, true, true},
		{desired{}, []string{"jwt"}, true, true},
		{desired{}, []string{"sha256_password"}, false, true},
		{desired{}, []string{"sha256_password"}, true, false},
	} {
		if got := needsShowCreate(tc.d, tc.auth, tc.vu); got != tc.want {
			t.Errorf("needsShowCreate(%v, %v, %t) = %t", tc.d.jwt != nil, tc.auth, tc.vu, got)
		}
	}

	want := &sqlx.JWTAuth{Processor: "p", Claims: `{"a":1}`}
	ok := &observed{authTypes: []string{"jwt"}, def: &sqlx.UserDefinition{Methods: []sqlx.AuthMethod{{Name: "jwt", Processor: "p", Claims: `{"a": 1}`}}}}
	if !jwtUpToDate(want, ok) {
		t.Error("jwtUpToDate: canonical claims match")
	}
	for name, o := range map[string]*observed{
		"NotRead":    {authTypes: []string{"jwt"}},
		"Password":   {authTypes: []string{"sha256_password"}, def: ok.def},
		"Expiring":   {authTypes: []string{"jwt"}, validUntil: []int64{1}, def: ok.def},
		"OtherClaim": {authTypes: []string{"jwt"}, def: &sqlx.UserDefinition{Methods: []sqlx.AuthMethod{{Name: "jwt", Processor: "p", Claims: `{"a":2}`}}}},
		"Unparsed":   {authTypes: []string{"jwt"}, def: &sqlx.UserDefinition{Methods: []sqlx.AuthMethod{{Name: "x", Processor: "p", Claims: `{"a":1}`}}}},
	} {
		if jwtUpToDate(want, o) {
			t.Errorf("jwtUpToDate(%s) must be false", name)
		}
	}
	if (&observed{}).processor() != "" || (&observed{def: &sqlx.UserDefinition{Methods: []sqlx.AuthMethod{{Name: "sha256_password"}, {Name: "jwt", Processor: "p"}}}}).processor() != "p" {
		t.Error("processor()")
	}
	if !(&observed{}).hostNone() || (&observed{hostLikes: []string{"x"}}).hostNone() {
		t.Error("hostNone()")
	}
	if (&observed{validUntil: []int64{0, 0}}).expires() || !(&observed{validUntil: []int64{0, 5}}).expires() {
		t.Error("expires()")
	}
}
