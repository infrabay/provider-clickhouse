// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package user

import (
	"context"
	"regexp"
	"strings"
	"testing"

	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	"github.com/google/go-cmp/cmp"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/crossplane/crossplane-runtime/v2/pkg/controller"
	"github.com/crossplane/crossplane-runtime/v2/pkg/gate"
	"github.com/crossplane/crossplane-runtime/v2/pkg/reconciler/managed"

	"github.com/infrabay/provider-clickhouse/apis/v1alpha1"
	ch "github.com/infrabay/provider-clickhouse/internal/clients/clickhouse"
	"github.com/infrabay/provider-clickhouse/internal/clients/clickhouse/fake"
	"github.com/infrabay/provider-clickhouse/internal/clients/clickhouse/sqlx"
	"github.com/infrabay/provider-clickhouse/internal/controller/access"
	"github.com/infrabay/provider-clickhouse/internal/controller/ownership"
	"github.com/infrabay/provider-clickhouse/internal/controller/providerconfig"
	"github.com/infrabay/provider-clickhouse/internal/reason"
)

// passwordClause matches the IDENTIFIED clause of a password user.
var passwordClause = regexp.MustCompile(`IDENTIFIED WITH sha256_hash BY '([0-9a-f]{64})' SALT '([0-9a-f]{32})'`)

// appliedPassword checks that stmt sets the hash of password, salted with a
// fresh 32-character hex salt as text, and returns the rest of stmt with the
// clause replaced by IDENTIFIED.
func appliedPassword(t *testing.T, stmt, password string) string {
	t.Helper()
	m := passwordClause.FindStringSubmatch(stmt)
	if m == nil {
		t.Fatalf("no password clause in %s", stmt)
	}
	if sqlx.HashPassword(password, m[2]) != m[1] {
		t.Errorf("the hash in %s is not hex(sha256(password + salt))", stmt)
	}
	return passwordClause.ReplaceAllString(stmt, "IDENTIFIED")
}

// creates makes CREATE USER statements create the user on the fake server.
func creates(db *fake.DB, err error) func(string) error {
	return func(stmt string) error {
		if err != nil {
			return err
		}
		if strings.HasPrefix(stmt, "CREATE USER ") {
			db.WithUser(userName, userID, ch.StorageLocalDirectory)
		}
		return nil
	}
}

