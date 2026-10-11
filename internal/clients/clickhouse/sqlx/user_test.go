// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package sqlx

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// testHash and testSalt are the password test vector of design 3.5: a user
// created with them logs in with "Pa55word".
const (
	testHash = "6257310c34f98d6e96c672db8cee44e0077f4e03eb3581a7260ccd6861848188" // gitleaks:allow
	testSalt = "00112233445566778899aabbccddeeff"
)

var testPassword = &PasswordHash{Hash: testHash, Salt: testSalt}

func TestUserQueries(t *testing.T) {
	cases := map[string]struct{ got, want string }{
		"User": {QueryUser, "SELECT toString(id), arrayMap(x -> toString(x), auth_type), host_ip, host_names, host_names_regexp, host_names_like, " +
			"default_roles_all, default_roles_list, default_roles_except, default_database FROM system.users WHERE name = {name:String}"},
		"UserWithValidUntil": {QueryUserWithValidUntil, "SELECT toString(id), arrayMap(x -> toString(x), auth_type), host_ip, host_names, host_names_regexp, host_names_like, " +
			"default_roles_all, default_roles_list, default_roles_except, default_database, arrayMap(x -> toInt64(x), valid_until) FROM system.users WHERE name = {name:String}"},
		"Settings":     {QueryUserSettings, "SELECT ifNull(inherit_profile, ''), ifNull(setting_name, '') FROM system.settings_profile_elements WHERE user_name = {name:String} ORDER BY index"},
		"GrantedRoles": {QueryUserGrantedRoles, "SELECT granted_role_name FROM system.role_grants WHERE user_name = {name:String}"},
		"ShowCreate":   {ShowCreateUser("a`b'c\\d@e.f*"), "SHOW CREATE USER `a\\`b'c\\\\d@e.f*`"},
		"Drop":         {DropUserStmt("svc@example.com"), "DROP USER IF EXISTS `svc@example.com`"},
		"DropTab":      {DropUserStmt("a\tb?$1"), "DROP USER IF EXISTS `a\tb?$1`"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("\ngot:  %s\nwant: %s", tc.got, tc.want)
			}
		})
	}
}

