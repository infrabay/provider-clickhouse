// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	chgo "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	pkgv1beta1 "github.com/crossplane/crossplane/apis/v2/pkg/v1beta1"
	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/crossplane/crossplane-runtime/v2/pkg/resource"

	"github.com/infrabay/provider-clickhouse/apis/v1alpha1"
	ch "github.com/infrabay/provider-clickhouse/internal/clients/clickhouse"
	"github.com/infrabay/provider-clickhouse/internal/reason"
	"github.com/infrabay/provider-clickhouse/test/fakegcp"
)

// Names of the GoogleIDToken scenario, shared with hack/e2e.
const (
	envGoogleAudience = "E2E_GOOGLE_AUDIENCE"
	nsGoogle          = "xpch-e2e-google"
	nsFakeGCP         = "xpch-e2e-fakegcp"
	fakeGCPHost       = "fakegcp." + nsFakeGCP + ".svc.cluster.local"
	fakeGCPControl    = 8081
	googleCPCName     = "xpch-e2e-google"
	googleDB          = "app_google"
	googleRole        = "app_google_reader"
)

// Token timing of the scenario. The fake issues tokens with a TTL of
// googleTokenTTL and a clock googleClockOffset behind, which cancels
// ClickHouse's default verifier_leeway of 60 s: ClickHouse accepts each token
// for about googleTokenTTL after it was issued (plus up to the processor's
// token_cache_lifetime once accepted), as it would accept a token of that
// lifetime without leeway. The provider's connections live
// googleConnMaxLifetime, so it dials, and presents its token, several times
// per token lifetime: with Google's 1 h tokens and the default 30 min
// connection lifetime the ratio is the same. The Google client library
// caches a metadata ID token for 55 minutes whatever its exp, so every
// replacement goes through the 516 retry (design 6.4).
const (
	googleTokenTTL        = 20 * time.Second
	googleClockOffset     = -60 * time.Second
	googleConnMaxLifetime = 15 * time.Second
	// googleRefreshWindow spans several token lifetimes.
	googleRefreshWindow = 6 * googleTokenTTL
)

// Google service accounts of the scenario (all examples). The provider's own
// account is the fake's --email (test/fakegcp/deploy/fakegcp.yaml).
var (
	googleWorkload = fakegcp.Identity{Email: "app-reporter@example-project.iam.gserviceaccount.com", UniqueID: "100000000000000000301"}
	// googleRecreated has the workload's email and a new unique ID, as after
	// the service account was deleted and created again.
	googleRecreated = fakegcp.Identity{Email: googleWorkload.Email, UniqueID: "100000000000000000302"}
	// googleAttacker is a service account of another project; googleIntruder
	// one of the right project whose name the policy allows, but for which
	// no ClickHouse user exists.
	googleAttacker = fakegcp.Identity{Email: "attacker@other-project.iam.gserviceaccount.com", UniqueID: "100000000000000000666"}
	googleIntruder = fakegcp.Identity{Email: "app-intruder@example-project.iam.gserviceaccount.com", UniqueID: "100000000000000000667"}
)

// jwtLike matches anything that looks like a JWT or a piece of one (base64url
// of a JSON object starts with "eyJ").
var jwtLike = regexp.MustCompile(`eyJ[A-Za-z0-9_.-]*`)

// redactTokens replaces everything that looks like a JWT with [REDACTED].
func redactTokens(s string) string { return jwtLike.ReplaceAllString(s, "[REDACTED]") }

// fakeGCP drives the fake metadata server through its control API, reached
// with a port-forward (the control API has no Service outside the cluster).
type fakeGCP struct {
	base string
	http *http.Client
}

func connectFakeGCP(t *testing.T) *fakeGCP {
	t.Helper()
	pod := readyPod(t, nsFakeGCP, "app.kubernetes.io/name=fakegcp")
	f := &fakeGCP{base: "http://" + portForward(t, nsFakeGCP, pod, fakeGCPControl) + fakegcp.ControlPrefix, http: &http.Client{Timeout: 30 * time.Second}}
	eventually(t, time.Minute, "the fake's control API", func() (bool, string) {
		var st fakegcp.State
		if err := f.call(http.MethodGet, "state", nil, &st); err != nil {
			return false, err.Error()
		}
		return true, ""
	})
	return f
}

// call sends one control request. Responses never hold a token except that
// of mint, which callers keep to themselves.
func (f *fakeGCP) call(method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, f.base+path, body)
	if err != nil {
		return err
	}
	resp, err := f.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck // Read-only.
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s %s: %s %s", method, path, resp.Status, redactTokens(string(b)))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(b, out)
}

func (f *fakeGCP) mustCall(t *testing.T, method, path string, in, out any) {
	t.Helper()
	if err := f.call(method, path, in, out); err != nil {
		t.Fatalf("fake control API: %v", err)
	}
}

func (f *fakeGCP) state(t *testing.T) fakegcp.State {
	t.Helper()
	var st fakegcp.State
	f.mustCall(t, http.MethodGet, "state", nil, &st)
	return st
}

func (f *fakeGCP) requests(t *testing.T) []fakegcp.Request {
	t.Helper()
	var rs []fakegcp.Request
	f.mustCall(t, http.MethodGet, "requests", nil, &rs)
	return rs
}

func (f *fakeGCP) issued(t *testing.T) []fakegcp.Issued {
	t.Helper()
	var is []fakegcp.Issued
	f.mustCall(t, http.MethodGet, "issued", nil, &is)
	return is
}

// identityRequests counts the ID token requests the fake answered with 200.
func (f *fakeGCP) identityRequests(t *testing.T) int {
	t.Helper()
	n := 0
	for _, r := range f.requests(t) {
		if r.Path == fakegcp.IdentityPath && r.Status == http.StatusOK {
			n++
		}
	}
	return n
}

