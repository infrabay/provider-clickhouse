// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package user

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/crossplane/crossplane-runtime/v2/pkg/event"

	"github.com/infrabay/provider-clickhouse/apis/v1alpha1"
	ch "github.com/infrabay/provider-clickhouse/internal/clients/clickhouse"
	"github.com/infrabay/provider-clickhouse/internal/clients/clickhouse/sqlx"
	"github.com/infrabay/provider-clickhouse/internal/controller/access"
	"github.com/infrabay/provider-clickhouse/internal/controller/ownership"
	"github.com/infrabay/provider-clickhouse/internal/reason"
)

// observed is what Observe read about a user.
type observed struct {
	authTypes       []string
	hostIP          []string
	hostNames       []string
	hostRegexps     []string
	hostLikes       []string
	defaultRole     sqlx.DefaultRole
	defaultDatabase string
	// validUntil is the expiry of every method in Unix seconds (0 when it
	// does not expire), or nil where system.users has no valid_until.
	validUntil []int64
	// def is the parsed SHOW CREATE USER output, nil when it was not read.
	def *sqlx.UserDefinition
	// settings are the settings profile elements, when settingsRead.
	settings     []settingsElement
	settingsRead bool
	// granted are the roles granted to the user, when grantedRead.
	granted     []string
	grantedRead bool
}

// A settingsElement is a row of system.settings_profile_elements: an
// inherited profile or an individual setting.
type settingsElement struct {
	profile string
	setting string
}

// expires reports whether any authentication method has VALID UNTIL, from
// system.users.valid_until where it exists and from SHOW CREATE USER where
// it was read.
func (o *observed) expires() bool {
	if slices.ContainsFunc(o.validUntil, func(t int64) bool { return t != 0 }) {
		return true
	}
	return o.def != nil && o.def.Expires()
}

// hostNone reports whether the user may connect from no host at all (HOST
// NONE): the server stores it as four empty lists. It never equals ANY,
// which is stored as host_ip = ['::/0'].
func (o *observed) hostNone() bool {
	return len(o.hostIP) == 0 && len(o.hostNames) == 0 && len(o.hostRegexps) == 0 && len(o.hostLikes) == 0
}

// processor returns the PROCESSOR of the user's first jwt method, if SHOW
// CREATE USER was read.
func (o *observed) processor() string {
	if o.def == nil {
		return ""
	}
	for _, m := range o.def.Methods {
		if m.Name == sqlx.AuthJWT {
			return m.Processor
		}
	}
	return ""
}

// profiles returns the inherited settings profiles, comma-separated.
func (o *observed) profiles() string {
	var p []string
	for _, s := range o.settings {
		if s.profile != "" {
			p = append(p, s.profile)
		}
	}
	return strings.Join(p, ", ")
}

// singleProfile returns the settings profile the elements amount to, when
// settingsProfile can express them: no elements ("": SETTINGS NONE), or
// exactly one inherited profile and no individual settings.
func singleProfile(elems []settingsElement) (string, bool) {
	switch {
	case len(elems) == 0:
		return "", true
	case len(elems) == 1 && elems[0].profile != "" && elems[0].setting == "":
		return elems[0].profile, true
	}
	return "", false
}

// status returns status.atProvider.
func (o *observed) status(ent access.Entity) v1alpha1.UserObservation {
	s := v1alpha1.UserObservation{
		ID:              ent.ID,
		Storage:         ent.Storage,
		AuthTypes:       o.authTypes,
		JWTProcessor:    o.processor(),
		HostIP:          o.hostIP,
		HostNames:       o.hostNames,
		DefaultDatabase: o.defaultDatabase,
		DefaultRoles:    o.defaultRole.String(),
	}
	if o.settingsRead {
		s.SettingsProfile = o.profiles()
	}
	return s
}

// needsShowCreate reports whether SHOW CREATE USER must be read: the
// processor and claims of a jwt user are visible only there, and so is
// VALID UNTIL on servers without system.users.valid_until.
func needsShowCreate(d desired, authTypes []string, validUntilColumn bool) bool {
	return d.jwt != nil || slices.Contains(authTypes, sqlx.AuthJWT) || !validUntilColumn
}

