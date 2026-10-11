// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package user

import (
	"strings"
	"sync"
	"testing"
	"time"

	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/crossplane/crossplane-runtime/v2/pkg/event"
	"github.com/crossplane/crossplane-runtime/v2/pkg/meta"
	"github.com/crossplane/crossplane-runtime/v2/pkg/resource"

	"github.com/infrabay/provider-clickhouse/apis/v1alpha1"
	ch "github.com/infrabay/provider-clickhouse/internal/clients/clickhouse"
	"github.com/infrabay/provider-clickhouse/internal/clients/clickhouse/fake"
	"github.com/infrabay/provider-clickhouse/internal/clients/clickhouse/sqlx"
	"github.com/infrabay/provider-clickhouse/internal/controller/access"
	"github.com/infrabay/provider-clickhouse/internal/controller/ownership"
	"github.com/infrabay/provider-clickhouse/internal/controller/providerconfig"
)

const (
	ns       = "team-a"
	mrUID    = "mr-uid"
	userID   = "33333333-3333-3333-3333-333333333333"
	otherID  = "22222222-2222-2222-2222-222222222222"
	roleID   = "11111111-1111-1111-1111-111111111111"
	userName = "team-a_etl"
	provider = "provider" // fake.NewClient's currentUser()
	connName = "etl-conn"
)

// --- test doubles ---

type recorder struct {
	mu     sync.Mutex
	events []event.Event
}

func (r *recorder) Event(_ runtime.Object, e event.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *recorder) WithAnnotations(...string) event.Recorder { return r }

func (r *recorder) reasons() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []string{}
	for _, e := range r.events {
		out = append(out, string(e.Reason))
	}
	return out
}

func (r *recorder) messages() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var b strings.Builder
	for _, e := range r.events {
		b.WriteString(e.Message + "\n")
	}
	return b.String()
}

func kubeWith(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.SchemeBuilder.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return clientfake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
}

// sharedPolicy is a shared ClusterProviderConfig as in design 3.3.
func sharedPolicy() v1alpha1.ClusterPolicy {
	return v1alpha1.ClusterPolicy{
		AllowedNamespaces:     []string{"team-.*"},
		UserNamePatterns:      []string{"{namespace}_[a-z0-9_]+", "provider"},
		RoleNamePatterns:      []string{"{namespace}_[a-z0-9_]+"},
		GrantableRoles:        []string{"{namespace}_[a-z0-9_]+"},
		AllowedAuthentication: []v1alpha1.AuthenticationMethod{v1alpha1.AuthenticationMethodPassword, v1alpha1.AuthenticationMethodJWT},
		AllowedJWTProcessors:  []string{"kubernetes"},
	}
}

var endpoint = providerconfig.Endpoint{Host: "clickhouse.example.com", Port: 9440, Protocol: v1alpha1.ProtocolNative, TLS: true}

func clusterResolved(t *testing.T, cp v1alpha1.ClusterPolicy) *providerconfig.Resolved {
	t.Helper()
	p, err := providerconfig.CompilePolicy(cp)
	if err != nil {
		t.Fatal(err)
	}
	return &providerconfig.Resolved{
		Kind: "ClusterProviderConfig", Name: "shared",
		CacheKey:        ch.Key{Identity: "ClusterProviderConfig//shared", Version: "v"},
		Endpoint:        endpoint,
		Policy:          p,
		AdoptionAllowed: p.AdoptionAllowed(),
	}
}

func namespacedResolved() *providerconfig.Resolved {
	return &providerconfig.Resolved{
		Kind: "ProviderConfig", Name: "own",
		CacheKey:        ch.Key{Identity: "ProviderConfig/team-a/own", Version: "v"},
		Endpoint:        endpoint,
		AdoptionAllowed: true,
	}
}

// --- MR fixtures ---

type uOpt func(*v1alpha1.User)

// newUser returns a User MR for the ClickHouse user team-a_etl with a
// generated password and the connection Secret etl-conn.
func newUser(opts ...uOpt) *v1alpha1.User {
	u := &v1alpha1.User{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "etl", UID: mrUID}}
	u.Spec.ProviderConfigReference = &xpv2.ProviderConfigReference{Kind: "ClusterProviderConfig", Name: "shared"}
	u.Spec.ForProvider.Name = new(userName)
	u.Spec.ForProvider.Authentication.Password = &v1alpha1.PasswordAuthentication{Generate: &v1alpha1.GeneratePassword{}}
	u.Spec.WriteConnectionSecretToReference = &xpv2.LocalSecretReference{Name: connName}
	for _, o := range opts {
		o(u)
	}
	return u
}