// jwksFetchesSince counts the JWKS fetches answered with 200 after the first
// skip requests of the log.
func (f *fakeGCP) jwksFetchesSince(t *testing.T, skip int) int {
	t.Helper()
	rs := f.requests(t)
	n := 0
	for _, r := range rs[min(skip, len(rs)):] {
		if r.Path == fakegcp.JWKSPath && r.Status == http.StatusOK {
			n++
		}
	}
	return n
}

// mint signs a token for id with the audience; kid "" is the active key. The
// token is never logged.
func (f *fakeGCP) mint(t *testing.T, id fakegcp.Identity, audience, kid string, fault fakegcp.Fault) string {
	t.Helper()
	var out struct {
		Token string `json:"token"`
	}
	f.mustCall(t, http.MethodPost, "mint", map[string]string{
		"audience": audience, "email": id.Email, "uniqueId": id.UniqueID, "format": "full", "kid": kid, "fault": string(fault),
	}, &out)
	return out.Token
}

func (f *fakeGCP) settings(t *testing.T, ttl, offset time.Duration) {
	t.Helper()
	f.mustCall(t, http.MethodPost, "settings", map[string]string{"ttl": ttl.String(), "clockOffset": offset.String()}, nil)
}

func (f *fakeGCP) fault(t *testing.T, fault fakegcp.Fault) {
	t.Helper()
	f.mustCall(t, http.MethodPost, "fault", map[string]any{"fault": fault, "count": 0}, nil)
}

func (f *fakeGCP) outage(t *testing.T, target fakegcp.Target, mode fakegcp.OutageMode) {
	t.Helper()
	f.mustCall(t, http.MethodPost, "outage", map[string]any{"target": target, "mode": mode, "count": 0}, nil)
}

func (f *fakeGCP) keys(t *testing.T, action, kid string) string {
	t.Helper()
	var out struct {
		KID string `json:"kid"`
	}
	f.mustCall(t, http.MethodPost, "keys", map[string]string{"action": action, "kid": kid}, &out)
	return out.KID
}

// googleTenant is the GoogleIDToken scenario: the provider logs in to
// Antalya as a Google service account with an ID token from the fake
// metadata server (Application Default Credentials, as on GKE with Workload
// Identity), validated by the google_jwt processor against the fake's JWKS.
type googleTenant struct {
	srv      *server
	fake     *fakeGCP
	audience string
	// email is the provider's Google service account.
	email string
	// cacheLifetime is the processor's jwks_cache_lifetime (and
	// token_cache_lifetime).
	cacheLifetime time.Duration
	cpc           *xpv2.ProviderConfigReference

	db        *v1alpha1.Database
	role      *v1alpha1.Role
	user      *v1alpha1.User
	grant     *v1alpha1.Grant
	roleGrant *v1alpha1.RoleGrant
	policyMRs []resource.Managed
	// comments counts the comment changes of the database.
	comments int
	// created is every object the scenario created, for cleanup.
	created []client.Object
}

// create creates obj with the harness label and remembers it for cleanup.
func (g *googleTenant) create(t *testing.T, obj client.Object) {
	t.Helper()
	create(t, obj)
	g.created = append(g.created, obj)
}

func (g *googleTenant) mrs() []resource.Managed {
	return []resource.Managed{g.db, g.role, g.user, g.grant, g.roleGrant}
}

func (g *googleTenant) meta(name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Namespace: nsGoogle, Name: name}
}

// testGoogleIDToken runs the GoogleIDToken scenario. It runs in parallel with
// the scenarios of the other servers: it uses its own namespace,
// ClusterProviderConfig and names, and only the fake's faults affect it.
func testGoogleIDToken(t *testing.T) {
	g := &googleTenant{
		audience: os.Getenv(envGoogleAudience),
		cpc:      &xpv2.ProviderConfigReference{Kind: v1alpha1.ClusterProviderConfigKind, Name: googleCPCName},
	}
	if g.audience == "" {
		t.Fatalf("%s is not set (run make e2e)", envGoogleAudience)
	}
	g.srv = connectServer(t, "antalya")
	g.fake = connectFakeGCP(t)
	// Whatever happens, leave a fake that answers and no Google MR behind:
	// make e2e-cleanup deletes MRs while the provider runs, and the Antalya
	// tenant's deletion checks every app_ object on the server.
	t.Cleanup(func() { g.cleanup(t) })
	for _, step := range []struct {
		name string
		fn   func(*testing.T)
	}{
		{"Setup", g.setup},
		{"Lifecycle", g.lifecycle},
		{"WorkloadLogin", g.workloadLogin},
		{"Policy", g.policy},
		{"TokenRefresh", g.tokenRefresh},
		{"KeyRotation", g.keyRotation},
		{"MetadataOutage", g.metadataOutage},
		{"FaultyToken", g.faultyToken},
		{"AttackerToken", g.attackerToken},
		{"Deletion", g.deletion},
	} {
		if !t.Run(step.name, step.fn) {
			return
		}
	}
}

// cleanup restores the fake and deletes what a failed scenario left. It does
// not fail the test.
func (g *googleTenant) cleanup(t *testing.T) {
	if err := g.fake.call(http.MethodPost, "reset", nil, nil); err != nil {
		t.Logf("reset the fake: %v", err)
	}
	ctx := context.Background()
	// The database would refuse its deletion while the workload's table is
	// there.
	if err := g.srv.admin.Exec(ctx, "DROP TABLE IF EXISTS "+googleDB+".events SYNC"); err != nil {
		t.Logf("drop the table of %s: %v", googleDB, err)
	}
	// MRs first (the config, created first, last), so that the provider can
	// still log in to delete what they own.
	left := slices.Clone(g.created)
	slices.Reverse(left)
	for _, obj := range left {
		if err := kube.Delete(ctx, obj); err != nil && !kerrors.IsNotFound(err) {
			t.Logf("delete %T %s: %v", obj, obj.GetName(), err)
		}
	}
	deadline := time.Now().Add(3 * time.Minute)
	for _, obj := range left {
		for {
			err := kube.Get(ctx, client.ObjectKeyFromObject(obj), obj)
			if kerrors.IsNotFound(err) {
				break
			}
			if time.Now().After(deadline) {
				t.Logf("%T %s is still there after the scenario", obj, obj.GetName())
				break
			}
			time.Sleep(2 * time.Second)
		}
	}
}