func TestCreateUserStmt(t *testing.T) {
	cases := map[string]struct {
		in   CreateUser
		want string
	}{
		"PasswordAnyHost": {
			in:   CreateUser{Name: "etl", Auth: UserAuth{Password: testPassword}, DefaultRole: DefaultRole{All: true}},
			want: "CREATE USER `etl` IDENTIFIED WITH sha256_hash BY '" + testHash + "' SALT '" + testSalt + "' HOST ANY DEFAULT ROLE ALL",
		},
		"PasswordEveryClause": {
			in: CreateUser{
				Name: "etl", Auth: UserAuth{Password: testPassword},
				Hosts:           Hosts{IPs: []string{"10.0.0.0/8", "192.168.1.1"}, Names: []string{"Host.Example.COM"}},
				DefaultRole:     DefaultRole{All: true, Roles: []string{"r2", "r1"}},
				DefaultDatabase: ptr("team-a_analytics"),
				SettingsProfile: ptr("default"),
			},
			want: "CREATE USER `etl` IDENTIFIED WITH sha256_hash BY '" + testHash + "' SALT '" + testSalt + "'" +
				" HOST IP '10.0.0.0/8', '192.168.1.1', NAME 'Host.Example.COM' DEFAULT ROLE ALL EXCEPT `r1`, `r2`" +
				" DEFAULT DATABASE `team-a_analytics` SETTINGS PROFILE 'default'",
		},
		"NoneValues": {
			in: CreateUser{Name: "etl", Auth: UserAuth{Password: testPassword}, Hosts: Hosts{IPs: []string{AnyHostIP}},
				DefaultDatabase: ptr(""), SettingsProfile: ptr("")},
			want: "CREATE USER `etl` IDENTIFIED WITH sha256_hash BY '" + testHash + "' SALT '" + testSalt + "' HOST ANY DEFAULT ROLE NONE DEFAULT DATABASE NONE SETTINGS NONE",
		},
		"JWTWithClaims": {
			in: CreateUser{Name: "alice@example.com", Auth: UserAuth{JWT: &JWTAuth{Processor: "keycloak", Claims: `{"groups":["ch-analysts"]}`}},
				Hosts: Hosts{Names: []string{"a.example.com"}}, DefaultRole: DefaultRole{All: true}},
			want: "CREATE USER `alice@example.com` IDENTIFIED WITH jwt PROCESSOR 'keycloak' CLAIMS '{\"groups\":[\"ch-analysts\"]}' HOST NAME 'a.example.com' DEFAULT ROLE ALL",
		},
		"JWTWithoutClaims": {
			in:   CreateUser{Name: "system:serviceaccount:team-a:api", Auth: UserAuth{JWT: &JWTAuth{Processor: "kubernetes"}}, Hosts: Hosts{IPs: []string{"10.0.0.0/8"}}},
			want: "CREATE USER `system:serviceaccount:team-a:api` IDENTIFIED WITH jwt PROCESSOR 'kubernetes' HOST IP '10.0.0.0/8' DEFAULT ROLE NONE",
		},
		// Every name and literal is quoted; nothing can escape its quotes.
		"Adversarial": {
			in: CreateUser{
				Name: "a`b'c\\d", Auth: UserAuth{JWT: &JWTAuth{Processor: "p'1", Claims: `{"hd":"it's \\ ok"}`}},
				Hosts:           Hosts{Names: []string{"h'1", "x\\y"}},
				DefaultRole:     DefaultRole{Roles: []string{"r`1"}},
				DefaultDatabase: ptr("d`b"),
				SettingsProfile: ptr("p'x"),
			},
			// The JSON escape of the backslash is escaped again by the
			// literal, as in the server's own SHOW CREATE USER output.
			want: "CREATE USER `a\\`b'c\\\\d` IDENTIFIED WITH jwt PROCESSOR 'p\\'1' CLAIMS '{\"hd\":\"it\\'s \\\\\\\\ ok\"}'" +
				" HOST NAME 'h\\'1', 'x\\\\y' DEFAULT ROLE `r\\`1` DEFAULT DATABASE `d\\`b` SETTINGS PROFILE 'p\\'x'",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := CreateUserStmt(tc.in)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("\ngot:  %s\nwant: %s", got, tc.want)
			}
			if !strings.Contains(got, " IDENTIFIED WITH ") {
				t.Error("CREATE USER must always carry an IDENTIFIED clause (and so have its errors redacted)")
			}
			if strings.Contains(got, "OR REPLACE") {
				t.Error("OR REPLACE is never used")
			}
		})
	}

	for name, auth := range map[string]UserAuth{
		"NoMethod":       {},
		"BothMethods":    {Password: testPassword, JWT: &JWTAuth{Processor: "p"}},
		"ShortHash":      {Password: &PasswordHash{Hash: "abcd", Salt: testSalt}},
		"NonHexHash":     {Password: &PasswordHash{Hash: strings.Repeat("z", 64), Salt: testSalt}},
		"NoSalt":         {Password: &PasswordHash{Hash: testHash}},
		"EmptyProcessor": {JWT: &JWTAuth{}},
	} {
		t.Run("Invalid"+name, func(t *testing.T) {
			if _, err := CreateUserStmt(CreateUser{Name: "u", Auth: auth}); err == nil {
				t.Error("want an error")
			}
		})
	}
	if _, err := CreateUserStmt(CreateUser{Name: "u", Auth: UserAuth{Password: &PasswordHash{Hash: "zz", Salt: "s"}}}); err == nil || strings.Contains(err.Error(), "zz") {
		t.Errorf("errors never quote the hash: %v", err)
	}
}