func TestCreate(t *testing.T) {
	cases := map[string]struct {
		mr      *v1alpha1.User
		profile fake.Profile
		res     *providerconfig.Resolved
		objs    []client.Object
		setup   func(db *fake.DB)
		execErr error

		// stmt is the statement, with a password clause as IDENTIFIED;
		// password the password it must set ("" for a jwt user, "*" for
		// a new random one).
		stmt     string
		password string
		details  func(password string) managed.ConnectionDetails
		reason   reason.Reason
		ready    string
		outcome  string
	}{
		"GeneratedPassword": {
			mr:       newUser(claimed()),
			stmt:     "CREATE USER `team-a_etl` IDENTIFIED HOST ANY DEFAULT ROLE ALL",
			password: "*",
			details:  func(pw string) managed.ConnectionDetails { return details(v1alpha1.ConnectionKeyPassword, pw) },
		},
		"GeneratedWithRotationToken": {
			mr:       newUser(claimed(), generate(20, "t1")),
			objs:     []client.Object{connSecret(mrUID, nil)},
			stmt:     "CREATE USER `team-a_etl` IDENTIFIED HOST ANY DEFAULT ROLE ALL",
			password: "*",
			details: func(pw string) managed.ConnectionDetails {
				if len(pw) != 20 {
					t.Errorf("password of %d characters, want 20", len(pw))
				}
				return details(v1alpha1.ConnectionKeyPassword, pw, v1alpha1.ConnectionKeyRotationToken, "t1")
			},
		},
		// Re-created after an out-of-band drop: the recorded password keeps
		// working for clients.
		"ReusesRecordedPassword": {
			mr:       newUser(claimed()),
			objs:     []client.Object{recorded(recordedPassword, "")},
			stmt:     "CREATE USER `team-a_etl` IDENTIFIED HOST ANY DEFAULT ROLE ALL",
			password: recordedPassword,
			details:  func(pw string) managed.ConnectionDetails { return details(v1alpha1.ConnectionKeyPassword, pw) },
		},
		"PendingRotationGeneratesNew": {
			mr:       newUser(claimed(), generate(0, "t2")),
			objs:     []client.Object{recorded(recordedPassword, "t1")},
			stmt:     "CREATE USER `team-a_etl` IDENTIFIED HOST ANY DEFAULT ROLE ALL",
			password: "*",
			details: func(pw string) managed.ConnectionDetails {
				if pw == recordedPassword {
					t.Error("a pending rotation needs a new password")
				}
				return details(v1alpha1.ConnectionKeyPassword, pw, v1alpha1.ConnectionKeyRotationToken, "t2")
			},
		},
		"StaleRotationTokenRemoved": {
			mr:       newUser(claimed()),
			objs:     []client.Object{recorded(recordedPassword, "t1")},
			stmt:     "CREATE USER `team-a_etl` IDENTIFIED HOST ANY DEFAULT ROLE ALL",
			password: recordedPassword,
			details: func(pw string) managed.ConnectionDetails {
				return details(v1alpha1.ConnectionKeyPassword, pw, v1alpha1.ConnectionKeyRotationToken, "<nil>")
			},
		},
		"SecretRef": {
			mr:       newUser(claimed(), secretRef("etl-pw", "pw")),
			objs:     []client.Object{passwordSecret("etl-pw", "pw", "Desired-Passw0rd")},
			stmt:     "CREATE USER `team-a_etl` IDENTIFIED HOST ANY DEFAULT ROLE ALL",
			password: "Desired-Passw0rd",
			details:  func(pw string) managed.ConnectionDetails { return details(v1alpha1.ConnectionKeyPassword, pw) },
		},
		"EveryClause": {
			mr: newUser(claimed(), hosts([]string{"10.1.2.3/8", "192.168.1.1/32"}, []string{"Host.Example.COM"}),
				roles(v1alpha1.DefaultRolesAllExcept, "team-a_r2", "team-a_r1"), managedFields("team-a_db", "default")),
			stmt: "CREATE USER `team-a_etl` IDENTIFIED HOST IP '10.0.0.0/8', '192.168.1.1', NAME 'Host.Example.COM'" +
				" DEFAULT ROLE ALL EXCEPT `team-a_r1`, `team-a_r2` DEFAULT DATABASE `team-a_db` SETTINGS PROFILE 'default'",
			password: "*",
		},
		// Listed roles must be granted before they can be default.
		"ListedCreatedWithNone": {
			mr:       newUser(claimed(), roles(v1alpha1.DefaultRolesListed, "team-a_r1"), managedFields("", "")),
			stmt:     "CREATE USER `team-a_etl` IDENTIFIED HOST ANY DEFAULT ROLE NONE DEFAULT DATABASE NONE SETTINGS NONE",
			password: "*",
		},
		"None": {
			mr:       newUser(claimed(), roles(v1alpha1.DefaultRolesNone)),
			stmt:     "CREATE USER `team-a_etl` IDENTIFIED HOST ANY DEFAULT ROLE NONE",
			password: "*",
		},
		"JWT": {
			mr:      newUser(claimed(), jwtUser("kubernetes", `{"b": 1, "a": ["x"]}`)),
			profile: fake.Antalya266,
			stmt:    `CREATE USER ` + "`team-a_etl`" + ` IDENTIFIED WITH jwt PROCESSOR 'kubernetes' CLAIMS '{"a":["x"],"b":1}' HOST ANY DEFAULT ROLE ALL`,
			details: func(string) managed.ConnectionDetails {
				return details(v1alpha1.ConnectionKeyJWTProcessor, "kubernetes")
			},
		},
		"JWTRemovesPasswordKeys": {
			mr:      newUser(claimed(), jwtUser("kubernetes", "")),
			profile: fake.Antalya263,
			objs:    []client.Object{recorded(recordedPassword, "t1")},
			stmt:    "CREATE USER `team-a_etl` IDENTIFIED WITH jwt PROCESSOR 'kubernetes' HOST ANY DEFAULT ROLE ALL",
			details: func(string) managed.ConnectionDetails {
				return details(v1alpha1.ConnectionKeyJWTProcessor, "kubernetes", v1alpha1.ConnectionKeyPassword, "<nil>", v1alpha1.ConnectionKeyRotationToken, "<nil>")
			},
		},
		"JWTWithoutConnectionSecret": {
			mr:      newUser(claimed(), jwtUser("kubernetes", ""), noConnectionSecret()),
			profile: fake.Antalya266,
			stmt:    "CREATE USER `team-a_etl` IDENTIFIED WITH jwt PROCESSOR 'kubernetes' HOST ANY DEFAULT ROLE ALL",
		},
		"JWTUnsupported": {
			mr:     newUser(claimed(), jwtUser("kubernetes", "")),
			reason: reason.Unsupported, ready: "False",
		},
		"JWTUnsupported253": {
			mr:      newUser(claimed(), jwtUser("kubernetes", "")),
			profile: fake.Upstream253,
			reason:  reason.Unsupported, ready: "False",
		},
		"JWTProcessorOutsidePolicy": {
			mr:      newUser(claimed(), jwtUser("other", "")),
			profile: fake.Antalya266,
			reason:  reason.PolicyViolation, ready: "False",
		},
		"MethodOutsidePolicy": {
			mr: newUser(claimed()),
			res: func() *providerconfig.Resolved {
				p := sharedPolicy()
				p.AllowedAuthentication = []v1alpha1.AuthenticationMethod{v1alpha1.AuthenticationMethodJWT}
				return clusterResolved(t, p)
			}(),
			reason: reason.PolicyViolation, ready: "False",
		},
		"InvalidHosts": {
			mr:     newUser(claimed(), hosts([]string{"not-an-ip"}, nil)),
			reason: reason.InvalidSpec,
		},
		"InvalidClaims": {
			mr:      newUser(claimed(), jwtUser("kubernetes", `[1]`)),
			profile: fake.Antalya266,
			reason:  reason.InvalidSpec,
		},
		"SecretConflict": {
			mr:     newUser(claimed()),
			objs:   []client.Object{connSecret("someone-else", map[string]string{v1alpha1.ConnectionKeyPassword: "x"})},
			reason: reason.SecretConflict, ready: "False",
		},
		"SecretRefMissing": {
			mr:     newUser(claimed(), secretRef("etl-pw", "pw")),
			reason: reason.DependencyMissing,
		},
		"PasswordWithoutConnectionSecretRef": {
			mr:     newUser(claimed(), noConnectionSecret()),
			reason: reason.InvalidSpec,
		},
		"NameCollision": {
			mr:     newUser(claimed()),
			setup:  func(db *fake.DB) { db.WithRole(userName, roleID, ch.StorageLocalDirectory) },
			reason: reason.NameCollision, ready: "False",
		},
		"PolicyViolation": {
			mr:     newUser(claimed(), named("team-b_etl")),
			reason: reason.PolicyViolation, ready: "False",
		},
		"SelfModification": {
			mr:     newUser(claimed(), named(provider)),
			res:    namespacedResolved(),
			reason: reason.SelfModification, ready: "False",
		},
		"MissingClaim": {
			mr:     newUser(),
			reason: reason.Internal,
		},
		// crossplane-runtime never calls Create without the Create
		// management policy; if it did, nothing is created, even with a
		// claim on the MR's own UID (D-26).
		"WithoutCreatePolicy": {
			mr:     newUser(claimed(), policies(xpv2.ManagementActionObserve, xpv2.ManagementActionUpdate, xpv2.ManagementActionDelete)),
			reason: reason.Internal,
		},
		// 36 is a configuration error the server found, e.g. claims it
		// refuses.
		"BadArguments": {
			mr:       newUser(claimed()),
			execErr:  fake.Exception(ch.CodeBadArguments, "Bad JWT claims"),
			stmt:     "CREATE USER `team-a_etl` IDENTIFIED HOST ANY DEFAULT ROLE ALL",
			password: "*",
			reason:   reason.InvalidSpec, outcome: v1alpha1.CreateOutcomeRejected,
		},
		"UnknownRole": {
			mr:       newUser(claimed(), roles(v1alpha1.DefaultRolesAllExcept, "team-a_nosuch")),
			execErr:  fake.Exception(ch.CodeUnknownRole, "There is no role `team-a_nosuch` in user directories"),
			stmt:     "CREATE USER `team-a_etl` IDENTIFIED HOST ANY DEFAULT ROLE ALL EXCEPT `team-a_nosuch`",
			password: "*",
			reason:   reason.DependencyMissing, outcome: v1alpha1.CreateOutcomeRejected,
		},
		"Uncertain": {
			mr:       newUser(claimed()),
			execErr:  &ch.TransportError{Category: "connection closed"},
			stmt:     "CREATE USER `team-a_etl` IDENTIFIED HOST ANY DEFAULT ROLE ALL",
			password: "*",
			outcome:  v1alpha1.CreateOutcomeUnknown,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			profile := tc.profile
			if profile.Version == "" {
				profile = fake.Upstream268
			}
			s := newServer(profile, row{}, "")
			if tc.setup != nil {
				tc.setup(s.db)
			}
			s.db.ExecFn = creates(s.db, tc.execErr)
			e := newEnv(t, s, tc.res, append([]client.Object{tc.mr.DeepCopy()}, tc.objs...)...)
			got, err := e.Create(context.Background(), tc.mr)
			switch {
			case tc.reason != "":
				if !reason.Is(err, tc.reason) {
					t.Fatalf("Create() error = %v, want %s", err, tc.reason)
				}
			case tc.execErr != nil:
				if err == nil {
					t.Fatal("want an error")
				}
			case err != nil:
				t.Fatalf("Create() error = %v", err)
			}
			if tc.ready != "" && readyStatus(tc.mr) != tc.ready {
				t.Errorf("Ready = %q, want %q", readyStatus(tc.mr), tc.ready)
			}
			if tc.stmt == "" {
				if len(e.db.Execs) > 0 {
					t.Fatalf("nothing may be sent, sent %v", e.db.Execs)
				}
				return
			}
			if len(e.db.Execs) != 1 {
				t.Fatalf("statements: %v", e.db.Execs)
			}
			stmt := e.db.Execs[0]
			pw := string(got.ConnectionDetails[v1alpha1.ConnectionKeyPassword])
			switch tc.password {
			case "":
			case "*":
				if err == nil && len(pw) < sqlx.MinPasswordLength {
					t.Errorf("generated password %q", pw)
				}
				if err == nil {
					stmt = appliedPassword(t, stmt, pw)
				} else {
					stmt = passwordClause.ReplaceAllString(stmt, "IDENTIFIED")
				}
			default:
				stmt = appliedPassword(t, stmt, tc.password)
			}
			if stmt != tc.stmt {
				t.Errorf("\ngot:  %s\nwant: %s", stmt, tc.stmt)
			}
			if c := ownership.ClaimOf(tc.mr); c.CreateOutcome != tc.outcome {
				t.Errorf("create-outcome = %q, want %q", c.CreateOutcome, tc.outcome)
			}
			if err != nil {
				return
			}
			if c := ownership.ClaimOf(tc.mr); c.OwnedID != userID {
				t.Errorf("owned-id = %q", c.OwnedID)
			}
			want := details(v1alpha1.ConnectionKeyPassword, pw)
			if j := tc.mr.Spec.ForProvider.Authentication.JWT; j != nil {
				want = details(v1alpha1.ConnectionKeyJWTProcessor, j.Processor)
			}
			if tc.details != nil {
				want = tc.details(pw)
			}
			if diff := cmp.Diff(want, got.ConnectionDetails); diff != "" {
				t.Errorf("connection details -want +got:\n%s", diff)
			}
		})
	}
}