// setup checks what hack/e2e/run.sh configured for the scenario, switches
// the fake to short-lived tokens and creates the ClusterProviderConfig.
func (g *googleTenant) setup(t *testing.T) {
	// The provider runs with GCE_METADATA_HOST set to the fake's Service,
	// the value that test/fakegcp/deploy/provider-runtime.yaml documents.
	drc := &pkgv1beta1.DeploymentRuntimeConfig{}
	readYAMLDocument(t, "../../test/fakegcp/deploy/provider-runtime.yaml", "DeploymentRuntimeConfig", drc)
	want := envValue(drc.Spec.DeploymentTemplate.Spec.Template.Spec.Containers, "package-runtime", "GCE_METADATA_HOST")
	if want != fakeGCPHost {
		t.Fatalf("provider-runtime.yaml sets GCE_METADATA_HOST=%q, want %q", want, fakeGCPHost)
	}
	pods, err := clientset.CoreV1().Pods(nsCrossplane).List(context.Background(), metav1.ListOptions{LabelSelector: "pkg.crossplane.io/provider=" + providerName})
	if err != nil || len(pods.Items) == 0 {
		t.Fatalf("provider pods: %d, %v", len(pods.Items), err)
	}
	for _, p := range pods.Items {
		if got := envValue(p.Spec.Containers, "package-runtime", "GCE_METADATA_HOST"); got != want {
			t.Errorf("provider pod %s runs with GCE_METADATA_HOST=%q, want %q", p.Name, got, want)
		}
	}

	// The google_jwt processor is the production one (as the renderer that
	// the docs are checked against prints it) with only jwks_uri, the
	// audience and the cache lifetimes changed, next to the processor of the
	// Kubernetes tokens.
	cm := &corev1.ConfigMap{}
	if err := kube.Get(context.Background(), client.ObjectKey{Namespace: nsClickHouse, Name: "antalya-config"}, cm); err != nil {
		t.Fatal(err)
	}
	var got struct {
		TokenProcessors map[string]map[string]any `json:"token_processors"`
	}
	if err := yaml.Unmarshal([]byte(cm.Data["token_processors.yaml"]), &got); err != nil {
		t.Fatal(err)
	}
	proc, ok := got.TokenProcessors[fakegcp.GoogleProcessorName]
	if _, k8s := got.TokenProcessors["kubernetes"]; !ok || !k8s {
		t.Fatalf("token processors %v, want kubernetes and %s", keys(got.TokenProcessors), fakegcp.GoogleProcessorName)
	}
	lifetime, _ := proc["jwks_cache_lifetime"].(float64)
	g.cacheLifetime = time.Duration(lifetime) * time.Second
	if g.cacheLifetime <= 0 || g.cacheLifetime > 30*time.Second {
		t.Fatalf("jwks_cache_lifetime %v; the scenario needs a short one", proc["jwks_cache_lifetime"])
	}
	rendered := fakegcp.GoogleProcessor{
		Name: fakegcp.GoogleProcessorName, JWKSURI: "http://" + fakeGCPHost + fakegcp.JWKSPath, Audience: g.audience,
		JWKSCacheLifetime: g.cacheLifetime, TokenCacheLifetime: g.cacheLifetime,
	}.YAML()
	var wantProc struct {
		TokenProcessors map[string]map[string]any `json:"token_processors"`
	}
	if err := yaml.Unmarshal([]byte(rendered), &wantProc); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(wantProc.TokenProcessors[fakegcp.GoogleProcessorName], proc); diff != "" {
		t.Fatalf("the google_jwt processor differs from fakegcp.GoogleProcessor.YAML (-want +got):\n%s", diff)
	}

	// The provider's Google service account is the fake's identity, a jwt
	// user pinned to google_jwt in users.d.
	st := g.fake.state(t)
	g.email = st.Identity.Email
	usersD := secretData(t, nsClickHouse, "antalya-users-d", "provider-clickhouse.yaml")
	var users struct {
		Users map[string]struct {
			JWT struct {
				Processor string `json:"processor"`
			} `json:"jwt"`
		} `json:"users"`
	}
	if err := yaml.Unmarshal(usersD, &users); err != nil {
		t.Fatal(err)
	}
	if u, ok := users.Users[g.email]; !ok || u.JWT.Processor != fakegcp.GoogleProcessorName {
		t.Fatalf("users.d has no jwt user %q pinned to %s", g.email, fakegcp.GoogleProcessorName)
	}
	t.Logf("provider service account %s, audience %s, key %s, cache lifetimes %s", g.email, g.audience, st.ActiveKID, g.cacheLifetime)

	// Short-lived tokens from the first one on (see googleTokenTTL).
	g.fake.settings(t, googleTokenTTL, googleClockOffset)

	port := int32(9000)
	cpc := &v1alpha1.ClusterProviderConfig{ObjectMeta: metav1.ObjectMeta{Name: googleCPCName}}
	cpc.Spec.ConnectionSpec = v1alpha1.ConnectionSpec{
		Endpoint: v1alpha1.Endpoint{Host: g.srv.host, Port: &port},
		// Plain native, like the other Antalya configs; production keeps TLS.
		AllowInsecureTransport: true,
		Pool:                   &v1alpha1.Pool{ConnMaxLifetime: &metav1.Duration{Duration: googleConnMaxLifetime}},
	}
	cpc.Spec.Credentials = v1alpha1.ClusterCredentials{
		Source:        v1alpha1.CredentialsSourceGoogleIDToken,
		GoogleIDToken: &v1alpha1.ClusterGoogleIDTokenCredentials{Audience: g.audience},
	}
	cpc.Spec.Policy = v1alpha1.ClusterPolicy{
		AllowedNamespaces:     []string{nsGoogle},
		DatabaseNamePatterns:  []string{"app_[a-z0-9_]+"},
		UserNamePatterns:      []string{`app-[a-z0-9-]+@example-project\.iam\.gserviceaccount\.com`},
		RoleNamePatterns:      []string{"app_[a-z0-9_]+"},
		GrantableRoles:        []string{googleRole},
		AllowedAuthentication: []v1alpha1.AuthenticationMethod{v1alpha1.AuthenticationMethodJWT},
		AllowedJWTProcessors:  []string{fakegcp.GoogleProcessorName},
	}
	g.create(t, cpc)
}