// read reads the details of the user named name, whose UUID ObserveEntity
// observed as id (design 5.4, steps 1 and 3 to 5). Every error is
// returned; none is turned into "not found".
func (e *External) read(ctx context.Context, cr *v1alpha1.User, name, id string, d desired) (*observed, error) {
	validUntil, err := e.Client.Capabilities().UsersValidUntil(ctx, e.Client)
	if err != nil {
		return nil, err
	}
	o := &observed{}
	var (
		gotID string
		all   uint8
		list  []string
		exc   []string
	)
	query := sqlx.QueryUser
	dest := []any{&gotID, &o.authTypes, &o.hostIP, &o.hostNames, &o.hostRegexps, &o.hostLikes, &all, &list, &exc, &o.defaultDatabase}
	if validUntil {
		query = sqlx.QueryUserWithValidUntil
		dest = append(dest, &o.validUntil)
	}
	err = ch.QueryRow(ctx, e.Client, query, ch.Params{"name": name}, dest...)
	if errors.Is(err, ch.ErrNoRows) || (err == nil && gotID != id) {
		return nil, reason.Errorf(reason.Replaced, "user %q was dropped or replaced while it was observed (id %s); it is observed again on the next reconcile", name, id)
	}
	if err != nil {
		return nil, ch.WithDefaultReason(err)
	}
	o.defaultRole = sqlx.DefaultRole{All: all == 1, Roles: list}
	if o.defaultRole.All {
		o.defaultRole.Roles = exc
	}

	if needsShowCreate(d, o.authTypes, validUntil) {
		var stmt string
		if err := ch.QueryRow(ctx, e.Client, sqlx.ShowCreateUser(name), nil, &stmt); err != nil {
			return nil, ch.WithDefaultReason(err)
		}
		def, err := sqlx.ParseShowCreateUser(stmt)
		if err != nil {
			return nil, reason.Wrap(reason.Internal, err)
		}
		o.def = &def
	}

	// The profile elements need SELECT ON system.settings_profile_elements.
	// A managed settingsProfile requires it; late initialization is
	// best-effort and is skipped without it, so that users which do not set
	// settingsProfile need no such grant. Once the server refused the read,
	// late initialization skips it for a while (ch.Capabilities.DenyTable),
	// so that every poll of every user does not send a query that the
	// server denies.
	caps := e.Client.Capabilities()
	lateInit := cr.Spec.ForProvider.SettingsProfile == nil && ownership.PoliciesOf(cr).LateInitialize() &&
		!caps.TableDenied(ch.TableSettingsProfileElements)
	if d.settingsProfile != nil || lateInit {
		err := e.Client.Query(ctx, sqlx.QueryUserSettings, ch.Params{"name": name}, func(r ch.Row) error {
			var s settingsElement
			if err := r.Scan(&s.profile, &s.setting); err != nil {
				return err
			}
			o.settings = append(o.settings, s)
			return nil
		})
		switch {
		case err == nil:
			o.settingsRead = true
		case d.settingsProfile == nil && ch.IsCode(err, ch.CodeAccessDenied):
			caps.DenyTable(ch.TableSettingsProfileElements)
			o.settings = nil
		default:
			return nil, ch.WithDefaultReason(err)
		}
	}

	if d.listed() {
		err := e.Client.Query(ctx, sqlx.QueryUserGrantedRoles, ch.Params{"name": name}, func(r ch.Row) error {
			var role string
			if err := r.Scan(&role); err != nil {
				return err
			}
			o.granted = append(o.granted, role)
			return nil
		})
		if err != nil {
			return nil, ch.WithDefaultReason(err)
		}
		o.grantedRead = true
	}
	return o, nil
}

// lateInitialize fills settingsProfile, defaultRoles and defaultDatabase
// from the server when they are nil and the management policies include
// LateInitialize. Hosts are never late-initialized: telling "unset" from
// ANY would need tri-state lists.
func lateInitialize(cr *v1alpha1.User, o *observed) bool {
	if !ownership.PoliciesOf(cr).LateInitialize() {
		return false
	}
	p := &cr.Spec.ForProvider
	changed := false
	if p.DefaultDatabase == nil {
		p.DefaultDatabase = new(o.defaultDatabase)
		changed = true
	}
	if p.DefaultRoles == nil {
		p.DefaultRoles = defaultRolesOf(o.defaultRole)
		changed = true
	}
	if p.SettingsProfile == nil && o.settingsRead {
		if prof, ok := singleProfile(o.settings); ok {
			p.SettingsProfile = &prof
			changed = true
		}
	}
	return changed
}

// plan is what Update changes: the aspects that drifted, in at most two
// statements (design 5.4). Observe computes it and Update applies it:
// crossplane-runtime calls both on the same external client within one
// reconcile. It is kept on the client, not in status, which a late
// initialization update between the two resets.
type plan struct {
	name string
	// auth is set when the IDENTIFIED clause is (re-)applied: for a jwt user
	// jwt, for a password user the password in pw.
	auth bool
	jwt  *sqlx.JWTAuth
	pw   *passwordState
	// hosts, defaultDatabase and settingsProfile are the other changes of
	// the first statement.
	hosts           *sqlx.Hosts
	defaultDatabase *string
	settingsProfile *string
	// defaultRole is the second statement.
	defaultRole *sqlx.DefaultRole
	// conn are the connection details of the user as it is.
	conn connection
}