func TestUpdate(t *testing.T) {
	// drifted is an owned user whose every aspect drifted on 26.8: an
	// expiry, HOST NONE, another default database and profile, and DEFAULT
	// ROLE ALL where None is desired.
	drifted := func() *server {
		return newServer(fake.Upstream268, modRow(func(r *row) { r.validUntil, r.ips, r.db = []int64{validUntil2031}, nil, "other" }), "")
	}
	cases := map[string]struct {
		mr   *v1alpha1.User
		srv  *server
		objs []client.Object
		// between runs after Observe and before Update.
		between func(s *server)
		execFn  func(stmt string) error
		// stmts are the statements, password clauses as IDENTIFIED;
		// password is the password the first must set ("*": new).
		stmts    []string
		password string
		details  func(password string) managed.ConnectionDetails
		events   []string
		reason   reason.Reason
	}{
		"RepairsEveryAspect": {
			mr:   newUser(owned(userID), roles(v1alpha1.DefaultRolesNone), managedFields("team-a_db", "default")),
			srv:  drifted(),
			objs: []client.Object{recorded(recordedPassword, "")},
			stmts: []string{
				"ALTER USER `team-a_etl` IDENTIFIED HOST ANY DEFAULT DATABASE `team-a_db` SETTINGS PROFILE 'default'",
				"ALTER USER `team-a_etl` DEFAULT ROLE NONE",
			},
			password: recordedPassword,
			details:  func(pw string) managed.ConnectionDetails { return details(v1alpha1.ConnectionKeyPassword, pw) },
		},
		// A new password must be published even if DEFAULT ROLE fails: the
		// default role change waits for the next reconcile.
		"RotationDefersDefaultRole": {
			mr:       newUser(owned(userID), generate(0, "t2"), roles(v1alpha1.DefaultRolesNone), managedFields("", "")),
			srv:      newServer(fake.Upstream268, passwordRow(), ""),
			objs:     []client.Object{recorded(recordedPassword, "t1")},
			stmts:    []string{"ALTER USER `team-a_etl` IDENTIFIED"},
			password: "*",
			details: func(pw string) managed.ConnectionDetails {
				if pw == recordedPassword {
					t.Error("rotation must change the password")
				}
				return details(v1alpha1.ConnectionKeyPassword, pw, v1alpha1.ConnectionKeyRotationToken, "t2")
			},
			events: []string{string(ReasonPasswordRotated)},
		},
		"Regenerated": {
			mr:       newUser(owned(userID), managedFields("", "")),
			srv:      newServer(fake.Upstream268, passwordRow(), ""),
			stmts:    []string{"ALTER USER `team-a_etl` IDENTIFIED"},
			password: "*",
			details:  func(pw string) managed.ConnectionDetails { return details(v1alpha1.ConnectionKeyPassword, pw) },
			events:   []string{string(ReasonPasswordRegenerated)},
		},
		"SecretRefRotated": {
			mr:       newUser(owned(userID), secretRef("etl-pw", "pw"), managedFields("", "")),
			srv:      newServer(fake.Upstream268, passwordRow(), ""),
			objs:     []client.Object{recorded(recordedPassword, ""), passwordSecret("etl-pw", "pw", "Desired-Passw0rd")},
			stmts:    []string{"ALTER USER `team-a_etl` IDENTIFIED"},
			password: "Desired-Passw0rd",
			details:  func(pw string) managed.ConnectionDetails { return details(v1alpha1.ConnectionKeyPassword, pw) },
			events:   []string{string(ReasonPasswordRotated)},
		},
		"SecretRefUnrecorded": {
			mr:       newUser(owned(userID), secretRef("etl-pw", "pw"), managedFields("", "")),
			srv:      newServer(fake.Upstream268, passwordRow(), ""),
			objs:     []client.Object{passwordSecret("etl-pw", "pw", "Desired-Passw0rd")},
			stmts:    []string{"ALTER USER `team-a_etl` IDENTIFIED"},
			password: "Desired-Passw0rd",
			details:  func(pw string) managed.ConnectionDetails { return details(v1alpha1.ConnectionKeyPassword, pw) },
		},
		// An expiry repaired on 25.3, where only SHOW CREATE USER shows it:
		// the recorded password is applied again.
		"ExpiryRepaired253": {
			mr:       newUser(owned(userID), managedFields("", "")),
			srv:      newServer(fake.Upstream253, passwordRow(), showPassword+" VALID UNTIL '2020-01-01 00:00:00'"),
			objs:     []client.Object{recorded(recordedPassword, "")},
			stmts:    []string{"ALTER USER `team-a_etl` IDENTIFIED"},
			password: recordedPassword,
			details:  func(pw string) managed.ConnectionDetails { return details(v1alpha1.ConnectionKeyPassword, pw) },
		},
		"SwitchToJWT": {
			mr:    newUser(owned(userID), jwtUser("kubernetes", `{"hd":"example.com"}`), managedFields("", "")),
			srv:   newServer(fake.Antalya266, passwordRow(), showPassword),
			objs:  []client.Object{recorded(recordedPassword, "t1")},
			stmts: []string{`ALTER USER ` + "`team-a_etl`" + ` IDENTIFIED WITH jwt PROCESSOR 'kubernetes' CLAIMS '{"hd":"example.com"}'`},
			details: func(string) managed.ConnectionDetails {
				return details(v1alpha1.ConnectionKeyJWTProcessor, "kubernetes", v1alpha1.ConnectionKeyPassword, "<nil>", v1alpha1.ConnectionKeyRotationToken, "<nil>")
			},
		},
		"SwitchToPassword": {
			mr:       newUser(owned(userID), generate(0, "t1"), managedFields("", "")),
			srv:      newServer(fake.Antalya266, jwtRow(), "CREATE USER `team-a_etl` IDENTIFIED WITH jwt PROCESSOR 'kubernetes'"),
			objs:     []client.Object{connSecret(mrUID, map[string]string{v1alpha1.ConnectionKeyJWTProcessor: "kubernetes"})},
			stmts:    []string{"ALTER USER `team-a_etl` IDENTIFIED"},
			password: "*",
			details: func(pw string) managed.ConnectionDetails {
				return details(v1alpha1.ConnectionKeyPassword, pw, v1alpha1.ConnectionKeyRotationToken, "t1", v1alpha1.ConnectionKeyJWTProcessor, "<nil>")
			},
			events: []string{string(ReasonPasswordRegenerated)},
		},
		"OnlyListedDefaultRoles": {
			mr:    newUser(owned(userID), roles(v1alpha1.DefaultRolesListed, "team-a_r1", "team-a_r2"), managedFields("", "")),
			srv:   newServer(fake.Upstream268, modRow(func(r *row) { r.all = 0 }), "").granted("team-a_r2", "team-a_r1"),
			objs:  []client.Object{recorded(recordedPassword, "")},
			stmts: []string{"ALTER USER `team-a_etl` DEFAULT ROLE `team-a_r1`, `team-a_r2`"},
			details: func(string) managed.ConnectionDetails {
				return details(v1alpha1.ConnectionKeyPassword, recordedPassword)
			},
		},
		"ReplacedSinceObserve": {
			mr:      newUser(owned(userID), managedFields("db1", "")),
			srv:     newServer(fake.Upstream268, passwordRow(), ""),
			objs:    []client.Object{recorded(recordedPassword, "")},
			between: func(s *server) { s.db.WithUser(userName, otherID, ch.StorageLocalDirectory) },
			reason:  reason.Replaced,
		},
		"GoneSinceObserve": {
			mr:      newUser(owned(userID), managedFields("db1", "")),
			srv:     newServer(fake.Upstream268, passwordRow(), ""),
			objs:    []client.Object{recorded(recordedPassword, "")},
			between: func(s *server) { s.db.RemoveUser(userName) },
		},
		"ProfileMissing": {
			mr:   newUser(owned(userID), managedFields("", "nosuch")),
			srv:  newServer(fake.Upstream268, passwordRow(), ""),
			objs: []client.Object{recorded(recordedPassword, "")},
			execFn: func(string) error {
				return fake.Exception(ch.CodeThereIsNoProfile, "There is no settings profile `nosuch`")
			},
			stmts:   []string{"ALTER USER `team-a_etl` SETTINGS PROFILE 'nosuch'"},
			reason:  reason.DependencyMissing,
			details: nil,
		},
		"DefaultRoleRace": {
			mr:   newUser(owned(userID), roles(v1alpha1.DefaultRolesListed, "team-a_r1"), managedFields("db1", "")),
			srv:  newServer(fake.Upstream268, modRow(func(r *row) { r.all = 0 }), "").granted("team-a_r1"),
			objs: []client.Object{recorded(recordedPassword, "")},
			execFn: func(stmt string) error {
				if strings.Contains(stmt, "DEFAULT ROLE") {
					return fake.Exception(ch.CodeSetNonGrantedRole, "Role should be granted to set default")
				}
				return nil
			},
			stmts:  []string{"ALTER USER `team-a_etl` DEFAULT DATABASE `db1`", "ALTER USER `team-a_etl` DEFAULT ROLE `team-a_r1`"},
			reason: reason.DependencyMissing,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, tc.srv, nil, append([]client.Object{tc.mr.DeepCopy()}, tc.objs...)...)
			ctx := context.Background()
			o, err := e.Observe(ctx, tc.mr)
			if err != nil || !o.ResourceExists || o.ResourceUpToDate {
				t.Fatalf("Observe() = %+v, %v; want a drifted user", o, err)
			}
			e.rec.events = nil
			if tc.between != nil {
				tc.between(tc.srv)
			}
			e.db.ExecFn = tc.execFn
			got, err := e.Update(ctx, tc.mr)
			if tc.reason != "" {
				if !reason.Is(err, tc.reason) {
					t.Fatalf("Update() error = %v, want %s", err, tc.reason)
				}
			} else if err != nil {
				t.Fatalf("Update() error = %v", err)
			}
			if len(e.db.Execs) != len(tc.stmts) {
				t.Fatalf("statements %v, want %d", e.db.Execs, len(tc.stmts))
			}
			pw := string(got.ConnectionDetails[v1alpha1.ConnectionKeyPassword])
			for i, stmt := range e.db.Execs {
				if i == 0 && tc.password != "" {
					want := tc.password
					if want == "*" {
						want = pw
					}
					stmt = appliedPassword(t, stmt, want)
				}
				if stmt != tc.stmts[i] {
					t.Errorf("statement %d\ngot:  %s\nwant: %s", i, stmt, tc.stmts[i])
				}
			}
			if tc.details != nil {
				if diff := cmp.Diff(tc.details(pw), got.ConnectionDetails); diff != "" {
					t.Errorf("connection details -want +got:\n%s", diff)
				}
			}
			if diff := cmp.Diff(append([]string{}, tc.events...), e.rec.reasons()); diff != "" {
				t.Errorf("events -want +got:\n%s", diff)
			}
			// No secret reaches an event or an error.
			for _, secret := range []string{pw, recordedPassword, "Desired-Passw0rd"} {
				if secret == "" {
					continue
				}
				if strings.Contains(e.rec.messages(), secret) || (err != nil && strings.Contains(err.Error(), secret)) {
					t.Errorf("a password leaked into an event or error")
				}
			}
		})
	}

	t.Run("WithoutObserve", func(t *testing.T) {
		e := newEnv(t, newServer(fake.Upstream268, passwordRow(), ""), nil)
		if _, err := e.Update(context.Background(), newUser(owned(userID))); !reason.Is(err, reason.Internal) || len(e.db.Execs) > 0 {
			t.Fatalf("Update() = %v, %v", err, e.db.Execs)
		}
	})
	t.Run("WithoutUpdatePolicy", func(t *testing.T) {
		// A drifted jwt user whose policies allow Create but not Update:
		// Observe plans the ALTER, and Update must refuse it even with a
		// claim on the MR's own UID, whatever crossplane-runtime calls
		// (D-26).
		s := newServer(fake.Antalya266, jwtRow(), "CREATE USER `team-a_etl` IDENTIFIED WITH jwt PROCESSOR 'other'")
		mr := newUser(owned(userID), jwtUser("kubernetes", ""), managedFields("", ""),
			policies(xpv2.ManagementActionObserve, xpv2.ManagementActionCreate, xpv2.ManagementActionDelete))
		e := newEnv(t, s, nil, mr.DeepCopy())
		o, err := e.Observe(context.Background(), mr)
		if err != nil || o.ResourceUpToDate {
			t.Fatalf("Observe() = %+v, %v; want a drifted user", o, err)
		}
		if _, err := e.Update(context.Background(), mr); !reason.Is(err, reason.Internal) || len(e.db.Execs) > 0 {
			t.Fatalf("Update() = %v, %v; want Internal and no statement", err, e.db.Execs)
		}
	})
	t.Run("PasswordWithoutSecret", func(t *testing.T) {
		// Observe without Create or Update policies does not read the
		// password; Update must not invent one.
		e := newEnv(t, newServer(fake.Upstream268, modRow(func(r *row) { r.auth = []string{"no_password"} }), ""), nil)
		mr := newUser(owned(userID), policies(xpv2.ManagementActionObserve, xpv2.ManagementActionDelete))
		if _, err := e.Observe(context.Background(), mr); err != nil {
			t.Fatal(err)
		}
		if _, err := e.Update(context.Background(), mr); !reason.Is(err, reason.Internal) || len(e.db.Execs) > 0 {
			t.Fatalf("Update() = %v, %v", err, e.db.Execs)
		}
	})
}