// lifecycle creates a Database, a Role, a JWT User for a workload service
// account, a Grant and a RoleGrant on the GoogleIDToken config and checks the
// server, and which identity ran every statement.
func (g *googleTenant) lifecycle(t *testing.T) {
	s := g.srv
	g.db = &v1alpha1.Database{ObjectMeta: g.meta("google")}
	g.db.Spec.ProviderConfigReference = g.cpc
	g.db.Spec.ForProvider.Name = new(googleDB)
	g.db.Spec.ForProvider.Comment = new("google v1")
	g.role = &v1alpha1.Role{ObjectMeta: g.meta("reader")}
	g.role.Spec.ProviderConfigReference = g.cpc
	g.role.Spec.ForProvider.Name = new(googleRole)
	// Pinned to the processor and to the service account's unique ID: an
	// email can be reused by a new service account after the old one was
	// deleted.
	g.user = &v1alpha1.User{ObjectMeta: g.meta("reporter")}
	g.user.Spec.ProviderConfigReference = g.cpc
	g.user.Spec.ForProvider.Name = new(googleWorkload.Email)
	g.user.Spec.ForProvider.DefaultDatabase = new(googleDB)
	g.user.Spec.ForProvider.Authentication.JWT = &v1alpha1.JWTAuthentication{
		Processor: fakegcp.GoogleProcessorName,
		Claims:    &apiextensionsv1.JSON{Raw: []byte(`{"sub":"` + googleWorkload.UniqueID + `"}`)},
	}
	g.user.Spec.WriteConnectionSecretToReference = &xpv2.LocalSecretReference{Name: "reporter-clickhouse"}
	g.grant = &v1alpha1.Grant{ObjectMeta: g.meta("reader-google")}
	g.grant.Spec.ProviderConfigReference = g.cpc
	g.grant.Spec.ForProvider = v1alpha1.GrantParameters{
		Privileges:  []string{"SELECT", "INSERT"},
		DatabaseRef: &xpv2.NamespacedReference{Name: "google"},
		RoleRef:     &xpv2.NamespacedReference{Name: "reader"},
	}
	g.roleGrant = &v1alpha1.RoleGrant{ObjectMeta: g.meta("reporter-reader")}
	g.roleGrant.Spec.ProviderConfigReference = g.cpc
	g.roleGrant.Spec.ForProvider = v1alpha1.RoleGrantParameters{
		MemberOfRef: &xpv2.NamespacedReference{Name: "reader"},
		UserRef:     &xpv2.NamespacedReference{Name: "reporter"},
	}
	for _, mg := range g.mrs() {
		g.create(t, mg)
	}
	waitReady(t, g.mrs()...)

	for _, c := range []struct {
		mg         resource.Managed
		kind, name string
	}{{g.db, kindDatabase, googleDB}, {g.role, kindRole, googleRole}, {g.user, kindUser, googleWorkload.Email}} {
		id := s.uuid(t, c.kind, c.name)
		if id == "" || id != c.mg.GetAnnotations()[v1alpha1.AnnotationOwnedID] {
			t.Errorf("%s %s: server UUID %q, owned-id %q", c.kind, c.name, id, c.mg.GetAnnotations()[v1alpha1.AnnotationOwnedID])
		}
		if c.kind != kindDatabase {
			if st := s.storage(t, c.kind, c.name); st != ch.StorageLocalDirectory {
				t.Errorf("%s %s is stored in %q, want %s", c.kind, c.name, st, ch.StorageLocalDirectory)
			}
		}
	}
	if got := s.comment(t, googleDB); got != "google v1" {
		t.Errorf("comment of %s: %q", googleDB, got)
	}
	show := s.showCreate(t, kindUser, googleWorkload.Email)
	wantShow := "CREATE USER `" + googleWorkload.Email + "` IDENTIFIED WITH jwt PROCESSOR 'google_jwt' CLAIMS '{\"sub\":\"" + googleWorkload.UniqueID + "\"}'"
	if !strings.HasPrefix(show, wantShow) || !strings.Contains(show, "DEFAULT DATABASE "+googleDB) {
		t.Errorf("SHOW CREATE USER:\n%s\nwant it to start with\n%s\nand set DEFAULT DATABASE %s", show, wantShow, googleDB)
	}
	granted := []string{"INSERT " + googleDB + ".*", "SELECT " + googleDB + ".*"}
	if got := s.grantsOf(t, kindRole, googleRole); !slices.Equal(got, granted) {
		t.Errorf("grants of %s: %v, want %v", googleRole, got, granted)
	}
	if ok, admin := s.member(t, googleWorkload.Email, googleRole); !ok || admin {
		t.Errorf("membership of %s in %s: %t, admin option %t", googleWorkload.Email, googleRole, ok, admin)
	}
	g.ranAsProvider(t)

	// The provider asked the metadata server for nothing but the project ID
	// and format=full ID tokens with the configured audience.
	g.checkMetadataRequests(t)
	g.setComment(t)
}

// ranAsProvider checks that every statement of the MRs on the GoogleIDToken
// config ran as the provider's Google service account
// (system.query_log.user, by log_comment).
func (g *googleTenant) ranAsProvider(t *testing.T) {
	t.Helper()
	g.srv.exec(t, "SYSTEM FLUSH LOGS")
	users := g.srv.strings(t, "SELECT DISTINCT user FROM system.query_log WHERE log_comment LIKE ? ORDER BY user", "%/"+nsGoogle+"/%")
	if !slices.Equal(users, []string{g.email}) {
		t.Errorf("the MRs of %s ran as %q, want only %q", googleCPCName, users, g.email)
	}
	for _, mg := range g.mrs() {
		lc := kindOf(mg) + "/" + nsGoogle + "/" + mg.GetName()
		if n := g.srv.count(t, "SELECT count() FROM system.query_log WHERE log_comment = ? AND type = 'QueryFinish'", lc); n == 0 {
			t.Errorf("%s ran no statement", lc)
		}
	}
}