func named(n string) uOpt { return func(u *v1alpha1.User) { u.Spec.ForProvider.Name = &n } }

// owned gives the MR a valid claim on the user with UUID id.
func owned(id string) uOpt {
	return func(u *v1alpha1.User) {
		meta.AddAnnotations(u, map[string]string{v1alpha1.AnnotationOwnerUID: mrUID, v1alpha1.AnnotationOwnedID: id})
	}
}

// claimed gives the MR a valid claim without owned-id, as the initializer
// leaves it.
func claimed() uOpt {
	return func(u *v1alpha1.User) { meta.AddAnnotations(u, map[string]string{v1alpha1.AnnotationOwnerUID: mrUID}) }
}

func annotated(k, v string) uOpt {
	return func(u *v1alpha1.User) { meta.AddAnnotations(u, map[string]string{k: v}) }
}

func jwtUser(processor, claims string) uOpt {
	return func(u *v1alpha1.User) {
		j := &v1alpha1.JWTAuthentication{Processor: processor}
		if claims != "" {
			j.Claims = &apiextensionsv1.JSON{Raw: []byte(claims)}
		}
		u.Spec.ForProvider.Authentication = v1alpha1.UserAuthentication{JWT: j}
	}
}

func generate(length int32, token string) uOpt {
	return func(u *v1alpha1.User) {
		g := &v1alpha1.GeneratePassword{}
		if length > 0 {
			g.Length = &length
		}
		if token != "" {
			g.RotationToken = &token
		}
		u.Spec.ForProvider.Authentication = v1alpha1.UserAuthentication{Password: &v1alpha1.PasswordAuthentication{Generate: g}}
	}
}

func secretRef(name, key string) uOpt {
	return func(u *v1alpha1.User) {
		u.Spec.ForProvider.Authentication = v1alpha1.UserAuthentication{Password: &v1alpha1.PasswordAuthentication{
			PasswordSecretRef: &xpv2.LocalSecretKeySelector{LocalSecretReference: xpv2.LocalSecretReference{Name: name}, Key: key},
		}}
	}
}

func hosts(ips, names []string) uOpt {
	return func(u *v1alpha1.User) { u.Spec.ForProvider.HostIP, u.Spec.ForProvider.HostNames = ips, names }
}

func roles(policy string, rs ...string) uOpt {
	return func(u *v1alpha1.User) {
		u.Spec.ForProvider.DefaultRoles = &v1alpha1.DefaultRoles{Policy: policy, Roles: rs}
	}
}

// managed sets defaultDatabase and settingsProfile, so that nothing is
// late-initialized.
func managedFields(db, profile string) uOpt {
	return func(u *v1alpha1.User) {
		u.Spec.ForProvider.DefaultDatabase, u.Spec.ForProvider.SettingsProfile = &db, &profile
		if u.Spec.ForProvider.DefaultRoles == nil {
			u.Spec.ForProvider.DefaultRoles = &v1alpha1.DefaultRoles{Policy: v1alpha1.DefaultRolesAll}
		}
	}
}

func deleting() uOpt {
	return func(u *v1alpha1.User) {
		now := metav1.NewTime(time.Now())
		u.SetDeletionTimestamp(&now)
		u.SetFinalizers([]string{"finalizer.managedresource.crossplane.io"})
	}
}

func policies(p ...xpv2.ManagementAction) uOpt {
	return func(u *v1alpha1.User) { u.SetManagementPolicies(p) }
}

func noConnectionSecret() uOpt {
	return func(u *v1alpha1.User) { u.Spec.WriteConnectionSecretToReference = nil }
}

// connSecret returns the connection Secret of the MR, controlled by uid
// ("" for no controller).
func connSecret(uid types.UID, data map[string]string) *corev1.Secret {
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: connName}, Data: map[string][]byte{}}
	for k, v := range data {
		s.Data[k] = []byte(v)
	}
	if uid != "" {
		ctrl := true
		s.OwnerReferences = []metav1.OwnerReference{{APIVersion: v1alpha1.SchemeGroupVersion.String(), Kind: "User", Name: "etl", UID: uid, Controller: &ctrl}}
	}
	return s
}

