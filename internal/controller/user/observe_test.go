// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package user

import (
	"context"
	"testing"

	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	"github.com/google/go-cmp/cmp"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/crossplane/crossplane-runtime/v2/pkg/reconciler/managed"

	"github.com/infrabay/provider-clickhouse/apis/v1alpha1"
	ch "github.com/infrabay/provider-clickhouse/internal/clients/clickhouse"
	"github.com/infrabay/provider-clickhouse/internal/clients/clickhouse/fake"
	"github.com/infrabay/provider-clickhouse/internal/clients/clickhouse/sqlx"
	"github.com/infrabay/provider-clickhouse/internal/controller/providerconfig"
	"github.com/infrabay/provider-clickhouse/internal/reason"
)

const (
	recordedPassword = "Recorded-Passw0rd-1"
	validUntil2031   = int64(1924992000)
	showPassword     = "CREATE USER `team-a_etl` IDENTIFIED WITH sha256_password"
)

// details returns the connection details of team-a_etl on the test
// endpoint plus kv (key, value pairs; the value "<nil>" is a nil value,
// which removes the key when published).
func details(kv ...string) managed.ConnectionDetails {
	d := managed.ConnectionDetails{
		v1alpha1.ConnectionKeyUsername: []byte(userName),
		v1alpha1.ConnectionKeyEndpoint: []byte("clickhouse.example.com"),
		v1alpha1.ConnectionKeyPort:     []byte("9440"),
		v1alpha1.ConnectionKeyProtocol: []byte("Native"),
		v1alpha1.ConnectionKeyTLS:      []byte("true"),
	}
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] == "<nil>" {
			d[kv[i]] = nil
			continue
		}
		d[kv[i]] = []byte(kv[i+1])
	}
	return d
}

// obs is the part of an observation the table compares.
type obs struct {
	exists, upToDate, lateInit bool
}

func modRow(mod func(*row)) row {
	r := passwordRow()
	mod(&r)
	return r
}