// kindOf returns the kind of an MR as in log_comment.
func kindOf(mg resource.Managed) string {
	switch mg.(type) {
	case *v1alpha1.Database:
		return "Database"
	case *v1alpha1.Role:
		return "Role"
	case *v1alpha1.User:
		return "User"
	case *v1alpha1.Grant:
		return "Grant"
	case *v1alpha1.RoleGrant:
		return "RoleGrant"
	}
	return fmt.Sprintf("%T", mg)
}

// checkMetadataRequests checks every metadata request the fake served: only
// the project ID and format=full ID tokens with the configured audience
// (what the Google library sends for Application Default Credentials), never
// GCE detection or anything else.
func (g *googleTenant) checkMetadataRequests(t *testing.T) {
	t.Helper()
	ids := 0
	for _, r := range g.fake.requests(t) {
		switch r.Path {
		case fakegcp.IdentityPath:
			ids++
			q, _ := url.ParseQuery(r.RawQuery)
			if q.Get("audience") != g.audience || q.Get("format") != "full" {
				t.Errorf("identity request %q, want audience=%s and format=full", r.RawQuery, g.audience)
			}
		case fakegcp.MetadataPrefix + "project/project-id", fakegcp.JWKSPath:
		default:
			t.Errorf("unexpected request %s %s (status %d)", r.Method, r.Path, r.Status)
		}
	}
	if ids == 0 {
		t.Error("the provider never asked the fake for an ID token")
	}
}

// setComment changes the database's comment and waits until the server has
// it: a reconcile that writes, as the provider's identity.
func (g *googleTenant) setComment(t *testing.T) {
	t.Helper()
	g.comments++
	want := fmt.Sprintf("google v%d", g.comments+1)
	eventually(t, time.Minute, "updating the Database", func() (bool, string) {
		db := &v1alpha1.Database{}
		if err := kube.Get(context.Background(), client.ObjectKeyFromObject(g.db), db); err != nil {
			return false, err.Error()
		}
		db.Spec.ForProvider.Comment = new(want)
		if err := kube.Update(context.Background(), db); err != nil {
			return false, err.Error()
		}
		return true, ""
	})
	start := time.Now()
	eventually(t, 2*time.Minute, "comment "+want+" on the server", func() (bool, string) {
		c := g.srv.comment(t, googleDB)
		return c == want, "comment " + c
	})
	t.Logf("comment %q reached the server after %s", want, time.Since(start).Round(100*time.Millisecond))
}

// workloadLogin logs in as the workload's service account with an ID token
// minted for its email, and checks that it can do exactly what the Grant and
// the RoleGrant give it, and that tokens of a recreated account or for
// another audience are refused.
func (g *googleTenant) workloadLogin(t *testing.T) {
	s := g.srv
	sec := &corev1.Secret{}
	if err := kube.Get(context.Background(), client.ObjectKey{Namespace: nsGoogle, Name: "reporter-clickhouse"}, sec); err != nil {
		t.Fatalf("connection Secret: %v", err)
	}
	for k, v := range map[string]string{
		v1alpha1.ConnectionKeyUsername:     googleWorkload.Email,
		v1alpha1.ConnectionKeyEndpoint:     s.host,
		v1alpha1.ConnectionKeyPort:         "9000",
		v1alpha1.ConnectionKeyProtocol:     string(v1alpha1.ProtocolNative),
		v1alpha1.ConnectionKeyTLS:          "false",
		v1alpha1.ConnectionKeyJWTProcessor: fakegcp.GoogleProcessorName,
	} {
		if got := string(sec.Data[k]); got != v {
			t.Errorf("connection Secret key %s = %q, want %q", k, got, v)
		}
	}
	if _, ok := sec.Data[v1alpha1.ConnectionKeyPassword]; ok {
		t.Error("the connection Secret of a JWT user has a password key")
	}

	s.exec(t, "CREATE TABLE "+googleDB+".events (id UInt64, what String) ENGINE = MergeTree ORDER BY id")
	conn, err := g.login(t, g.fake.mint(t, googleWorkload, g.audience, "", fakegcp.FaultNone))
	if err != nil {
		t.Fatalf("the workload's login with its ID token: %v", err)
	}
	if who, db := whoami(t, conn); who != googleWorkload.Email || db != googleDB {
		t.Fatalf("logged in as %q in %q, want %q in %q", who, db, googleWorkload.Email, googleDB)
	}
	if err := conn.Exec(context.Background(), "INSERT INTO "+googleDB+".events VALUES (1, 'login')"); err != nil {
		t.Errorf("INSERT: %v", err)
	}
	expectSelect(t, conn, "SELECT count() FROM "+googleDB+".events")
	for _, stmt := range []string{
		"CREATE TABLE " + googleDB + ".extra (x UInt8) ENGINE = Memory",
		"DROP TABLE " + googleDB + ".events",
		"DROP DATABASE " + googleDB,
		"CREATE USER `app-evil@example-project.iam.gserviceaccount.com`",
		"CREATE ROLE app_evil",
		"GRANT SELECT ON " + googleDB + ".* TO `" + googleWorkload.Email + "`",
	} {
		expectDenied(t, conn, stmt)
	}
	for what, token := range map[string]string{
		"a token of the recreated service account (same email, new unique ID)": g.fake.mint(t, googleRecreated, g.audience, "", fakegcp.FaultNone),
		"a token of the workload for another audience":                         g.fake.mint(t, googleWorkload, "https://other.example.com", "", fakegcp.FaultNone),
	} {
		g.refused(t, what, token)
	}
}