// upToDate reports whether nothing has drifted.
func (p *plan) upToDate() bool {
	return !p.auth && p.hosts == nil && p.defaultDatabase == nil && p.settingsProfile == nil && p.defaultRole == nil
}

// claimsEqual compares desired canonical claims with the observed CLAIMS
// literal, which the server keeps as it was sent.
func claimsEqual(want, got string) bool {
	if got == "" {
		return want == ""
	}
	c, err := sqlx.CanonicalClaims([]byte(got))
	return err == nil && c == want
}

// jwtUpToDate reports whether a jwt user has exactly one method, jwt, with
// the desired processor and claims, and no expiry.
func jwtUpToDate(want *sqlx.JWTAuth, o *observed) bool {
	if !slices.Equal(o.authTypes, []string{sqlx.AuthJWT}) || o.def == nil || len(o.def.Methods) != 1 || o.expires() {
		return false
	}
	m := o.def.Methods[0]
	return m.Name == sqlx.AuthJWT && m.Processor == want.Processor && claimsEqual(want.Claims, m.Claims)
}

// planOf compares the desired state with the observed one (design 5.4, step
// 7). mutates is set when the management policies allow Create or Update:
// only then is the password aspect (and with it the connection Secret)
// considered.
func (e *External) planOf(ctx context.Context, cr *v1alpha1.User, name string, d desired, o *observed, mutates bool) (*plan, error) {
	p := &plan{name: name, conn: connection{name: name}}

	if d.jwt != nil {
		p.conn.processor = d.jwt.Processor
		if !jwtUpToDate(d.jwt, o) {
			p.auth, p.jwt = true, d.jwt
		}
		if mutates {
			// Read for SecretConflict and to remove the keys of a password
			// user that was switched to jwt.
			sec, err := e.connectionSecret(ctx, cr)
			if err != nil {
				return nil, access.Unavailable(cr, err)
			}
			if sec != nil {
				p.conn.secret = sec.Data
			}
		}
	} else {
		methodOK := slices.Equal(o.authTypes, []string{sqlx.AuthSHA256Password}) && !o.expires()
		p.auth = !methodOK
		if mutates {
			ps, err := e.passwordStateOf(ctx, cr)
			if err != nil {
				if reason.Is(err, reason.SecretConflict) {
					return nil, access.Unavailable(cr, err)
				}
				return nil, err
			}
			p.pw = &ps
			p.auth = p.auth || ps.changed()
			p.conn.password, p.conn.token, p.conn.secret = ps.recorded, ps.recordedToken, ps.secret
			if ps.token == "" {
				// generate.rotationToken is unset: the key is not published.
				p.conn.token = nil
			}
		}
	}

	// Until the IDENTIFIED clause is applied, the credentials of the method
	// the user has now still work: Observe keeps them in the Secret.
	p.conn.keepStale = p.auth

	// HOST NONE is checked first: its empty lists would compare as ANY. A
	// REGEXP or LIKE entry is always drift; the provider never writes them.
	if o.hostNone() || len(o.hostRegexps) > 0 || len(o.hostLikes) > 0 || !d.hosts.Equal(sqlx.ObservedHosts(o.hostIP, o.hostNames)) {
		p.hosts = &d.hosts
	}
	if d.defaultDatabase != nil && *d.defaultDatabase != o.defaultDatabase {
		p.defaultDatabase = d.defaultDatabase
	}
	if d.settingsProfile != nil && o.settingsRead {
		if prof, ok := singleProfile(o.settings); !ok || prof != *d.settingsProfile {
			p.settingsProfile = d.settingsProfile
		}
	}
	// A Listed defaultRoles that was late-initialized in this reconcile has
	// no granted roles read yet; it is compared on the next one.
	if r := d.defaultRoles; r != nil && (r.Policy != v1alpha1.DefaultRolesListed || o.grantedRead) {
		want, notGranted := wantDefaultRole(*r, o.granted)
		if len(notGranted) > 0 {
			e.record(cr, event.Warning(ReasonDefaultRoleNotGranted, fmt.Errorf(
				"default roles %s are not granted to user %q and are left out of its default roles; they become default once granted, for example with a RoleGrant",
				strings.Join(notGranted, ", "), name)))
		}
		if !want.Equal(o.defaultRole) {
			p.defaultRole = &want
		}
	}
	return p, nil
}