func TestDelete(t *testing.T) {
	drop := "DROP USER IF EXISTS `team-a_etl`"
	cases := map[string]struct {
		mr      *v1alpha1.User
		setup   func(db *fake.DB)
		res     *providerconfig.Resolved
		execErr error
		stmts   []string
		reason  reason.Reason
	}{
		"Owned":    {mr: newUser(owned(userID), deleting()), stmts: []string{drop}},
		"Gone":     {mr: newUser(owned(userID), deleting()), setup: func(db *fake.DB) { db.RemoveUser(userName) }},
		"Replaced": {mr: newUser(owned(otherID), deleting()), reason: reason.Replaced},
		"AlreadyDropped": {
			mr: newUser(owned(userID), deleting()), execErr: fake.Exception(ch.CodeUnknownUser, "There is no user"), stmts: []string{drop},
		},
		"PermissionDenied": {
			mr: newUser(owned(userID), deleting()), execErr: fake.Exception(ch.CodeAccessDenied, "necessary to have the grant DROP USER"),
			stmts: []string{drop}, reason: reason.PermissionDenied,
		},
		"NoClaim":       {mr: newUser(claimed(), deleting()), reason: reason.NotOwned},
		"CopiedClaim":   {mr: newUser(annotated(v1alpha1.AnnotationOwnerUID, "other-uid"), annotated(v1alpha1.AnnotationOwnedID, userID), deleting()), reason: reason.NotOwned},
		"OutsidePolicy": {mr: newUser(owned(userID), deleting(), named("team-b_etl")), reason: reason.PolicyViolation},
		"ReadOnly": {
			mr: newUser(owned(userID), deleting()), setup: func(db *fake.DB) { db.WithUser(userName, userID, ch.StorageUsersXML) },
			reason: reason.ReadOnlyStorage,
		},
		"Self": {mr: newUser(owned(userID), deleting(), named(provider)), res: namespacedResolved(), reason: reason.SelfModification},
		// crossplane-runtime never calls Delete then; nothing is dropped if
		// it did.
		"WithoutDeletePolicy": {mr: newUser(owned(userID), deleting(), policies(xpv2.ManagementActionObserve, xpv2.ManagementActionCreate))},
		"MultipleServers": {
			mr: newUser(owned(userID), deleting()), reason: reason.AccessStorageNotReplicated,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s := newServer(fake.Upstream268, passwordRow(), "")
			if tc.setup != nil {
				tc.setup(s.db)
			}
			s.db.ExecFn = func(string) error { return tc.execErr }
			e := newEnv(t, s, tc.res)
			if name == "MultipleServers" {
				e.cl.Servers = []string{"s1", "s2"}
			}
			_, err := e.Delete(context.Background(), tc.mr)
			if tc.reason != "" {
				if !reason.Is(err, tc.reason) {
					t.Fatalf("Delete() error = %v, want %s", err, tc.reason)
				}
			} else if err != nil {
				t.Fatalf("Delete() error = %v", err)
			}
			if diff := cmp.Diff(tc.stmts, e.db.Execs); diff != "" {
				t.Errorf("statements -want +got:\n%s", diff)
			}
			if c := tc.mr.GetCondition(xpv2.TypeReady); c.Reason != xpv2.ReasonDeleting {
				t.Errorf("Ready reason = %s", c.Reason)
			}
		})
	}
}