// login connects to Antalya with a JWT.
func (g *googleTenant) login(t *testing.T, token string) (driver.Conn, error) {
	t.Helper()
	return g.srv.open(t, &chgo.Options{GetJWT: func(context.Context) (string, error) { return token, nil }})
}

// refused checks that a login with token fails with 516.
func (g *googleTenant) refused(t *testing.T, what, token string) {
	t.Helper()
	_, err := g.login(t, token)
	if code := exceptionCode(err); code != 516 {
		t.Errorf("%s: code %d (%v), want 516 (AUTHENTICATION_FAILED)", what, code, err)
	}
}

// policy checks the ClusterProviderConfig's policy: a user of another
// project and a password user are refused with PolicyViolation and run no
// statement.
func (g *googleTenant) policy(t *testing.T) {
	outsider := &v1alpha1.User{ObjectMeta: g.meta("outsider")}
	outsider.Spec.ProviderConfigReference = g.cpc
	outsider.Spec.ForProvider.Name = new(googleAttacker.Email)
	outsider.Spec.ForProvider.Authentication.JWT = &v1alpha1.JWTAuthentication{Processor: fakegcp.GoogleProcessorName}
	password := &v1alpha1.User{ObjectMeta: g.meta("password")}
	password.Spec.ProviderConfigReference = g.cpc
	password.Spec.ForProvider.Name = new("app-password@example-project.iam.gserviceaccount.com")
	password.Spec.ForProvider.Authentication.Password = &v1alpha1.PasswordAuthentication{Generate: &v1alpha1.GeneratePassword{}}
	password.Spec.WriteConnectionSecretToReference = &xpv2.LocalSecretReference{Name: "password-clickhouse"}
	g.policyMRs = []resource.Managed{outsider, password}
	for _, mg := range g.policyMRs {
		g.create(t, mg)
	}
	for _, mg := range g.policyMRs {
		waitMR(t, mg, 2*time.Minute, "refused with PolicyViolation", refusedWith(reason.PolicyViolation))
	}
	for _, name := range []string{googleAttacker.Email, "app-password@example-project.iam.gserviceaccount.com"} {
		if id := g.srv.uuid(t, kindUser, name); id != "" {
			t.Errorf("user %s exists although the policy refuses it", name)
		}
	}
	for _, mg := range g.policyMRs {
		if n := g.srv.statements(t, "User/"+nsGoogle+"/"+mg.GetName(), false); n != 0 {
			t.Errorf("User %s ran %d statements that change something", mg.GetName(), n)
		}
	}
	deleteAndWait(t, 2*time.Minute, objects(g.policyMRs...)...)
}

// syncWatch samples MRs every second in the background and records every
// time one of them is not Synced.
type syncWatch struct {
	stop chan struct{}
	done chan struct{}
	mu   sync.Mutex
	bad  []string
}

func watchSynced(mrs ...resource.Managed) *syncWatch {
	w := &syncWatch{stop: make(chan struct{}), done: make(chan struct{})}
	copies := make([]resource.Managed, 0, len(mrs))
	for _, mg := range mrs {
		copies = append(copies, mg.DeepCopyObject().(resource.Managed)) //nolint:forcetypeassert // A copy of an MR is an MR.
	}
	go func() {
		defer close(w.done)
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			for _, mg := range copies {
				if err := kube.Get(context.Background(), client.ObjectKeyFromObject(mg), mg); err != nil {
					continue
				}
				synced := mg.GetCondition(xpv2.TypeSynced)
				// A conflicting update of the MR (the test changes specs
				// meanwhile) is not an authentication problem.
				if synced.Status == corev1.ConditionFalse && !strings.Contains(synced.Message, "the object has been modified") {
					w.mu.Lock()
					w.bad = append(w.bad, fmt.Sprintf("%s %s at %s: %s", kindOf(mg), mg.GetName(), time.Now().Format(time.TimeOnly), redactTokens(synced.Message)))
					w.mu.Unlock()
				}
			}
			select {
			case <-w.stop:
				return
			case <-t.C:
			}
		}
	}()
	return w
}

// Stop ends the watch and returns what it saw.
func (w *syncWatch) Stop() []string {
	close(w.stop)
	<-w.done
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.bad)
}

func (g *googleTenant) mustStaySynced(t *testing.T, w *syncWatch, what string) {
	t.Helper()
	if bad := w.Stop(); len(bad) > 0 {
		t.Errorf("%s: MRs were not Synced %d times, first: %s", what, len(bad), bad[0])
	}
}

// tokenRefresh runs reconciles that write across several token lifetimes and
// checks that they keep succeeding, and that the provider fetched fresh
// tokens from the fake meanwhile.
func (g *googleTenant) tokenRefresh(t *testing.T) {
	before := g.fake.identityRequests(t)
	issuedBefore := len(g.fake.issued(t))
	w := watchSynced(g.mrs()...)
	start := time.Now()
	for time.Since(start) < googleRefreshWindow {
		g.setComment(t)
		time.Sleep(googleTokenTTL / 2)
	}
	g.mustStaySynced(t, w, "token refresh")
	for _, mg := range g.mrs() {
		waitMR(t, mg, time.Minute, "Ready and Synced after the token refresh", readyAndSynced)
	}
	fresh := g.fake.identityRequests(t) - before
	t.Logf("the provider fetched %d ID tokens in %s (tokens accepted for about %s)", fresh, time.Since(start).Round(time.Second), googleTokenTTL)
	if fresh < 2 {
		t.Errorf("the provider fetched %d ID tokens in %s, want at least 2: it did not replace its expiring tokens", fresh, time.Since(start).Round(time.Second))
	}
	for _, is := range g.fake.issued(t)[issuedBefore:] {
		if is.Via != "metadata" {
			continue
		}
		if is.Email != g.email || is.Audience != g.audience || is.Expiry.Sub(is.IssuedAt) != googleTokenTTL || is.Fault != fakegcp.FaultNone {
			t.Errorf("issued token %+v, want %s for %s with a TTL of %s", is, g.email, g.audience, googleTokenTTL)
		}
	}
	g.ranAsProvider(t)
}