func TestAlterUserStmt(t *testing.T) {
	cases := map[string]struct {
		in   AlterUser
		want string
	}{
		"Password": {
			in:   AlterUser{Name: "etl", Auth: &UserAuth{Password: testPassword}},
			want: "ALTER USER `etl` IDENTIFIED WITH sha256_hash BY '" + testHash + "' SALT '" + testSalt + "'",
		},
		"JWT": {
			in:   AlterUser{Name: "u", Auth: &UserAuth{JWT: &JWTAuth{Processor: "test", Claims: `{"a":1}`}}},
			want: "ALTER USER `u` IDENTIFIED WITH jwt PROCESSOR 'test' CLAIMS '{\"a\":1}'",
		},
		"HostsAny": {
			in:   AlterUser{Name: "u", Hosts: &Hosts{}},
			want: "ALTER USER `u` HOST ANY",
		},
		"HostsNamesOnly": {
			in:   AlterUser{Name: "u", Hosts: &Hosts{Names: []string{"a.example.com", "B.example.com"}}},
			want: "ALTER USER `u` HOST NAME 'a.example.com', 'B.example.com'",
		},
		"DefaultDatabaseNone": {
			in:   AlterUser{Name: "u", DefaultDatabase: ptr("")},
			want: "ALTER USER `u` DEFAULT DATABASE NONE",
		},
		"SettingsNone": {
			in:   AlterUser{Name: "u", SettingsProfile: ptr("")},
			want: "ALTER USER `u` SETTINGS NONE",
		},
		"Everything": {
			in: AlterUser{Name: "u", Auth: &UserAuth{Password: testPassword}, Hosts: &Hosts{IPs: []string{"10.0.0.0/8"}},
				DefaultDatabase: ptr("db2"), SettingsProfile: ptr("default")},
			want: "ALTER USER `u` IDENTIFIED WITH sha256_hash BY '" + testHash + "' SALT '" + testSalt + "' HOST IP '10.0.0.0/8' DEFAULT DATABASE `db2` SETTINGS PROFILE 'default'",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := AlterUserStmt(tc.in)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("\ngot:  %s\nwant: %s", got, tc.want)
			}
		})
	}
	if _, err := AlterUserStmt(AlterUser{Name: "u"}); err == nil {
		t.Error("nothing to alter must be an error")
	}
	if _, err := AlterUserStmt(AlterUser{Name: "u", Auth: &UserAuth{}}); err == nil {
		t.Error("an empty IDENTIFIED clause must be an error")
	}
}

func TestDefaultRole(t *testing.T) {
	cases := map[string]struct {
		in     DefaultRole
		stmt   string
		status string
	}{
		"All":       {DefaultRole{All: true}, "ALTER USER `u` DEFAULT ROLE ALL", "ALL"},
		"None":      {DefaultRole{}, "ALTER USER `u` DEFAULT ROLE NONE", "NONE"},
		"Listed":    {DefaultRole{Roles: []string{"r2", "r1", "r2"}}, "ALTER USER `u` DEFAULT ROLE `r1`, `r2`", "r1, r2"},
		"AllExcept": {DefaultRole{All: true, Roles: []string{"r`1"}}, "ALTER USER `u` DEFAULT ROLE ALL EXCEPT `r\\`1`", "ALL EXCEPT r`1"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := AlterUserDefaultRoleStmt("u", tc.in); got != tc.stmt {
				t.Errorf("\ngot:  %s\nwant: %s", got, tc.stmt)
			}
			if got := tc.in.String(); got != tc.status {
				t.Errorf("String() = %q, want %q", got, tc.status)
			}
		})
	}
	if !(DefaultRole{Roles: []string{"b", "a"}}).Equal(DefaultRole{Roles: []string{"a", "b", "a"}}) {
		t.Error("role lists compare as sets")
	}
	if (DefaultRole{All: true}).Equal(DefaultRole{}) || (DefaultRole{All: true, Roles: []string{"a"}}).Equal(DefaultRole{All: true}) {
		t.Error("ALL differs from NONE and from ALL EXCEPT")
	}
	if !(DefaultRole{All: true, Roles: []string{}}).Equal(DefaultRole{All: true}) {
		t.Error("an empty list is no list")
	}
}

func TestHashPassword(t *testing.T) {
	if got := HashPassword("Pa55word", testSalt); got != testHash {
		t.Errorf("test vector: HashPassword() = %s", got)
	}
	// Hashing the bytes the salt encodes instead of its text creates a user
	// who can never log in (516). The provider never does that.
	decoded := "2191cd7ebf3122ccffcfedb685384d767ee5dab600de58ee4c83d5cb9a6b6d7d"
	if HashPassword("Pa55word", testSalt) == decoded {
		t.Error("the salt must be appended as hex text, not as decoded bytes")
	}

	h, err := newPasswordHash(strings.NewReader(strings.Repeat("\x00\x11\x22\x33\x44\x55\x66\x77\x88\x99\xaa\xbb\xcc\xdd\xee\xff", 2)), "Pa55word")
	if err != nil {
		t.Fatal(err)
	}
	if h.Salt != testSalt || h.Hash != testHash {
		t.Errorf("newPasswordHash() = %+v", h)
	}
	r1, err := NewPasswordHash("x")
	if err != nil {
		t.Fatal(err)
	}
	r2, _ := NewPasswordHash("x")
	if len(r1.Salt) != 32 || !isHexDigest(r1.Hash) || r1.Hash != HashPassword("x", r1.Salt) {
		t.Errorf("NewPasswordHash() = %+v", r1)
	}
	if r1.Salt == r2.Salt {
		t.Error("every application of a password gets a fresh salt")
	}
	if _, err := newPasswordHash(strings.NewReader("short"), "x"); err == nil {
		t.Error("a failing random source must be an error")
	}
}