// TestUpdateRecheckUnavailable: what the re-check before an ALTER finds
// makes the user unavailable, as it would in Observe (design 5.10): an
// endpoint that reaches two servers with local user directories, and a user
// replaced since Observe. Nothing is sent.
func TestUpdateRecheckUnavailable(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		between func(e *env)
		reason  reason.Reason
	}{
		"MultipleServers": {between: func(e *env) { e.cl.Servers = []string{"s1", "s2"} }, reason: reason.AccessStorageNotReplicated},
		"Replaced":        {between: func(e *env) { e.db.WithUser(userName, otherID, ch.StorageLocalDirectory) }, reason: reason.Replaced},
	} {
		t.Run(name, func(t *testing.T) {
			mr := newUser(owned(userID), managedFields("team-a_db", ""))
			s := newServer(fake.Upstream268, modRow(func(r *row) { r.db = "other" }), "")
			e := newEnv(t, s, nil, recorded(recordedPassword, ""))
			if o, err := e.Observe(ctx, mr); err != nil || o.ResourceUpToDate || readyStatus(mr) != "True" {
				t.Fatalf("Observe() = %+v, %v, Ready %s; want a drifted, available user", o, err, readyStatus(mr))
			}
			tc.between(e)
			if _, err := e.Update(ctx, mr); !reason.Is(err, tc.reason) {
				t.Fatalf("Update() = %v, want %s", err, tc.reason)
			}
			if readyStatus(mr) != "False" || len(e.db.Execs) > 0 {
				t.Errorf("Ready %s, statements %v; want False and none", readyStatus(mr), e.db.Execs)
			}
		})
	}
}