// keyRotation rotates the fake's signing key as Google does: the new key is
// published first and used only once ClickHouse has fetched it (after
// jwks_cache_lifetime), and the old key is retired once the provider holds a
// token of the new one. The provider keeps working throughout, and a token
// of the retired key is refused afterwards.
func (g *googleTenant) keyRotation(t *testing.T) {
	old := g.fake.state(t).ActiveKID
	w := watchSynced(g.mrs()...)

	kid := g.fake.keys(t, "add", "")
	g.awaitJWKSFetch(t, "with the new key")
	g.fake.keys(t, "activate", kid)
	start := time.Now()
	eventually(t, 3*time.Minute, "the provider holding a token of the new key", func() (bool, string) {
		for _, is := range g.fake.issued(t) {
			if is.Via == "metadata" && is.Kid == kid {
				return true, ""
			}
		}
		return false, "no metadata token of " + kid + " yet"
	})
	t.Logf("the provider fetched a token of the new key %s after %s", kid, time.Since(start).Round(time.Second))
	g.setComment(t)

	g.fake.keys(t, "retire", old)
	g.awaitJWKSFetch(t, "without the old key")
	// A new token of the retired key is refused (no cached validation can
	// hold it), one of the new key accepted.
	g.refused(t, "a token signed with the retired key", g.fake.mint(t, googleWorkload, g.audience, old, fakegcp.FaultNone))
	if _, err := g.login(t, g.fake.mint(t, googleWorkload, g.audience, kid, fakegcp.FaultNone)); err != nil {
		t.Errorf("a token of the new key: %v", err)
	}
	// The provider's own token of the old key, if it still had one, is now
	// refused on its next connection; it keeps working with a new token.
	time.Sleep(g.cacheLifetime + googleConnMaxLifetime)
	g.setComment(t)
	g.mustStaySynced(t, w, "key rotation")
	if st := g.fake.state(t); st.ActiveKID != kid || slices.Contains(st.PublishedKIDs, old) {
		t.Errorf("fake state: active %s, published %v", st.ActiveKID, st.PublishedKIDs)
	}
	g.ranAsProvider(t)
}

// awaitJWKSFetch waits until ClickHouse has fetched the JWKS twice since the
// call: the first fetch may have been answered just before a change of the
// keys, the second one cannot.
func (g *googleTenant) awaitJWKSFetch(t *testing.T, what string) {
	t.Helper()
	skip := len(g.fake.requests(t))
	start := time.Now()
	eventually(t, 2*time.Minute, "ClickHouse fetching the JWKS "+what, func() (bool, string) {
		n := g.fake.jwksFetchesSince(t, skip)
		return n >= 2, fmt.Sprintf("%d fetches", n)
	})
	t.Logf("ClickHouse fetched the JWKS %s within %s (jwks_cache_lifetime %s)", what, time.Since(start).Round(time.Second), g.cacheLifetime)
}

// failing returns a check that Synced is False with a message that contains
// every one of substrs.
func failing(substrs ...string) func(resource.Managed) bool {
	return func(mg resource.Managed) bool {
		synced := mg.GetCondition(xpv2.TypeSynced)
		if synced.Status != corev1.ConditionFalse {
			return false
		}
		for _, s := range substrs {
			if !strings.Contains(synced.Message, s) {
				return false
			}
		}
		return true
	}
}

// cleanMessage fails unless the Synced message of mg is a single line of
// reasonable length without anything that looks like a token or any of
// forbidden.
func cleanMessage(t *testing.T, mg resource.Managed, forbidden ...string) {
	t.Helper()
	msg := mg.GetCondition(xpv2.TypeSynced).Message
	switch {
	case jwtLike.MatchString(msg):
		t.Errorf("%s %s: the message contains a token: %s", kindOf(mg), mg.GetName(), redactTokens(msg))
	case strings.ContainsAny(msg, "\r\n"):
		t.Errorf("%s %s: the message spans several lines: %q", kindOf(mg), mg.GetName(), msg)
	case len(msg) > 1024:
		t.Errorf("%s %s: the message is %d bytes long", kindOf(mg), mg.GetName(), len(msg))
	}
	for _, f := range forbidden {
		if strings.Contains(msg, f) {
			t.Errorf("%s %s: the message contains %q: %s", kindOf(mg), mg.GetName(), f, msg)
		}
	}
}

// recovered waits until every MR is Ready and Synced again and a write goes
// through.
func (g *googleTenant) recovered(t *testing.T, what string) {
	t.Helper()
	start := time.Now()
	for _, mg := range g.mrs() {
		waitMR(t, mg, 4*time.Minute, "Ready and Synced after "+what, readyAndSynced)
	}
	t.Logf("every MR recovered %s after %s", time.Since(start).Round(time.Second), what)
	g.setComment(t)
}

// metadataOutage makes the fake's metadata endpoints fail with 500: once the
// provider's short-lived token is refused, it cannot get a new one, and the
// MRs report a clean AuthenticationFailed without the response body. They
// recover once the metadata server answers again.
func (g *googleTenant) metadataOutage(t *testing.T) {
	g.fake.outage(t, fakegcp.TargetMetadata, fakegcp.Outage500)
	start := time.Now()
	for _, mg := range g.mrs() {
		waitMR(t, mg, 4*time.Minute, "failing with AuthenticationFailed during the metadata outage",
			failing(string(reason.AuthenticationFailed), "GoogleIDToken", "500"))
		// The body's second line names details that must stay out.
		cleanMessage(t, mg, "must not reach a managed resource")
	}
	t.Logf("every MR failed %s after the outage began: %s", time.Since(start).Round(time.Second), g.db.GetCondition(xpv2.TypeSynced).Message)
	failed := 0
	for _, r := range g.fake.requests(t) {
		if strings.HasPrefix(r.Note, "outage") {
			failed++
		}
	}
	if failed == 0 {
		t.Error("the fake answered no request with the outage")
	}
	g.fake.outage(t, fakegcp.TargetMetadata, fakegcp.OutageNone)
	g.recovered(t, "the metadata outage")
}

