// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/infrabay/provider-clickhouse/internal/reason"
)

// jwt builds an unsigned token with the given claims. The signature segment
// is a placeholder: the pre-send checks never verify signatures.
func jwt(t *testing.T, claims map[string]any) string {
	t.Helper()
	h := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	b, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return h + "." + base64.RawURLEncoding.EncodeToString(b) + ".c2ln"
}

func TestChecker(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	c := Checker{Audience: "clickhouse", Forbidden: []string{"https://kubernetes.default.svc"}, Now: func() time.Time { return now }}
	cases := map[string]struct {
		token string
		ok    bool
	}{
		"AudienceString":   {jwt(t, map[string]any{"aud": "clickhouse", "exp": now.Unix() + 60}), true},
		"AudienceArray":    {jwt(t, map[string]any{"aud": []string{"x", "clickhouse"}}), true},
		"WrongAudience":    {jwt(t, map[string]any{"aud": "other"}), false},
		"NoAudience":       {jwt(t, map[string]any{"sub": "x"}), false},
		"APIServerAud":     {jwt(t, map[string]any{"aud": []string{"clickhouse", "https://kubernetes.default.svc"}}), false},
		"Expired":          {jwt(t, map[string]any{"aud": "clickhouse", "exp": now.Unix() - 120}), false},
		"WithinLeeway":     {jwt(t, map[string]any{"aud": "clickhouse", "exp": now.Unix() - 30}), true},
		"NotAJWT":          {"opaque-token", false},
		"TwoSegments":      {"a.b", false},
		"PayloadNotBase64": {"a.!!!.c", false},
		"PayloadNotJSON":   {"a." + base64.RawURLEncoding.EncodeToString([]byte("nope")) + ".c", false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := c.Check(tc.token)
			if (err == nil) != tc.ok {
				t.Fatalf("Check() = %v, want ok=%t", err, tc.ok)
			}
			if err != nil && strings.Contains(err.Error(), tc.token) {
				t.Errorf("error leaks the token: %v", err)
			}
		})
	}
}

func TestFileSourceRereadsAndRotates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	c := Checker{Audience: "clickhouse"}
	s, err := NewFileSource(path, c)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if _, err := s.Token(ctx); !reason.Is(err, reason.InvalidConfig) {
		t.Fatalf("missing file: %v", err)
	}
	t1 := jwt(t, map[string]any{"aud": "clickhouse", "n": 1})
	if err := os.WriteFile(path, []byte(t1+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Token(ctx); err != nil || got != t1 {
		t.Fatalf("Token() = %q, %v", got, err)
	}
	// The kubelet rotates the file: the next call returns the new token.
	t2 := jwt(t, map[string]any{"aud": "clickhouse", "n": 2})
	if err := os.WriteFile(path, []byte(t2), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Token(ctx); err != nil || got != t2 {
		t.Fatalf("Token() after rotation = %q, %v", got, err)
	}
	// Empty, oversized and wrong-audience files are refused.
	for name, content := range map[string]string{
		"Empty":     "  \n",
		"Oversized": strings.Repeat("a", maxTokenBytes+10),
		"Audience":  jwt(t, map[string]any{"aud": "kubernetes"}),
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := s.Token(ctx)
		var ae *Error
		if !reason.Is(err, reason.InvalidConfig) || !errors.As(err, &ae) {
			t.Errorf("%s: Token() = %v, want an InvalidConfig auth.Error", name, err)
		}
	}
	s.Invalidate() // no-op
}

func TestFileSourceRefusesServiceAccountPath(t *testing.T) {
	for _, p := range []string{
		"/var/run/secrets/kubernetes.io/serviceaccount/token",
		"/var/run/secrets/kubernetes.io/serviceaccount/../serviceaccount/token",
		"/var/run/secrets/kubernetes.io/serviceaccount",
		"relative/token",
	} {
		if _, err := NewFileSource(p, Checker{Audience: "a"}); !reason.Is(err, reason.InvalidConfig) {
			t.Errorf("NewFileSource(%q) = %v, want InvalidConfig", p, err)
		}
	}
	if _, err := NewFileSource("/var/run/secrets/clickhouse/token", Checker{Audience: "a"}); err != nil {
		t.Errorf("a projected token path must be allowed: %v", err)
	}
}

func TestSecretSourceRereads(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	t1 := jwt(t, map[string]any{"aud": "clickhouse", "n": 1})
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "tok"}, Data: map[string][]byte{"token": []byte(t1)}}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sec).Build()
	ctx := context.Background()

	s := NewSecretSource(kube, types.NamespacedName{Namespace: "ns", Name: "tok"}, "token", Checker{Audience: "clickhouse"})
	if got, err := s.Token(ctx); err != nil || got != t1 {
		t.Fatalf("Token() = %q, %v", got, err)
	}
	t2 := jwt(t, map[string]any{"aud": "clickhouse", "n": 2})
	sec.Data["token"] = []byte(t2)
	if err := kube.Update(ctx, sec); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Token(ctx); err != nil || got != t2 {
		t.Fatalf("Token() after rotation = %q, %v", got, err)
	}
	missing := NewSecretSource(kube, types.NamespacedName{Namespace: "ns", Name: "tok"}, "other", Checker{Audience: "clickhouse"})
	if _, err := missing.Token(ctx); !reason.Is(err, reason.InvalidConfig) {
		t.Errorf("missing key: %v", err)
	}
	gone := NewSecretSource(kube, types.NamespacedName{Namespace: "ns", Name: "nope"}, "token", Checker{Audience: "clickhouse"})
	if _, err := gone.Token(ctx); !reason.Is(err, reason.InvalidConfig) {
		t.Errorf("missing Secret: %v", err)
	}
}