func TestGeneratePassword(t *testing.T) {
	for _, n := range []int{MinPasswordLength, DefaultPasswordLength, MaxPasswordLength} {
		p, err := GeneratePassword(n)
		if err != nil {
			t.Fatal(err)
		}
		if len(p) != n || strings.Trim(p, passwordAlphabet) != "" {
			t.Errorf("GeneratePassword(%d) = %q", n, p)
		}
	}
	a, _ := GeneratePassword(32)
	b, _ := GeneratePassword(32)
	if a == b {
		t.Error("passwords must be random")
	}
	for _, n := range []int{0, MinPasswordLength - 1, MaxPasswordLength + 1} {
		if _, err := GeneratePassword(n); err == nil {
			t.Errorf("GeneratePassword(%d) must fail", n)
		}
	}
	// Bytes from 248 up are rejected: 248 = 4*62, so every character stays
	// equally likely.
	src := strings.NewReader(strings.Repeat("\xff\xf8", 8) + "\x00\x01\x3d\x3e" + strings.Repeat("\x05", 12))
	p, err := generatePassword(src, 16)
	if err != nil {
		t.Fatal(err)
	}
	if want := "AB9A" + strings.Repeat("F", 12); p != want {
		t.Errorf("generatePassword() = %q, want %q", p, want)
	}
	if _, err := generatePassword(strings.NewReader("abc"), 16); err == nil {
		t.Error("a failing random source must be an error")
	}
}