// faultyToken makes the fake issue tokens for another audience: ClickHouse
// refuses them, the provider retries once with a fresh token (also faulty)
// and the MRs report a clean AuthenticationFailed. They recover once the
// fake issues good tokens again.
func (g *googleTenant) faultyToken(t *testing.T) {
	issuedBefore := len(g.fake.issued(t))
	g.fake.fault(t, fakegcp.FaultWrongAudience)
	for _, mg := range g.mrs() {
		waitMR(t, mg, 4*time.Minute, "failing with AuthenticationFailed while the tokens are for another audience",
			failing(string(reason.AuthenticationFailed)))
		cleanMessage(t, mg)
	}
	t.Logf("with tokens for another audience: %s", g.db.GetCondition(xpv2.TypeSynced).Message)
	faulty := 0
	for _, is := range g.fake.issued(t)[issuedBefore:] {
		if is.Via == "metadata" && is.Fault == fakegcp.FaultWrongAudience {
			faulty++
		}
	}
	if faulty < 2 {
		t.Errorf("the fake issued %d tokens for another audience, want at least 2 (a token and the fresh one of the 516 retry)", faulty)
	}
	g.fake.fault(t, fakegcp.FaultNone)
	g.recovered(t, "the faulty tokens")
}

// attackerToken presents valid tokens of the fake's key and the right
// audience for service accounts without a ClickHouse user, and a forged
// token for the provider's own account: every login is refused, and nothing
// runs as those accounts.
func (g *googleTenant) attackerToken(t *testing.T) {
	g.refused(t, "a token of another project's service account", g.fake.mint(t, googleAttacker, g.audience, "", fakegcp.FaultNone))
	g.refused(t, "a token of a service account whose name the policy allows but that has no user", g.fake.mint(t, googleIntruder, g.audience, "", fakegcp.FaultNone))
	provider := fakegcp.Identity{Email: g.email, UniqueID: "100000000000000000999"}
	g.refused(t, "a token for the provider's account signed with a key that is not in the JWKS", g.fake.mint(t, provider, g.audience, "", fakegcp.FaultUnknownKid))
	g.srv.exec(t, "SYSTEM FLUSH LOGS")
	for _, name := range []string{googleAttacker.Email, googleIntruder.Email} {
		if id := g.srv.uuid(t, kindUser, name); id != "" {
			t.Errorf("user %s exists", name)
		}
		if n := g.srv.count(t, "SELECT count() FROM system.query_log WHERE user = ?", name); n != 0 {
			t.Errorf("%d queries ran as %s", n, name)
		}
	}
}

// deletion deletes the MRs: the Grant and the RoleGrant revoke, the database
// (once its table is gone), the role and the user are dropped, every
// statement ran as the provider's service account, and the config can be
// deleted.
func (g *googleTenant) deletion(t *testing.T) {
	s := g.srv
	deleteAndWait(t, 2*time.Minute, g.grant, g.roleGrant)
	if got := s.grantsOf(t, kindRole, googleRole); len(got) != 0 {
		t.Errorf("grants of %s after deleting the Grant: %v", googleRole, got)
	}
	if ok, _ := s.member(t, googleWorkload.Email, googleRole); ok {
		t.Errorf("%s is still a member of %s after deleting the RoleGrant", googleWorkload.Email, googleRole)
	}
	s.exec(t, "DROP TABLE "+googleDB+".events SYNC")
	deleteAndWait(t, 3*time.Minute, g.db, g.role, g.user)
	for _, c := range [][2]string{{kindDatabase, googleDB}, {kindRole, googleRole}, {kindUser, googleWorkload.Email}} {
		if id := s.uuid(t, c[0], c[1]); id != "" {
			t.Errorf("%s %s still exists", c[0], c[1])
		}
	}
	s.exec(t, "SYSTEM FLUSH LOGS")
	users := s.strings(t, "SELECT DISTINCT user FROM system.query_log WHERE log_comment LIKE ? ORDER BY user", "%/"+nsGoogle+"/%")
	if !slices.Equal(users, []string{g.email}) {
		t.Errorf("the MRs of %s ran as %q, want only %q", googleCPCName, users, g.email)
	}
	for _, lc := range []string{"Grant/" + nsGoogle + "/reader-google", "RoleGrant/" + nsGoogle + "/reporter-reader"} {
		if n := s.count(t, "SELECT count() FROM system.query_log WHERE log_comment = ? AND type = 'QueryFinish' AND query_kind = 'Revoke'", lc); n == 0 {
			t.Errorf("%s never revoked", lc)
		}
	}
	g.checkMetadataRequests(t)
	deleteAndWait(t, 2*time.Minute, &v1alpha1.ClusterProviderConfig{ObjectMeta: metav1.ObjectMeta{Name: googleCPCName}})
}

// readYAMLDocument decodes the document of the given kind of a file of the
// repository strictly.
func readYAMLDocument(t *testing.T, path, kind string, into any) {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // A file of the repository.
	if err != nil {
		t.Fatal(err)
	}
	for doc := range strings.SplitSeq(string(b), "\n---\n") {
		var meta struct {
			Kind string `json:"kind"`
		}
		if err := yaml.Unmarshal([]byte(doc), &meta); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if meta.Kind != kind {
			continue
		}
		if err := yaml.UnmarshalStrict([]byte(doc), into); err != nil {
			t.Fatalf("%s: %s: %v", path, kind, err)
		}
		return
	}
	t.Fatalf("%s has no %s", path, kind)
}

// envValue returns the value of an environment variable of a container.
func envValue(containers []corev1.Container, container, name string) string {
	for _, c := range containers {
		if c.Name != container {
			continue
		}
		for _, e := range c.Env {
			if e.Name == name {
				return e.Value
			}
		}
	}
	return ""
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
