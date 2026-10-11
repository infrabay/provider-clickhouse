// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package sqlx

import (
	"bufio"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func sha256Only(name string) UserDefinition {
	return UserDefinition{Name: name, Methods: []AuthMethod{{Name: AuthSHA256Password}}}
}

func TestParseShowCreateUser(t *testing.T) {
	cases := map[string]struct {
		in   string
		want UserDefinition
	}{
		"Password": {
			in:   "CREATE USER pw1 IDENTIFIED WITH sha256_password",
			want: sha256Only("pw1"),
		},
		"EveryClause": {
			in:   "CREATE USER vu1 IDENTIFIED WITH sha256_password VALID UNTIL '2031-01-01 00:00:00', plaintext_password VALID UNTIL '2032-02-02 00:00:00' HOST IP '10.0.0.0/8' DEFAULT DATABASE db1 DEFAULT ROLE NONE SETTINGS PROFILE `default`",
			want: UserDefinition{Name: "vu1", Methods: []AuthMethod{{Name: "sha256_password", ValidUntil: "2031-01-01 00:00:00"}, {Name: "plaintext_password", ValidUntil: "2032-02-02 00:00:00"}}},
		},
		"ValidUntilOnSecondMethodOnly": {
			in:   "CREATE USER u6 IDENTIFIED WITH sha256_password, plaintext_password VALID UNTIL '2032-02-02 00:00:00' HOST IP '10.0.0.0/8', NAME 'x.example.com'",
			want: UserDefinition{Name: "u6", Methods: []AuthMethod{{Name: "sha256_password"}, {Name: "plaintext_password", ValidUntil: "2032-02-02 00:00:00"}}},
		},
		"JWTEscapes": {
			in:   `CREATE USER jwt1 IDENTIFIED WITH jwt PROCESSOR 'p\'1' CLAIMS '{"hd":"it\'s \\\\ ok"}'`,
			want: UserDefinition{Name: "jwt1", Methods: []AuthMethod{{Name: "jwt", Processor: "p'1", Claims: `{"hd":"it's \\ ok"}`}}},
		},
		"JWTWithoutProcessor": {
			in:   "CREATE USER j2 IDENTIFIED WITH jwt",
			want: UserDefinition{Name: "j2", Methods: []AuthMethod{{Name: "jwt"}}},
		},
		"JWTTwoMethods": {
			in: `CREATE USER j1 IDENTIFIED WITH jwt PROCESSOR 'test' CLAIMS '{"b": 1, "a": "x y"}' VALID UNTIL '2031-01-01 00:00:00', jwt PROCESSOR 'other' HOST NAME 'a.example.com'`,
			want: UserDefinition{Name: "j1", Methods: []AuthMethod{
				{Name: "jwt", Processor: "test", Claims: `{"b": 1, "a": "x y"}`, ValidUntil: "2031-01-01 00:00:00"},
				{Name: "jwt", Processor: "other"},
			}},
		},
		"QuotedName": {
			in:   "CREATE USER `we!rd.n@me\\`q'\\\\z` IDENTIFIED WITH sha256_password HOST NAME 'h\\'1'",
			want: sha256Only("we!rd.n@me`q'\\z"),
		},
		"SettingsWithIndividualSetting": {
			in:   "CREATE USER u2 IDENTIFIED WITH sha256_password HOST IP '192.168.1.1', '10.0.0.0/8', NAME 'Host.Example.COM' DEFAULT DATABASE db1 DEFAULT ROLE ALL EXCEPT r1, r2 SETTINGS PROFILE `default`, max_threads = 2",
			want: sha256Only("u2"),
		},
		"HostNone":   {in: "CREATE USER h3 IDENTIFIED WITH sha256_password HOST NONE", want: sha256Only("h3")},
		"HostRegexp": {in: "CREATE USER h6 IDENTIFIED WITH sha256_password HOST REGEXP '.*\\\\.example\\\\.com'", want: sha256Only("h6")},
		"NoPassword": {
			in:   "CREATE USER nopw IDENTIFIED WITH no_password",
			want: UserDefinition{Name: "nopw", Methods: []AuthMethod{{Name: "no_password"}}},
		},
		// Older servers print a passwordless user this way.
		"NotIdentified": {
			in:   "CREATE USER nopw NOT IDENTIFIED HOST LOCAL",
			want: UserDefinition{Name: "nopw", Methods: []AuthMethod{{Name: AuthNoPassword}}},
		},
		"NoIdentifiedClause": {
			in:   "CREATE USER u HOST ANY",
			want: UserDefinition{Name: "u"},
		},
		"NothingAfterTheName": {
			in:   "CREATE USER u",
			want: UserDefinition{Name: "u"},
		},
		// With display_secrets_in_show_and_select the hash and salt are
		// printed; they are skipped, never kept.
		"DisplayedSecrets": {
			in:   "CREATE USER s IDENTIFIED WITH sha256_hash BY '" + testHash + "' SALT '" + testSalt + "' VALID UNTIL '2031-01-01 00:00:00' HOST ANY",
			want: UserDefinition{Name: "s", Methods: []AuthMethod{{Name: "sha256_hash", ValidUntil: "2031-01-01 00:00:00"}}},
		},
		// Methods the provider does not manage are reported by name; their
		// parameters, including lists of literals, are skipped.
		"UnknownMethods": {
			in: "CREATE USER x IDENTIFIED WITH ssl_certificate CN 'a.example.com', 'b.example.com', ldap SERVER 'srv' VALID UNTIL '2031-01-01 00:00:00', kerberos REALM 'R' DEFAULT ROLE ALL",
			want: UserDefinition{Name: "x", Methods: []AuthMethod{
				{Name: "ssl_certificate"}, {Name: "ldap", ValidUntil: "2031-01-01 00:00:00"}, {Name: "kerberos"},
			}},
		},
		// Literals that look like keywords end nothing.
		"KeywordsInLiterals": {
			in:   "CREATE USER `HOST` IDENTIFIED WITH jwt PROCESSOR 'HOST' CLAIMS '{\"DEFAULT\":\"SETTINGS\"}' HOST NAME 'IN'",
			want: UserDefinition{Name: "HOST", Methods: []AuthMethod{{Name: "jwt", Processor: "HOST", Claims: `{"DEFAULT":"SETTINGS"}`}}},
		},
		"IdentifiedByWithoutWith": {
			in:   "CREATE USER u IDENTIFIED BY 'x' HOST ANY",
			want: UserDefinition{Name: "u", Methods: []AuthMethod{{}}},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := ParseShowCreateUser(tc.in)
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("-want +got:\n%s", diff)
			}
		})
	}
}