func TestNormalizeHostIP(t *testing.T) {
	cases := map[string]string{
		// The server's rewrites (design 5.4).
		"10.1.2.3/8":                  "10.0.0.0/8",
		"10.0.0.0/255.0.0.0":          "10.0.0.0/8",
		"10.1.2.3/32":                 "10.1.2.3",
		"10.1.2.3":                    "10.1.2.3",
		"2001:db8::1/128":             "2001:db8::1",
		"fd00:0::1":                   "fd00::1",
		"::ffff:192.168.0.1":          "::ffff:192.168.0.1",
		"0.0.0.0/0":                   "0.0.0.0/0",
		"10.0.0.1/0.0.0.0":            "0.0.0.0/0",
		"::/0":                        "::/0",
		"2001:db8:1::/ffff::":         "2001::/16",
		"192.168.7.9/24":              "192.168.7.0/24",
		"192.168.7.9/255.255.255.255": "192.168.7.9",
		"2001:DB8::A/64":              "2001:db8::/64",
		"172.16.0.0/255.240.0.0":      "172.16.0.0/12",
	}
	for in, want := range cases {
		got, err := NormalizeHostIP(in)
		if err != nil || got != want {
			t.Errorf("NormalizeHostIP(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{
		"", "example.com", "10.0.0.0/33", "2001:db8::/129", "10.0.0.0/255.0.255.0", "10.0.0.0/x", "10.0.0.0/",
		"10.0.0.0/-1", "10.0.0.0/+8", "fe80::1%eth0", "10.0.0.0/ffff::", "2001:db8::/255.0.0.0", "010.0.0.1",
	} {
		if got, err := NormalizeHostIP(in); err == nil {
			t.Errorf("NormalizeHostIP(%q) = %q, want an error", in, got)
		}
	}
}

func TestNormalizeHosts(t *testing.T) {
	cases := map[string]struct {
		ips, names []string
		want       Hosts
	}{
		"Empty":    {want: Hosts{IPs: []string{AnyHostIP}}},
		"FullIPv4": {ips: []string{"10.0.0.0/8", "0.0.0.0/0"}, want: Hosts{IPs: []string{AnyHostIP}}},
		"FullIPv6": {ips: []string{"::/0"}, want: Hosts{IPs: []string{AnyHostIP}}},
		"SortedAndDeduplicated": {
			ips:   []string{"192.168.1.1/32", "10.1.2.3/8", "10.0.0.0/255.0.0.0", "fd00:0::1"},
			names: []string{"b.example.com", "Host.Example.COM", "b.example.com"},
			want:  Hosts{IPs: []string{"10.0.0.0/8", "192.168.1.1", "fd00::1"}, Names: []string{"Host.Example.COM", "b.example.com"}},
		},
		"NamesOnly": {names: []string{"x.example.com"}, want: Hosts{Names: []string{"x.example.com"}}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := NormalizeHosts(tc.ips, tc.names)
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("-want +got:\n%s", diff)
			}
		})
	}
	for name, tc := range map[string]struct{ ips, names []string }{
		"FullRangeWithNames": {ips: []string{"0.0.0.0/0"}, names: []string{"x.example.com"}},
		"BadIP":              {ips: []string{"10.0.0.0/33"}},
		"EmptyName":          {names: []string{""}},
	} {
		if _, err := NormalizeHosts(tc.ips, tc.names); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestHostsCompare(t *testing.T) {
	desired, err := NormalizeHosts([]string{"10.1.2.3/8", "2001:db8::1/128"}, []string{"Host.Example.COM"})
	if err != nil {
		t.Fatal(err)
	}
	// As the server stores them: rewritten and reordered.
	if got := ObservedHosts([]string{"2001:db8::1", "10.0.0.0/8"}, []string{"Host.Example.COM"}); !desired.Equal(got) {
		t.Errorf("observed %+v must equal desired %+v", got, desired)
	}
	if desired.Equal(ObservedHosts([]string{"2001:db8::1", "10.0.0.0/8"}, []string{"host.example.com"})) {
		t.Error("host names are compared case-sensitively")
	}
	if desired.Equal(ObservedHosts([]string{"10.0.0.0/8"}, []string{"Host.Example.COM"})) {
		t.Error("a missing entry is drift")
	}
	anyHost, _ := NormalizeHosts(nil, nil)
	if !anyHost.Equal(ObservedHosts([]string{"::/0"}, nil)) || !anyHost.Any() {
		t.Error("HOST ANY is stored as ['::/0']")
	}
	if anyHost.Equal(ObservedHosts([]string{"not-an-ip"}, nil)) {
		t.Error("an unparsable observed entry never equals a desired one")
	}
	if got := ObservedHosts([]string{"0.0.0.0/0"}, nil); !got.Any() {
		t.Errorf("ObservedHosts(0.0.0.0/0) = %+v", got)
	}
	if (Hosts{IPs: []string{AnyHostIP}, Names: []string{"x"}}).Any() {
		t.Error("names restrict the hosts")
	}
}

func TestCanonicalClaims(t *testing.T) {
	cases := map[string]string{
		`{"b": 1, "a": "x y"}`: `{"a":"x y","b":1}`,
		`{"groups": ["ch-analysts", "b"], "z": {"y": true, "x": null}}`: `{"groups":["ch-analysts","b"],"z":{"x":null,"y":true}}`,
		`{"n": 1.50, "big": 12345678901234567890}`:                      `{"big":12345678901234567890,"n":1.50}`,
		`{"html": "<a&b>", "u": "üé"}`:                                  `{"html":"<a&b>","u":"üé"}`,
		`{"hd":"it's \\ ok"}`:                                           `{"hd":"it's \\ ok"}`,
		` {"a":1} `:                                                     `{"a":1}`,
		`{}`:                                                            ``,
		`{ }`:                                                           ``,
	}
	for in, want := range cases {
		got, err := CanonicalClaims([]byte(in))
		if err != nil || got != want {
			t.Errorf("CanonicalClaims(%s) = %s, %v; want %s", in, got, err, want)
		}
		if got != "" {
			if again, err := CanonicalClaims([]byte(got)); err != nil || again != got {
				t.Errorf("the canonical form must be stable: %s -> %s, %v", got, again, err)
			}
		}
	}
	for _, in := range []string{`[1]`, `"x"`, `1`, `null`, `{`, `{"a":1} {"b":2}`, `{"a":1} x`, ``} {
		if _, err := CanonicalClaims([]byte(in)); err == nil {
			t.Errorf("CanonicalClaims(%s) must fail", in)
		}
	}
}