type fakeOAuth struct {
	mu  sync.Mutex
	n   int
	err error
}

func (f *fakeOAuth) Token() (*oauth2.Token, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	f.n++
	exp := time.Now().Add(time.Hour)
	return &oauth2.Token{AccessToken: "id-token-" + string(rune('0'+f.n)), Expiry: exp}, nil
}

func TestGoogleSourceCachesAndInvalidates(t *testing.T) {
	built := 0
	var underlying *fakeOAuth
	s := NewGoogleSourceWith(func() (oauth2.TokenSource, error) {
		built++
		underlying = &fakeOAuth{}
		return underlying, nil
	})
	ctx := context.Background()
	a, err := s.Token(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := s.Token(ctx)
	if a != b || underlying.n != 1 || built != 1 {
		t.Fatalf("tokens must be cached until expiry: %q %q n=%d built=%d", a, b, underlying.n, built)
	}
	s.Invalidate()
	c, _ := s.Token(ctx)
	if built != 2 || c != "id-token-1" {
		t.Fatalf("Invalidate must rebuild the source: built=%d token=%q", built, c)
	}
}

func TestGoogleSourceScrubsErrors(t *testing.T) {
	body := `{"error":"invalid_grant","error_description":"secret-response-body"}`
	s := NewGoogleSourceWith(func() (oauth2.TokenSource, error) {
		return &fakeOAuth{err: errors.New("oauth2: cannot fetch token: 400 Bad Request\nResponse: " + body)}, nil
	})
	_, err := s.Token(context.Background())
	if !reason.Is(err, reason.AuthenticationFailed) {
		t.Fatalf("Token() = %v, want AuthenticationFailed", err)
	}
	if strings.Contains(err.Error(), "secret-response-body") {
		t.Fatalf("response body leaked: %v", err)
	}
}

func TestValidateServiceAccountKey(t *testing.T) {
	cases := map[string]struct {
		key string
		ok  bool
	}{
		"Valid":           {`{"type":"service_account","token_uri":"https://oauth2.googleapis.com/token","universe_domain":"googleapis.com"}`, true},
		"ValidMinimal":    {`{"type":"service_account"}`, true},
		"ExternalAccount": {`{"type":"external_account","token_url":"https://sts.googleapis.com/v1/token"}`, false},
		"AuthorizedUser":  {`{"type":"authorized_user"}`, false},
		"EvilTokenURI":    {`{"type":"service_account","token_uri":"https://attacker.example.com/token"}`, false},
		"OtherUniverse":   {`{"type":"service_account","universe_domain":"example.com"}`, false},
		"NotJSON":         {`nope`, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := ValidateServiceAccountKey([]byte(tc.key))
			if (err == nil) != tc.ok {
				t.Fatalf("ValidateServiceAccountKey() = %v, want ok=%t", err, tc.ok)
			}
			if err != nil && !reason.Is(err, reason.InvalidConfig) {
				t.Errorf("reason = %v, want InvalidConfig", err)
			}
		})
	}
	if _, err := NewGoogleSource("aud", []byte(`{"type":"external_account"}`)); !reason.Is(err, reason.InvalidConfig) {
		t.Errorf("NewGoogleSource(external_account) = %v, want InvalidConfig", err)
	}
	if _, err := NewGoogleSource("", nil); !reason.Is(err, reason.InvalidConfig) {
		t.Errorf("NewGoogleSource without audience = %v", err)
	}
}

func TestKubernetesAPIAudiences(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if got := KubernetesAPIAudiences(path); got != nil {
		t.Errorf("missing file: %v", got)
	}
	_ = os.WriteFile(path, []byte(jwt(t, map[string]any{"aud": []string{"https://kubernetes.default.svc.cluster.local", "k3s"}})), 0o600)
	if got := KubernetesAPIAudiences(path); len(got) != 2 || got[1] != "k3s" {
		t.Errorf("KubernetesAPIAudiences() = %v", got)
	}
}