// recorded is the connection Secret of a password user with a recorded
// password (and rotation token, when tok is set).
func recorded(password, tok string) *corev1.Secret {
	d := map[string]string{
		v1alpha1.ConnectionKeyUsername: userName,
		v1alpha1.ConnectionKeyPassword: password,
	}
	if tok != "" {
		d[v1alpha1.ConnectionKeyRotationToken] = tok
	}
	return connSecret(mrUID, d)
}

func passwordSecret(name, key, value string) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Data: map[string][]byte{key: []byte(value)}}
}

// --- server fixtures ---

// row is a row of system.users as the details query reads it.
type row struct {
	id                         string
	auth                       []string
	ips, names, regexps, likes []string
	all                        uint8
	list, except               []string
	db                         string
	validUntil                 []int64
}

// passwordRow is an owned sha256 user with HOST ANY and DEFAULT ROLE ALL.
func passwordRow() row {
	return row{id: userID, auth: []string{sqlx.AuthSHA256Password}, ips: []string{"::/0"}, all: 1, validUntil: []int64{0}}
}

func jwtRow() row {
	r := passwordRow()
	r.auth = []string{sqlx.AuthJWT}
	return r
}

func (r row) values(validUntil bool) []any {
	v := []any{r.id, r.auth, r.ips, r.names, r.regexps, r.likes, r.all, r.list, r.except, r.db}
	if validUntil {
		v = append(v, r.validUntil)
	}
	return v
}

// server is a scripted server with one user.
type server struct {
	db      *fake.DB
	profile fake.Profile
}

// newServer scripts a server of profile p on which the user name exists
// (unless r.id is "") with row r and, where SHOW CREATE USER is read, the
// statement show.
func newServer(p fake.Profile, r row, show string) *server {
	db := fake.NewDB().WithEntities().
		On(sqlx.QueryUserSettings, fake.Rows()).
		On(sqlx.QueryUserGrantedRoles, fake.Rows())
	s := &server{db: db, profile: p}
	if r.id != "" {
		db.WithUser(userName, r.id, ch.StorageLocalDirectory)
		res := fake.Row(r.values(p.ValidUntil)...)
		if p.ValidUntil {
			db.On(sqlx.QueryUserWithValidUntil, res)
		} else {
			db.On(sqlx.QueryUser, res)
		}
	}
	if show != "" {
		db.On(sqlx.ShowCreateUser(userName), fake.Row(show))
	}
	return s
}

func (s *server) settings(elems ...[2]string) *server {
	rows := make([][]any, 0, len(elems))
	for _, e := range elems {
		rows = append(rows, []any{e[0], e[1]})
	}
	s.db.On(sqlx.QueryUserSettings, fake.Rows(rows...))
	return s
}

func (s *server) granted(rs ...string) *server {
	rows := make([][]any, 0, len(rs))
	for _, r := range rs {
		rows = append(rows, []any{r})
	}
	s.db.On(sqlx.QueryUserGrantedRoles, fake.Rows(rows...))
	return s
}

// env is an external client over a scripted server and a fake Kubernetes
// API holding objs.
type env struct {
	*External
	db   *fake.DB
	cl   *fake.Client
	rec  *recorder
	kube client.Client
}

func newEnv(t *testing.T, s *server, res *providerconfig.Resolved, objs ...client.Object) *env {
	t.Helper()
	if res == nil {
		res = clusterResolved(t, sharedPolicy())
	}
	cl := fake.NewClientFor(s.db, s.profile)
	kube := kubeWith(t, objs...)
	rec := &recorder{}
	e := &External{Env: &access.Env{Client: cl, Resolved: res, Kube: kube, Reader: kube, APIReader: kube, Recorder: rec, Journal: ownership.NewJournal()}}
	return &env{External: e, db: s.db, cl: cl, rec: rec, kube: kube}
}

func readyStatus(mg resource.Managed) string {
	return string(mg.GetCondition(xpv2.TypeReady).Status)
}

// seen reports whether the query ran.
func (e *env) seen(q string) bool {
	for _, s := range e.db.Seen {
		if s == q {
			return true
		}
	}
	return false
}