func TestParseShowCreateUserErrors(t *testing.T) {
	for _, in := range []string{
		"",
		"SHOW USERS",
		"CREATE ROLE r",
		"CREATE USER",
		"CREATE USER 'u' IDENTIFIED WITH sha256_password",
		"CREATE USER u IDENTIFIED WITH sha256_hash BY '" + testHash + "' SALT '" + testSalt,
	} {
		_, err := ParseShowCreateUser(in)
		if err == nil {
			t.Errorf("ParseShowCreateUser(%q) must fail", in)
			continue
		}
		if strings.Contains(err.Error(), testHash) || strings.Contains(err.Error(), testSalt) {
			t.Errorf("errors never quote the input: %v", err)
		}
	}
}

func TestUserDefinitionHelpers(t *testing.T) {
	d := UserDefinition{Methods: []AuthMethod{{Name: "sha256_password"}, {Name: "plaintext_password", ValidUntil: "2032-02-02 00:00:00"}}}
	if !d.Expires() || (UserDefinition{Methods: []AuthMethod{{Name: "jwt"}}}).Expires() {
		t.Error("Expires()")
	}
	if diff := cmp.Diff([]string{"sha256_password", "plaintext_password"}, d.MethodNames()); diff != "" {
		t.Error(diff)
	}
}