func TestObserve(t *testing.T) {
	transport := &ch.TransportError{Category: "dial failed", Message: "connection refused"}
	cases := map[string]struct {
		mr  *v1alpha1.User
		srv *server
		res *providerconfig.Resolved
		// objs are stored in the Kubernetes API besides the MR.
		objs        []client.Object
		noAPIReader bool

		want    obs
		details managed.ConnectionDetails
		reason  reason.Reason
		anyErr  bool
		ready   string
		events  []string
		noQuery bool
		check   func(t *testing.T, e *env, mr *v1alpha1.User)
	}{
		// --- existence, policy and ownership ---
		"NotFound": {
			mr:  newUser(claimed()),
			srv: newServer(fake.Upstream268, row{}, ""),
		},
		"PolicyViolation": {
			mr:     newUser(owned(userID), named("team-b_etl")),
			srv:    newServer(fake.Upstream268, passwordRow(), ""),
			reason: reason.PolicyViolation, ready: "False", noQuery: true,
		},
		"SelfModification": {
			mr:     newUser(owned(userID), named(provider)),
			srv:    newServer(fake.Upstream268, passwordRow(), ""),
			reason: reason.SelfModification, ready: "False", noQuery: true,
		},
		"NotOwned": {
			mr:     newUser(claimed()),
			srv:    newServer(fake.Upstream268, passwordRow(), ""),
			reason: reason.NotOwned, ready: "False",
			check: func(t *testing.T, _ *env, mr *v1alpha1.User) {
				t.Helper()
				if mr.Status.AtProvider.ID != userID || mr.Status.AtProvider.Storage != ch.StorageLocalDirectory {
					t.Errorf("the observed id must be reported: %+v", mr.Status.AtProvider)
				}
			},
		},
		"ReadOnly": {
			mr: newUser(owned(userID)),
			srv: func() *server {
				s := newServer(fake.Upstream268, passwordRow(), "")
				s.db.WithUser(userName, userID, ch.StorageUsersXML)
				return s
			}(),
			reason: reason.ReadOnlyStorage, ready: "False",
		},
		"DeletingOwned": {
			mr:   newUser(owned(userID), deleting()),
			srv:  newServer(fake.Upstream268, passwordRow(), ""),
			want: obs{exists: true},
			check: func(t *testing.T, e *env, _ *v1alpha1.User) {
				t.Helper()
				if e.seen(sqlx.QueryUserWithValidUntil) {
					t.Error("no details are read while deleting")
				}
			},
		},
		"DeletingNotOwnedIsOrphaned": {
			mr:     newUser(owned(otherID), deleting()),
			srv:    newServer(fake.Upstream268, passwordRow(), ""),
			events: []string{"Orphaned"},
		},
		"DeletingOutsidePolicyIsOrphaned": {
			mr:     newUser(owned(userID), deleting(), named("team-b_etl")),
			srv:    newServer(fake.Upstream268, passwordRow(), ""),
			events: []string{"OrphanedByPolicy"}, noQuery: true,
		},
		// crossplane-runtime releases such an MR without calling Delete; the
		// second guard does not depend on it.
		"DeletingWithoutDeletePolicyIsOrphaned": {
			mr:     newUser(owned(userID), deleting(), policies(xpv2.ManagementActionObserve, xpv2.ManagementActionCreate, xpv2.ManagementActionUpdate)),
			srv:    newServer(fake.Upstream268, passwordRow(), ""),
			events: []string{"Orphaned"}, noQuery: true,
		},

		// --- password users ---
		"PasswordUpToDate": {
			mr:      newUser(owned(userID), generate(0, "t1"), managedFields("", "")),
			srv:     newServer(fake.Upstream268, passwordRow(), ""),
			objs:    []client.Object{recorded(recordedPassword, "t1")},
			want:    obs{exists: true, upToDate: true},
			details: details(v1alpha1.ConnectionKeyPassword, recordedPassword, v1alpha1.ConnectionKeyRotationToken, "t1"),
			ready:   "True",
			check: func(t *testing.T, e *env, mr *v1alpha1.User) {
				t.Helper()
				if e.seen(sqlx.ShowCreateUser(userName)) {
					t.Error("with system.users.valid_until a password user needs no SHOW CREATE USER")
				}
				want := v1alpha1.UserObservation{ID: userID, Storage: ch.StorageLocalDirectory, AuthTypes: []string{"sha256_password"}, HostIP: []string{"::/0"}, DefaultRoles: "ALL"}
				if diff := cmp.Diff(want, mr.Status.AtProvider); diff != "" {
					t.Errorf("atProvider: %s", diff)
				}
			},
		},
		// 25.3 and 25.8 have no valid_until: SHOW CREATE USER is read.
		"PasswordUpToDate253": {
			mr:      newUser(owned(userID), managedFields("", "")),
			srv:     newServer(fake.Upstream253, passwordRow(), showPassword+" HOST ANY"),
			objs:    []client.Object{recorded(recordedPassword, "")},
			want:    obs{exists: true, upToDate: true},
			details: details(v1alpha1.ConnectionKeyPassword, recordedPassword),
			check: func(t *testing.T, e *env, _ *v1alpha1.User) {
				t.Helper()
				if !e.seen(sqlx.ShowCreateUser(userName)) || !e.seen(sqlx.QueryUser) {
					t.Errorf("queries: %v", e.db.Seen)
				}
			},
		},
		"ExpiryFromValidUntil": {
			mr:   newUser(owned(userID), managedFields("", "")),
			srv:  newServer(fake.Upstream268, modRow(func(r *row) { r.validUntil = []int64{validUntil2031} }), ""),
			objs: []client.Object{recorded(recordedPassword, "")},
			want: obs{exists: true},
		},
		"ExpiryFromShowCreate": {
			mr:   newUser(owned(userID), managedFields("", "")),
			srv:  newServer(fake.Upstream253, passwordRow(), showPassword+" VALID UNTIL '2031-01-01 00:00:00'"),
			objs: []client.Object{recorded(recordedPassword, "")},
			want: obs{exists: true},
		},
		"ExpiryOnASecondMethod": {
			mr: newUser(owned(userID), managedFields("", "")),
			srv: newServer(fake.Upstream253, modRow(func(r *row) { r.auth = []string{"sha256_password", "plaintext_password"} }),
				showPassword+", plaintext_password VALID UNTIL '2032-02-02 00:00:00'"),
			objs: []client.Object{recorded(recordedPassword, "")},
			want: obs{exists: true},
		},
		"SecondMethod": {
			mr:   newUser(owned(userID), managedFields("", "")),
			srv:  newServer(fake.Upstream268, modRow(func(r *row) { r.auth = []string{"sha256_password", "plaintext_password"}; r.validUntil = []int64{0, 0} }), ""),
			objs: []client.Object{recorded(recordedPassword, "")},
			want: obs{exists: true},
		},
		"NoPassword": {
			mr:   newUser(owned(userID), managedFields("", "")),
			srv:  newServer(fake.Upstream268, modRow(func(r *row) { r.auth = []string{"no_password"} }), ""),
			objs: []client.Object{recorded(recordedPassword, "")},
			want: obs{exists: true},
		},
		"SecretMissingRegenerates": {
			mr:      newUser(owned(userID), generate(0, "t1"), managedFields("", "")),
			srv:     newServer(fake.Upstream268, passwordRow(), ""),
			want:    obs{exists: true},
			details: details(),
		},
		"SecretWithoutPassword": {
			mr:      newUser(owned(userID), managedFields("", "")),
			srv:     newServer(fake.Upstream268, passwordRow(), ""),
			objs:    []client.Object{connSecret(mrUID, map[string]string{v1alpha1.ConnectionKeyUsername: userName})},
			want:    obs{exists: true},
			details: details(),
		},
		"RotationTokenChanged": {
			mr:   newUser(owned(userID), generate(0, "t2"), managedFields("", "")),
			srv:  newServer(fake.Upstream268, passwordRow(), ""),
			objs: []client.Object{recorded(recordedPassword, "t1")},
			want: obs{exists: true},
			// The recorded password and token are published until Update
			// applies the new ones.
			details: details(v1alpha1.ConnectionKeyPassword, recordedPassword, v1alpha1.ConnectionKeyRotationToken, "t1"),
		},
		"RotationTokenSetFirstTime": {
			mr:      newUser(owned(userID), generate(0, "t1"), managedFields("", "")),
			srv:     newServer(fake.Upstream268, passwordRow(), ""),
			objs:    []client.Object{recorded(recordedPassword, "")},
			want:    obs{exists: true},
			details: details(v1alpha1.ConnectionKeyPassword, recordedPassword),
		},
		"RotationTokenUnsetRemovesKey": {
			mr:      newUser(owned(userID), managedFields("", "")),
			srv:     newServer(fake.Upstream268, passwordRow(), ""),
			objs:    []client.Object{recorded(recordedPassword, "t1")},
			want:    obs{exists: true, upToDate: true},
			details: details(v1alpha1.ConnectionKeyPassword, recordedPassword, v1alpha1.ConnectionKeyRotationToken, "<nil>"),
		},
		"StaleProcessorKeyRemoved": {
			mr:      newUser(owned(userID), managedFields("", "")),
			srv:     newServer(fake.Upstream268, passwordRow(), ""),
			objs:    []client.Object{connSecret(mrUID, map[string]string{v1alpha1.ConnectionKeyPassword: recordedPassword, v1alpha1.ConnectionKeyJWTProcessor: "kubernetes"})},
			want:    obs{exists: true, upToDate: true},
			details: details(v1alpha1.ConnectionKeyPassword, recordedPassword, v1alpha1.ConnectionKeyJWTProcessor, "<nil>"),
		},
		"SecretRefUpToDate": {
			mr:      newUser(owned(userID), secretRef("etl-pw", "pw"), managedFields("", "")),
			srv:     newServer(fake.Upstream268, passwordRow(), ""),
			objs:    []client.Object{recorded(recordedPassword, ""), passwordSecret("etl-pw", "pw", recordedPassword)},
			want:    obs{exists: true, upToDate: true},
			details: details(v1alpha1.ConnectionKeyPassword, recordedPassword),
		},
		"SecretRefChanged": {
			mr:      newUser(owned(userID), secretRef("etl-pw", "pw"), managedFields("", "")),
			srv:     newServer(fake.Upstream268, passwordRow(), ""),
			objs:    []client.Object{recorded(recordedPassword, ""), passwordSecret("etl-pw", "pw", "Another-Passw0rd")},
			want:    obs{exists: true},
			details: details(v1alpha1.ConnectionKeyPassword, recordedPassword),
		},
		"SecretRefMissing": {
			mr:     newUser(owned(userID), secretRef("etl-pw", "pw"), managedFields("", "")),
			srv:    newServer(fake.Upstream268, passwordRow(), ""),
			objs:   []client.Object{recorded(recordedPassword, "")},
			reason: reason.DependencyMissing,
		},
		"SecretRefMissingKey": {
			mr:     newUser(owned(userID), secretRef("etl-pw", "pw"), managedFields("", "")),
			srv:    newServer(fake.Upstream268, passwordRow(), ""),
			objs:   []client.Object{passwordSecret("etl-pw", "other", "x")},
			reason: reason.DependencyMissing,
		},
		"SecretRefEmpty": {
			mr:     newUser(owned(userID), secretRef("etl-pw", "pw"), managedFields("", "")),
			srv:    newServer(fake.Upstream268, passwordRow(), ""),
			objs:   []client.Object{passwordSecret("etl-pw", "pw", "")},
			reason: reason.InvalidSpec,
		},
		"SecretOfAnotherOwner": {
			mr:     newUser(owned(userID), managedFields("", "")),
			srv:    newServer(fake.Upstream268, passwordRow(), ""),
			objs:   []client.Object{connSecret("someone-else", map[string]string{v1alpha1.ConnectionKeyPassword: "x"})},
			reason: reason.SecretConflict, ready: "False",
		},
		"UncontrolledSecret": {
			mr:     newUser(owned(userID), managedFields("", "")),
			srv:    newServer(fake.Upstream268, passwordRow(), ""),
			objs:   []client.Object{connSecret("", map[string]string{v1alpha1.ConnectionKeyPassword: "x"})},
			reason: reason.SecretConflict, ready: "False",
		},
		// Without a record every reconcile would apply a new password.
		"PasswordWithoutConnectionSecretRef": {
			mr:     newUser(owned(userID), noConnectionSecret(), managedFields("", "")),
			srv:    newServer(fake.Upstream268, passwordRow(), ""),
			reason: reason.InvalidSpec,
		},
		"NoUncachedReader": {
			mr:          newUser(owned(userID), managedFields("", "")),
			srv:         newServer(fake.Upstream268, passwordRow(), ""),
			noAPIReader: true,
			reason:      reason.Internal,
		},
		// An observe-only MR never reads the connection Secret: the password
		// is not its to manage.
		"ObserveOnly": {
			mr:          newUser(policies(xpv2.ManagementActionObserve)),
			srv:         newServer(fake.Upstream268, passwordRow(), ""),
			noAPIReader: true,
			want:        obs{exists: true, upToDate: true},
			details:     details(),
			ready:       "True",
		},
		"ObserveOnlyNeverClaims": {
			mr:   newUser(policies(xpv2.ManagementActionObserve, xpv2.ManagementActionLateInitialize), annotated(v1alpha1.AnnotationAdopt, userID)),
			srv:  newServer(fake.Upstream268, passwordRow(), "").settings([2]string{"default", ""}),
			res:  namespacedResolved(),
			want: obs{exists: true, upToDate: true, lateInit: true},
			check: func(t *testing.T, _ *env, mr *v1alpha1.User) {
				t.Helper()
				if mr.GetAnnotations()[v1alpha1.AnnotationOwnerUID] != "" || mr.Spec.ForProvider.SettingsProfile == nil || *mr.Spec.ForProvider.SettingsProfile != "default" {
					t.Errorf("observe-only import: %v, %+v", mr.GetAnnotations(), mr.Spec.ForProvider)
				}
			},
		},

		// --- late initialization ---
		"LateInitialize": {
			mr: newUser(owned(userID)),
			srv: newServer(fake.Upstream268, modRow(func(r *row) { r.db, r.all, r.list = "db1", 0, []string{"team-a_r2", "team-a_r1"} }), "").
				settings([2]string{"default", ""}),
			objs:    []client.Object{recorded(recordedPassword, "")},
			want:    obs{exists: true, upToDate: true, lateInit: true},
			details: details(v1alpha1.ConnectionKeyPassword, recordedPassword),
			check: func(t *testing.T, e *env, mr *v1alpha1.User) {
				t.Helper()
				p := mr.Spec.ForProvider
				want := &v1alpha1.DefaultRoles{Policy: v1alpha1.DefaultRolesListed, Roles: []string{"team-a_r1", "team-a_r2"}}
				if p.DefaultDatabase == nil || *p.DefaultDatabase != "db1" || p.SettingsProfile == nil || *p.SettingsProfile != "default" || !cmp.Equal(want, p.DefaultRoles) {
					t.Errorf("late-initialized: %+v", p)
				}
				// Listed roles that were just late-initialized are compared
				// on the next reconcile.
				if e.seen(sqlx.QueryUserGrantedRoles) {
					t.Error("granted roles are not read for a late-initialized Listed")
				}
				if mr.Status.AtProvider.SettingsProfile != "default" || mr.Status.AtProvider.DefaultRoles != "team-a_r1, team-a_r2" || mr.Status.AtProvider.DefaultDatabase != "db1" {
					t.Errorf("status: %+v", mr.Status.AtProvider)
				}
			},
		},
		"LateInitializeAllExceptAndNoSettings": {
			mr:   newUser(owned(userID)),
			srv:  newServer(fake.Upstream268, modRow(func(r *row) { r.except = []string{"team-a_r1"} }), ""),
			objs: []client.Object{recorded(recordedPassword, "")},
			want: obs{exists: true, upToDate: true, lateInit: true},
			check: func(t *testing.T, _ *env, mr *v1alpha1.User) {
				t.Helper()
				p := mr.Spec.ForProvider
				if p.SettingsProfile == nil || *p.SettingsProfile != "" || p.DefaultRoles.Policy != v1alpha1.DefaultRolesAllExcept || *p.DefaultDatabase != "" {
					t.Errorf("late-initialized: %+v", p)
				}
			},
		},
		// Individual settings cannot be expressed by settingsProfile.
		"LateInitializeSkipsIndividualSettings": {
			mr:   newUser(owned(userID)),
			srv:  newServer(fake.Upstream268, passwordRow(), "").settings([2]string{"", "max_threads"}),
			objs: []client.Object{recorded(recordedPassword, "")},
			want: obs{exists: true, upToDate: true, lateInit: true},
			check: func(t *testing.T, _ *env, mr *v1alpha1.User) {
				t.Helper()
				if mr.Spec.ForProvider.SettingsProfile != nil {
					t.Errorf("settingsProfile = %q", *mr.Spec.ForProvider.SettingsProfile)
				}
			},
		},
		// Late initialization of the profile is best-effort: without SELECT
		// ON system.settings_profile_elements it is skipped.
		"LateInitializeWithoutProfileGrant": {
			mr: newUser(owned(userID)),
			srv: func() *server {
				s := newServer(fake.Upstream268, passwordRow(), "")
				s.db.On(sqlx.QueryUserSettings, fake.Error(fake.Exception(497, "necessary to have the grant SELECT ON system.settings_profile_elements")))
				return s
			}(),
			objs: []client.Object{recorded(recordedPassword, "")},
			want: obs{exists: true, upToDate: true, lateInit: true},
			check: func(t *testing.T, _ *env, mr *v1alpha1.User) {
				t.Helper()
				if mr.Spec.ForProvider.SettingsProfile != nil || mr.Status.AtProvider.SettingsProfile != "" {
					t.Errorf("settingsProfile = %v", mr.Spec.ForProvider.SettingsProfile)
				}
			},
		},
		"ManagedProfileNeedsGrant": {
			mr: newUser(owned(userID), managedFields("", "default")),
			srv: func() *server {
				s := newServer(fake.Upstream268, passwordRow(), "")
				s.db.On(sqlx.QueryUserSettings, fake.Error(fake.Exception(497, "necessary to have the grant SELECT ON system.settings_profile_elements")))
				return s
			}(),
			objs:   []client.Object{recorded(recordedPassword, "")},
			reason: reason.PermissionDenied,
		},
		"NoLateInitWithoutPolicy": {
			mr:   newUser(owned(userID), policies(xpv2.ManagementActionObserve, xpv2.ManagementActionCreate, xpv2.ManagementActionUpdate, xpv2.ManagementActionDelete)),
			srv:  newServer(fake.Upstream268, passwordRow(), ""),
			objs: []client.Object{recorded(recordedPassword, "")},
			want: obs{exists: true, upToDate: true},
			check: func(t *testing.T, e *env, mr *v1alpha1.User) {
				t.Helper()
				if e.seen(sqlx.QueryUserSettings) || mr.Spec.ForProvider.DefaultRoles != nil {
					t.Errorf("nothing is late-initialized: %+v", mr.Spec.ForProvider)
				}
			},
		},

		// --- drift of the other aspects ---
		"ProfileDrift": {
			mr:   newUser(owned(userID), managedFields("", "default")),
			srv:  newServer(fake.Upstream268, passwordRow(), ""),
			objs: []client.Object{recorded(recordedPassword, "")},
			want: obs{exists: true},
		},
		"ProfileWithIndividualSetting": {
			mr:   newUser(owned(userID), managedFields("", "default")),
			srv:  newServer(fake.Upstream268, passwordRow(), "").settings([2]string{"default", ""}, [2]string{"", "max_threads"}),
			objs: []client.Object{recorded(recordedPassword, "")},
			want: obs{exists: true},
		},
		"ProfileUpToDate": {
			mr:   newUser(owned(userID), managedFields("", "default")),
			srv:  newServer(fake.Upstream268, passwordRow(), "").settings([2]string{"default", ""}),
			objs: []client.Object{recorded(recordedPassword, "")},
			want: obs{exists: true, upToDate: true},
		},
		"DefaultDatabaseDrift": {
			mr:   newUser(owned(userID), managedFields("db1", "")),
			srv:  newServer(fake.Upstream268, passwordRow(), ""),
			objs: []client.Object{recorded(recordedPassword, "")},
			want: obs{exists: true},
		},
		"HostNone": {
			mr:   newUser(owned(userID), managedFields("", "")),
			srv:  newServer(fake.Upstream268, modRow(func(r *row) { r.ips = nil }), ""),
			objs: []client.Object{recorded(recordedPassword, "")},
			want: obs{exists: true},
		},
		"HostRegexp": {
			mr:   newUser(owned(userID), managedFields("", "")),
			srv:  newServer(fake.Upstream268, modRow(func(r *row) { r.ips, r.regexps = nil, []string{`.*\.example\.com`} }), ""),
			objs: []client.Object{recorded(recordedPassword, "")},
			want: obs{exists: true},
		},
		"HostLikeBesidesAny": {
			mr:   newUser(owned(userID), managedFields("", "")),
			srv:  newServer(fake.Upstream268, modRow(func(r *row) { r.likes = []string{"%.example.org"} }), ""),
			objs: []client.Object{recorded(recordedPassword, "")},
			want: obs{exists: true},
		},
		"HostsNormalized": {
			mr:   newUser(owned(userID), managedFields("", ""), hosts([]string{"10.1.2.3/8", "2001:db8::1/128", "10.0.0.0/255.0.0.0"}, []string{"Host.Example.COM"})),
			srv:  newServer(fake.Upstream268, modRow(func(r *row) { r.ips, r.names = []string{"2001:db8::1", "10.0.0.0/8"}, []string{"Host.Example.COM"} }), ""),
			objs: []client.Object{recorded(recordedPassword, "")},
			want: obs{exists: true, upToDate: true},
		},
		"HostNameCase": {
			mr:   newUser(owned(userID), managedFields("", ""), hosts(nil, []string{"Host.Example.COM"})),
			srv:  newServer(fake.Upstream268, modRow(func(r *row) { r.ips, r.names = nil, []string{"host.example.com"} }), ""),
			objs: []client.Object{recorded(recordedPassword, "")},
			want: obs{exists: true},
		},
		"InvalidHosts": {
			mr:     newUser(owned(userID), hosts([]string{"0.0.0.0/0"}, []string{"a.example.com"})),
			srv:    newServer(fake.Upstream268, passwordRow(), ""),
			objs:   []client.Object{recorded(recordedPassword, "")},
			reason: reason.InvalidSpec,
		},
		"DefaultRolesAllDrift": {
			mr:   newUser(owned(userID), managedFields("", "")),
			srv:  newServer(fake.Upstream268, modRow(func(r *row) { r.all = 0 }), ""),
			objs: []client.Object{recorded(recordedPassword, "")},
			want: obs{exists: true},
		},
		"DefaultRolesAllExceptUpToDate": {
			mr:   newUser(owned(userID), roles(v1alpha1.DefaultRolesAllExcept, "team-a_r1", "team-a_r2"), managedFields("", "")),
			srv:  newServer(fake.Upstream268, modRow(func(r *row) { r.except = []string{"team-a_r2", "team-a_r1"} }), ""),
			objs: []client.Object{recorded(recordedPassword, "")},
			want: obs{exists: true, upToDate: true},
		},
		// Listed roles that are not granted are left out, with a Warning.
		"DefaultRolesListedGrantedSubset": {
			mr:     newUser(owned(userID), roles(v1alpha1.DefaultRolesListed, "team-a_r1", "team-a_r2"), managedFields("", "")),
			srv:    newServer(fake.Upstream268, modRow(func(r *row) { r.all, r.list = 0, []string{"team-a_r1"} }), "").granted("team-a_r1", "team-a_other"),
			objs:   []client.Object{recorded(recordedPassword, "")},
			want:   obs{exists: true, upToDate: true},
			events: []string{string(ReasonDefaultRoleNotGranted)},
		},
		// 25.3 keeps a revoked role in default_roles_list.
		"DefaultRolesListedStaleEntry": {
			mr:     newUser(owned(userID), roles(v1alpha1.DefaultRolesListed, "team-a_r1", "team-a_r2"), managedFields("", "")),
			srv:    newServer(fake.Upstream253, modRow(func(r *row) { r.all, r.list = 0, []string{"team-a_r1", "team-a_r2"} }), showPassword).granted("team-a_r2"),
			objs:   []client.Object{recorded(recordedPassword, "")},
			want:   obs{exists: true},
			events: []string{string(ReasonDefaultRoleNotGranted)},
		},
		"DefaultRolesOutsidePolicy": {
			mr:     newUser(owned(userID), roles(v1alpha1.DefaultRolesListed, "team-b_r1"), managedFields("", "")),
			srv:    newServer(fake.Upstream268, passwordRow(), ""),
			reason: reason.PolicyViolation, ready: "False",
		},
		"MethodOutsidePolicy": {
			mr:  newUser(owned(userID), managedFields("", "")),
			srv: newServer(fake.Upstream268, passwordRow(), ""),
			res: func() *providerconfig.Resolved {
				p := sharedPolicy()
				p.AllowedAuthentication = []v1alpha1.AuthenticationMethod{v1alpha1.AuthenticationMethodJWT}
				return clusterResolved(t, p)
			}(),
			reason: reason.PolicyViolation, ready: "False",
			check: func(t *testing.T, e *env, _ *v1alpha1.User) {
				t.Helper()
				if e.seen(sqlx.QueryUserWithValidUntil) {
					t.Error("write checks run before the details are read")
				}
			},
		},

		// --- jwt users ---
		"JWTUpToDate": {
			mr:      newUser(owned(userID), jwtUser("kubernetes", `{"groups": ["ch-analysts"], "hd": "example.com"}`), managedFields("", "")),
			srv:     newServer(fake.Antalya266, jwtRow(), "CREATE USER `team-a_etl` IDENTIFIED WITH jwt PROCESSOR 'kubernetes' CLAIMS '{\"hd\": \"example.com\", \"groups\": [\"ch-analysts\"]}'"),
			want:    obs{exists: true, upToDate: true},
			details: details(v1alpha1.ConnectionKeyJWTProcessor, "kubernetes"),
			ready:   "True",
			check: func(t *testing.T, _ *env, mr *v1alpha1.User) {
				t.Helper()
				if mr.Status.AtProvider.JWTProcessor != "kubernetes" {
					t.Errorf("atProvider: %+v", mr.Status.AtProvider)
				}
			},
		},
		"JWTWithoutClaims": {
			mr:   newUser(owned(userID), jwtUser("kubernetes", `{}`), managedFields("", "")),
			srv:  newServer(fake.Antalya263, jwtRow(), "CREATE USER `team-a_etl` IDENTIFIED WITH jwt PROCESSOR 'kubernetes' CLAIMS '{}'"),
			want: obs{exists: true, upToDate: true},
		},
		"JWTProcessorDrift": {
			mr:   newUser(owned(userID), jwtUser("kubernetes", ""), managedFields("", "")),
			srv:  newServer(fake.Antalya266, jwtRow(), "CREATE USER `team-a_etl` IDENTIFIED WITH jwt PROCESSOR 'other'"),
			want: obs{exists: true},
		},
		"JWTNoProcessor": {
			mr:   newUser(owned(userID), jwtUser("kubernetes", ""), managedFields("", "")),
			srv:  newServer(fake.Antalya266, jwtRow(), "CREATE USER `team-a_etl` IDENTIFIED WITH jwt"),
			want: obs{exists: true},
		},
		"JWTClaimsDrift": {
			mr:   newUser(owned(userID), jwtUser("kubernetes", `{"hd":"example.com"}`), managedFields("", "")),
			srv:  newServer(fake.Antalya266, jwtRow(), "CREATE USER `team-a_etl` IDENTIFIED WITH jwt PROCESSOR 'kubernetes' CLAIMS '{\"hd\":\"other.org\"}'"),
			want: obs{exists: true},
		},
		"JWTClaimsAdded": {
			mr:   newUser(owned(userID), jwtUser("kubernetes", ""), managedFields("", "")),
			srv:  newServer(fake.Antalya266, jwtRow(), "CREATE USER `team-a_etl` IDENTIFIED WITH jwt PROCESSOR 'kubernetes' CLAIMS '{\"hd\":\"other.org\"}'"),
			want: obs{exists: true},
		},
		"JWTExpiry": {
			mr: newUser(owned(userID), jwtUser("kubernetes", ""), managedFields("", "")),
			srv: newServer(fake.Antalya266, modRow(func(r *row) { r.auth, r.validUntil = []string{"jwt"}, []int64{validUntil2031} }),
				"CREATE USER `team-a_etl` IDENTIFIED WITH jwt PROCESSOR 'kubernetes' VALID UNTIL '2031-01-01 00:00:00'"),
			want: obs{exists: true},
		},
		"JWTTwoMethods": {
			mr: newUser(owned(userID), jwtUser("kubernetes", ""), managedFields("", "")),
			srv: newServer(fake.Antalya266, modRow(func(r *row) { r.auth, r.validUntil = []string{"jwt", "jwt"}, []int64{0, 0} }),
				"CREATE USER `team-a_etl` IDENTIFIED WITH jwt PROCESSOR 'kubernetes', jwt PROCESSOR 'other'"),
			want: obs{exists: true},
		},
		// Until the switch is applied the password still works: it stays in
		// the Secret.
		"SwitchToJWTKeepsPasswordUntilApplied": {
			mr:      newUser(owned(userID), jwtUser("kubernetes", ""), managedFields("", "")),
			srv:     newServer(fake.Antalya266, passwordRow(), showPassword),
			objs:    []client.Object{recorded(recordedPassword, "t1")},
			want:    obs{exists: true},
			details: details(v1alpha1.ConnectionKeyJWTProcessor, "kubernetes", v1alpha1.ConnectionKeyPassword, recordedPassword, v1alpha1.ConnectionKeyRotationToken, "t1"),
		},
		"JWTRemovesStalePassword": {
			mr:      newUser(owned(userID), jwtUser("kubernetes", ""), managedFields("", "")),
			srv:     newServer(fake.Antalya266, jwtRow(), "CREATE USER `team-a_etl` IDENTIFIED WITH jwt PROCESSOR 'kubernetes'"),
			objs:    []client.Object{recorded(recordedPassword, "t1")},
			want:    obs{exists: true, upToDate: true},
			details: details(v1alpha1.ConnectionKeyJWTProcessor, "kubernetes", v1alpha1.ConnectionKeyPassword, "<nil>", v1alpha1.ConnectionKeyRotationToken, "<nil>"),
		},
		"JWTSecretOfAnotherOwner": {
			mr:     newUser(owned(userID), jwtUser("kubernetes", ""), managedFields("", "")),
			srv:    newServer(fake.Antalya266, jwtRow(), "CREATE USER `team-a_etl` IDENTIFIED WITH jwt PROCESSOR 'kubernetes'"),
			objs:   []client.Object{connSecret("someone-else", nil)},
			reason: reason.SecretConflict, ready: "False",
		},
		"JWTUnsupportedUpstream": {
			mr:     newUser(owned(userID), jwtUser("kubernetes", ""), managedFields("", "")),
			srv:    newServer(fake.Upstream268, passwordRow(), ""),
			reason: reason.Unsupported, ready: "False",
			check: func(t *testing.T, e *env, _ *v1alpha1.User) {
				t.Helper()
				if e.seen(sqlx.QueryUserWithValidUntil) {
					t.Error("no details are read after Unsupported")
				}
			},
		},
		"JWTUnsupportedObserveOnly": {
			mr:   newUser(jwtUser("kubernetes", ""), policies(xpv2.ManagementActionObserve)),
			srv:  newServer(fake.Upstream268, passwordRow(), showPassword),
			want: obs{exists: true},
		},
		"JWTProcessorOutsidePolicy": {
			mr:     newUser(owned(userID), jwtUser("other", ""), managedFields("", "")),
			srv:    newServer(fake.Antalya266, jwtRow(), ""),
			reason: reason.PolicyViolation, ready: "False",
		},
		// Processor names are checked on every config kind.
		"JWTProcessorInvalidName": {
			mr:     newUser(owned(userID), jwtUser("bad.name", ""), managedFields("", "")),
			srv:    newServer(fake.Antalya266, jwtRow(), ""),
			res:    namespacedResolved(),
			reason: reason.InvalidSpec, ready: "False",
		},

		// --- errors are never "not found" ---
		"DetailsPermissionDenied": {
			mr: newUser(owned(userID), managedFields("", "")),
			srv: func() *server {
				s := newServer(fake.Upstream268, passwordRow(), "")
				s.db.On(sqlx.QueryUserWithValidUntil, fake.Error(fake.Exception(497, "necessary to have the grant SHOW USERS")))
				return s
			}(),
			objs:   []client.Object{recorded(recordedPassword, "")},
			reason: reason.PermissionDenied,
		},
		"DetailsTransportError": {
			mr: newUser(owned(userID), managedFields("", "")),
			srv: func() *server {
				s := newServer(fake.Upstream268, passwordRow(), "")
				s.db.On(sqlx.QueryUserWithValidUntil, fake.Error(transport))
				return s
			}(),
			anyErr: true,
		},
		"DroppedBetweenReads": {
			mr: newUser(owned(userID), managedFields("", "")),
			srv: func() *server {
				s := newServer(fake.Upstream268, passwordRow(), "")
				s.db.On(sqlx.QueryUserWithValidUntil, fake.Rows())
				return s
			}(),
			reason: reason.Replaced,
		},
		"ReplacedBetweenReads": {
			mr: newUser(owned(userID), managedFields("", "")),
			srv: func() *server {
				s := newServer(fake.Upstream268, passwordRow(), "")
				s.db.On(sqlx.QueryUserWithValidUntil, fake.Row(modRow(func(r *row) { r.id = otherID }).values(true)...))
				return s
			}(),
			reason: reason.Replaced,
		},
		"ShowCreateUserUnparsable": {
			mr:     newUser(owned(userID), managedFields("", "")),
			srv:    newServer(fake.Upstream253, passwordRow(), "CREATE USER 'unterminated"),
			objs:   []client.Object{recorded(recordedPassword, "")},
			reason: reason.Internal,
		},
		"ShowCreateUserDenied": {
			mr: newUser(owned(userID), managedFields("", "")),
			srv: func() *server {
				s := newServer(fake.Upstream253, passwordRow(), "")
				s.db.On(sqlx.ShowCreateUser(userName), fake.Error(fake.Exception(497, "necessary to have the grant SHOW USERS")))
				return s
			}(),
			reason: reason.PermissionDenied,
		},
		"GrantedRolesDenied": {
			mr: newUser(owned(userID), roles(v1alpha1.DefaultRolesListed, "team-a_r1"), managedFields("", "")),
			srv: func() *server {
				s := newServer(fake.Upstream268, passwordRow(), "")
				s.db.On(sqlx.QueryUserGrantedRoles, fake.Error(fake.Exception(497, "necessary to have the grant SELECT ON system.role_grants")))
				return s
			}(),
			reason: reason.PermissionDenied,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, tc.srv, tc.res, append([]client.Object{tc.mr.DeepCopy()}, tc.objs...)...)
			if tc.noAPIReader {
				e.APIReader = nil
			}
			got, err := e.Observe(context.Background(), tc.mr)
			switch {
			case tc.anyErr:
				if err == nil || got.ResourceExists {
					t.Fatalf("an error must be returned, never 'not found': %+v, %v", got, err)
				}
			case tc.reason != "":
				if !reason.Is(err, tc.reason) {
					t.Fatalf("Observe() error = %v, want %s", err, tc.reason)
				}
			case err != nil:
				t.Fatalf("Observe() error = %v", err)
			default:
				if diff := cmp.Diff(tc.want, obs{got.ResourceExists, got.ResourceUpToDate, got.ResourceLateInitialized}, cmp.AllowUnexported(obs{})); diff != "" {
					t.Errorf("Observe() -want +got:\n%s", diff)
				}
				if tc.details != nil {
					if diff := cmp.Diff(tc.details, got.ConnectionDetails); diff != "" {
						t.Errorf("connection details -want +got:\n%s", diff)
					}
				}
			}
			if tc.ready != "" && readyStatus(tc.mr) != tc.ready {
				t.Errorf("Ready = %q, want %q", readyStatus(tc.mr), tc.ready)
			}
			if diff := cmp.Diff(append([]string{}, tc.events...), e.rec.reasons()); diff != "" {
				t.Errorf("events -want +got:\n%s", diff)
			}
			if tc.noQuery && len(e.db.Seen) > 0 {
				t.Errorf("no query may run, ran %v", e.db.Seen)
			}
			if len(e.db.Execs) > 0 {
				t.Errorf("Observe must never write, ran %v", e.db.Execs)
			}
			if tc.check != nil {
				tc.check(t, e, tc.mr)
			}
		})
	}
}