// TestCreateObserveUpdate runs a user through its life on a scripted server
// that applies the statements: the connection details of Create are what
// the next Observe finds up to date.
func TestCreateObserveUpdate(t *testing.T) {
	ctx := context.Background()
	s := newServer(fake.Upstream268, row{}, "")
	s.db.ExecFn = creates(s.db, nil)
	mr := newUser(claimed(), generate(0, "t1"), managedFields("", ""))
	e := newEnv(t, s, nil, mr.DeepCopy())
	c, err := e.Create(ctx, mr)
	if err != nil {
		t.Fatal(err)
	}
	// Publish the details as crossplane-runtime does: a Secret controlled
	// by the MR.
	sec := connSecret(mrUID, nil)
	for k, v := range c.ConnectionDetails {
		sec.Data[k] = v
	}
	if err := e.kube.Create(ctx, sec); err != nil {
		t.Fatal(err)
	}
	s.db.On(sqlx.QueryUserWithValidUntil, fake.Row(passwordRow().values(true)...))
	o, err := e.Observe(ctx, mr)
	if err != nil || !o.ResourceExists || !o.ResourceUpToDate {
		t.Fatalf("Observe() after Create = %+v, %v", o, err)
	}
	if diff := cmp.Diff(c.ConnectionDetails, o.ConnectionDetails); diff != "" {
		t.Errorf("Observe must publish what Create published (-create +observe):\n%s", diff)
	}
}

func TestSetupGated(t *testing.T) {
	o := controller.Options{Gate: new(gate.Gate[schema.GroupVersionKind])}
	if err := SetupGated(nil, o, access.Dependencies{}); err != nil {
		t.Fatal(err)
	}
	if got := forProviderName(newUser()); got == nil || *got != userName {
		t.Errorf("forProviderName() = %v", got)
	}
	if got := forProviderName(&v1alpha1.Role{}); got != nil {
		t.Errorf("forProviderName(Role) = %v", got)
	}
	if _, ok := NewExternal(&access.Env{}).(*External); !ok {
		t.Error("NewExternal must return the User client")
	}
}