// userFixtures are the expected parses of the SHOW CREATE USER fixtures in
// testdata/user, keyed by case. The fixtures were read raw through the
// driver from every server version of the CI matrix by the integration test
// TestUser (XPCH_UPDATE_FIXTURES=1 rewrites them); their statements are
// created by userFixtureStatements there. Every case has the same parse on
// every version.
var userFixtures = map[string]UserDefinition{
	"password":      sha256Only("fx_password"),
	"hosts":         sha256Only("fx_hosts"),
	"host_none":     sha256Only("fx_host_none"),
	"host_local":    sha256Only("fx_host_local"),
	"host_regexp":   sha256Only("fx_host_regexp"),
	"host_like":     sha256Only("fx_host_like"),
	"default_role":  sha256Only("fx_default_role"),
	"settings":      sha256Only("fx_settings"),
	"quoted_name":   sha256Only("fx_we!rd.n@me`q'\\z"),
	"no_password":   {Name: "fx_no_password", Methods: []AuthMethod{{Name: AuthNoPassword}}},
	"valid_until":   {Name: "fx_valid_until", Methods: []AuthMethod{{Name: AuthSHA256Password, ValidUntil: "2031-01-01 00:00:00"}}},
	"every_clause":  {Name: "fx_every_clause", Methods: []AuthMethod{{Name: AuthSHA256Password, ValidUntil: "2031-01-01 00:00:00"}}},
	"added_method":  {Name: "fx_added_method", Methods: []AuthMethod{{Name: AuthSHA256Password}, {Name: "plaintext_password", ValidUntil: "2032-02-02 00:00:00"}}},
	"two_expiring":  {Name: "fx_two_expiring", Methods: []AuthMethod{{Name: AuthSHA256Password, ValidUntil: "2031-01-01 00:00:00"}, {Name: "plaintext_password", ValidUntil: "2031-01-01 00:00:00"}}},
	"jwt":           {Name: "fx_jwt", Methods: []AuthMethod{{Name: AuthJWT, Processor: "p'1", Claims: `{"hd":"it's \\ ok"}`}}},
	"jwt_bare":      {Name: "fx_jwt_bare", Methods: []AuthMethod{{Name: AuthJWT}}},
	"jwt_clauses":   {Name: "fx_jwt_clauses", Methods: []AuthMethod{{Name: AuthJWT, Processor: "test", Claims: `{"groups":["a","b"]}`}}},
	"jwt_two":       {Name: "fx_jwt_two", Methods: []AuthMethod{{Name: AuthJWT, Processor: "test", Claims: `{"b": 1, "a": "x y"}`, ValidUntil: "2031-01-01 00:00:00"}, {Name: AuthJWT, Processor: "other"}}},
	"jwt_and_other": {Name: "fx_jwt_and_other", Methods: []AuthMethod{{Name: AuthJWT, Processor: "test"}, {Name: "plaintext_password"}}},
}

// readUserFixtures reads testdata/user/<version>.txt: one "<case>\t<SHOW
// CREATE USER output>" line per case.
func readUserFixtures(t *testing.T) map[string]map[string]string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("testdata", "user", "*.txt"))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]map[string]string{}
	for _, f := range files {
		fh, err := os.Open(f) //nolint:gosec // Test fixture path.
		if err != nil {
			t.Fatal(err)
		}
		cases := map[string]string{}
		sc := bufio.NewScanner(fh)
		for sc.Scan() {
			name, stmt, ok := strings.Cut(sc.Text(), "\t")
			if !ok {
				t.Fatalf("%s: malformed line %q", f, sc.Text())
			}
			cases[name] = stmt
		}
		_ = fh.Close()
		if err := sc.Err(); err != nil {
			t.Fatal(err)
		}
		out[strings.TrimSuffix(filepath.Base(f), ".txt")] = cases
	}
	return out
}

// TestParseShowCreateUserFixtures parses SHOW CREATE USER output of every
// server version, read through the driver.
func TestParseShowCreateUserFixtures(t *testing.T) {
	versions := readUserFixtures(t)
	if len(versions) == 0 {
		t.Fatal("no fixtures in testdata/user")
	}
	for version, cases := range versions {
		t.Run(version, func(t *testing.T) {
			jwt := strings.Contains(version, "altinityantalya")
			for name := range userFixtures {
				_, ok := cases[name]
				if !ok && (jwt || !strings.HasPrefix(name, "jwt")) {
					t.Errorf("case %q is missing", name)
				}
			}
			for name, stmt := range cases {
				want, ok := userFixtures[name]
				if !ok {
					t.Errorf("case %q has no expectation", name)
					continue
				}
				got, err := ParseShowCreateUser(stmt)
				if err != nil {
					t.Errorf("%s: %v", name, err)
					continue
				}
				if diff := cmp.Diff(want, got); diff != "" {
					t.Errorf("%s: %q\n-want +got:\n%s", name, stmt, diff)
				}
				if slices.Contains(got.MethodNames(), "sha256_hash") {
					t.Errorf("%s: the server reports sha256_hash users as sha256_password", name)
				}
			}
		})
	}
}
